package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dana-team/capp-backend/internal/config"
	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// Client manages git operations for publishing Capp values to a remote repository.
type Client struct {
	repoURL  string
	branch   string
	auth     transport.AuthMethod
	cloneDir string
	mu       sync.Mutex
}

// NewClient creates a Client from the provided GitOps configuration.
// It sets up authentication (HTTPS token or SSH key) and prepares a
// local directory for the repository clone.
func NewClient(cfg config.GitOpsConfig) (*Client, error) {
	auth, err := buildAuth(cfg)
	if err != nil {
		return nil, fmt.Errorf("gitops: failed to build auth: %w", err)
	}

	cloneDir := filepath.Join(os.TempDir(), "capp-gitops-repo")

	return &Client{
		repoURL:  cfg.RepoURL,
		branch:   cfg.Branch,
		auth:     auth,
		cloneDir: cloneDir,
	}, nil
}

func buildAuth(cfg config.GitOpsConfig) (transport.AuthMethod, error) {
	switch cfg.AuthMethod {
	case "token":
		return &githttp.BasicAuth{
			Username: "git",
			Password: cfg.Token,
		}, nil
	case "ssh":
		keys, err := gitssh.NewPublicKeysFromFile("git", cfg.SSHKeyPath, "")
		if err != nil {
			return nil, fmt.Errorf("failed to load SSH key from %s: %w", cfg.SSHKeyPath, err)
		}
		return keys, nil
	default:
		return nil, fmt.Errorf("unsupported auth method: %s", cfg.AuthMethod)
	}
}

// PublishValues writes a values.yaml file into overrides/<namespace>/<name>/
// in the git repository, commits, and pushes. It returns the commit SHA on success.
func (c *Client) PublishValues(ctx context.Context, namespace, name string, valuesYAML []byte) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	repo, wt, err := c.ensureRepo(ctx)
	if err != nil {
		return "", fmt.Errorf("gitops: failed to prepare repo: %w", err)
	}

	relPath := filepath.Join("overrides", namespace, name, "values.yaml")
	absPath := filepath.Join(c.cloneDir, relPath)
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return "", fmt.Errorf("gitops: mkdir %s: %w", filepath.Dir(absPath), err)
	}
	if err := os.WriteFile(absPath, valuesYAML, 0o644); err != nil {
		return "", fmt.Errorf("gitops: write %s: %w", absPath, err)
	}
	if _, err := wt.Add(relPath); err != nil {
		return "", fmt.Errorf("gitops: git add %s: %w", relPath, err)
	}

	status, err := wt.Status()
	if err != nil {
		return "", fmt.Errorf("gitops: git status: %w", err)
	}
	if status.IsClean() {
		head, err := repo.Head()
		if err != nil {
			return "", fmt.Errorf("gitops: get HEAD: %w", err)
		}
		return head.Hash().String(), nil
	}

	hash, err := wt.Commit(
		fmt.Sprintf("publish capp %s/%s", namespace, name),
		&git.CommitOptions{
			Author: &object.Signature{
				Name:  "capp-backend",
				Email: "capp-backend@dana-team.io",
				When:  time.Now(),
			},
		},
	)
	if err != nil {
		return "", fmt.Errorf("gitops: commit: %w", err)
	}

	if err := repo.PushContext(ctx, &git.PushOptions{Auth: c.auth}); err != nil {
		return "", fmt.Errorf("gitops: push: %w", err)
	}

	return hash.String(), nil
}

// ensureRepo clones the repository if it doesn't exist locally, or fetches
// and resets to the latest remote state if it does.
func (c *Client) ensureRepo(ctx context.Context) (*git.Repository, *git.Worktree, error) {
	refName := plumbing.NewBranchReferenceName(c.branch)

	repo, err := git.PlainOpen(c.cloneDir)
	if err != nil {
		repo, err = git.PlainCloneContext(ctx, c.cloneDir, false, &git.CloneOptions{
			URL:           c.repoURL,
			Auth:          c.auth,
			ReferenceName: refName,
			SingleBranch:  true,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("clone: %w", err)
		}
		wt, err := repo.Worktree()
		return repo, wt, err
	}

	err = repo.FetchContext(ctx, &git.FetchOptions{
		Auth: c.auth,
		RefSpecs: []gitconfig.RefSpec{
			gitconfig.RefSpec(fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", c.branch, c.branch)),
		},
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return nil, nil, fmt.Errorf("fetch: %w", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		return nil, nil, fmt.Errorf("worktree: %w", err)
	}

	remoteRef := plumbing.NewRemoteReferenceName("origin", c.branch)
	ref, err := repo.Reference(remoteRef, true)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve remote ref: %w", err)
	}

	if err := wt.Reset(&git.ResetOptions{
		Commit: ref.Hash(),
		Mode:   git.HardReset,
	}); err != nil {
		return nil, nil, fmt.Errorf("reset: %w", err)
	}

	return repo, wt, nil
}
