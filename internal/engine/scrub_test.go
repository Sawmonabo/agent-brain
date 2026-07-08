package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// TestScrubIntegratedRemovesDeepGitMeta plants git-meta at three depths a
// hostile PUSH could deliver (folder level, unit level, nested) plus a
// legitimate memory file, simulates their arrival via a tracked commit,
// and asserts scrubIntegrated deletes every git-meta path — including the
// folder-level one mirror-in's unit-scoped pass cannot see — while the
// memory file and the repo's own root .gitattributes survive.
func TestScrubIntegratedRemovesDeepGitMeta(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	ctx := context.Background()

	// Each level carries `* -filter`, the line that unsets the encryption
	// clean filter for its subtree. folder-level sits ABOVE the unit dir,
	// so mirror-in's unit-scoped scrub never sees it (the M1 hole). Listed
	// in the lexical order filepath.WalkDir visits them, so this doubles as
	// the expected `healed` slice.
	gitMeta := []string{
		"alpha/.gitattributes",                 // folder level
		"alpha/claude/.gitattributes",          // unit level
		"alpha/claude/memories/.gitattributes", // nested
	}
	for _, rel := range gitMeta {
		writeCheckout(t, checkout, rel, "* -filter\n")
	}
	writeCheckout(t, checkout, "alpha/claude/memories/keep.md", "# real fact\n")
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "-m", "simulate poisoned integrate")

	healed, err := engine.scrubIntegrated(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(gitMeta, healed); diff != "" {
		t.Fatalf("healed paths mismatch (-want +got):\n%s", diff)
	}
	for _, rel := range gitMeta {
		if _, err := os.Stat(filepath.Join(checkout, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("git-meta %q still on disk after scrub", rel)
		}
	}

	tracked := lsFiles(t, checkout)
	for _, rel := range gitMeta {
		if tracked[rel] {
			t.Fatalf("git-meta %q still tracked in the index after scrub", rel)
		}
	}
	if !tracked[".gitattributes"] {
		t.Fatal("root .gitattributes wrongly dropped from the index")
	}
	if !tracked["alpha/claude/memories/keep.md"] {
		t.Fatal("innocent memory file wrongly dropped from the index")
	}

	// The repo's own root .gitattributes must be untouched (byte-canonical).
	if got, err := os.ReadFile(filepath.Join(checkout, ".gitattributes")); err != nil {
		t.Fatal(err)
	} else if want := repo.GenerateAttributes(engine.registry); string(got) != want {
		t.Fatalf("root .gitattributes changed:\n got %q\nwant %q", got, want)
	}
	if got, err := os.ReadFile(filepath.Join(checkout, "alpha", "claude", "memories", "keep.md")); err != nil || string(got) != "# real fact\n" {
		t.Fatalf("memory file disturbed: %q, %v", got, err)
	}
}

// TestScrubIntegratedHealsRootAttributes overwrites the checkout's root
// .gitattributes with a hostile unscoped version (no filter lines),
// commits it as if integrate delivered it, and asserts scrubIntegrated
// rewrites it byte-identical to repo.GenerateAttributes(registry) and
// stages the heal.
func TestScrubIntegratedHealsRootAttributes(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	ctx := context.Background()

	// No `filter=agentbrain` line: every later `git add` would store memory
	// content as plaintext. Commit it the way a hostile push delivers it.
	writeCheckout(t, checkout, ".gitattributes", "* text\n")
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "-m", "hostile push unscoped root attributes")

	healed, err := engine.scrubIntegrated(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff([]string{".gitattributes"}, healed); diff != "" {
		t.Fatalf("healed paths mismatch (-want +got):\n%s", diff)
	}
	want := repo.GenerateAttributes(engine.registry)
	got, err := os.ReadFile(filepath.Join(checkout, ".gitattributes"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("root .gitattributes not healed byte-identical:\n got %q\nwant %q", got, want)
	}
	staged := strings.TrimSpace(mustGit(t, checkout, "diff", "--cached", "--name-only").Stdout)
	if staged != ".gitattributes" {
		t.Fatalf("heal not staged for commit: git diff --cached --name-only = %q", staged)
	}
}

// TestScrubIntegratedNoopOnCleanTree asserts a clean checkout yields
// healed == nil and leaves `git status --porcelain` empty (no commit
// churn on the happy path — determinism matters: every cycle runs this).
func TestScrubIntegratedNoopOnCleanTree(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)

	healed, err := engine.scrubIntegrated(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if healed != nil {
		t.Fatalf("clean tree healed = %v, want nil", healed)
	}
	if status := strings.TrimSpace(mustGit(t, checkout, "status", "--porcelain").Stdout); status != "" {
		t.Fatalf("clean tree perturbed: git status --porcelain = %q", status)
	}
}

// writeCheckout plants a file at a repo-relative path inside the checkout,
// creating parent dirs — the checkout-side analogue of writeLocal.
func writeCheckout(t *testing.T, checkout, rel, content string) {
	t.Helper()
	full := filepath.Join(checkout, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// lsFiles returns the set of index-tracked paths (repo-relative, slashes).
func lsFiles(t *testing.T, checkout string) map[string]bool {
	t.Helper()
	set := map[string]bool{}
	for _, p := range strings.Split(mustGit(t, checkout, "ls-files", "-z").Stdout, "\x00") {
		if p != "" {
			set[p] = true
		}
	}
	return set
}
