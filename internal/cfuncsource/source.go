// Package cfuncsource fetches the code of a cfunc function from a git
// repository. Fission cannot clone a private repository, so the backend
// clones with its own credential and hands the code to Fission as an archive.
package cfuncsource

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dana-team/capp-backend/internal/config"
	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
)

const (
	// maxSourceBytes bounds what is read from the folder before compression,
	// so that a large folder is refused before it is held in memory twice.
	maxSourceBytes = 8 << 20
	maxSourceFiles = 2000

	// FetchTimeout bounds one fetch. A handler that fetches must give its
	// response at least this long.
	FetchTimeout = 60 * time.Second

	// maxConcurrentFetches bounds how many repositories are held in memory
	// at one time. A clone is read into memory in full before any size
	// limit can apply, so without it a few requests for a large repository
	// could take all the memory of the backend. It bounds the count only:
	// one clone is still as large as its repository. The wait for a slot
	// counts against FetchTimeout, so slow clones can make other requests
	// fail with ErrBusy.
	maxConcurrentFetches = 4
)

var commitHashPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// zipEpoch is the modification time of every archive entry. A fixed value
// makes the archive of one commit byte-identical on every fetch.
var zipEpoch = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// ErrBusy reports that every fetch slot stayed taken for the whole wait. The
// git server was not contacted, so it is not a failure of that server.
var ErrBusy = errors.New("the backend is cloning other repositories")

// InputError reports a problem with what the client asked for: a URL outside
// the base URL, a ref or path that does not exist, or code that is too large.
type InputError struct{ msg string }

func (e *InputError) Error() string { return e.msg }

func inputErrorf(format string, args ...any) error {
	return &InputError{msg: fmt.Sprintf(format, args...)}
}

// Archive is the code of one function at one commit.
type Archive struct {
	// Zip holds the folder as a zip archive with paths relative to the folder.
	Zip []byte

	// SingleFile holds the content of the only file when the path names one
	// file or a folder with exactly one, and is nil otherwise.
	SingleFile []byte

	// IsFile is true when the path names one file, not a folder.
	IsFile bool

	// Commit is the full hash of the commit the code was read from.
	Commit string
}

// Fetcher clones repositories of one git server with one credential.
type Fetcher struct {
	baseURL *url.URL
	auth    transport.AuthMethod
	slots   chan struct{}
}

// New returns a Fetcher for the configured git server.
func New(cfg config.CfuncGitConfig) (*Fetcher, error) {
	base, err := url.Parse(cfg.BaseURL)
	if err != nil || base.Host == "" {
		return nil, errors.New("cfunc git base URL is not a valid URL")
	}
	return &Fetcher{
		baseURL: base,
		auth:    &githttp.BasicAuth{Username: cfg.Username, Password: cfg.Token},
		slots:   make(chan struct{}, maxConcurrentFetches),
	}, nil
}

// CheckURL returns an InputError unless repoURL is under the base URL. The
// credential is sent to whatever host is cloned, so no other host is allowed.
func (f *Fetcher) CheckURL(repoURL string) error {
	parsed, err := url.Parse(repoURL)
	if err != nil {
		return inputErrorf("source.url is not a valid URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return inputErrorf("source.url must not carry credentials, a query or a fragment")
	}
	basePath := strings.TrimSuffix(f.baseURL.Path, "/")
	underBase := parsed.Scheme == f.baseURL.Scheme &&
		strings.EqualFold(parsed.Host, f.baseURL.Host) &&
		strings.HasPrefix(parsed.Path, basePath+"/") &&
		path.Clean(parsed.Path) == strings.TrimSuffix(parsed.Path, "/")
	if !underBase || len(parsed.Path) <= len(basePath)+1 {
		return inputErrorf("source.url must be a repository under %s", f.baseURL.String())
	}
	return nil
}

// CleanPath returns the path of the folder or file in the form the repository is read with:
// relative, slash-separated, "" for the root. It returns an InputError for a
// path that leaves the repository.
func CleanPath(folder string) (string, error) {
	if strings.Contains(folder, `\`) {
		return "", inputErrorf("source.path must use forward slashes")
	}
	trimmed := strings.Trim(folder, "/")
	if trimmed == "" {
		return "", nil
	}
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == ".." || segment == "." || segment == "" || segment == ".git" {
			return "", inputErrorf("source.path %q must be a file or a folder inside the repository", folder)
		}
	}
	return trimmed, nil
}

// Fetch clones repoURL at ref and returns the folder as an archive. An empty
// ref means the default branch. Problems with the request are InputErrors;
// every other error is a failure to reach or read the git server.
//
// The archive is bounded only by maxSourceBytes of files. The caller checks
// the size of what it stores, because that is the zip for code that is built
// and the file itself for one file that is not.
func (f *Fetcher) Fetch(ctx context.Context, repoURL, ref, folder string) (*Archive, error) {
	if err := f.CheckURL(repoURL); err != nil {
		return nil, err
	}
	cleanFolder, err := CleanPath(folder)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()

	select {
	case f.slots <- struct{}{}:
		defer func() { <-f.slots }()
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", ErrBusy, ctx.Err())
	}

	tree, commit, err := f.checkout(ctx, repoURL, ref)
	if err != nil {
		return nil, err
	}
	return archiveFolder(tree, cleanFolder, commit)
}

// checkout returns the work tree of repoURL at ref and the commit hash.
func (f *Fetcher) checkout(ctx context.Context, repoURL, ref string) (billy.Filesystem, string, error) {
	remote := git.NewRemote(memory.NewStorage(), &gitconfig.RemoteConfig{Name: "origin", URLs: []string{repoURL}})
	refs, err := remote.ListContext(ctx, &git.ListOptions{Auth: f.auth})
	if errors.Is(err, transport.ErrRepositoryNotFound) || errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return nil, "", inputErrorf("source.url names no repository that the backend can read; check the URL and the access of the backend's git token")
	}
	if err != nil {
		return nil, "", fmt.Errorf("list the refs of the repository: %w", err)
	}

	options := &git.CloneOptions{URL: repoURL, Auth: f.auth, Tags: git.NoTags}
	var commit plumbing.Hash
	switch target, found := resolveRef(refs, ref); {
	case found:
		options.ReferenceName = target
		options.SingleBranch = true
		options.Depth = 1
	case commitHashPattern.MatchString(ref):
		// A server does not have to serve a commit that no ref points to in
		// a shallow fetch, so a commit needs the full history.
		commit = plumbing.NewHash(ref)
	default:
		return nil, "", inputErrorf("source.ref %q is not a branch, a tag or a full commit hash of the repository", ref)
	}

	tree := memfs.New()
	repo, err := git.CloneContext(ctx, memory.NewStorage(), tree, options)
	if err != nil {
		return nil, "", fmt.Errorf("clone the repository: %w", err)
	}

	if !commit.IsZero() {
		worktree, err := repo.Worktree()
		if err != nil {
			return nil, "", fmt.Errorf("open the work tree: %w", err)
		}
		if err := worktree.Checkout(&git.CheckoutOptions{Hash: commit, Force: true}); err != nil {
			return nil, "", inputErrorf("source.ref %q is not a commit of the repository", ref)
		}
		return tree, commit.String(), nil
	}

	head, err := repo.Head()
	if err != nil {
		return nil, "", fmt.Errorf("read the cloned commit: %w", err)
	}
	return tree, head.Hash().String(), nil
}

// resolveRef finds the remote reference a ref names. An empty ref names the
// branch HEAD points to.
func resolveRef(refs []*plumbing.Reference, ref string) (plumbing.ReferenceName, bool) {
	if ref == "" {
		for _, candidate := range refs {
			if candidate.Name() == plumbing.HEAD && candidate.Type() == plumbing.SymbolicReference {
				return candidate.Target(), true
			}
		}
		return "", false
	}
	for _, name := range []plumbing.ReferenceName{plumbing.NewBranchReferenceName(ref), plumbing.NewTagReferenceName(ref)} {
		for _, candidate := range refs {
			if candidate.Name() == name {
				return name, true
			}
		}
	}
	return "", false
}

type sourceFile struct {
	name       string
	content    []byte
	executable bool
}

// archiveFolder zips folder of tree with entries in name order. folder can
// also name one file, which gives an archive of that file alone.
func archiveFolder(tree billy.Filesystem, folder, commit string) (*Archive, error) {
	root := folder
	if root == "" {
		root = "/"
	}
	info, err := tree.Lstat(root)
	if err != nil {
		return nil, inputErrorf("source.path %q is not a file or a folder of the repository at commit %s", folder, commit)
	}

	var files []sourceFile
	var total int64
	isFile := false
	switch {
	case info.IsDir():
		if err := collectFiles(tree, root, "", &files, &total); err != nil {
			return nil, err
		}
	case info.Mode().IsRegular() && info.Size() <= maxSourceBytes:
		content, err := readFile(tree, root)
		if err != nil {
			return nil, err
		}
		files = []sourceFile{{name: path.Base(root), content: content, executable: info.Mode()&0o100 != 0}}
		isFile = true
	case info.Mode().IsRegular():
		return nil, inputErrorf("source.path %q holds more than %d bytes", folder, maxSourceBytes)
	default:
		return nil, inputErrorf("source.path %q is a symbolic link or a special file, which a function cannot hold", folder)
	}
	if len(files) == 0 {
		return nil, inputErrorf("source.path %q holds no files at commit %s", folder, commit)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })

	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, file := range files {
		header := &zip.FileHeader{Name: file.name, Method: zip.Deflate, Modified: zipEpoch}
		// Git records one permission bit, and a build script needs it.
		header.SetMode(0o644)
		if file.executable {
			header.SetMode(0o755)
		}
		entry, err := writer.CreateHeader(header)
		if err != nil {
			return nil, fmt.Errorf("archive %s: %w", file.name, err)
		}
		if _, err := entry.Write(file.content); err != nil {
			return nil, fmt.Errorf("archive %s: %w", file.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close the archive: %w", err)
	}

	archive := &Archive{Zip: buffer.Bytes(), Commit: commit, IsFile: isFile}
	if len(files) == 1 {
		archive.SingleFile = files[0].content
	}
	return archive, nil
}

func collectFiles(tree billy.Filesystem, dir, relative string, files *[]sourceFile, total *int64) error {
	entries, err := tree.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if relative == "" && name == ".git" {
			continue
		}
		full := tree.Join(dir, name)
		rel := path.Join(relative, name)
		switch {
		case entry.IsDir():
			if err := collectFiles(tree, full, rel, files, total); err != nil {
				return err
			}
		case !entry.Mode().IsRegular():
			return inputErrorf("%s is a symbolic link or a special file, which a function cannot hold", rel)
		default:
			if len(*files) >= maxSourceFiles {
				return inputErrorf("the folder holds more than %d files", maxSourceFiles)
			}
			*total += entry.Size()
			if *total > maxSourceBytes {
				return inputErrorf("the folder holds more than %d bytes of files", maxSourceBytes)
			}
			content, err := readFile(tree, full)
			if err != nil {
				return err
			}
			*files = append(*files, sourceFile{name: rel, content: content, executable: entry.Mode()&0o100 != 0})
		}
	}
	return nil
}

func readFile(tree billy.Filesystem, name string) ([]byte, error) {
	file, err := tree.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	return content, nil
}
