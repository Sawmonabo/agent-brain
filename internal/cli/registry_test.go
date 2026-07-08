package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/provider"
)

// TestBuildRegistryReturnsBothProviders pins buildRegistry as THE
// composition point (daemon, doctor, init, track all need the same
// registry, or generated attributes and classification drift apart):
// with default settings it registers both shipped adapters.
func TestBuildRegistryReturnsBothProviders(t *testing.T) {
	t.Parallel()
	registry, err := buildRegistry(config.DefaultSettings(), t.TempDir())
	if err != nil {
		t.Fatalf("buildRegistry() error = %v", err)
	}
	if _, ok := registry.Get("claude"); !ok {
		t.Error(`buildRegistry() registry has no "claude" provider`)
	}
	if _, ok := registry.Get("codex"); !ok {
		t.Error(`buildRegistry() registry has no "codex" provider`)
	}
}

// TestBuildRegistryAppliesCodexOverrides pins that classify overrides
// configured under [providers.codex] reach the actual codex.Adapter
// buildRegistry constructs — not just a parallel, disconnected table.
func TestBuildRegistryAppliesCodexOverrides(t *testing.T) {
	t.Parallel()
	settings := config.DefaultSettings()
	settings.Providers = map[string]config.ProviderSettings{
		"codex": {Classify: []config.ClassifyRule{{Glob: "**", Class: "ignore"}}},
	}

	registry, err := buildRegistry(settings, t.TempDir())
	if err != nil {
		t.Fatalf("buildRegistry() error = %v", err)
	}
	codexProvider, ok := registry.Get("codex")
	if !ok {
		t.Fatal(`buildRegistry() registry has no "codex" provider`)
	}
	want := []provider.Pattern{{Glob: "**", Class: provider.ClassIgnore}}
	if diff := cmp.Diff(want, codexProvider.Patterns()); diff != "" {
		t.Fatalf("codex Patterns() mismatch (-want +got):\n%s", diff)
	}
}

// TestBuildRegistryHonorsCodexHomeEnv pins that $CODEX_HOME overrides
// the home/.codex default (spec §6). Deliberately not parallel:
// t.Setenv mutates process-wide state that a concurrently-running
// parallel test could observe.
func TestBuildRegistryHonorsCodexHomeEnv(t *testing.T) {
	codexHome := t.TempDir()
	memoriesDir := filepath.Join(codexHome, "memories")
	if err := os.MkdirAll(memoriesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)

	// A distinct, unrelated home dir proves the env var takes priority
	// over home/.codex — if buildRegistry ignored it, Discover below
	// would find nothing.
	registry, err := buildRegistry(config.DefaultSettings(), t.TempDir())
	if err != nil {
		t.Fatalf("buildRegistry() error = %v", err)
	}
	codexProvider, ok := registry.Get("codex")
	if !ok {
		t.Fatal(`buildRegistry() registry has no "codex" provider`)
	}
	discovered, err := codexProvider.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(discovered) != 1 || discovered[0].LocalDir != memoriesDir {
		t.Fatalf("Discover() = %+v, want exactly one root at %s (CODEX_HOME honored)", discovered, memoriesDir)
	}
}
