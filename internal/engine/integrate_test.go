package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// secondClone clones the same bare remote as an independent "machine".
func secondClone(t *testing.T, bare string) string {
	t.Helper()
	other := filepath.Join(t.TempDir(), "memories-b")
	mustGit(t, t.TempDir(), "clone", bare, other)
	mustGit(t, other, "config", "user.name", "engine-test-b")
	mustGit(t, other, "config", "user.email", "engine-test-b@example.invalid")
	return other
}

func commitFileOn(t *testing.T, checkout, rel, content, message string) {
	t.Helper()
	full := filepath.Join(checkout, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, checkout, "add", "-A", "--", rel)
	mustGit(t, checkout, "commit", "--quiet", "-m", message)
}

func TestIntegrateFastForwardsWhenBehind(t *testing.T) {
	t.Parallel()
	checkout, bare := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	other := secondClone(t, bare)
	commitFileOn(t, other, "alpha/claude/memories/from-b.md", "B's fact\n", "memory: host-b alpha ts")
	mustGit(t, other, "push", "origin", "main")

	outcome, err := engine.integrate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Integrated || outcome.Offline || len(outcome.Degraded) != 0 {
		t.Fatalf("outcome = %+v, want clean integration", outcome)
	}
	if _, err := os.Stat(filepath.Join(checkout, "alpha", "claude", "memories", "from-b.md")); err != nil {
		t.Fatal("remote file did not land:", err)
	}
}

func TestIntegrateRebasesLocalCommitsLinearly(t *testing.T) {
	t.Parallel()
	checkout, bare := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	commitFileOn(t, checkout, "alpha/claude/memories/local.md", "local fact\n", "memory: host-a alpha ts")
	other := secondClone(t, bare)
	commitFileOn(t, other, "beta/claude/memories/remote.md", "remote fact\n", "memory: host-b beta ts")
	mustGit(t, other, "push", "origin", "main")

	outcome, err := engine.integrate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Integrated {
		t.Fatalf("outcome = %+v, want Integrated", outcome)
	}
	for _, rel := range []string{"alpha/claude/memories/local.md", "beta/claude/memories/remote.md"} {
		if _, err := os.Stat(filepath.Join(checkout, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("%s missing after integrate: %v", rel, err)
		}
	}
	// Rebase, not merge: exactly the one local commit sits atop origin/main.
	ahead := mustGit(t, checkout, "rev-list", "--count", "origin/main..HEAD")
	if got := strings.TrimSpace(ahead.Stdout); got != "1" {
		t.Fatalf("commits ahead = %s, want 1 (linear rebase)", got)
	}
}

func TestIntegrateOfflineIsNotAnError(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	// Point origin at an unbindable loopback port (port 1 needs root, so
	// nothing ever listens): fetch fails instantly with a curl "failed to
	// connect"/"connection refused" spelling — a genuine network-unreachable
	// signature, robust across curl versions and sandboxes. A vanished local
	// path would now (correctly) be a cycle error, not offline.
	mustGit(t, checkout, "remote", "set-url", "origin", "http://127.0.0.1:1/agent-brain-memories.git")

	outcome, err := engine.integrate(context.Background())
	if err != nil {
		t.Fatal("offline fetch must not error:", err)
	}
	if !outcome.Offline || outcome.Integrated {
		t.Fatalf("outcome = %+v, want Offline", outcome)
	}
}

// TestFetchFailureIsOffline pins fetchFailureIsOffline's fail-closed contract:
// every known transport-unreachable stderr signature classifies as offline
// (one realistic full stderr line per signature), and every other fetch
// failure — auth, permission, a 403, a missing repo, a vanished local path,
// disabled terminal prompts, an empty capture — classifies as a cycle error.
// The negative corpus is the load-bearing half: mislabeling any of these
// "offline" hides a silently-broken machine behind a benign banner.
func TestFetchFailureIsOffline(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		stderr  string
		offline bool
	}{
		// Positive corpus: one realistic full stderr line per offline signature.
		{"https could-not-resolve-host", "fatal: unable to access 'https://github.com/octo/repo.git/': Could not resolve host: github.com", true},
		{"ssh temporary-failure-in-name-resolution", "ssh: Could not resolve hostname github.com: Temporary failure in name resolution", true},
		{"ssh name-or-service-not-known", "ssh: Could not resolve hostname vault.internal: Name or service not known", true},
		{"macos nodename-nor-servname", "ssh: Could not resolve hostname git.example.com: nodename nor servname provided, or not known", true},
		{"curl failed-to-connect-umbrella", "fatal: unable to access 'http://127.0.0.1:1/agent-brain-memories.git/': Failed to connect to 127.0.0.1 port 1 after 0 ms: Couldn't connect to server", true},
		{"curl connection-refused", "fatal: unable to access 'https://git.example.com/x/y.git/': Failed to connect to git.example.com port 443 after 2 ms: Connection refused", true},
		{"curl connection-timed-out", "fatal: unable to access 'https://github.com/octo/repo.git/': Failed to connect to github.com port 443 after 21000 ms: Connection timed out", true},
		{"macos operation-timed-out", "fatal: unable to access 'https://github.com/octo/repo.git/': Failed to connect to github.com port 443 after 75012 ms: Operation timed out", true},
		{"curl timeout-was-reached", "fatal: unable to access 'https://github.com/octo/repo.git/': Timeout was reached", true},
		{"tls connection-reset-by-peer", "fatal: unable to access 'https://github.com/octo/repo.git/': OpenSSL SSL_read: Connection reset by peer, errno 54", true},
		{"curl network-is-unreachable", "fatal: unable to access 'https://github.com/octo/repo.git/': Failed to connect to github.com port 443 after 5 ms: Network is unreachable", true},
		{"curl no-route-to-host", "fatal: unable to access 'https://192.0.2.1/x/y.git/': Failed to connect to 192.0.2.1 port 443 after 3 ms: No route to host", true},
		{"ssh connect-to-host-umbrella", "ssh: connect to host github.com port 22: Host is down", true},

		// Negative corpus: every non-network fetch failure is a cycle error.
		{"ssh auth publickey", "Permission denied (publickey).\nfatal: Could not read from remote repository.", false},
		{"https 403 forbidden", "fatal: unable to access 'https://github.com/x/y.git/': The requested URL returned error: 403", false},
		{"https authentication failed", "fatal: Authentication failed for 'https://github.com/x/y.git/'", false},
		{"repository not found", "fatal: repository 'https://github.com/x/y.git/' not found", false},
		{"vanished local path", "fatal: '/tmp/gone.git' does not appear to be a git repository", false},
		{"terminal prompts disabled", "fatal: could not read Username for 'https://github.com': terminal prompts disabled", false},
		{"empty stderr", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := fetchFailureIsOffline(tc.stderr); got != tc.offline {
				t.Fatalf("fetchFailureIsOffline(%q) = %v, want %v", tc.stderr, got, tc.offline)
			}
		})
	}
}

// TestIntegrateDriverFailureDegradesProject forces the exact spec §4
// scenario: the merge driver unexpectedly fails (driver = `false`), the
// rebase aborts clean, the merge-commit fallback also fails, and the
// owning project is degraded while the checkout returns to its
// pre-integrate state.
func TestIntegrateDriverFailureDegradesProject(t *testing.T) {
	t.Parallel()
	checkout, bare := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	mustGit(t, checkout, "config", "merge.agentbrain.name", "failing driver")
	mustGit(t, checkout, "config", "merge.agentbrain.driver", "false")
	mustGit(t, checkout, "config", "merge.agentbrain-lww.name", "failing driver")
	mustGit(t, checkout, "config", "merge.agentbrain-lww.driver", "false")

	conflictPath := "alpha/claude/memories/clash.md"
	commitFileOn(t, checkout, conflictPath, "ours\n", "memory: host-a alpha ts")
	other := secondClone(t, bare)
	commitFileOn(t, other, conflictPath, "theirs\n", "memory: host-b alpha ts")
	mustGit(t, other, "push", "origin", "main")

	outcome, err := engine.integrate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Integrated || outcome.DegradedAll {
		t.Fatalf("outcome = %+v, want project-scoped degradation", outcome)
	}
	if len(outcome.Degraded) != 1 || outcome.Degraded[0] != "alpha" {
		t.Fatalf("Degraded = %v, want [alpha]", outcome.Degraded)
	}
	// Aborts restored the local state: no rebase/merge in progress, ours intact.
	gitDir := filepath.Join(checkout, ".git")
	for _, marker := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD"} {
		if _, err := os.Lstat(filepath.Join(gitDir, marker)); !os.IsNotExist(err) {
			t.Fatalf("stranded %s after aborts", marker)
		}
	}
	data, err := os.ReadFile(filepath.Join(checkout, filepath.FromSlash(conflictPath)))
	if err != nil || string(data) != "ours\n" {
		t.Fatalf("local content = %q, %v; want pre-integrate state", data, err)
	}
}

func TestIntegrateMetaConflictDegradesAll(t *testing.T) {
	t.Parallel()
	checkout, bare := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	// .agent-brain/** carries `-merge` (the generated .gitattributes): an add/add
	// there cannot content-merge, forcing the degrade-all path.
	notePath := repo.MetaDirName + "/note.txt"
	commitFileOn(t, checkout, notePath, "ours\n", "meta ours")
	other := secondClone(t, bare)
	commitFileOn(t, other, notePath, "theirs\n", "meta theirs")
	mustGit(t, other, "push", "origin", "main")

	outcome, err := engine.integrate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Integrated || !outcome.DegradedAll {
		t.Fatalf("outcome = %+v, want DegradedAll", outcome)
	}
}

// TestRestoreWorktreeToHeadPropagatesFailure pins addition (1) of the
// degrade->recover fix: a failed heal must SURFACE, never be swallowed —
// continuing past a failed heal would re-open the exact data-loss window it
// closes. A canceled context kills the checkout mid-run; restoreWorktreeToHead
// must return that error wrapping context.Canceled (the same execution-failure
// contract recoverState holds in recover_test.go).
func TestRestoreWorktreeToHeadPropagatesFailure(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := engine.restoreWorktreeToHead(ctx)
	if err == nil {
		t.Fatal("restoreWorktreeToHead returned nil under a canceled context; a failed heal must surface, never be swallowed")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("restoreWorktreeToHead error does not wrap context.Canceled: %v", err)
	}
}

// TestIntegrateHealsWorktreeOnHardFailReturn is the deterministic proof for the
// F1 fix: a NON-degraded (hard-fail, err != nil) integrate return heals a
// diverged worktree AND preserves the original error. It drives
// healAfterFailedIntegrate — integrate's deferred-heal body — with the exact
// shape those returns take (integrateOutcome{} + an error). The full-ladder
// hard-fail returns cannot be forced deterministically (they need a real
// mid-rebase ctx cancel / signal / spawn failure), so this unit test plus the
// one-line defer wiring in integrate plus the e2e degrade->recover
// characterization pin the guarantee without racing real git.
func TestIntegrateHealsWorktreeOnHardFailReturn(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)

	// Strand the worktree the way a smudge-failed rebase does: the file is gone
	// from the worktree but intact in HEAD and the index.
	rel := "alpha/claude/memories/architecture.md"
	commitFileOn(t, checkout, rel, "kept fact\n", "memory: host-a alpha ts")
	full := filepath.Join(checkout, filepath.FromSlash(rel))
	if err := os.Remove(full); err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("integrate: rebase --abort: boom")
	got := engine.healAfterFailedIntegrate(integrateOutcome{}, sentinel)

	if !errors.Is(got, sentinel) {
		t.Fatalf("heal dropped the original hard-fail error: got %v, want it to wrap %v", got, sentinel)
	}
	data, err := os.ReadFile(full)
	if err != nil || string(data) != "kept fact\n" {
		t.Fatalf("worktree not restored to HEAD after a hard-fail return: content=%q err=%v", data, err)
	}
}

// TestHealJoinsOriginalAndHealErrors pins integrate.go's errors.Join(err,
// healErr): when BOTH the integrate error that triggered the heal AND the heal
// itself fail, neither is swallowed. TestRestoreWorktreeToHeadPropagatesFailure
// and TestIntegrateHealsWorktreeOnHardFailReturn each cover one side; the join of
// the two is the one line they leave unpinned, on a data-loss-adjacent path — a
// failed heal must surface without masking (or being masked by) the original.
func TestHealJoinsOriginalAndHealErrors(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)

	// Force the heal itself to fail: with the checkout gone, healAfterFailedIntegrate's
	// own `git checkout --force HEAD -- .` cannot run. It heals off
	// context.Background(), so it never observes a caller's canceled ctx — the
	// missing dir is what makes the git op fail deterministically.
	if err := os.RemoveAll(checkout); err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("integrate: rebase: boom")
	got := engine.healAfterFailedIntegrate(integrateOutcome{}, sentinel)

	if got == nil {
		t.Fatal("healAfterFailedIntegrate returned nil when both the integrate op and the heal failed")
	}
	if !errors.Is(got, sentinel) {
		t.Fatalf("joined error dropped the original integrate error: %v", got)
	}
	if !strings.Contains(got.Error(), "restore worktree after degrade") {
		t.Fatalf("joined error dropped the heal failure: %v", got)
	}
}

// TestIntegrateSkipsHealWhenIntegrated guards the other half of the outcome
// switch: a clean (Integrated) return must NOT re-check-out the worktree.
// Without this skip every successful multi-commit integration would re-smudge
// the whole repo. A diverged worktree left diverged proves the heal never ran.
func TestIntegrateSkipsHealWhenIntegrated(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)

	rel := "alpha/claude/memories/architecture.md"
	commitFileOn(t, checkout, rel, "kept fact\n", "memory: host-a alpha ts")
	full := filepath.Join(checkout, filepath.FromSlash(rel))
	if err := os.Remove(full); err != nil {
		t.Fatal(err)
	}

	if err := engine.healAfterFailedIntegrate(integrateOutcome{Integrated: true}, nil); err != nil {
		t.Fatalf("Integrated outcome returned err = %v, want nil", err)
	}
	if _, err := os.Stat(full); !os.IsNotExist(err) {
		t.Fatalf("Integrated outcome healed the worktree (stat err = %v); the clean path must not re-check-out", err)
	}
}
