package repo_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func validUnit() repo.Unit {
	return repo.Unit{
		Provider:  "claude",
		ProjectID: "github.com/sawmonabo/agent-brain",
		Folder:    "agent-brain",
		LocalDir:  "/home/u/.claude/projects/-home-u-dev-agent-brain/memory",
	}
}

func TestLocalRegistryEnrollValidatesAndDedupes(t *testing.T) {
	t.Parallel()
	r := repo.NewLocalRegistry()

	if err := r.Enroll(validUnit()); err != nil {
		t.Fatal(err)
	}
	// Same (provider, local dir) again → idempotent no-op.
	if err := r.Enroll(validUnit()); err != nil {
		t.Fatal(err)
	}
	if len(r.Units) != 1 {
		t.Fatalf("dedupe failed: %d units", len(r.Units))
	}

	// A second local dir feeding the SAME (provider, folder) would make
	// two sources mirror into one checkout dir — ping-pong. Reject.
	dup := validUnit()
	dup.LocalDir = "/home/u/elsewhere/memory"
	if err := r.Enroll(dup); err == nil {
		t.Fatal("second local dir for same (provider, folder) accepted; want error")
	}

	bad := []repo.Unit{
		{Provider: "", ProjectID: "x", Folder: "f", LocalDir: "/abs"},
		{Provider: "claude", ProjectID: "x", Folder: "_global2", LocalDir: "/abs"}, // '_' reserved
		{Provider: "claude", ProjectID: "x", Folder: "ok", LocalDir: "relative/dir"},
	}
	for i, u := range bad {
		if err := r.Enroll(u); err == nil {
			t.Fatalf("bad unit %d accepted: %+v", i, u)
		}
	}

	// Global-scope pseudo-project: GlobalFolder IS valid here (and only
	// here — user-facing folder validation still rejects it; the
	// registry accepts it for ScopeGlobal units with empty ProjectID).
	global := repo.Unit{Provider: "codex", Folder: repo.GlobalFolder, LocalDir: "/home/u/.codex/memories"}
	if err := r.Enroll(global); err != nil {
		t.Fatalf("global unit rejected: %v", err)
	}
}

func TestLocalRegistryRoundtripAndRemove(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "deep", "registry-local.toml")

	r := repo.NewLocalRegistry()
	if err := r.Enroll(validUnit()); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("registry-local mode = %o, want 0600", got)
	}

	loaded, err := repo.LoadLocalRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(r, loaded); diff != "" {
		t.Fatalf("roundtrip mismatch:\n%s", diff)
	}

	if !loaded.Remove("claude", validUnit().LocalDir) {
		t.Fatal("Remove returned false for enrolled unit")
	}
	if loaded.Remove("claude", validUnit().LocalDir) {
		t.Fatal("Remove returned true for absent unit")
	}
}

func TestLoadLocalRegistryMissingAndInvalid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	r, err := repo.LoadLocalRegistry(filepath.Join(dir, "absent.toml"))
	if err != nil || len(r.Units) != 0 {
		t.Fatalf("missing file: got %+v, %v; want empty registry, nil", r, err)
	}

	// Corrupt entries must fail loudly at load, naming the entry — a
	// silently-skipped unit is a project that silently stops syncing.
	bad := filepath.Join(dir, "bad.toml")
	content := "version = 1\n\n[[units]]\nprovider = \"claude\"\nproject_id = \"x\"\nfolder = \"ok\"\nlocal_dir = \"not-absolute\"\n"
	if err := os.WriteFile(bad, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LoadLocalRegistry(bad); err == nil {
		t.Fatal("invalid unit accepted at load; want error")
	}
}

func TestLoadLocalRegistryRejectsCrossUnitDuplicates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Two units sharing (provider, folder): Enroll rejects this, but a
	// hand-edited file can list both entries directly.
	dupFolder := filepath.Join(dir, "dup-folder.toml")
	dupFolderContent := `version = 1

[[units]]
provider = "claude"
project_id = "x"
folder = "agent-brain"
local_dir = "/home/u/one"

[[units]]
provider = "claude"
project_id = "x"
folder = "agent-brain"
local_dir = "/home/u/two"
`
	if err := os.WriteFile(dupFolder, []byte(dupFolderContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LoadLocalRegistry(dupFolder); err == nil {
		t.Fatal("two units sharing (provider, folder) accepted at load; want error")
	}

	// Two units sharing (provider, local_dir): the same source feeding
	// two different folders is equally nonsensical.
	dupLocalDir := filepath.Join(dir, "dup-local-dir.toml")
	dupLocalDirContent := `version = 1

[[units]]
provider = "claude"
project_id = "x"
folder = "agent-brain"
local_dir = "/home/u/shared"

[[units]]
provider = "claude"
project_id = "y"
folder = "other-project"
local_dir = "/home/u/shared"
`
	if err := os.WriteFile(dupLocalDir, []byte(dupLocalDirContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LoadLocalRegistry(dupLocalDir); err == nil {
		t.Fatal("two units sharing (provider, local_dir) accepted at load; want error")
	}
}

// TestUnitRepoSubdirValidation pins Unit.validate()'s new RepoSubdir
// check (routed through ValidateRelPath), exercised via Enroll since
// validate is unexported.
func TestUnitRepoSubdirValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		repoSubdir string
		wantErr    bool
	}{
		{"empty stays valid", "", false},
		{"simple subdir valid", "memories", false},
		{"nested subdir valid", "a/b", false},
		{"parent traversal rejected", "../x", true},
		{"absolute path rejected", "/abs", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			u := validUnit()
			u.RepoSubdir = tt.repoSubdir
			r := repo.NewLocalRegistry()
			err := r.Enroll(u)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Enroll(RepoSubdir=%q) error = %v, wantErr %v", tt.repoSubdir, err, tt.wantErr)
			}
		})
	}
}

// TestUnitRepoSubdirRoundtripAndOmitsWhenEmpty pins the TOML shape: the
// key is `omitempty`, so a unit with no RepoSubdir serializes exactly
// like a unit predating RepoSubdir (no repo_subdir line at all) and a mixed registry
// round-trips losslessly.
func TestUnitRepoSubdirRoundtripAndOmitsWhenEmpty(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "registry-local.toml")

	r := repo.NewLocalRegistry()
	plain := validUnit() // RepoSubdir == ""
	withSubdir := repo.Unit{Provider: "codex", Folder: repo.GlobalFolder, LocalDir: "/home/u/.codex/memories", RepoSubdir: "memories"}
	if err := r.Enroll(plain); err != nil {
		t.Fatal(err)
	}
	if err := r.Enroll(withSubdir); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), "repo_subdir"); got != 1 {
		t.Fatalf("\"repo_subdir\" appears %d times in TOML, want 1 (omitted for the empty-subdir unit):\n%s", got, raw)
	}

	loaded, err := repo.LoadLocalRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(r, loaded); diff != "" {
		t.Fatalf("roundtrip mismatch:\n%s", diff)
	}
}

// TestLoadLocalRegistryFileWithoutRepoSubdirLoadsUnchanged pins backward
// compatibility: a registry file written before RepoSubdir existed (no
// repo_subdir key at all) must still load, defaulting the field to "".
func TestLoadLocalRegistryFileWithoutRepoSubdirLoadsUnchanged(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "phase2.toml")
	content := `version = 1

[[units]]
provider = "claude"
project_id = "x"
folder = "agent-brain"
local_dir = "/home/u/one"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := repo.LoadLocalRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Units) != 1 || loaded.Units[0].RepoSubdir != "" {
		t.Fatalf("Phase-2 registry loaded wrong: %+v", loaded.Units)
	}
}

// TestLocalRegistryEnrollAllowsDifferentRepoSubdirsSameFolder pins the
// codex two-root shape (spec §3: memories + chronicle both under
// _global/codex/): two units sharing (provider, folder) but mapped to
// DIFFERENT RepoSubdirs must both enroll — they mirror into disjoint
// checkout subtrees, so the ping-pong-overwrite risk the same-folder
// guard exists for does not apply. The SAME (provider, folder,
// repo_subdir) must still be rejected.
func TestLocalRegistryEnrollAllowsDifferentRepoSubdirsSameFolder(t *testing.T) {
	t.Parallel()
	r := repo.NewLocalRegistry()
	memories := repo.Unit{Provider: "codex", Folder: repo.GlobalFolder, LocalDir: "/home/u/.codex/memories", RepoSubdir: "memories"}
	chronicle := repo.Unit{Provider: "codex", Folder: repo.GlobalFolder, LocalDir: "/home/u/.codex/memories_extensions/chronicle", RepoSubdir: "chronicle"}

	if err := r.Enroll(memories); err != nil {
		t.Fatalf("Enroll(memories) = %v, want nil", err)
	}
	if err := r.Enroll(chronicle); err != nil {
		t.Fatalf("Enroll(chronicle) = %v, want nil — different RepoSubdir under the same folder+provider must coexist", err)
	}
	if len(r.Units) != 2 {
		t.Fatalf("len(Units) = %d, want 2", len(r.Units))
	}

	dup := memories
	dup.LocalDir = "/home/u/elsewhere"
	if err := r.Enroll(dup); err == nil {
		t.Fatal("second local dir for the SAME (provider, folder, repo_subdir) accepted; want error")
	}
}

// TestLoadLocalRegistryAllowsDifferentRepoSubdirsRejectsSame is the
// LoadLocalRegistry-side counterpart: its cross-unit invariant check
// must key on (provider, folder, repo_subdir), not just (provider,
// folder), or a hand-edited two-root codex registry could never load.
func TestLoadLocalRegistryAllowsDifferentRepoSubdirsRejectsSame(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	ok := filepath.Join(dir, "ok.toml")
	okContent := `version = 1

[[units]]
provider = "codex"
project_id = ""
folder = "_global"
local_dir = "/home/u/.codex/memories"
repo_subdir = "memories"

[[units]]
provider = "codex"
project_id = ""
folder = "_global"
local_dir = "/home/u/.codex/memories_extensions/chronicle"
repo_subdir = "chronicle"
`
	if err := os.WriteFile(ok, []byte(okContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LoadLocalRegistry(ok); err != nil {
		t.Fatalf("two units sharing (provider, folder) with different repo_subdir rejected: %v", err)
	}

	dup := filepath.Join(dir, "dup.toml")
	dupContent := `version = 1

[[units]]
provider = "codex"
project_id = ""
folder = "_global"
local_dir = "/home/u/one"
repo_subdir = "memories"

[[units]]
provider = "codex"
project_id = ""
folder = "_global"
local_dir = "/home/u/two"
repo_subdir = "memories"
`
	if err := os.WriteFile(dup, []byte(dupContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LoadLocalRegistry(dup); err == nil {
		t.Fatal("two units sharing (provider, folder, repo_subdir) accepted at load; want error")
	}
}
