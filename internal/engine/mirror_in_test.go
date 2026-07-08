package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func TestMirrorInCopiesChangedFilesAndSkipsIgnored(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	writeLocal(t, u, "memories/go-style.md", "# fact\n")
	writeLocal(t, u, "scratch.tmp", "never syncs\n")

	manifest := repo.NewManifest()
	stats, snapshot, err := engine.mirrorIn(context.Background(), []repo.Unit{u}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 1 {
		t.Fatalf("Copied = %d, want 1", stats.Copied)
	}
	copied := filepath.Join(checkout, "alpha", "claude", "memories", "go-style.md")
	data, err := os.ReadFile(copied)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "# fact\n" {
		t.Fatalf("checkout content = %q", data)
	}
	if _, err := os.Stat(filepath.Join(checkout, "alpha", "claude", "scratch.tmp")); !os.IsNotExist(err) {
		t.Fatal("ClassIgnore file reached the checkout")
	}
	if !manifest.Has("alpha/claude/memories/go-style.md") {
		t.Fatal("manifest missing the synced path")
	}
	if _, ok := snapshot["alpha/claude/memories/go-style.md"]; !ok {
		t.Fatal("snapshot missing the synced path")
	}
}

func TestMirrorInSecondRunIsNoop(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	writeLocal(t, u, "memories/a.md", "content\n")

	manifest := repo.NewManifest()
	if _, _, err := engine.mirrorIn(context.Background(), []repo.Unit{u}, manifest); err != nil {
		t.Fatal(err)
	}
	stats, _, err := engine.mirrorIn(context.Background(), []repo.Unit{u}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 0 || stats.Deleted != 0 {
		t.Fatalf("second run stats = %+v, want zero copies/deletes", stats)
	}
}

func TestMirrorInDeletesViaManifestOnly(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	writeLocal(t, u, "memories/gone.md", "will be deleted\n")

	manifest := repo.NewManifest()
	ctx := context.Background()
	if _, _, err := engine.mirrorIn(ctx, []repo.Unit{u}, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.commitProjects(ctx, fixedStamp); err != nil {
		t.Fatal(err)
	}

	// Case 1: in manifest + gone locally = deleted here → git rm.
	if err := os.Remove(filepath.Join(u.LocalDir, "memories", "gone.md")); err != nil {
		t.Fatal(err)
	}
	// Case 2: in checkout + NOT in manifest = new from remote → untouched.
	fromRemote := filepath.Join(checkout, "alpha", "claude", "memories", "remote-new.md")
	if err := os.WriteFile(fromRemote, []byte("landed via integrate\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stats, _, err := engine.mirrorIn(ctx, []repo.Unit{u}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deleted != 1 {
		t.Fatalf("Deleted = %d, want 1", stats.Deleted)
	}
	if _, err := os.Stat(filepath.Join(checkout, "alpha", "claude", "memories", "gone.md")); !os.IsNotExist(err) {
		t.Fatal("deleted-here file still in checkout")
	}
	if manifest.Has("alpha/claude/memories/gone.md") {
		t.Fatal("manifest still lists the deleted path")
	}
	if _, err := os.Stat(fromRemote); err != nil {
		t.Fatal("new-from-remote file was wrongly removed:", err)
	}
}

func TestMirrorInRefusesSymlinks(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")

	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("keyset material\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(u.LocalDir, "memories"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(u.LocalDir, "memories", "planted.md")); err != nil {
		t.Fatal(err)
	}

	manifest := repo.NewManifest()
	stats, _, err := engine.mirrorIn(context.Background(), []repo.Unit{u}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 0 || stats.Skipped != 1 {
		t.Fatalf("stats = %+v, want 0 copied / 1 skipped", stats)
	}
	if _, err := os.Stat(filepath.Join(checkout, "alpha", "claude", "memories", "planted.md")); !os.IsNotExist(err) {
		t.Fatal("symlink target content reached the checkout — exfiltration path")
	}
}

func TestMirrorInUnknownProviderIsError(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	bad := repo.Unit{Provider: "gemini", ProjectID: "x", Folder: "alpha", LocalDir: t.TempDir()}
	if _, _, err := engine.mirrorIn(context.Background(), []repo.Unit{bad}, repo.NewManifest()); err == nil {
		t.Fatal("unenrollable provider silently skipped; want loud error")
	}
}
