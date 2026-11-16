package cfuncsource

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/config"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testRepo struct {
	url     string
	first   string
	second  string
	fetcher *Fetcher
}

// newTestRepo builds a repository with two commits on main, a tag on the
// first one and a branch "dev", and a Fetcher whose base URL is its parent
// folder.
func newTestRepo(t *testing.T) testRepo {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "team", "functions")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")},
	})
	require.NoError(t, err)
	worktree, err := repo.Worktree()
	require.NoError(t, err)

	write := func(name, content string) {
		full := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
		_, err := worktree.Add(name)
		require.NoError(t, err)
	}
	commit := func(message string) string {
		hash, err := worktree.Commit(message, &git.CommitOptions{
			Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Unix(1700000000, 0)},
		})
		require.NoError(t, err)
		return hash.String()
	}

	write("hello/main.py", "def main():\n    return 'one'\n")
	write("shop/main.py", "def main():\n    return 'shop'\n")
	write("shop/lib/util.py", "X = 1\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "shop", "build.sh"), []byte("#!/bin/sh\n"), 0o755))
	_, err = worktree.Add("shop/build.sh")
	require.NoError(t, err)
	write("README.md", "functions\n")
	first := commit("first")
	_, err = repo.CreateTag("v1", plumbing.NewHash(first), nil)
	require.NoError(t, err)

	require.NoError(t, worktree.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("dev"), Create: true}))
	write("hello/main.py", "def main():\n    return 'dev'\n")
	commit("dev")
	require.NoError(t, worktree.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("main")}))

	write("hello/main.py", "def main():\n    return 'two'\n")
	second := commit("second")

	baseURL, err := url.Parse("file://" + filepath.ToSlash(base))
	require.NoError(t, err)
	return testRepo{
		url:     "file://" + filepath.ToSlash(dir),
		first:   first,
		second:  second,
		fetcher: &Fetcher{baseURL: baseURL, slots: make(chan struct{}, maxConcurrentFetches)},
	}
}

func unzip(t *testing.T, data []byte) map[string]string {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	files := make(map[string]string, len(reader.File))
	for _, entry := range reader.File {
		handle, err := entry.Open()
		require.NoError(t, err)
		content, err := io.ReadAll(handle)
		require.NoError(t, err)
		require.NoError(t, handle.Close())
		files[entry.Name] = string(content)
	}
	return files
}

func TestFetch_Refs(t *testing.T) {
	repo := newTestRepo(t)

	tests := []struct {
		name       string
		ref        string
		wantCommit string
		wantBody   string
	}{
		{name: "default branch", ref: "", wantCommit: repo.second, wantBody: "two"},
		{name: "branch", ref: "dev", wantBody: "dev"},
		{name: "tag", ref: "v1", wantCommit: repo.first, wantBody: "one"},
		{name: "commit", ref: repo.first, wantCommit: repo.first, wantBody: "one"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			archive, err := repo.fetcher.Fetch(t.Context(), repo.url, tc.ref, "hello")
			require.NoError(t, err)

			if tc.wantCommit != "" {
				assert.Equal(t, tc.wantCommit, archive.Commit)
			}
			assert.Len(t, archive.Commit, 40)
			assert.Contains(t, string(archive.SingleFile), tc.wantBody)
			assert.Equal(t, map[string]string{"main.py": string(archive.SingleFile)}, unzip(t, archive.Zip))
		})
	}
}

func TestFetch_FolderWithSubfolders(t *testing.T) {
	repo := newTestRepo(t)

	archive, err := repo.fetcher.Fetch(t.Context(), repo.url, "main", "/shop/")
	require.NoError(t, err)

	assert.Nil(t, archive.SingleFile, "a folder with more than one file has no single file")
	assert.False(t, archive.IsFile)
	assert.Equal(t, map[string]string{
		"main.py":     "def main():\n    return 'shop'\n",
		"lib/util.py": "X = 1\n",
		"build.sh":    "#!/bin/sh\n",
	}, unzip(t, archive.Zip))

	reader, err := zip.NewReader(bytes.NewReader(archive.Zip), int64(len(archive.Zip)))
	require.NoError(t, err)
	modes := map[string]os.FileMode{}
	for _, entry := range reader.File {
		modes[entry.Name] = entry.Mode().Perm()
	}
	assert.Equal(t, os.FileMode(0o755), modes["build.sh"], "a build script must stay executable")
	assert.Equal(t, os.FileMode(0o644), modes["main.py"])
}

func TestFetch_OneFile(t *testing.T) {
	repo := newTestRepo(t)

	archive, err := repo.fetcher.Fetch(t.Context(), repo.url, "main", "shop/lib/util.py")
	require.NoError(t, err)

	assert.True(t, archive.IsFile)
	assert.Equal(t, "X = 1\n", string(archive.SingleFile))
	assert.Equal(t, map[string]string{"util.py": "X = 1\n"}, unzip(t, archive.Zip))
}

func TestFetch_RootLeavesOutGitFolder(t *testing.T) {
	repo := newTestRepo(t)

	archive, err := repo.fetcher.Fetch(t.Context(), repo.url, "", "")
	require.NoError(t, err)

	files := unzip(t, archive.Zip)
	assert.Contains(t, files, "README.md")
	assert.Contains(t, files, "shop/lib/util.py")
	for name := range files {
		assert.False(t, strings.HasPrefix(name, ".git"), "%s must not be in the archive", name)
	}
}

func TestFetch_SameCommitGivesTheSameBytes(t *testing.T) {
	repo := newTestRepo(t)

	first, err := repo.fetcher.Fetch(t.Context(), repo.url, "main", "shop")
	require.NoError(t, err)
	second, err := repo.fetcher.Fetch(t.Context(), repo.url, "main", "shop")
	require.NoError(t, err)

	assert.Equal(t, first.Zip, second.Zip, "an unchanged commit must not look like new code")
}

func TestFetch_InputErrors(t *testing.T) {
	repo := newTestRepo(t)

	tests := []struct {
		name       string
		url        string
		ref        string
		folder     string
		errContain string
	}{
		{name: "unknown ref", url: repo.url, ref: "nope", folder: "hello", errContain: `source.ref "nope" is not a branch`},
		{name: "unknown commit", url: repo.url, ref: strings.Repeat("a", 40), folder: "hello", errContain: "is not a commit of the repository"},
		{name: "missing folder", url: repo.url, folder: "absent", errContain: `source.path "absent" is not a file or a folder`},
		{name: "path leaves the repository", url: repo.url, folder: "hello/../..", errContain: "must be a file or a folder inside the repository"},
		{name: "git folder", url: repo.url, folder: ".git", errContain: "must be a file or a folder inside the repository"},
		{name: "backslash path", url: repo.url, folder: `hello\x`, errContain: "forward slashes"},
		{name: "url outside the base", url: "file:///somewhere/else", folder: "hello", errContain: "must be a repository under"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := repo.fetcher.Fetch(t.Context(), tc.url, tc.ref, tc.folder)

			var inputErr *InputError
			require.ErrorAs(t, err, &inputErr)
			assert.Contains(t, err.Error(), tc.errContain)
		})
	}
}

func TestFetch_WaitsForAFreeSlot(t *testing.T) {
	repo := newTestRepo(t)
	for range maxConcurrentFetches {
		repo.fetcher.slots <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	_, err := repo.fetcher.Fetch(ctx, repo.url, "", "hello")

	require.ErrorIs(t, err, ErrBusy)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	var inputErr *InputError
	assert.NotErrorAs(t, err, &inputErr, "a busy backend is not the client's mistake")

	<-repo.fetcher.slots
	_, err = repo.fetcher.Fetch(t.Context(), repo.url, "", "hello")
	require.NoError(t, err)
}

func TestFetch_MissingRepositoryIsAnInputError(t *testing.T) {
	repo := newTestRepo(t)

	_, err := repo.fetcher.Fetch(t.Context(), repo.url+"-missing", "", "")

	var inputErr *InputError
	require.ErrorAs(t, err, &inputErr)
	assert.Contains(t, err.Error(), "names no repository that the backend can read")
}

func TestCheckURL(t *testing.T) {
	const token = "s3cr3t-token"
	fetcher, err := New(config.CfuncGitConfig{BaseURL: "https://git.example/group", Token: token, Username: "oauth2"})
	require.NoError(t, err)

	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "repository under the base", url: "https://git.example/group/team/fn.git"},
		{name: "host in another case", url: "https://GIT.example/group/fn"},
		{name: "the base itself", url: "https://git.example/group", wantErr: true},
		{name: "the base with a slash", url: "https://git.example/group/", wantErr: true},
		{name: "sibling with the same prefix", url: "https://git.example/group-evil/fn", wantErr: true},
		{name: "path that climbs out", url: "https://git.example/group/../other/fn", wantErr: true},
		{name: "other host", url: "https://evil.example/group/fn", wantErr: true},
		{name: "host with the base as prefix", url: "https://git.example.evil.io/group/fn", wantErr: true},
		{name: "http", url: "http://git.example/group/fn", wantErr: true},
		{name: "ssh form", url: "git@git.example:group/fn.git", wantErr: true},
		{name: "credentials", url: "https://u:p@git.example/group/fn", wantErr: true},
		{name: "query", url: "https://git.example/group/fn?x=1", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := fetcher.CheckURL(tc.url)

			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			var inputErr *InputError
			require.ErrorAs(t, err, &inputErr)
			assert.NotContains(t, err.Error(), token)
		})
	}
}
