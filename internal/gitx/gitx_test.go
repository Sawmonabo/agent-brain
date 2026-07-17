package gitx

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/gitx/gitxtest"
)

// TestMain isolates every git invocation in this package from the developer's
// real configuration and disables git's auto-maintenance entirely (see
// gitxtest.HermeticGitConfig): no test can read or write ~/.gitconfig or
// /etc/gitconfig (the safety rule: never touch real git config), and no git
// child this package spawns can fork a detached gc/maintenance process that
// outlives the test that started it. Set once before parallel tests start, so
// it is safe with t.Parallel().
func TestMain(m *testing.M) {
	_, cleanup, err := gitxtest.Setenv()
	if err != nil {
		log.Fatalf("gitx test: %v", err)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func TestRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()

	if _, err := Run(ctx, dir, "init", "--quiet"); err != nil {
		t.Fatal(err)
	}
	result, err := Run(ctx, dir, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(result.Stdout) != "true" {
		t.Fatalf("rev-parse: %v %q", err, result.Stdout)
	}
	if _, err := Run(ctx, dir, "no-such-subcommand"); err == nil {
		t.Fatal("Run of invalid subcommand succeeded; want error")
	}
	result, err = RunStatus(ctx, dir, "no-such-subcommand")
	if err != nil {
		t.Fatalf("RunStatus must not error on non-zero exit: %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatal("RunStatus.ExitCode = 0 for failing command")
	}
}

// TestChildEnv pins the daemon-safety env contract at the unit level: every
// git child gets the full inherited environment (so user git config,
// credential helpers, and AGENT_BRAIN_* vars keep propagating) plus exactly
// the two overrides, appended last so they win over any duplicate the
// inherited environment already carried.
func TestChildEnv(t *testing.T) {
	t.Parallel()
	base := os.Environ()
	got := childEnv()

	tests := []struct {
		name  string
		check func(t *testing.T)
	}{
		{
			name: "length is os.Environ() plus the two overrides",
			check: func(t *testing.T) {
				if len(got) != len(base)+2 {
					t.Errorf("len(childEnv()) = %d, want %d (len(os.Environ())+2)", len(got), len(base)+2)
				}
			},
		},
		{
			name: "leaves the inherited environment untouched",
			check: func(t *testing.T) {
				if diff := cmp.Diff(base, got[:len(base)]); diff != "" {
					t.Errorf("childEnv() prefix diverges from os.Environ() (-want +got):\n%s", diff)
				}
			},
		},
		{
			name: "ends with the two daemon-safety overrides",
			check: func(t *testing.T) {
				want := []string{"LC_ALL=C", "GIT_TERMINAL_PROMPT=0"}
				if diff := cmp.Diff(want, got[len(got)-2:]); diff != "" {
					t.Errorf("childEnv() suffix mismatch (-want +got):\n%s", diff)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, test.check)
	}
}

// TestRunPropagatesDaemonSafeEnv proves childEnv's contract reaches an actual
// git child, not just the slice childEnv() computes. git has no builtin way
// to echo its own environment back to the caller, so a pre-commit hook is
// used as the observer: git execs it as a real subprocess inheriting git's
// environment, exactly like the credential-helper and filter subprocesses
// this fix targets. A locale- or terminal-dependent behavioral probe (a
// translated git message, or a hang on /dev/tty) was ruled out — neither is
// portable across machines and CI (NLS catalogs and a controlling terminal
// are not guaranteed present), so it would either be flaky or pass
// vacuously depending on the host. Reading the hook's own env dump is
// deterministic and hermetic instead.
func TestRunPropagatesDaemonSafeEnv(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := Run(ctx, dir, "init", "--quiet"); err != nil {
		t.Fatal(err)
	}

	const hook = `#!/bin/sh
printf '%s\n' "$LC_ALL" > env-dump.txt
printf '%s\n' "$GIT_TERMINAL_PROMPT" >> env-dump.txt
`
	hookPath := filepath.Join(dir, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hookPath, []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(ctx, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "test"); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "env-dump.txt"))
	if err != nil {
		t.Fatalf("pre-commit hook did not run: %v", err)
	}
	if want := "C\n0\n"; string(got) != want {
		t.Errorf("child env observed by git's own hook = %q, want %q", got, want)
	}
}

func TestInstallFilters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := Run(ctx, dir, "init", "--quiet"); err != nil {
		t.Fatal(err)
	}
	if err := InstallFilters(ctx, dir, "/usr/local/bin/agent-brain"); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"filter.agentbrain.required":  "true",
		"filter.agentbrain.clean":     `'/usr/local/bin/agent-brain' git-clean`,
		"filter.agentbrain.smudge":    `'/usr/local/bin/agent-brain' git-smudge`,
		"diff.agentbrain.textconv":    `'/usr/local/bin/agent-brain' git-textconv`,
		"merge.agentbrain.driver":     `'/usr/local/bin/agent-brain' git-merge --mode fact -- %O %A %B %P`,
		"merge.agentbrain-lww.driver": `'/usr/local/bin/agent-brain' git-merge --mode lww -- %O %A %B %P`,
		"merge.renormalize":           "true",
	} {
		result, err := Run(ctx, dir, "config", "--get", key)
		if err != nil {
			t.Fatalf("%s not set: %v", key, err)
		}
		if got := strings.TrimSpace(result.Stdout); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// TestRunContextCanceled pins the hardening beyond the brief: a done context is
// surfaced as an error by BOTH Run and RunStatus, never mapped to an exit code.
// This matters for RunStatus, whose non-zero exit is otherwise DATA (git
// merge-file's conflict count is read from ExitCode); a cancellation kill
// must not masquerade as a conflict count of -1.
func TestRunContextCanceled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := Run(context.Background(), dir, "init", "--quiet"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	wantCanceled := func(err error) {
		t.Helper()
		if err == nil {
			t.Fatal("want error for canceled context, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error does not wrap context.Canceled: %v", err)
		}
	}
	_, err := Run(ctx, dir, "status")
	wantCanceled(err)
	_, err = RunStatus(ctx, dir, "status")
	wantCanceled(err)
}

// TestRunEmptyDir pins the empty-dir guard: without it, an empty dir silently
// runs git in the process CWD — the exact footgun the safety rule forbids
// (never run git against an unintended repo).
func TestRunEmptyDir(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, err := Run(ctx, "", "status"); err == nil {
		t.Error("Run with empty dir succeeded; want error")
	}
	if _, err := RunStatus(ctx, "", "status"); err == nil {
		t.Error("RunStatus with empty dir succeeded; want error")
	}
}

// TestInstallFiltersEmptyBinPath pins the fail-closed guard: a required filter
// wired to an empty command would brick the repo (git can neither clean nor
// smudge), so InstallFilters must reject it up front.
func TestInstallFiltersEmptyBinPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := Run(ctx, dir, "init", "--quiet"); err != nil {
		t.Fatal(err)
	}
	if err := InstallFilters(ctx, dir, ""); err == nil {
		t.Error("InstallFilters with empty binPath succeeded; want error")
	}
}

// TestInstallFiltersIdempotent pins the re-run contract: InstallFilters runs on
// init AND doctor AND after every clone (spec §5), so a second run must replace,
// not append. --get-all returns exactly the one expected value; a duplicate
// would appear as a second line and fail this equality.
func TestInstallFiltersIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := Run(ctx, dir, "init", "--quiet"); err != nil {
		t.Fatal(err)
	}
	const bin = "/opt/agent-brain/bin/agent-brain"
	for i := range 2 {
		if err := InstallFilters(ctx, dir, bin); err != nil {
			t.Fatalf("InstallFilters run %d: %v", i, err)
		}
	}
	result, err := Run(ctx, dir, "config", "--get-all", "filter.agentbrain.clean")
	if err != nil {
		t.Fatalf("filter.agentbrain.clean: %v", err)
	}
	if got, want := strings.TrimSpace(result.Stdout), `'`+bin+`' git-clean`; got != want {
		t.Errorf("clean = %q, want %q", got, want)
	}
}

// TestInstallFiltersQuotesBinPathForSh pins the POSIX-sh quoting. git
// runs filter/merge command lines through `sh -c`, so a binPath carrying sh
// metacharacters must be single-quoted (embedded quotes escaped the POSIX way),
// never Go %q — which double-quotes and so diverges from sh on $, backtick, and
// backslash. The want string is hand-computed, not recomputed from the impl, so
// it fails if the escape idiom itself is wrong; the second check proves the
// metacharacters land literally (a Go %q would have wrapped them in
// expansion-prone double quotes).
func TestInstallFiltersQuotesBinPathForSh(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := Run(ctx, dir, "init", "--quiet"); err != nil {
		t.Fatal(err)
	}
	const hostile = `/opt/o'brien/$(touch pwned)/agent-brain`
	if err := InstallFilters(ctx, dir, hostile); err != nil {
		t.Fatal(err)
	}
	result, err := Run(ctx, dir, "config", "--get", "filter.agentbrain.clean")
	if err != nil {
		t.Fatal(err)
	}
	const want = `'/opt/o'\''brien/$(touch pwned)/agent-brain' git-clean`
	if got := strings.TrimSpace(result.Stdout); got != want {
		t.Errorf("binPath not POSIX-sh quoted:\n got: %q\nwant: %q", got, want)
	}
	if !strings.Contains(result.Stdout, `$(touch pwned)`) {
		t.Errorf("sh metacharacters not preserved literally inside single quotes: %q", result.Stdout)
	}
}

// TestRunStatusSignalKilledErrors pins the signal-termination guard. A git terminated by a signal — crash, OOM, an external
// SIGKILL, none of them a context cancel — exits with code -1, which is NOT a
// real exit code and must never reach a caller as data: RunStatus reports the
// exit code AS data (merge-file's conflict count lives there), so a leaked -1
// would be read as "0 conflicts" and let MergeFact encrypt an empty merge over
// %A. A PATH-shim fake `git` that SIGKILLs itself reproduces the signal exit
// hermetically, with no dependence on the real git ever crashing.
func TestRunStatusSignalKilledErrors(t *testing.T) {
	// t.Setenv forbids t.Parallel: this test shims PATH process-wide.
	fakeBin := t.TempDir()
	script := "#!/bin/sh\nkill -KILL $$\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	result, err := RunStatus(context.Background(), t.TempDir(), "merge-file")
	if err == nil {
		t.Fatal("RunStatus must error when git is terminated by a signal; a -1 exit code is not trustworthy data")
	}
	if result.ExitCode == -1 {
		t.Errorf("signal kill leaked as ExitCode -1 alongside the error; want the exit code left unset, not fake data")
	}
}
