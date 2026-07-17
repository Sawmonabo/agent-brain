package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/keys"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// mustGitCLI runs git in dir, failing the test on any error — the doctor
// CLI tests' equivalent of daemon_test.go's mustGit, duplicated rather than
// exported across packages (internal/cli must not gain a test-only daemon
// package dependency).
func mustGitCLI(t *testing.T, dir string, args ...string) {
	t.Helper()
	if res, err := gitx.Run(context.Background(), dir, args...); err != nil {
		t.Fatalf("git %v: %v\nstderr: %s", args, err, res.Stderr)
	}
}

// fakeGhOnPath puts a trivial always-exit-0 "gh" on PATH so checkGH's
// AuthOK probe (a bare `gh auth status` exit-code check) succeeds without a
// real gh binary, a real login, or any network access.
func fakeGhOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "gh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// provisionHealthyDoctorMachine builds a from-scratch machine that passes
// every check in the battery: a real checkout with a local bare remote (git
// happily ls-remotes a filesystem path — no network involved), a generated
// keyset, installed filter/merge wiring, and a canonical .gitattributes for
// the identical registry buildRegistry(config.DefaultSettings(), home) will
// construct later. HOME and the AGENT_BRAIN_*_DIR overrides make every path
// hermetic — no real home directory is ever touched. AGENT_BRAIN_RUNTIME_DIR
// is set here too (even though most callers don't care about the "daemon"
// check) so newAPIClient never falls back to a real per-user runtime dir.
//
// Filter wiring points at testBinaryPath (testmain_test.go), a REAL built
// binary — never os.Executable(), which inside this test process is the
// cli.test binary itself (see testBinaryPath's doc comment for the
// incident this avoids). AGENT_BRAIN_TEST_BINARY_PATH
// mirrors that same value into the doctor COMMAND's own resolution
// (buildDoctorDeps, doctor.go) so checkFilters' containment comparison —
// exercised when tests below run `doctor` via runCmd, not by calling
// buildDoctorDeps directly — checks against the identical binary the git
// config was actually wired with, not a fresh os.Executable() resolution
// that would again be cli.test.
//
// Only .gitattributes is staged and committed here — never a memory file
// under the registry's `*` glob — which is why the clean filter is never
// actually invoked by this fixture: repo.GenerateAttributes emits
// `.gitattributes -filter -diff -merge text eol=lf` AFTER the catch-all
// `* filter=agentbrain …` line, and gitattributes resolves last-match-wins,
// so .gitattributes exempts itself from its own filter. That is what keeps
// this fixture latent-safe even before testBinaryPath existed; the fix
// above closes the margin so a future test that DOES stage a filtered file
// (e.g. track/untrack cycles) fails loud instead of
// fork-bombing if it copies this fixture.
func provisionHealthyDoctorMachine(t *testing.T) config.Paths {
	t.Helper()
	base, err := os.MkdirTemp("", "ab-doctor")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", filepath.Join(base, "cfg"))
	t.Setenv("AGENT_BRAIN_DATA_DIR", filepath.Join(base, "data"))
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", filepath.Join(base, "run"))
	t.Setenv(testBinaryPathEnv, testBinaryPath)

	paths, err := config.DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}

	bare := filepath.Join(base, "remote.git")
	checkout := paths.MemoriesDir()
	mustGitCLI(t, base, "init", "--bare", "-b", "main", bare)
	mustGitCLI(t, base, "clone", bare, checkout)
	mustGitCLI(t, checkout, "config", "user.name", "doctor-test")
	mustGitCLI(t, checkout, "config", "user.email", "doctor-test@example.invalid")

	registry, err := buildRegistry(config.DefaultSettings(), home)
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.Generate(paths.Keyset()); err != nil {
		t.Fatal(err)
	}
	if err := gitx.InstallFilters(context.Background(), checkout, testBinaryPath); err != nil {
		t.Fatal(err)
	}
	if err := gitx.InstallMaintenancePosture(context.Background(), checkout); err != nil {
		t.Fatal(err)
	}
	if err := repo.WriteAttributes(repo.NewLayout(checkout), registry); err != nil {
		t.Fatal(err)
	}
	mustGitCLI(t, checkout, "add", "-A")
	mustGitCLI(t, checkout, "commit", "-m", "init: repo skeleton")
	mustGitCLI(t, checkout, "push", "-u", "origin", "main")
	return paths
}

func TestDoctorCommandHealthyMachineExitsZero(t *testing.T) {
	provisionHealthyDoctorMachine(t)
	fakeGhOnPath(t)
	startFakeDaemon(t, api.StatusResponse{State: "ready"}, api.SyncResponse{}, api.ProjectsResponse{})

	out, err := runCmd(t, nil, "doctor", "--offline")
	if err != nil {
		t.Fatalf("doctor on a healthy machine failed: %v\noutput:\n%s", err, out)
	}
	for _, want := range []string{"checkout", "keyset", "filters", "attributes", "gh", "daemon"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("doctor output missing check %q:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "FAIL") {
		t.Fatalf("healthy machine reported a FAIL:\n%s", out)
	}
}

func TestDoctorCommandReportsBrokenAxisAndExitsNonZero(t *testing.T) {
	paths := provisionHealthyDoctorMachine(t)
	fakeGhOnPath(t)
	startFakeDaemon(t, api.StatusResponse{State: "ready"}, api.SyncResponse{}, api.ProjectsResponse{})

	// Break exactly the "attributes" axis.
	attributesFile := repo.NewLayout(paths.MemoriesDir()).AttributesFile()
	if err := os.WriteFile(attributesFile, []byte("corrupted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, nil, "doctor", "--offline")
	if err == nil {
		t.Fatalf("doctor with a broken checkout succeeded; output:\n%s", out)
	}
	if !strings.Contains(string(out), "FAIL") || !strings.Contains(string(out), "attributes") {
		t.Fatalf("doctor output does not name the broken axis:\n%s", out)
	}
}

func TestDoctorCommandJSONOutput(t *testing.T) {
	provisionHealthyDoctorMachine(t)
	fakeGhOnPath(t)
	startFakeDaemon(t, api.StatusResponse{State: "ready"}, api.SyncResponse{}, api.ProjectsResponse{})

	out, err := runCmd(t, nil, "doctor", "--offline", "--json")
	if err != nil {
		t.Fatalf("doctor --json on a healthy machine failed: %v\noutput:\n%s", err, out)
	}
	var report doctor.Report
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("doctor --json did not produce valid JSON: %v\noutput:\n%s", err, out)
	}
	if len(report.Results) == 0 {
		t.Fatal("doctor --json report has no results")
	}
	if report.Failed() {
		t.Fatalf("doctor --json reported a failure on a healthy machine: %+v", report.Results)
	}
}

func TestDoctorCommandFixRepairs(t *testing.T) {
	paths := provisionHealthyDoctorMachine(t)
	fakeGhOnPath(t)
	startFakeDaemon(t, api.StatusResponse{State: "ready"}, api.SyncResponse{}, api.ProjectsResponse{})
	checkout := paths.MemoriesDir()

	// Break both filters (required is flipped off) and attributes (corrupted).
	mustGitCLI(t, checkout, "config", "--local", "filter.agentbrain.required", "false")
	attributesFile := repo.NewLayout(checkout).AttributesFile()
	if err := os.WriteFile(attributesFile, []byte("corrupted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, nil, "doctor", "--offline", "--fix")
	if err != nil {
		t.Fatalf("doctor --fix did not repair the machine: %v\noutput:\n%s", err, out)
	}
	for _, want := range []string{"filters", "attributes", "fixed"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("doctor --fix output missing %q:\n%s", want, out)
		}
	}

	// A second, unmodified run must be clean — the repairs actually landed.
	out, err = runCmd(t, nil, "doctor", "--offline")
	if err != nil {
		t.Fatalf("doctor after --fix still fails: %v\noutput:\n%s", err, out)
	}
	if strings.Contains(string(out), "FAIL") {
		t.Fatalf("doctor after --fix still reports a FAIL:\n%s", out)
	}
}

// TestDoctorFixQuiescesLiveDaemon pins that `doctor --fix`, like init's
// repo-state step, holds a resident daemon's cycles during its wiring
// surgery and releases after — the recording fake daemon (which repoints
// AGENT_BRAIN_RUNTIME_DIR at its own socket) must see exactly one hold and
// one resume, and the fix must still land.
func TestDoctorFixQuiescesLiveDaemon(t *testing.T) {
	paths := provisionHealthyDoctorMachine(t)
	fakeGhOnPath(t)
	hits := startFakeDaemonRecordingQuiesce(t)

	attributesFile := repo.NewLayout(paths.MemoriesDir()).AttributesFile()
	if err := os.WriteFile(attributesFile, []byte("corrupted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, nil, "doctor", "--offline", "--fix")
	if err != nil {
		t.Fatalf("doctor --fix: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(string(out), "fixed") {
		t.Fatalf("doctor --fix did not repair:\n%s", out)
	}
	got := hits()
	if len(got.held) != 1 || got.held[0] != quiesceHoldForInit {
		t.Fatalf("quiesce holds = %v, want exactly one of %d seconds", got.held, quiesceHoldForInit)
	}
	if got.resumed != 1 {
		t.Fatalf("resume count = %d, want 1", got.resumed)
	}
}

// startFakeDaemonQuiesceFails serves /v0/status (always ready) but makes
// /v0/quiesce always fail — the absorbed-Minor test's way of exercising
// doctor --fix's best-effort quiesce against a daemon that refuses the
// hold. Modeled on startFakeDaemonRecordingQuiesce (init_test.go).
func startFakeDaemonQuiesceFails(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", dir)

	mux := http.NewServeMux()
	mux.HandleFunc("/v0/status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.StatusResponse{State: "ready"})
	})
	mux.HandleFunc("/v0/quiesce", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "quiesce refused", http.StatusServiceUnavailable)
	})
	listener, err := net.Listen("unix", filepath.Join(dir, "agent-brain.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
}

// TestDoctorFixNotesFailedQuiesce closes a gap: doctor --fix
// silently ignored a failed daemon Quiesce while init's identical situation
// (stepRepoState, initsteps.go) prints a note — both correctly proceed
// either way, but only one of them told the operator why. This pins
// doctor's side: a fake daemon whose /v0/quiesce always errors must still
// let doctor --fix run and repair, but must also print an operator-visible
// note (stderr, so --json's stdout stays parseable).
func TestDoctorFixNotesFailedQuiesce(t *testing.T) {
	paths := provisionHealthyDoctorMachine(t)
	fakeGhOnPath(t)
	startFakeDaemonQuiesceFails(t)

	attributesFile := repo.NewLayout(paths.MemoriesDir()).AttributesFile()
	if err := os.WriteFile(attributesFile, []byte("corrupted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := runCmdWithStderr(t, nil, "doctor", "--offline", "--fix")
	if err != nil {
		t.Fatalf("doctor --fix: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if !strings.Contains(string(stdout), "fixed") {
		t.Fatalf("doctor --fix did not repair despite the quiesce failure:\n%s", stdout)
	}
	if !strings.Contains(string(stderr), "could not quiesce the daemon") {
		t.Fatalf("doctor --fix did not note the failed quiesce on stderr:\n%s", stderr)
	}
}

// TestRunDoctorFixWithQuiesceQuiesces pins the shared --fix orchestration the
// command and the hub both call (extracted from doctor.go's RunE): a live
// daemon is held exactly once and resumed, the repair lands — and, critically,
// a FAILED fix still attempts the resume (the resume is deferred, never
// sequenced after a successful Fix), so a fix error can never strand the
// daemon quiesced. Driven against the recording fake daemon the command's own
// --fix tests use, but calling the function directly.
func TestRunDoctorFixWithQuiesceQuiesces(t *testing.T) {
	t.Run("quiesce, fix, resume", func(t *testing.T) {
		paths := provisionHealthyDoctorMachine(t)
		fakeGhOnPath(t)
		hits := startFakeDaemonRecordingQuiesce(t)

		// Break a repairable axis so Fix has real work and the re-check confirms it.
		mustGitCLI(t, paths.MemoriesDir(), "config", "--local", "filter.agentbrain.required", "false")

		var stderr bytes.Buffer
		report, err := runDoctorFixWithQuiesce(context.Background(), true, &stderr)
		if err != nil {
			t.Fatalf("runDoctorFixWithQuiesce: %v\nstderr: %s", err, stderr.String())
		}
		if report.Failed() {
			t.Fatalf("report still failed after fix: %+v", report.Results)
		}
		got := hits()
		if len(got.held) != 1 || got.held[0] != quiesceHoldForInit {
			t.Fatalf("quiesce holds = %v, want exactly one of %d seconds", got.held, quiesceHoldForInit)
		}
		if got.resumed != 1 {
			t.Fatalf("resume count = %d, want 1", got.resumed)
		}
	})

	t.Run("failed fix still resumes", func(t *testing.T) {
		paths := provisionHealthyDoctorMachine(t)
		fakeGhOnPath(t)
		hits := startFakeDaemonRecordingQuiesce(t)

		// Remove the checkout's .git so Fix's first step (InstallFilters, which
		// `git config --local` fails closed off a repo) errors AFTER the quiesce
		// hold is taken — exercising the deferred resume on the error return.
		if err := os.RemoveAll(filepath.Join(paths.MemoriesDir(), ".git")); err != nil {
			t.Fatal(err)
		}

		_, err := runDoctorFixWithQuiesce(context.Background(), true, io.Discard)
		if err == nil {
			t.Fatal("runDoctorFixWithQuiesce succeeded on a broken checkout; want a fix error")
		}
		got := hits()
		if len(got.held) != 1 || got.held[0] != quiesceHoldForInit {
			t.Fatalf("quiesce holds = %v, want exactly one of %d seconds", got.held, quiesceHoldForInit)
		}
		if got.resumed != 1 {
			t.Fatalf("resume count = %d after a failed fix, want 1 — resume must still be attempted", got.resumed)
		}
	})
}

// TestRunDoctorFixWithQuiesceThreadsOfflineFlag pins the threaded offline contract: the
// extracted --fix orchestration threads its offline argument into doctor.Fix's
// re-check (Fix re-runs the FULL battery under the same deps), so the `remote`
// reachability row — and the exit code an unreachable origin drives — track the
// flag exactly as plain `doctor` does. A hardcoded offline=true would silently
// drop that row and flip an unreachable-origin `doctor --fix` from exit 1 to
// exit 0, breaking the CI/health-gate contract; this test fails if the hardcode
// ever returns.
//
// The origin is pointed at a closed loopback port, so `git ls-remote` gets an
// immediate connection-refused — deterministic, no external network or DNS, well
// inside remoteCheckTimeout. An https origin also activates checkCredentialHelper,
// but doctor.Fix wires the gh helper before re-checking (deps.GH is the fake gh
// on PATH), so that row passes and `remote` is the SOLE offline/online difference.
func TestRunDoctorFixWithQuiesceThreadsOfflineFlag(t *testing.T) {
	setup := func(t *testing.T) {
		paths := provisionHealthyDoctorMachine(t)
		fakeGhOnPath(t)
		startFakeDaemon(t, api.StatusResponse{State: "ready"}, api.SyncResponse{}, api.ProjectsResponse{})
		// An https origin at a closed loopback port: the reachability probe fails
		// fast and deterministically (connection refused) when online, and is
		// omitted entirely when offline — the exact axis the flag governs.
		mustGitCLI(t, paths.MemoriesDir(), "config", "--local", "remote.origin.url", "https://127.0.0.1:1/agent-brain-doctor-test.git")
	}

	remoteRow := func(report doctor.Report) (doctor.CheckResult, bool) {
		for _, result := range report.Results {
			if result.Name == "remote" {
				return result, true
			}
		}
		return doctor.CheckResult{}, false
	}

	t.Run("online (offline=false) keeps the failing remote row and fails the report", func(t *testing.T) {
		setup(t)
		report, err := runDoctorFixWithQuiesce(context.Background(), false, io.Discard)
		if err != nil {
			t.Fatalf("runDoctorFixWithQuiesce(offline=false): %v", err)
		}
		row, ok := remoteRow(report)
		if !ok || row.Status != doctor.StatusFail {
			t.Fatalf("online --fix re-check must carry a failing remote row; got ok=%v row=%+v\nresults:\n%s", ok, row, reportNames(report))
		}
		if !report.Failed() {
			t.Fatalf("online --fix with an unreachable origin did not fail the report (exit-1 contract broken):\n%s", reportNames(report))
		}
	})

	t.Run("offline (offline=true) omits the remote row and passes", func(t *testing.T) {
		setup(t)
		report, err := runDoctorFixWithQuiesce(context.Background(), true, io.Discard)
		if err != nil {
			t.Fatalf("runDoctorFixWithQuiesce(offline=true): %v", err)
		}
		if _, ok := remoteRow(report); ok {
			t.Fatalf("offline --fix re-check still probed the remote:\n%s", reportNames(report))
		}
		if report.Failed() {
			t.Fatalf("offline --fix reported a failure on an otherwise-healthy machine:\n%s", reportNames(report))
		}
	})
}

// reportNames lists each result's name, status, and detail — a compact dump for
// a failing doctor-report assertion.
func reportNames(report doctor.Report) string {
	var b strings.Builder
	for _, result := range report.Results {
		fmt.Fprintf(&b, "  %-20s %-4s %s\n", result.Name, result.Status, result.Detail)
	}
	return b.String()
}
