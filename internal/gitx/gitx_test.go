package gitx

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain isolates every git invocation in this package from the developer's
// real configuration. Pointing GIT_CONFIG_GLOBAL/SYSTEM at throwaway files
// guarantees no test can read or write ~/.gitconfig or /etc/gitconfig (the
// safety rule: never touch real git config) and makes results hermetic — a
// host filter/init.defaultBranch/user setting can neither leak in nor break a
// run. Set once before parallel tests start, so it is safe with t.Parallel().
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gitx-config")
	if err != nil {
		log.Fatalf("gitx test: temp config dir: %v", err)
	}
	for env, path := range map[string]string{
		"GIT_CONFIG_GLOBAL": filepath.Join(dir, "gitconfig"),
		"GIT_CONFIG_SYSTEM": filepath.Join(dir, "system"),
	} {
		if err := os.Setenv(env, path); err != nil {
			log.Fatalf("gitx test: setenv %s: %v", env, err)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
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
// This matters for RunStatus, whose non-zero exit is otherwise DATA (Task 9
// reads git merge-file's conflict count from ExitCode); a cancellation kill
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

// TestInstallFiltersQuotesBinPathForSh pins the POSIX-sh quoting (Minor #2). git
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

// TestRunStatusSignalKilledErrors pins the signal-termination guard (the final
// review's Important #1). A git terminated by a signal — crash, OOM, an external
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
