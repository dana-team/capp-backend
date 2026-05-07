// Package gitops implements a git client that pushes and deletes per-capp
// Helm values files in a remote GitOps repository. It uses go-git for all
// git operations (clone, fetch, commit, push).
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
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"go.uber.org/zap"
)

// Client manages a local clone of the GitOps repository and provides
// thread-safe operations for syncing and deleting per-capp values files.
type Client struct {
	mu         sync.Mutex
	repo       *git.Repository
	auth       transport.AuthMethod
	branch     string
	pathPrefix string
	logger     *zap.Logger
}

// NewClient clones (or opens an existing clone of) the GitOps repository
// described by cfg into a local directory and returns a ready-to-use Client.
// The clone is stored under cloneDir; if empty a temp directory is used.
func NewClient(cfg config.GitOpsConfig, logger *zap.Logger, cloneDir string) (*Client, error) {
	auth, err := buildAuth(cfg)
	if err != nil {
		return nil, fmt.Errorf("gitops auth: %w", err)
	}

	if cloneDir == "" {
		cloneDir, err = os.MkdirTemp("", "gitops-*")
		if err != nil {
			return nil, fmt.Errorf("create temp dir: %w", err)
		}
	}

	refName := plumbing.NewBranchReferenceName(cfg.Branch)

	repo, err := git.PlainClone(cloneDir, false, &git.CloneOptions{
		URL:           cfg.RepoURL,
		Auth:          auth,
		ReferenceName: refName,
		SingleBranch:  true,
		Depth:         1,
	})
	if err != nil {
		return nil, fmt.Errorf("clone %s: %w", cfg.RepoURL, err)
	}

	return &Client{
		repo:       repo,
		auth:       auth,
		branch:     cfg.Branch,
		pathPrefix: cfg.PathPrefix,
		logger:     logger,
	}, nil
}

// NewClientFromRepo creates a Client backed by an already-opened repository.
// Useful for testing.
func NewClientFromRepo(repo *git.Repository, auth transport.AuthMethod, branch, pathPrefix string, logger *zap.Logger) *Client {
	return &Client{
		repo:       repo,
		auth:       auth,
		branch:     branch,
		pathPrefix: pathPrefix,
		logger:     logger,
	}
}

// BuildRelPath returns the repository-relative path for a capp values file:
// <pathPrefix>/<gitOpsPath>/<namespace>/<cappName>.yaml
func (c *Client) BuildRelPath(gitOpsPath, namespace, cappName string) string {
	return filepath.Join(c.pathPrefix, gitOpsPath, namespace, cappName+".yaml")
}

// SyncValues writes a per-capp values file to the GitOps repository,
// commits it, and pushes to the remote. Returns the commit SHA on success.
func (c *Client) SyncValues(_ context.Context, gitOpsPath, namespace, cappName string, valuesYAML []byte) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.pull(); err != nil {
		return "", fmt.Errorf("pull: %w", err)
	}

	wt, err := c.repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("worktree: %w", err)
	}

	relPath := c.BuildRelPath(gitOpsPath, namespace, cappName)
	absPath := filepath.Join(wt.Filesystem.Root(), relPath)

	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", filepath.Dir(relPath), err)
	}

	if err := os.WriteFile(absPath, valuesYAML, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", relPath, err)
	}

	if _, err := wt.Add(relPath); err != nil {
		return "", fmt.Errorf("stage %s: %w", relPath, err)
	}

	msg := fmt.Sprintf("sync %s/%s/%s.yaml", gitOpsPath, namespace, cappName)
	hash, err := wt.Commit(msg, &git.CommitOptions{
		Author: commitAuthor(),
	})
	if err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}

	if err := c.push(); err != nil {
		c.resetToRemote()
		return "", fmt.Errorf("push: %w", err)
	}

	c.logger.Info("synced values",
		zap.String("path", relPath),
		zap.String("commit", hash.String()),
	)

	return hash.String(), nil
}

// DeleteValues removes a per-capp values file from the GitOps repository,
// commits the removal, and pushes. If the file does not exist the call is a
// no-op and returns ("", nil).
func (c *Client) DeleteValues(_ context.Context, gitOpsPath, namespace, cappName string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.pull(); err != nil {
		return "", fmt.Errorf("pull: %w", err)
	}

	wt, err := c.repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("worktree: %w", err)
	}

	relPath := c.BuildRelPath(gitOpsPath, namespace, cappName)
	absPath := filepath.Join(wt.Filesystem.Root(), relPath)

	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		c.logger.Debug("values file not found, nothing to delete",
			zap.String("path", relPath),
		)
		return "", nil
	}

	if err := os.Remove(absPath); err != nil {
		return "", fmt.Errorf("remove %s: %w", relPath, err)
	}

	if _, err := wt.Remove(relPath); err != nil {
		return "", fmt.Errorf("stage removal %s: %w", relPath, err)
	}

	msg := fmt.Sprintf("remove %s/%s/%s.yaml", gitOpsPath, namespace, cappName)
	hash, err := wt.Commit(msg, &git.CommitOptions{
		Author: commitAuthor(),
	})
	if err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}

	if err := c.push(); err != nil {
		c.resetToRemote()
		return "", fmt.Errorf("push: %w", err)
	}

	c.logger.Info("deleted values",
		zap.String("path", relPath),
		zap.String("commit", hash.String()),
	)

	return hash.String(), nil
}

// pull fetches and fast-forwards the local branch to match the remote.
func (c *Client) pull() error {
	wt, err := c.repo.Worktree()
	if err != nil {
		return err
	}

	err = wt.Pull(&git.PullOptions{
		RemoteName:    "origin",
		ReferenceName: plumbing.NewBranchReferenceName(c.branch),
		Auth:          c.auth,
		SingleBranch:  true,
		Force:         true,
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return err
	}

	return nil
}

// resetToRemote hard-resets the local branch to match origin, discarding any
// unpushed local commits. Called after a push failure to prevent the local
// clone from diverging from the remote.
func (c *Client) resetToRemote() {
	remoteRef, err := c.repo.Reference(
		plumbing.NewRemoteReferenceName("origin", c.branch), true,
	)
	if err != nil {
		c.logger.Warn("reset-to-remote: failed to resolve remote ref", zap.Error(err))
		return
	}

	wt, err := c.repo.Worktree()
	if err != nil {
		c.logger.Warn("reset-to-remote: failed to get worktree", zap.Error(err))
		return
	}

	if err := wt.Reset(&git.ResetOptions{
		Commit: remoteRef.Hash(),
		Mode:   git.HardReset,
	}); err != nil {
		c.logger.Warn("reset-to-remote: hard reset failed", zap.Error(err))
		return
	}

	c.logger.Info("reset local branch to remote after push failure")
}

// push pushes the current branch to the remote.
func (c *Client) push() error {
	return c.repo.Push(&git.PushOptions{
		RemoteName: "origin",
		RefSpecs: []gitconfig.RefSpec{
			gitconfig.RefSpec("refs/heads/" + c.branch + ":refs/heads/" + c.branch),
		},
		Auth: c.auth,
	})
}

func commitAuthor() *object.Signature {
	return &object.Signature{
		Name:  "capp-backend",
		Email: "capp-backend@dana.io",
		When:  time.Now(),
	}
}

func buildAuth(cfg config.GitOpsConfig) (transport.AuthMethod, error) {
	switch cfg.AuthMethod {
	case "token":
		if cfg.Token == "" {
			return nil, fmt.Errorf("gitops token is required when authMethod is \"token\"")
		}
		return &http.BasicAuth{
			Username: "git",
			Password: cfg.Token,
		}, nil
	case "ssh":
		if cfg.SSHKeyPath == "" {
			return nil, fmt.Errorf("sshKeyPath is required when authMethod is \"ssh\"")
		}
		keys, err := ssh.NewPublicKeysFromFile("git", cfg.SSHKeyPath, "")
		if err != nil {
			return nil, fmt.Errorf("load SSH key %s: %w", cfg.SSHKeyPath, err)
		}
		return keys, nil
	default:
		return nil, fmt.Errorf("unsupported authMethod: %q", cfg.AuthMethod)
	}
}
