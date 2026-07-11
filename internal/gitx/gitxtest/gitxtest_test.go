package gitxtest_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/gitx/gitxtest"
)

// configPath is the path Setenv wrote the hermetic config to, captured once
// by TestMain and reused by every test below — the same layering every
// package that adopts gitxtest follows in its own TestMain.
var configPath string

func TestMain(m *testing.M) {
	path, cleanup, err := gitxtest.Setenv()
	if err != nil {
		panic(err) // TestMain has no *testing.T to fail through
	}
	configPath = path
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// TestSetenvResolvesForSpawnedGit proves the environment Setenv wrote
// actually reaches a real git child, not merely that a file landed on disk —
// the guarantee every consumer TestMain depends on.
func TestSetenvResolvesForSpawnedGit(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"gc.auto":          "0",
		"gc.autoDetach":    "false",
		"maintenance.auto": "false",
	}
	for key, wantValue := range want {
		cmd := exec.Command("git", "config", "--get", key)
		// Outside any git working tree, so a repo-local config cannot supply
		// these keys instead and mask a regression here.
		cmd.Dir = t.TempDir()
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git config --get %s: %v", key, err)
		}
		if got := strings.TrimSpace(string(out)); got != wantValue {
			t.Errorf("git config --get %s = %q, want %q", key, got, wantValue)
		}
	}
}

// TestEnvReturnsExactOverridePair pins Env's contract for a spawned child
// that does not inherit os.Environ(): exactly the two entries, exact values.
func TestEnvReturnsExactOverridePair(t *testing.T) {
	t.Parallel()
	want := []string{
		"GIT_CONFIG_GLOBAL=" + configPath,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
	}
	if diff := cmp.Diff(want, gitxtest.Env(configPath)); diff != "" {
		t.Errorf("Env(%q) mismatch (-want +got):\n%s", configPath, diff)
	}
}
