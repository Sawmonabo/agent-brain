// Package gitx wraps system git — the engine's git backend (ADR 06: go-git
// cannot run filters or merge drivers, so v2 shells out).
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// waitDelay bounds cleanup after a git invocation's context is canceled or the
// process exits while an I/O pipe is still held open. git can leave descendants
// (hooks, pagers, and — in this design — our own clean/smudge/merge filter
// subprocesses) holding that pipe; without a bound, Wait blocks until they
// release it. Long enough never to fire on a healthy git call, short enough to
// cap a pathological hang.
const waitDelay = 10 * time.Second

// Result carries a finished git invocation's output and exit code.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Run executes git in dir and errors on any non-zero exit.
func Run(ctx context.Context, dir string, args ...string) (Result, error) {
	result, err := RunStatus(ctx, dir, args...)
	if err != nil {
		return result, err
	}
	if result.ExitCode != 0 {
		return result, fmt.Errorf("git %v exited %d: %s", args, result.ExitCode, result.Stderr)
	}
	return result, nil
}

// childEnv is the environment for every git child process this package
// spawns: the inherited environment — so user git config, credential
// helpers, and AGENT_BRAIN_* vars keep propagating — with two overrides
// appended. os/exec keeps only the last value for a duplicate key, so the
// appended pair always wins even if the inherited environment already set
// either one.
//   - LC_ALL=C pins git's textual output to English. The sync engine parses
//     that output — push-rejection classification ([rejected],
//     non-fast-forward, fetch first) and porcelain fallbacks — and a
//     translated locale (e.g. LANG=de_DE.UTF-8) would misclassify it.
//   - GIT_TERMINAL_PROMPT=0 makes a fetch/push that would otherwise prompt
//     for credentials fail fast instead of blocking forever. This runner
//     backs an unattended daemon: the engine treats a failed fetch as a
//     clean Offline outcome, not as a goroutine wedged on a terminal that
//     does not exist.
func childEnv() []string {
	return append(os.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
}

// RunStatus executes git and reports the exit code as data — needed for
// commands like merge-file whose exit code is a count, not a failure. It errors
// only when git cannot run to a trustworthy completion: a missing dir, a failed
// spawn, a canceled/expired context, or a child terminated by a signal. The
// last two both kill git mid-run, surfacing as a bogus -1 exit code a caller
// would otherwise misread as real data (e.g. a merge-file conflict count).
// Every invocation runs with childEnv: LC_ALL=C and GIT_TERMINAL_PROMPT=0, so
// output stays parseable and a credential prompt can never hang this daemon.
func RunStatus(ctx context.Context, dir string, args ...string) (Result, error) {
	if dir == "" {
		// A wrapper whose whole identity is "run git in dir" must never fall
		// back to the process CWD — that is how a stray call would mutate an
		// unintended repo. Treat the missing dir as a pre-spawn failure.
		return Result{}, fmt.Errorf("gitx: empty dir for git %v", args)
	}
	// The command is always the constant "git"; args come only from our own
	// call sites (internal paths and git's own %-placeholders), so there is no
	// untrusted-input boundary — exec-ing git with caller args is this
	// wrapper's entire purpose (ADR 06).
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: git is a constant and args are internally sourced (see above); no untrusted boundary.
	cmd.Dir = dir
	cmd.Env = childEnv()
	cmd.WaitDelay = waitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	// A canceled/expired context kills git; report that rather than mapping the
	// signal-kill (ExitCode -1) to data a caller would misread as a real exit
	// code. Checked before the exit code so a genuine non-zero exit under a
	// live context stays data.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, fmt.Errorf("git %v: %w", args, ctxErr)
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return result, nil
	case errors.As(err, &exitErr):
		// A signal-terminated child (crash, OOM, external SIGKILL — the ctx-cancel
		// case is already caught above) did not exit normally: ExitCode() is -1,
		// not a real exit code, and never the merge-file conflict count a caller
		// would read it as. Surface it as an error so no caller mistakes the
		// process's truncated output for a trustworthy result.
		if !exitErr.Exited() {
			return result, fmt.Errorf("git %v terminated by signal: %w", args, err)
		}
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	default:
		return result, fmt.Errorf("spawn git %v: %w", args, err)
	}
}
