package engine

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// seedSyncedFile establishes "this host synced rel before": content in
// LocalDir and checkout, manifest entry, snapshot entry. Routes through
// engine.unitDir/the folder+provider+repo_subdir key shape so it stays
// correct for RepoSubdir-mapped units too (a no-op change in shape for
// every existing RepoSubdir=="" caller).
func seedSyncedFile(t *testing.T, engine *Engine, u repo.Unit, manifest *repo.Manifest, snapshot localSnapshot, rel, content string) {
	t.Helper()
	writeLocal(t, u, rel, content)
	dest := filepath.Join(engine.unitDir(u), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entry, err := repo.HashFile(filepath.Join(u.LocalDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	repoRel := path.Join(u.Folder, u.Provider, u.RepoSubdir, rel)
	if err := manifest.Set(repoRel, entry); err != nil {
		t.Fatal(err)
	}
	snapshot[repoRel] = entry
}

func TestMirrorOutAppliesRemoteAddsAndManifestGatedDeletions(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	manifest, snapshot := repo.NewManifest(), localSnapshot{}

	// Previously synced file that the remote has since deleted:
	// present locally + in manifest, absent from checkout.
	seedSyncedFile(t, engine, u, manifest, snapshot, "memories/deleted-remotely.md", "old\n")
	if err := os.Remove(filepath.Join(engine.layout.UnitDir("alpha", "claude"), "memories", "deleted-remotely.md")); err != nil {
		t.Fatal(err)
	}
	// New-from-remote file: in checkout, absent locally and from manifest.
	remoteNew := filepath.Join(engine.layout.UnitDir("alpha", "claude"), "memories", "remote-new.md")
	if err := os.MkdirAll(filepath.Dir(remoteNew), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remoteNew, []byte("from B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Never-synced local-absent checkout file in an UNMANIFESTED unit
	// dir on another machine's project must NOT be deleted locally —
	// covered by the manifest gate below.

	stats, err := engine.mirrorOut(context.Background(), []repo.Unit{u}, manifest, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 1 || stats.Deleted != 1 {
		t.Fatalf("stats = %+v, want 1 copied / 1 deleted", stats)
	}
	if _, err := os.Stat(filepath.Join(u.LocalDir, "memories", "deleted-remotely.md")); !os.IsNotExist(err) {
		t.Fatal("remote deletion not applied locally")
	}
	data, err := os.ReadFile(filepath.Join(u.LocalDir, "memories", "remote-new.md"))
	if err != nil || string(data) != "from B\n" {
		t.Fatalf("remote-new content = %q, %v", data, err)
	}
	if manifest.Has("alpha/claude/memories/deleted-remotely.md") {
		t.Fatal("manifest still lists deleted path")
	}
	if !manifest.Has("alpha/claude/memories/remote-new.md") {
		t.Fatal("manifest missing newly mirrored-out path")
	}
}

func TestMirrorOutNeverOverwritesMidCycleLocalEdits(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	manifest, snapshot := repo.NewManifest(), localSnapshot{}
	seedSyncedFile(t, engine, u, manifest, snapshot, "memories/racy.md", "synced\n")

	// Remote updated the checkout copy...
	dest := filepath.Join(engine.layout.UnitDir("alpha", "claude"), "memories", "racy.md")
	if err := os.WriteFile(dest, []byte("remote change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// ...while a live agent session ALSO wrote locally mid-cycle.
	writeLocal(t, u, "memories/racy.md", "local mid-cycle edit\n")

	stats, err := engine.mirrorOut(context.Background(), []repo.Unit{u}, manifest, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Skipped != 1 || stats.Copied != 0 {
		t.Fatalf("stats = %+v, want 1 skipped / 0 copied", stats)
	}
	data, err := os.ReadFile(filepath.Join(u.LocalDir, "memories", "racy.md"))
	if err != nil || string(data) != "local mid-cycle edit\n" {
		t.Fatalf("local edit destroyed: %q, %v", data, err)
	}
}

func TestMirrorOutDeletionSkippedWhenLocalChanged(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	manifest, snapshot := repo.NewManifest(), localSnapshot{}
	seedSyncedFile(t, engine, u, manifest, snapshot, "memories/edited.md", "synced\n")

	// Remote deleted it, but the user edited it locally mid-cycle.
	if err := os.Remove(filepath.Join(engine.layout.UnitDir("alpha", "claude"), "memories", "edited.md")); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, u, "memories/edited.md", "user's new thoughts\n")

	stats, err := engine.mirrorOut(context.Background(), []repo.Unit{u}, manifest, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deleted != 0 || stats.Skipped != 1 {
		t.Fatalf("stats = %+v, want deletion skipped", stats)
	}
	if _, err := os.Stat(filepath.Join(u.LocalDir, "memories", "edited.md")); err != nil {
		t.Fatal("user's local edit was deleted:", err)
	}
}

// TestMirrorOutRefusesGitMetaFiles pins the outbound guard (spec §5): a
// git-meta file that reaches the checkout via integrate AFTER this cycle's
// mirror-in scrub must never be written into the user's provider dir, and
// must not earn a manifest entry. Next cycle's scrub removes it from the
// checkout.
func TestMirrorOutRefusesGitMetaFiles(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	manifest, snapshot := repo.NewManifest(), localSnapshot{}

	unitDir := engine.layout.UnitDir("alpha", "claude")
	if err := os.MkdirAll(filepath.Join(unitDir, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, "x", ".gitignore"), []byte("memories/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "-m", "poisoned .gitignore in checkout")

	stats, err := engine.mirrorOut(context.Background(), []repo.Unit{u}, manifest, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}

	if stats.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1 (the git-meta file)", stats.Skipped)
	}
	if _, err := os.Stat(filepath.Join(u.LocalDir, "x", ".gitignore")); !os.IsNotExist(err) {
		t.Fatal("git-meta file was written into the provider dir")
	}
	if manifest.Has("alpha/claude/x/.gitignore") {
		t.Fatal("git-meta file got a manifest entry on mirror-out")
	}
}

// TestMirrorOutSeparatesManifestKeysAcrossRepoSubdirUnits is the
// mirror-out counterpart to the mirror-in test of the same shape: two
// units sharing (folder, provider) but different RepoSubdir must not
// let one unit's manifest-gated deletion pass see — and potentially
// drop — the other's ledger entries.
func TestMirrorOutSeparatesManifestKeysAcrossRepoSubdirUnits(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	manifest, snapshot := repo.NewManifest(), localSnapshot{}

	memories := repo.Unit{Provider: "claude", ProjectID: "a", Folder: "shared", LocalDir: t.TempDir(), RepoSubdir: "memories"}
	chronicle := repo.Unit{Provider: "claude", ProjectID: "b", Folder: "shared", LocalDir: t.TempDir(), RepoSubdir: "chronicle"}
	seedSyncedFile(t, engine, memories, manifest, snapshot, "note.md", "memories content\n")
	seedSyncedFile(t, engine, chronicle, manifest, snapshot, "note.md", "chronicle content\n")

	stats, err := engine.mirrorOut(context.Background(), []repo.Unit{memories, chronicle}, manifest, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 0 || stats.Deleted != 0 {
		t.Fatalf("stats = %+v, want a no-op — nothing changed since seeding", stats)
	}
	if !manifest.Has("shared/claude/memories/note.md") {
		t.Fatal("manifest lost the memories unit's entry — key collided with chronicle's")
	}
	if !manifest.Has("shared/claude/chronicle/note.md") {
		t.Fatal("manifest lost the chronicle unit's entry — key collided with memories'")
	}
}

func TestMirrorOutWithheldForDegradedProjects(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	manifest, snapshot := repo.NewManifest(), localSnapshot{}
	remoteNew := filepath.Join(engine.layout.UnitDir("alpha", "claude"), "memories", "remote-new.md")
	if err := os.MkdirAll(filepath.Dir(remoteNew), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remoteNew, []byte("from B\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stats, err := engine.mirrorOut(context.Background(), []repo.Unit{u}, manifest, snapshot, map[string]bool{"alpha": true})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 0 {
		t.Fatalf("stats = %+v; degraded project must not mirror out", stats)
	}
	if _, err := os.Stat(filepath.Join(u.LocalDir, "memories", "remote-new.md")); !os.IsNotExist(err) {
		t.Fatal("degraded project mirrored out anyway")
	}
}
