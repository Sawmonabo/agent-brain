package doctor_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/doctor"
)

// plantGitMeta writes a git-meta file at a checkout-relative path — the
// state a clone of a poisoned main materializes.
func plantGitMeta(t *testing.T, checkout, rel, content string) {
	t.Helper()
	full := filepath.Join(checkout, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCheckGitMetaHealthyCheckout: the fixture's canonical root
// .gitattributes is legitimate and must not be reported, and the checkout's
// own .git dir is not repo content.
func TestCheckGitMetaHealthyCheckout(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	res := result(t, doctor.Run(context.Background(), fx.deps), "git-meta")
	if res.Status != doctor.StatusOK {
		t.Fatalf("healthy checkout: git-meta = %v (%s), want ok", res.Status, res.Detail)
	}
}

// TestCheckGitMetaWarnsOnResidentPoison pins the observability half of the
// git-meta poison defense: doctor NAMES folder-level poison a fresh clone materialized,
// at every depth and shape (file above the unit dir, file inside it, and a
// meta-named tree), while leaving the root file alone.
func TestCheckGitMetaWarnsOnResidentPoison(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rel  string
	}{
		{"folder_level", "alpha/.gitattributes"},
		{"unit_level", "alpha/claude/.gitattributes"},
		{"gitignore", "alpha/claude/.gitignore"},
		{"meta_named_tree", "alpha/.gitignore/decoy.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fx := newFixture(t)
			plantGitMeta(t, fx.dir, tt.rel, "* -filter\n")

			res := result(t, doctor.Run(context.Background(), fx.deps), "git-meta")
			if res.Status != doctor.StatusWarn {
				t.Fatalf("resident %s: git-meta = %v (%s), want warn", tt.rel, res.Status, res.Detail)
			}
			// The warn names the offending path so an operator can act.
			wantPath := strings.SplitN(tt.rel, "/decoy.md", 2)[0]
			if !strings.Contains(res.Detail, wantPath) {
				t.Fatalf("git-meta detail does not name %q: %s", wantPath, res.Detail)
			}
			if res.Fix == "" {
				t.Fatal("git-meta warn must carry a Fix telling the operator how to heal it")
			}
		})
	}
}

// TestCheckGitMetaNeverFailsTheReport pins the ADVISORY contract. Report.Failed()
// drives `doctor`'s exit code, and resident poison is self-healing (the engine
// scrubs it at the top of the next cycle). If this check ever hardened to
// StatusFail, `doctor` would exit non-zero on a condition the system repairs by
// itself — and worse, would invite someone to add it to SafetyGate, which gates
// the very cycle that heals it. See checkGitMeta's doc comment.
func TestCheckGitMetaNeverFailsTheReport(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)

	// Baseline first: this fixture builds no gh client, so `gh` already fails
	// and Report.Failed() is true for reasons unrelated to git-meta. The
	// invariant under test is that planting poison does not CHANGE the failed
	// verdict — which isolates this check's contribution to the exit code.
	failedBefore := doctor.Run(context.Background(), fx.deps).Failed()

	plantGitMeta(t, fx.dir, "alpha/.gitattributes", "* -filter\n")
	report := doctor.Run(context.Background(), fx.deps)

	if res := result(t, report, "git-meta"); res.Status == doctor.StatusFail {
		t.Fatal("git-meta must never be StatusFail: it is self-healing, and failing it would deadlock the heal if ever gated")
	}
	if report.Failed() != failedBefore {
		t.Fatalf("resident git-meta changed Report.Failed() %v → %v; it is advisory and must not affect the exit code", failedBefore, report.Failed())
	}
}

// TestSafetyGateIgnoresResidentGitMeta is the deadlock pin, asserted through
// the exported gate the daemon actually calls before every cycle. Resident
// poison must NOT block the sync — engine.prepareCheckout's scrub, which runs
// inside that sync, is the only thing that removes it. A gate that refused
// here would strand the checkout poisoned forever.
func TestSafetyGateIgnoresResidentGitMeta(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	plantGitMeta(t, fx.dir, "alpha/.gitattributes", "* -filter\n")

	if err := doctor.SafetyGate(context.Background(), fx.deps.Paths, fx.deps.Registry, fx.deps.BinaryPath); err != nil {
		t.Fatalf("SafetyGate refused a cycle over resident git-meta — the scrub that heals it can never run: %v", err)
	}
}
