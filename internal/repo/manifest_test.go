package repo_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func TestManifestRoundtripDeterministic(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "deep", "host.json")

	m := repo.NewManifest()
	for _, rel := range []string{"zeta/claude/notes.md", "alpha/claude/MEMORY.md"} {
		if err := m.Set(rel, repo.ManifestEntry{Size: 3, MTimeUnixNano: 42, SHA256: "abc"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The manifest is committed to git — nondeterministic bytes = churn.
	if string(first) != string(second) {
		t.Fatal("Save is nondeterministic")
	}

	loaded, err := repo.LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(m, loaded); diff != "" {
		t.Fatalf("roundtrip mismatch:\n%s", diff)
	}
}

func TestManifestMissingUnknownVersionCorrupt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	m, err := repo.LoadManifest(filepath.Join(dir, "absent.json"))
	if err != nil || len(m.Files) != 0 {
		t.Fatalf("missing manifest: got %+v, %v; want empty, nil", m, err)
	}

	v99 := filepath.Join(dir, "v99.json")
	if err := os.WriteFile(v99, []byte(`{"version":99,"files":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LoadManifest(v99); err == nil {
		t.Fatal("unknown version accepted; want error")
	}

	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte(`{"version":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LoadManifest(corrupt); err == nil {
		t.Fatal("corrupt JSON accepted; want error")
	}

	traversal := filepath.Join(dir, "traversal.json")
	payload := `{"version":1,"files":{"../escape.md":{"size":1,"mtime_unix_nano":1,"sha256":"x"}}}`
	if err := os.WriteFile(traversal, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LoadManifest(traversal); err == nil {
		t.Fatal("traversal path in manifest accepted; want error (repo file is remote-influenced input)")
	}
}

func TestValidateRelPath(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"a/claude/x.md", "x.md", "_global/codex/raw_memories.md"} {
		if err := repo.ValidateRelPath(ok); err != nil {
			t.Fatalf("valid rel %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/abs.md", "../up.md", "a/../b.md", "a//b.md", `a\b.md`, "./x.md", "a/./b.md"} {
		if err := repo.ValidateRelPath(bad); err == nil {
			t.Fatalf("ValidateRelPath(%q) = nil, want error", bad)
		}
	}
}

func TestHashFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "f.md")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry, err := repo.HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// sha256("hello\n")
	want := "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"
	if entry.SHA256 != want {
		t.Fatalf("SHA256 = %s, want %s", entry.SHA256, want)
	}
	if entry.Size != 6 || entry.MTimeUnixNano == 0 {
		t.Fatalf("Size/MTime not populated: %+v", entry)
	}
	if !strings.EqualFold(entry.SHA256, want) {
		t.Fatal("hash must be lowercase hex")
	}
	if _, err := repo.HashFile(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("HashFile(absent) = nil error")
	}
}

func TestManifestSetGetDelete(t *testing.T) {
	t.Parallel()
	m := repo.NewManifest()
	if err := m.Set("../bad.md", repo.ManifestEntry{}); err == nil {
		t.Fatal("Set accepted traversal path")
	}
	if err := m.Set("p/claude/a.md", repo.ManifestEntry{Size: 1, MTimeUnixNano: 2, SHA256: "x"}); err != nil {
		t.Fatal(err)
	}
	if !m.Has("p/claude/a.md") {
		t.Fatal("Has = false after Set")
	}
	if entry, ok := m.Get("p/claude/a.md"); !ok || entry.Size != 1 {
		t.Fatalf("Get = %+v, %v", entry, ok)
	}
	m.Delete("p/claude/a.md")
	if m.Has("p/claude/a.md") {
		t.Fatal("Has = true after Delete")
	}
}
