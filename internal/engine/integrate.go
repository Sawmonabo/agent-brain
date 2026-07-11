package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

const (
	remoteName    = "origin"
	defaultBranch = "main"
	upstreamRef   = remoteName + "/" + defaultBranch
)

// worktreeHealTimeout bounds integrate's post-failure worktree heal. The heal
// is a single local `git checkout` and must run even when the cycle context
// that triggered the failed integrate is already canceled (daemon shutdown), so
// it runs off context.Background() — the same independent-context idiom the
// daemon uses for shutdown-time work (daemon.go). This caps that borrowed context.
const worktreeHealTimeout = 30 * time.Second

// integrateOutcome reports spec §4 step 3. Integrate is all-or-nothing:
// Integrated means HEAD now contains origin/main; otherwise the checkout
// is back at its pre-integrate state and Degraded names the project
// folders whose paths conflicted (mirror-out withheld for them, §11).
type integrateOutcome struct {
	// Offline means the fetch failed with a network-unreachable signature
	// (fetchFailureIsOffline): the remote was genuinely unreachable this cycle,
	// a benign outcome — integrate is skipped and local commits queue. Every
	// other fetch failure is a cycle error, never an Offline outcome.
	Offline     bool
	Integrated  bool
	DegradedAll bool
	Degraded    []string
}

// fetchFailureIsOffline reports whether a failed fetch's stderr positively
// identifies a transport-unreachable condition — the machine (or the remote's
// network path) is offline. The contract is fail-closed: only known
// network-unreachable signatures qualify, and every other fetch failure —
// auth expiry, permission denied, repository not found, a vanished local
// path, disk full, anything unrecognized — surfaces as a cycle error instead,
// because labeling a broken remote "offline" hides it behind a benign banner
// while the machine silently stops converging.
func fetchFailureIsOffline(stderr string) bool {
	lowered := strings.ToLower(stderr)
	for _, signature := range offlineFetchSignatures {
		if strings.Contains(lowered, signature) {
			return true
		}
	}
	return false
}

// offlineFetchSignatures are lowercase substrings of git/curl/ssh stderr that
// positively identify network unreachability. Sources: curl connect errors
// (https transport), getaddrinfo failures (glibc and macOS spellings), kernel
// socket errno text, and OpenSSH connect diagnostics. Auth (401/403,
// publickey), not-found, and local-path failures never match — by design.
var offlineFetchSignatures = []string{
	"could not resolve host",               // https DNS (also covers ssh "could not resolve hostname")
	"temporary failure in name resolution", // glibc EAI_AGAIN
	"name or service not known",            // glibc EAI_NONAME
	"nodename nor servname provided",       // macOS getaddrinfo
	"failed to connect to",                 // curl connect umbrella (refused/timeout/EPERM spellings)
	"connection refused",
	"connection timed out",
	"operation timed out", // macOS ETIMEDOUT
	"timeout was reached", // curl CURLE_OPERATION_TIMEDOUT
	"connection reset by peer",
	"network is unreachable",
	"no route to host",
	"ssh: connect to host", // OpenSSH connect diagnostics umbrella
}

// integrate fetches and rebases onto origin/main, falling back per the
// spec §4 ladder: rebase → abort → merge commit → abort → degraded.
// A network-unreachable fetch failure (fetchFailureIsOffline) is the normal
// Offline outcome, not an error; every other fetch failure — auth expiry, a
// missing/renamed remote, a vanished checkout, disk full — is an
// infrastructure error, surfaced loudly so a silently-broken machine cannot
// hide behind the offline banner. Non-fetch errors are infrastructure
// failures only.
//
// Invariant: integrate never returns a non-Integrated outcome with a worktree
// diverged from HEAD. The deferred heal below upholds it — see
// healAfterFailedIntegrate.
func (e *Engine) integrate(ctx context.Context) (outcome integrateOutcome, err error) {
	if fetch, fetchErr := gitx.RunStatus(ctx, e.checkout, "fetch", "--quiet", remoteName); fetchErr != nil {
		return integrateOutcome{}, fmt.Errorf("integrate: fetch: %w", fetchErr)
	} else if fetch.ExitCode != 0 {
		if fetchFailureIsOffline(fetch.Stderr) {
			return integrateOutcome{Offline: true}, nil
		}
		return integrateOutcome{}, fmt.Errorf("integrate: fetch failed (exit %d): %s", fetch.ExitCode, strings.TrimSpace(fetch.Stderr))
	}

	behind, behindErr := gitx.Run(ctx, e.checkout, "rev-list", "--count", "HEAD.."+upstreamRef)
	if behindErr != nil {
		return integrateOutcome{}, fmt.Errorf("integrate: behind count: %w", behindErr)
	}
	if strings.TrimSpace(behind.Stdout) == "0" {
		return integrateOutcome{Integrated: true}, nil
	}

	// PLACEMENT CONSTRAINT: this deferred heal MUST stay registered before the
	// first worktree-touching op (the rebase just below). The invariant's truth
	// depends on it — any op that can mutate the worktree has to sit AFTER this
	// line, or a non-Integrated return from that op escapes the heal. The returns
	// ABOVE it need none: fetch and rev-list are read-only, so the worktree still
	// equals HEAD there.
	//
	// From here the rebase/merge ladder can partially update the worktree and
	// then smudge-fail (a stale key cannot decrypt a rotated upstream blob),
	// stranding the worktree diverged from HEAD even after git's own --abort.
	// The heal restores it on EVERY non-Integrated return past this point — a
	// conflict degrade OR an infra/ctx-cancel failure in any rung of the ladder.
	defer func() { err = e.healAfterFailedIntegrate(outcome, err) }()

	rebase, rebaseErr := gitx.RunStatus(ctx, e.checkout, "rebase", upstreamRef)
	if rebaseErr != nil {
		return integrateOutcome{}, fmt.Errorf("integrate: rebase: %w", rebaseErr)
	}
	if rebase.ExitCode == 0 {
		return integrateOutcome{Integrated: true}, nil
	}

	// Rebase failed (spec: "unexpected driver failure"). Capture the
	// conflicted paths for attribution, abort clean, try a merge commit.
	rebaseConflicts, _ := e.conflictedPaths(ctx)
	if _, abortErr := gitx.RunStatus(ctx, e.checkout, "rebase", "--abort"); abortErr != nil {
		return integrateOutcome{}, fmt.Errorf("integrate: rebase --abort: %w", abortErr)
	}

	merge, mergeErr := gitx.RunStatus(ctx, e.checkout, "merge", "--no-edit", upstreamRef)
	if mergeErr != nil {
		return integrateOutcome{}, fmt.Errorf("integrate: merge fallback: %w", mergeErr)
	}
	if merge.ExitCode == 0 {
		return integrateOutcome{Integrated: true}, nil
	}

	mergeConflicts, _ := e.conflictedPaths(ctx)
	if _, abortErr := gitx.RunStatus(ctx, e.checkout, "merge", "--abort"); abortErr != nil {
		return integrateOutcome{}, fmt.Errorf("integrate: merge --abort: %w", abortErr)
	}

	conflicts := mergeConflicts
	if len(conflicts) == 0 {
		conflicts = rebaseConflicts
	}
	return degradeByPaths(conflicts), nil
}

// healAfterFailedIntegrate restores the worktree when integrate is returning a
// non-Integrated outcome, then reports the (possibly augmented) error. It is the
// body of integrate's deferred heal, extracted so the guarantee is unit-testable
// without racing a real mid-rebase cancellation.
//
// A clean integrate leaves the worktree at the new HEAD, so an Integrated
// outcome skips the heal. Every other outcome — a conflict degrade, or an
// infra/ctx-cancel failure in the rebase/merge ladder — may have stranded a
// partial, smudge-failed worktree update that git's --abort does not restore
// (see restoreWorktreeToHead for the data-loss class this closes), so it heals.
//
// The heal runs under its OWN bounded context, not the cycle ctx: the failure
// that triggered it is often that very ctx being canceled on daemon shutdown,
// and the restorative cleanup must still run. A heal failure is JOINED into the
// returned error — never swallowed, never masking the original — because
// proceeding past a failed heal would re-open the exact window this closes.
// Process death between here and the next cycle stays Task 4.6's cycle-start
// backstop; this closes the in-process paths.
func (e *Engine) healAfterFailedIntegrate(outcome integrateOutcome, err error) error {
	if outcome.Integrated {
		return err
	}
	healCtx, cancel := context.WithTimeout(context.Background(), worktreeHealTimeout)
	defer cancel()
	if healErr := e.restoreWorktreeToHead(healCtx); healErr != nil {
		return errors.Join(err, healErr)
	}
	return err
}

// restoreWorktreeToHead re-checks-out every tracked path from HEAD, healing a
// worktree a failed rebase/merge left diverged. healAfterFailedIntegrate is its
// only caller and owns the "when"; this is the mechanism.
//
// Class it closes: a partial worktree update whose smudge fails mid-rebase/merge
// — the local keyset cannot decrypt an upstream blob after a key rotation (or
// any admin re-encrypt), so git deletes the old worktree file, fails to write
// the new one, and its --abort does NOT restore the deletion (git treats the
// half-applied change as a local edit to preserve). Left unhealed, the next
// cycle's commitProjects `git add -A` stages that stray deletion and commits it
// as a real memory deletion, which mirror-out propagates: silent data loss
// (spec §5/§11).
//
// The heal cannot clobber legitimate work: integrate runs after mirror-in and
// commitProjects (spec §4), so the worktree equals HEAD on entry, and a degraded
// peer always smudges its OWN pre-rotation HEAD. A failure here (disk, unexpected
// git state) is surfaced, never swallowed — proceeding past a failed heal would
// re-open the exact data-loss window this closes.
func (e *Engine) restoreWorktreeToHead(ctx context.Context) error {
	if _, err := gitx.Run(ctx, e.checkout, "checkout", "--force", "HEAD", "--", "."); err != nil {
		return fmt.Errorf("integrate: restore worktree after degrade: %w", err)
	}
	return nil
}

// conflictedPaths lists unmerged paths while a rebase/merge conflict is
// live. Best-effort: attribution failing must not mask the abort.
func (e *Engine) conflictedPaths(ctx context.Context) ([]string, error) {
	res, err := gitx.RunStatus(ctx, e.checkout, "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil || res.ExitCode != 0 {
		return nil, err
	}
	var paths []string
	for p := range strings.SplitSeq(res.Stdout, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// degradeByPaths maps conflicted paths to project folders. A conflict
// under .agent-brain/ (shared metadata) or an empty attribution is not
// project-scoped — degrade everything, conservatively.
func degradeByPaths(paths []string) integrateOutcome {
	if len(paths) == 0 {
		return integrateOutcome{DegradedAll: true}
	}
	set := map[string]bool{}
	for _, p := range paths {
		folder := topSegment(p)
		if folder == repo.MetaDirName {
			return integrateOutcome{DegradedAll: true}
		}
		set[folder] = true
	}
	folders := make([]string, 0, len(set))
	for folder := range set {
		folders = append(folders, folder)
	}
	sort.Strings(folders)
	return integrateOutcome{Degraded: folders}
}
