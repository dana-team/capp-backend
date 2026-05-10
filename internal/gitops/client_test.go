package gitops

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dana-team/capp-backend/internal/config"
	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// initBareRepo creates a bare "remote" repo, seeds it with an initial commit,
// clones it to a working directory, and returns a Client wired to the clone.
func initBareRepo(t *testing.T) (*Client, string) {
	t.Helper()

	// Create and seed the bare "remote" via a temporary init repo.
	bareDir := filepath.Join(t.TempDir(), "remote.git")
	_, err := git.PlainInit(bareDir, true)
	require.NoError(t, err)

	initDir := filepath.Join(t.TempDir(), "init")
	initRepo, err := git.PlainInit(initDir, false)
	require.NoError(t, err)

	_, err = initRepo.CreateRemote(&gitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{bareDir},
	})
	require.NoError(t, err)

	wt, err := initRepo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(initDir, ".gitkeep"), []byte(""), 0o644))
	_, err = wt.Add(".gitkeep")
	require.NoError(t, err)
	_, err = wt.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "test",
			Email: "test@test.com",
			When:  time.Now(),
		},
	})
	require.NoError(t, err)
	require.NoError(t, initRepo.Push(&git.PushOptions{}))

	// Clone from the seeded bare repo.
	cloneDir := filepath.Join(t.TempDir(), "clone")
	clone, err := git.PlainClone(cloneDir, false, &git.CloneOptions{
		URL: bareDir,
	})
	require.NoError(t, err)

	logger, _ := zap.NewDevelopment()
	c := NewClientFromRepo(clone, nil, "master", "sites", logger)

	return c, cloneDir
}

func TestBuildRelPath(t *testing.T) {
	c := &Client{pathPrefix: "sites"}

	tests := []struct {
		name      string
		gitOps    string
		namespace string
		cappName  string
		want      string
	}{
		{
			name:      "standard path",
			gitOps:    "nova",
			namespace: "my-namespace",
			cappName:  "my-capp",
			want:      "sites/nova/my-namespace/my-capp.yaml",
		},
		{
			name:      "different prefix",
			gitOps:    "five",
			namespace: "prod",
			cappName:  "web-app",
			want:      "sites/five/prod/web-app.yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.BuildRelPath(tt.gitOps, tt.namespace, tt.cappName)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBuildRelPath_CustomPrefix(t *testing.T) {
	c := &Client{pathPrefix: "overrides"}
	got := c.BuildRelPath("nova", "ns", "app")
	assert.Equal(t, "overrides/nova/ns/app.yaml", got)
}

func TestPublishValues(t *testing.T) {
	c, cloneDir := initBareRepo(t)
	ctx := context.Background()

	valuesYAML := []byte("image: nginx:1.25\nname: my-capp\n")
	sha, err := c.PublishValues(ctx, "nova", "production", "my-capp", valuesYAML)
	require.NoError(t, err)
	assert.Len(t, sha, 40)

	written, err := os.ReadFile(filepath.Join(cloneDir, "sites", "nova", "production", "my-capp.yaml"))
	require.NoError(t, err)
	assert.Equal(t, valuesYAML, written)
}

func TestPublishValues_Overwrite(t *testing.T) {
	c, cloneDir := initBareRepo(t)
	ctx := context.Background()

	v1 := []byte("image: nginx:1.24\n")
	_, err := c.PublishValues(ctx, "nova", "ns", "app", v1)
	require.NoError(t, err)

	v2 := []byte("image: nginx:1.25\n")
	sha, err := c.PublishValues(ctx, "nova", "ns", "app", v2)
	require.NoError(t, err)
	assert.Len(t, sha, 40)

	written, err := os.ReadFile(filepath.Join(cloneDir, "sites", "nova", "ns", "app.yaml"))
	require.NoError(t, err)
	assert.Equal(t, v2, written)
}

func TestDeleteValues(t *testing.T) {
	c, cloneDir := initBareRepo(t)
	ctx := context.Background()

	valuesYAML := []byte("image: nginx:1.25\n")
	_, err := c.PublishValues(ctx, "nova", "ns", "my-capp", valuesYAML)
	require.NoError(t, err)

	filePath := filepath.Join(cloneDir, "sites", "nova", "ns", "my-capp.yaml")
	_, err = os.Stat(filePath)
	require.NoError(t, err, "file should exist after publish")

	sha, err := c.DeleteValues(ctx, "nova", "ns", "my-capp")
	require.NoError(t, err)
	assert.Len(t, sha, 40)

	_, err = os.Stat(filePath)
	assert.True(t, os.IsNotExist(err), "file should not exist after delete")
}

func TestDeleteValues_NonExistent(t *testing.T) {
	c, _ := initBareRepo(t)
	ctx := context.Background()

	sha, err := c.DeleteValues(ctx, "nova", "ns", "does-not-exist")
	require.NoError(t, err)
	assert.Empty(t, sha, "no-op should return empty SHA")
}

func TestPublishValues_CommitHistory(t *testing.T) {
	c, _ := initBareRepo(t)
	ctx := context.Background()

	_, err := c.PublishValues(ctx, "nova", "ns", "app1", []byte("v1"))
	require.NoError(t, err)
	_, err = c.PublishValues(ctx, "nova", "ns", "app2", []byte("v2"))
	require.NoError(t, err)

	ref, err := c.repo.Head()
	require.NoError(t, err)

	commit, err := c.repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	assert.Contains(t, commit.Message, "publish nova/ns/app2.yaml")
}

func TestDeleteValues_CommitMessage(t *testing.T) {
	c, _ := initBareRepo(t)
	ctx := context.Background()

	_, err := c.PublishValues(ctx, "five", "prod", "web", []byte("v1"))
	require.NoError(t, err)

	_, err = c.DeleteValues(ctx, "five", "prod", "web")
	require.NoError(t, err)

	ref, err := c.repo.Head()
	require.NoError(t, err)

	commit, err := c.repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	assert.Contains(t, commit.Message, "remove five/prod/web.yaml")
}

func TestBuildAuth_Token(t *testing.T) {
	auth, err := buildAuth(config.GitOpsConfig{
		AuthMethod: "token",
		Token:      "my-secret-token",
	})
	require.NoError(t, err)
	assert.NotNil(t, auth)
}

func TestBuildAuth_TokenMissing(t *testing.T) {
	_, err := buildAuth(config.GitOpsConfig{
		AuthMethod: "token",
		Token:      "",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "token is required")
}

func TestBuildAuth_UnsupportedMethod(t *testing.T) {
	_, err := buildAuth(config.GitOpsConfig{
		AuthMethod: "magic",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported authMethod")
}
