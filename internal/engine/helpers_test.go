package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/providertest"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// fixedNow keeps commit messages deterministic across a test.
var fixedNow = func() time.Time { return time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC) }

const fixedStamp = "2026-07-08T12:00:00Z"

func testRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	fake := providertest.New("claude", provider.ScopePerProject, []provider.Pattern{
		{Glob: "MEMORY.md", Class: provider.ClassDerivedIndex},
		{Glob: "memories/**", Class: provider.ClassFact},
		{Glob: "*.tmp", Class: provider.ClassIgnore},
	})
	registry, err := provider.NewRegistry(fake)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func mustGit(t *testing.T, dir string, args ...string) gitx.Result {
	t.Helper()
	res, err := gitx.Run(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v\nstderr: %s", args, err, res.Stderr)
	}
	return res
}

// newTestCheckout builds the two-repo shape every engine test needs: a
// bare "remote" and a cloned checkout seeded the way Phase-3 init will
// seed it (attributes file committed on main, pushed upstream).
func newTestCheckout(t *testing.T) (checkout, bare string) {
	t.Helper()
	root := t.TempDir()
	bare = filepath.Join(root, "remote.git")
	checkout = filepath.Join(root, "memories")
	mustGit(t, root, "init", "--bare", "-b", "main", bare)
	mustGit(t, root, "clone", bare, checkout)
	mustGit(t, checkout, "config", "user.name", "engine-test")
	mustGit(t, checkout, "config", "user.email", "engine-test@example.invalid")
	if err := repo.WriteAttributes(repo.NewLayout(checkout), testRegistry(t)); err != nil {
		t.Fatal(err)
	}
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "-m", "init: repo skeleton")
	mustGit(t, checkout, "push", "-u", "origin", "main")
	return checkout, bare
}

func newTestEngine(t *testing.T, checkout string) *Engine {
	t.Helper()
	engine, err := New(checkout, "host-a", testRegistry(t), fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

// unit enrolls a provider dir under t.TempDir and returns it.
func unit(t *testing.T, folder string) repo.Unit {
	t.Helper()
	localDir := filepath.Join(t.TempDir(), "project", ".claude", "memory")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return repo.Unit{Provider: "claude", ProjectID: "id-" + folder, Folder: folder, LocalDir: localDir}
}

func writeLocal(t *testing.T, u repo.Unit, rel, content string) {
	t.Helper()
	full := filepath.Join(u.LocalDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
