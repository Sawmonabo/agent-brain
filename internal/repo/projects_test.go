package repo_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func TestProjectsAddIsIdempotentAndDisambiguates(t *testing.T) {
	t.Parallel()
	p := repo.NewProjects()

	folder, err := p.Add("github.com/sawmonabo/agent-brain", "agent-brain")
	if err != nil {
		t.Fatal(err)
	}
	if folder != "agent-brain" {
		t.Fatalf("first Add folder = %q, want agent-brain", folder)
	}

	// Same ID again → same folder, no growth (idempotent re-enrollment).
	again, err := p.Add("github.com/sawmonabo/agent-brain", "agent-brain")
	if err != nil {
		t.Fatal(err)
	}
	if again != "agent-brain" {
		t.Fatalf("re-Add folder = %q, want agent-brain", again)
	}

	// Different ID, colliding basename → deterministic -2 suffix,
	// recorded in the registry (spec §3: registry-recorded disambiguation).
	other, err := p.Add("gitlab.com/other/agent-brain", "agent-brain")
	if err != nil {
		t.Fatal(err)
	}
	if other != "agent-brain-2" {
		t.Fatalf("colliding Add folder = %q, want agent-brain-2", other)
	}

	if got, ok := p.FolderFor("gitlab.com/other/agent-brain"); !ok || got != "agent-brain-2" {
		t.Fatalf("FolderFor = %q,%v", got, ok)
	}
}

func TestProjectsAddRejectsInvalidInputs(t *testing.T) {
	t.Parallel()
	p := repo.NewProjects()
	if _, err := p.Add("", "x"); err == nil {
		t.Fatal("empty id accepted")
	}
	if _, err := p.Add("id", "_global"); err == nil {
		t.Fatal("reserved folder accepted")
	}
	if _, err := p.Add("id", "a/b"); err == nil {
		t.Fatal("separator folder accepted")
	}
}

func TestProjectsRoundtripDeterministic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "projects.toml")

	p := repo.NewProjects()
	for _, add := range []struct{ id, folder string }{
		{"github.com/sawmonabo/zeta", "zeta"},
		{"github.com/sawmonabo/alpha", "alpha"},
	} {
		if _, err := p.Add(add.id, add.folder); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Save(path); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Deterministic bytes: save again, expect identical output — the
	// file lives in a git repo; nondeterministic key order = diff churn.
	if err := p.Save(path); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("Save is nondeterministic:\n--- first\n%s\n--- second\n%s", first, second)
	}

	loaded, err := repo.LoadProjects(path)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(p, loaded); diff != "" {
		t.Fatalf("roundtrip mismatch (-saved +loaded):\n%s", diff)
	}
}

func TestLoadProjectsMissingFileIsEmptyRegistry(t *testing.T) {
	t.Parallel()
	p, err := repo.LoadProjects(filepath.Join(t.TempDir(), "nope", "projects.toml"))
	if err != nil {
		t.Fatalf("missing file must yield an empty registry (first machine), got %v", err)
	}
	if p.Version != repo.RegistryVersion || len(p.Entries) != 0 {
		t.Fatalf("empty registry expected, got %+v", p)
	}
}

func TestLoadProjectsRejectsUnknownVersionAndCorruptTOML(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	vpath := filepath.Join(dir, "v99.toml")
	if err := os.WriteFile(vpath, []byte("version = 99\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LoadProjects(vpath); err == nil {
		t.Fatal("unknown version accepted; want explicit error (forward-compat contract)")
	}

	cpath := filepath.Join(dir, "corrupt.toml")
	if err := os.WriteFile(cpath, []byte("version = [broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LoadProjects(cpath); err == nil {
		t.Fatal("corrupt TOML accepted; want error")
	}
}
