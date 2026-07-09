package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
// keyset, installed filter/merge wiring (binaryPath = os.Executable(), the
// same value buildDoctorDeps will independently resolve in this same test
// process), and a canonical .gitattributes for the identical registry
// buildRegistry(config.DefaultSettings(), home) will construct later. HOME
// and the AGENT_BRAIN_*_DIR overrides make every path hermetic — no real
// home directory is ever touched.  AGENT_BRAIN_RUNTIME_DIR is set here too
// (even though most callers don't care about the "daemon" check) so
// newAPIClient never falls back to a real per-user runtime dir.
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
	binaryPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := gitx.InstallFilters(context.Background(), checkout, binaryPath); err != nil {
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
