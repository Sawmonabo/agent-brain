package codex_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/codex"
)

// var _ provider.Provider = (*codex.Adapter)(nil) pins the adapter against
// the full Phase-3 interface at compile time.
var _ provider.Provider = (*codex.Adapter)(nil)

func TestNewNameAndScope(t *testing.T) {
	t.Parallel()
	adapter := codex.New(t.TempDir(), nil)
	if got := adapter.Name(); got != "codex" {
		t.Errorf("Name() = %q, want %q", got, "codex")
	}
	if got := adapter.Scope(); got != provider.ScopeGlobal {
		t.Errorf("Scope() = %v, want %v", got, provider.ScopeGlobal)
	}
}

// TestPatternsClassification pins the built-in classification table
// (spec §6). Paths are relative to the provider's memory roots, exactly
// as Classify receives them at sync time — "memories/..." and
// "chronicle/..." prefixes included.
func TestPatternsClassification(t *testing.T) {
	t.Parallel()
	adapter := codex.New(t.TempDir(), nil)

	tests := []struct {
		rel  string
		want provider.Class
	}{
		{"memories/raw_memories.md", provider.ClassFact},
		{"memories/memory_summary.md", provider.ClassRegenerated},
		{"memories/MEMORY.md", provider.ClassRegenerated},
		{"memories/rollout_summaries/2026/x.md", provider.ClassRegenerated},
		{"memories/skills/foo/SKILL.md", provider.ClassFact},
		{"chronicle/2026/07/log.md", provider.ClassFact},
		{".DS_Store", provider.ClassIgnore},
		{"memories/.DS_Store", provider.ClassIgnore},
	}
	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			t.Parallel()
			if got := provider.Classify(adapter, tt.rel); got != tt.want {
				t.Errorf("Classify(%q) = %v, want %v", tt.rel, got, tt.want)
			}
		})
	}
}

// TestOverridesReplaceWholeTable pins New's documented override contract:
// a non-empty overrides table REPLACES the built-in one wholesale, not a
// patch merge — paths that are Fact/Regenerated in the built-in table
// must classify per the override alone.
func TestOverridesReplaceWholeTable(t *testing.T) {
	t.Parallel()
	adapter := codex.New(t.TempDir(), []provider.Pattern{{Glob: "**", Class: provider.ClassIgnore}})

	tests := []string{
		"memories/raw_memories.md", // Fact in the built-in table
		"chronicle/2026/07/log.md", // Fact in the built-in table
		"memories/MEMORY.md",       // Regenerated in the built-in table
	}
	for _, rel := range tests {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			if got := provider.Classify(adapter, rel); got != provider.ClassIgnore {
				t.Errorf("Classify(%q) = %v, want %v (override replaces the whole table)", rel, got, provider.ClassIgnore)
			}
		})
	}
}

// TestDiscoverBothRootsPresent fabricates a $CODEX_HOME with both
// rescue-worthy roots present (plus a sibling auth.json, exactly as a
// real $CODEX_HOME holds), then asserts Discover finds exactly two
// roots, in memories-then-chronicle order, with absolute LocalDirs and
// the documented labels — and never surfaces $CODEX_HOME itself
// (secret-adjacency, spec §5).
func TestDiscoverBothRootsPresent(t *testing.T) {
	t.Parallel()
	codexHome := t.TempDir()
	memoriesDir := filepath.Join(codexHome, "memories")
	chronicleDir := filepath.Join(codexHome, "memories_extensions", "chronicle")
	if err := os.MkdirAll(memoriesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(chronicleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// auth.json lives directly under codexHome, exactly like a real
	// $CODEX_HOME — Discover must never surface codexHome itself.
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	adapter := codex.New(codexHome, nil)
	got, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	want := []provider.Discovered{
		{LocalDir: memoriesDir, RepoSubdir: "memories", Label: "codex memories"},
		{LocalDir: chronicleDir, RepoSubdir: "chronicle", Label: "codex chronicle"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Discover() mismatch (-want +got):\n%s", diff)
	}
	for _, d := range got {
		if !filepath.IsAbs(d.LocalDir) {
			t.Errorf("Discover() LocalDir %q is not absolute", d.LocalDir)
		}
		if d.LocalDir == codexHome {
			t.Fatalf("Discover() returned $CODEX_HOME itself as a root — auth.json adjacency violation")
		}
	}
}

// TestDiscoverChronicleMissing pins that each root is independently
// optional: chronicle absent (e.g. the extension was never enabled)
// still yields the memories root.
func TestDiscoverChronicleMissing(t *testing.T) {
	t.Parallel()
	codexHome := t.TempDir()
	memoriesDir := filepath.Join(codexHome, "memories")
	if err := os.MkdirAll(memoriesDir, 0o755); err != nil {
		t.Fatal(err)
	}

	adapter := codex.New(codexHome, nil)
	got, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	want := []provider.Discovered{
		{LocalDir: memoriesDir, RepoSubdir: "memories", Label: "codex memories"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Discover() mismatch (-want +got):\n%s", diff)
	}
}

// TestDiscoverNeitherPresent pins the "Codex not installed / memories
// feature never enabled" contract: neither root existing is not an
// error.
func TestDiscoverNeitherPresent(t *testing.T) {
	t.Parallel()
	adapter := codex.New(t.TempDir(), nil)
	got, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v, want nil", err)
	}
	if got != nil {
		t.Fatalf("Discover() = %+v, want nil", got)
	}
}

// TestIdentifyAlwaysZeroIdentity pins Codex's global scope: there is no
// per-project identity to resolve, so Identify always returns the zero
// Identity regardless of its arguments — the enrolled unit's folder is
// repo.GlobalFolder by construction at enrollment.
func TestIdentifyAlwaysZeroIdentity(t *testing.T) {
	t.Parallel()
	adapter := codex.New(t.TempDir(), nil)
	got, err := adapter.Identify(context.Background(), provider.Discovered{Label: "codex memories"}, "/some/path")
	if err != nil {
		t.Fatalf("Identify() error = %v, want nil", err)
	}
	want := provider.Identity{}
	if got != want {
		t.Fatalf("Identify() = %+v, want %+v", got, want)
	}
}

// TestReconcileIndexIsANoOp pins that Codex's own background
// consolidator owns its derived indexes (memory_summary.md, MEMORY.md,
// rollout_summaries — all class-Regenerated precisely because Codex,
// not us, regenerates them): ReconcileIndex must never touch the
// filesystem.
func TestReconcileIndexIsANoOp(t *testing.T) {
	t.Parallel()
	adapter := codex.New(t.TempDir(), nil)
	dir := filepath.Join(t.TempDir(), "does-not-exist-yet")
	if err := adapter.ReconcileIndex(context.Background(), dir); err != nil {
		t.Fatalf("ReconcileIndex() error = %v, want nil", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("ReconcileIndex() created %s; want a true no-op", dir)
	}
}
