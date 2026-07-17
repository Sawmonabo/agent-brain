package doctor_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
	"github.com/Sawmonabo/agent-brain/internal/ghx"
	"github.com/Sawmonabo/agent-brain/internal/ghx/ghxtest"
	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/keys"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/providertest"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if res, err := gitx.Run(context.Background(), dir, args...); err != nil {
		t.Fatalf("git %v: %v\nstderr: %s", args, err, res.Stderr)
	}
}

func testRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	fake := providertest.New("claude", provider.ScopePerProject, []provider.Pattern{
		{Glob: "memories/**", Class: provider.ClassFact},
	})
	registry, err := provider.NewRegistry(fake)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

// fixture is a fabricated, init-shaped machine: a real git checkout (with
// a real local bare remote, so `remote`/`credential-helper` exercise real
// git, never the network) plus canonical filters/attributes and a real
// keyset. Individual tests mutate one axis at a time off this healthy
// baseline. GH is deliberately left nil here — most tests don't care
// about the "gh" check, and a scripted ghxtest.Fake must have every
// expectation consumed exactly once (see authOKGH), so only the tests
// that DO care build their own.
type fixture struct {
	base string
	home string
	dir  string // memories checkout
	deps doctor.Deps
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	paths := config.Paths{ConfigDir: filepath.Join(base, "cfg"), DataDir: filepath.Join(base, "data")}
	checkout := paths.MemoriesDir()

	bare := filepath.Join(base, "remote.git")
	mustGit(t, base, "init", "--bare", "-b", "main", bare)
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mustGit(t, base, "clone", bare, checkout)
	mustGit(t, checkout, "config", "user.name", "doctor-test")
	mustGit(t, checkout, "config", "user.email", "doctor-test@example.invalid")

	registry := testRegistry(t)
	if err := repo.WriteAttributes(repo.NewLayout(checkout), registry); err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(base, "bin", "agent-brain")
	if err := gitx.InstallFilters(context.Background(), checkout, binaryPath); err != nil {
		t.Fatal(err)
	}
	if err := gitx.InstallMaintenancePosture(context.Background(), checkout); err != nil {
		t.Fatal(err)
	}
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "-m", "init: repo skeleton")
	mustGit(t, checkout, "push", "-u", "origin", "main")

	if err := keys.Generate(paths.Keyset()); err != nil {
		t.Fatal(err)
	}

	return fixture{
		base: base,
		home: home,
		dir:  checkout,
		deps: doctor.Deps{
			Paths:      paths,
			Settings:   config.DefaultSettings(),
			Registry:   registry,
			BinaryPath: binaryPath,
			Home:       home,
		},
	}
}

// authOKGH returns a gh client scripted to report `gh auth status`
// succeeding exactly `calls` times — the number of times the caller's
// Run()/Fix() invocations will exercise the "gh" check. ghxtest.Fake
// fails the test if any scripted call goes unconsumed, so callers must
// size this to their exact usage.
func authOKGH(t *testing.T, calls int) *ghx.Client {
	t.Helper()
	script := make([]ghxtest.Call, calls)
	for i := range script {
		script[i] = ghxtest.Call{Args: []string{"auth", "status"}, Result: ghx.Result{ExitCode: 0}}
	}
	return ghx.NewClientWithRunner(ghxtest.New(t, script...), "/usr/bin/gh")
}

func result(t *testing.T, report doctor.Report, name string) doctor.CheckResult {
	t.Helper()
	for _, r := range report.Results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("report has no check named %q; results: %+v", name, report.Results)
	return doctor.CheckResult{}
}

func hasCheck(report doctor.Report, name string) bool {
	for _, r := range report.Results {
		if r.Name == name {
			return true
		}
	}
	return false
}

// TestRunHealthyMachineAllApplicableChecksPass pins the healthy baseline:
// every check that applies to this fixture reports ok, and checks that
// are only conditionally applicable (no daemon, no https remote, no
// enrolled codex unit) are absent from the report entirely rather than
// forced into some default state.
func TestRunHealthyMachineAllApplicableChecksPass(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.deps.GH = authOKGH(t, 1)

	report := doctor.Run(context.Background(), fx.deps)
	if report.Failed() {
		t.Fatalf("healthy machine reported failures: %+v", report.Results)
	}
	for _, name := range []string{
		"settings", "keyset", "checkout", "filters", "maintenance-posture",
		"attributes", "remote", "gh", "registry-local", "conflict-log",
		"claude-prereqs", "legacy-leftovers",
	} {
		if got := result(t, report, name).Status; got != doctor.StatusOK {
			t.Errorf("check %q = %v, want ok", name, got)
		}
	}
	for _, name := range []string{"daemon", "codex-prereqs", "credential-helper"} {
		if hasCheck(report, name) {
			t.Errorf("check %q present but should be inapplicable on this fixture", name)
		}
	}
}

func TestRunKeysetMissing(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	if err := os.Remove(fx.deps.Paths.Keyset()); err != nil {
		t.Fatal(err)
	}
	got := result(t, doctor.Run(context.Background(), fx.deps), "keyset")
	if got.Status != doctor.StatusFail || got.Fix == "" {
		t.Fatalf("keyset check = %+v, want fail with a fix", got)
	}
}

func TestRunKeysetCorrupt(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	if err := os.WriteFile(fx.deps.Paths.Keyset(), []byte("not a keyset"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := result(t, doctor.Run(context.Background(), fx.deps), "keyset")
	if got.Status != doctor.StatusFail {
		t.Fatalf("keyset check = %+v, want fail", got)
	}
}

func TestRunAttributesCorrupt(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	if err := os.WriteFile(repo.NewLayout(fx.dir).AttributesFile(), []byte("corrupted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := result(t, doctor.Run(context.Background(), fx.deps), "attributes")
	if got.Status != doctor.StatusFail || got.Fix == "" {
		t.Fatalf("attributes check = %+v, want fail with a fix", got)
	}
}

func TestRunAttributesMissing(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	if err := os.Remove(repo.NewLayout(fx.dir).AttributesFile()); err != nil {
		t.Fatal(err)
	}
	got := result(t, doctor.Run(context.Background(), fx.deps), "attributes")
	if got.Status != doctor.StatusFail {
		t.Fatalf("attributes check = %+v, want fail", got)
	}
}

func TestRunFiltersRequiredUnset(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	mustGit(t, fx.dir, "config", "--local", "filter.agentbrain.required", "false")
	got := result(t, doctor.Run(context.Background(), fx.deps), "filters")
	if got.Status != doctor.StatusFail {
		t.Fatalf("filters check = %+v, want fail", got)
	}
}

func TestRunFiltersCleanPointsElsewhere(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	mustGit(t, fx.dir, "config", "--local", "filter.agentbrain.clean", "/some/other/bin git-clean")
	got := result(t, doctor.Run(context.Background(), fx.deps), "filters")
	if got.Status != doctor.StatusFail {
		t.Fatalf("filters check = %+v, want fail", got)
	}
}

func TestRunFiltersMissingMergeEntry(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	mustGit(t, fx.dir, "config", "--local", "--unset", "merge.agentbrain.driver")
	got := result(t, doctor.Run(context.Background(), fx.deps), "filters")
	if got.Status != doctor.StatusFail {
		t.Fatalf("filters check = %+v, want fail", got)
	}
}

// TestRunFiltersEmptyBinaryPathFailsClosed pins the guard:
// strings.Contains(anything, "") is always true, so an unguarded
// containment comparison against an empty BinaryPath would report
// "filters" ok no matter what filter.agentbrain.clean actually holds.
// Never reachable via daemon/CLI (both always resolve a real path before
// building Deps), but Deps and SafetyGate are exported — a caller that
// forgets to set BinaryPath must get a named failure, not a silent pass.
func TestRunFiltersEmptyBinaryPathFailsClosed(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.deps.BinaryPath = ""
	got := result(t, doctor.Run(context.Background(), fx.deps), "filters")
	if got.Status != doctor.StatusFail {
		t.Fatalf("filters check with empty BinaryPath = %+v, want fail", got)
	}
}

// TestRunCheckoutNotAGitRepo pins that a totally uninitialized machine
// (no checkout at all) degrades every dependent check to a named fail
// rather than panicking.
func TestRunCheckoutNotAGitRepo(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	deps := doctor.Deps{
		Paths:    config.Paths{ConfigDir: filepath.Join(base, "cfg"), DataDir: filepath.Join(base, "data")},
		Registry: testRegistry(t),
		Home:     filepath.Join(base, "home"),
	}
	report := doctor.Run(context.Background(), deps)
	got := result(t, report, "checkout")
	if got.Status != doctor.StatusFail {
		t.Fatalf("checkout check = %+v, want fail", got)
	}
	if !report.Failed() {
		t.Fatal("Report.Failed() = false on a totally uninitialized machine")
	}
}

func TestRunGHMissing(t *testing.T) {
	t.Parallel()
	fx := newFixture(t) // deps.GH left nil
	got := result(t, doctor.Run(context.Background(), fx.deps), "gh")
	if got.Status != doctor.StatusFail {
		t.Fatalf("gh check = %+v, want fail", got)
	}
}

func TestRunGHAuthFails(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.deps.GH = ghx.NewClientWithRunner(ghxtest.New(t, ghxtest.Call{
		Args: []string{"auth", "status"}, Result: ghx.Result{ExitCode: 1, Stderr: "not logged in"},
	}), "/usr/bin/gh")
	got := result(t, doctor.Run(context.Background(), fx.deps), "gh")
	if got.Status != doctor.StatusFail {
		t.Fatalf("gh check = %+v, want fail", got)
	}
}

// TestRunGHAuthInvalidLeavesClassifiableDetail pins the PRODUCER half of the
// single-classifier-seam design: checkGH must leave gh's raw `gh auth status`
// stderr in CheckResult.Detail so the hub's arm-from-doctor path can re-classify
// it through ghx.Classify. The fixture is the real multi-line keyring block whose
// auth-invalid signatures sit on the second and third lines (never the first), so
// a future Detail sanitization — a firstLine() truncation, a generic replacement —
// would drop the signature and silently kill arm-from-doctor while every other
// suite stayed green. This is the guard at the seam that would break.
func TestRunGHAuthInvalidLeavesClassifiableDetail(t *testing.T) {
	t.Parallel()
	const keyringStderr = "github.com\n" +
		"  X Failed to log in to github.com account Sawmonabo (keyring)\n" +
		"  - The token in keyring is invalid.\n" +
		"  - To re-authenticate, run: gh auth login -h github.com"
	fx := newFixture(t)
	fx.deps.GH = ghx.NewClientWithRunner(ghxtest.New(t, ghxtest.Call{
		Args: []string{"auth", "status"}, Result: ghx.Result{ExitCode: 1, Stderr: keyringStderr},
	}), "/usr/bin/gh")

	got := result(t, doctor.Run(context.Background(), fx.deps), "gh")
	if got.Status != doctor.StatusFail {
		t.Fatalf("gh check = %+v, want fail", got)
	}
	if class := ghx.Classify(got.Detail); class != ghx.FailureAuthInvalid {
		t.Fatalf("ghx.Classify(gh Detail) = %v, want FailureAuthInvalid; Detail = %q", class, got.Detail)
	}
}

func TestRunRemoteUnreachable(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	mustGit(t, fx.dir, "remote", "set-url", "origin", filepath.Join(fx.base, "does-not-exist.git"))
	got := result(t, doctor.Run(context.Background(), fx.deps), "remote")
	if got.Status != doctor.StatusFail {
		t.Fatalf("remote check = %+v, want fail", got)
	}
	if !strings.Contains(got.Detail, "unreachable") {
		t.Fatalf("remote Detail = %q, want it to say unreachable", got.Detail)
	}
	// The row must carry git's own first stderr line, not just the label —
	// the operator needs to see WHY the probe failed.
	if got.Detail == "origin is unreachable — commits will queue locally" {
		t.Fatalf("remote Detail carries no git diagnostic beyond the label: %q", got.Detail)
	}
}

func TestRunRemoteSkippedWhenOffline(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.deps.Offline = true
	report := doctor.Run(context.Background(), fx.deps)
	if hasCheck(report, "remote") {
		t.Fatalf("remote check present despite Offline: %+v", report.Results)
	}
}

func TestRunCredentialHelperMissingOnHTTPSRemote(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.deps.Offline = true // this remote doesn't really exist; skip the network check
	mustGit(t, fx.dir, "remote", "set-url", "origin", "https://example.invalid/agent-brain-memories.git")
	got := result(t, doctor.Run(context.Background(), fx.deps), "credential-helper")
	if got.Status != doctor.StatusFail {
		t.Fatalf("credential-helper check = %+v, want fail", got)
	}
}

func TestRunCredentialHelperWiredOnHTTPSRemote(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.deps.Offline = true
	mustGit(t, fx.dir, "remote", "set-url", "origin", "https://example.invalid/agent-brain-memories.git")
	if err := gitx.InstallCredentialHelper(context.Background(), fx.dir, "/usr/bin/gh"); err != nil {
		t.Fatal(err)
	}
	got := result(t, doctor.Run(context.Background(), fx.deps), "credential-helper")
	if got.Status != doctor.StatusOK {
		t.Fatalf("credential-helper check = %+v, want ok", got)
	}
}

func TestRunCredentialHelperInapplicableOnSSHRemote(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	mustGit(t, fx.dir, "remote", "set-url", "origin", "git@github.com:example/agent-brain-memories.git")
	report := doctor.Run(context.Background(), fx.deps)
	if hasCheck(report, "credential-helper") {
		t.Fatalf("credential-helper present on an ssh remote: %+v", report.Results)
	}
}

func TestRunRegistryLocalCorrupt(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	if err := os.WriteFile(fx.deps.Paths.LocalRegistryFile(), []byte("not valid toml {{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := result(t, doctor.Run(context.Background(), fx.deps), "registry-local")
	if got.Status != doctor.StatusFail {
		t.Fatalf("registry-local check = %+v, want fail", got)
	}
}

func TestRunConflictLogOversized(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	big := make([]byte, 5<<20)
	if err := os.WriteFile(fx.deps.Paths.ConflictLogFile(), big, 0o600); err != nil {
		t.Fatal(err)
	}
	got := result(t, doctor.Run(context.Background(), fx.deps), "conflict-log")
	if got.Status != doctor.StatusWarn {
		t.Fatalf("conflict-log check = %+v, want warn", got)
	}
}

// TestRunClaudePrereqsEnvDisabled uses t.Setenv, so it cannot run in
// parallel with other subtests that touch the same process-wide env var.
func TestRunClaudePrereqsEnvDisabled(t *testing.T) {
	fx := newFixture(t)
	t.Setenv("CLAUDE_CODE_DISABLE_AUTO_MEMORY", "1")
	got := result(t, doctor.Run(context.Background(), fx.deps), "claude-prereqs")
	if got.Status != doctor.StatusWarn {
		t.Fatalf("claude-prereqs check = %+v, want warn", got)
	}
}

func TestRunClaudePrereqsSettingsDisabled(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	claudeDir := filepath.Join(fx.home, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(`{"autoMemoryEnabled": false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := result(t, doctor.Run(context.Background(), fx.deps), "claude-prereqs")
	if got.Status != doctor.StatusWarn {
		t.Fatalf("claude-prereqs check = %+v, want warn", got)
	}
}

func TestRunCodexPrereqsSkippedWhenNotEnrolled(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	report := doctor.Run(context.Background(), fx.deps)
	if hasCheck(report, "codex-prereqs") {
		t.Fatalf("codex-prereqs present with no codex unit enrolled: %+v", report.Results)
	}
}

func TestRunCodexPrereqsWarnsWhenFeatureDisabled(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.deps.Enrolled = []repo.Unit{{Provider: "codex", Folder: repo.GlobalFolder, LocalDir: filepath.Join(fx.home, ".codex", "memories")}}
	codexHome := filepath.Join(fx.home, ".codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("[features]\nmemories = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := result(t, doctor.Run(context.Background(), fx.deps), "codex-prereqs")
	if got.Status != doctor.StatusWarn {
		t.Fatalf("codex-prereqs check = %+v, want warn", got)
	}
}

func TestRunCodexPrereqsOKWhenFeatureEnabled(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.deps.Enrolled = []repo.Unit{{Provider: "codex", Folder: repo.GlobalFolder, LocalDir: filepath.Join(fx.home, ".codex", "memories")}}
	codexHome := filepath.Join(fx.home, ".codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("[features]\nmemories = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := result(t, doctor.Run(context.Background(), fx.deps), "codex-prereqs")
	if got.Status != doctor.StatusOK {
		t.Fatalf("codex-prereqs check = %+v, want ok", got)
	}
}

func TestRunLegacyLeftoversWarns(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	if err := os.MkdirAll(filepath.Join(fx.home, ".agent-brain"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(fx.home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "ab-claude"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := result(t, doctor.Run(context.Background(), fx.deps), "legacy-leftovers")
	if got.Status != doctor.StatusWarn {
		t.Fatalf("legacy-leftovers check = %+v, want warn", got)
	}
}

func TestRunSettingsErrSurfaced(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.deps.SettingsErr = errors.New("boom: bad toml")
	got := result(t, doctor.Run(context.Background(), fx.deps), "settings")
	if got.Status != doctor.StatusFail || !strings.Contains(got.Detail, "boom") {
		t.Fatalf("settings check = %+v, want fail naming the error", got)
	}
}

func TestRunDaemonPingApplicability(t *testing.T) {
	t.Parallel()
	t.Run("nil_is_skipped", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t)
		report := doctor.Run(context.Background(), fx.deps)
		if hasCheck(report, "daemon") {
			t.Fatalf("daemon check present with a nil DaemonPing: %+v", report.Results)
		}
	})
	t.Run("error_fails", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t)
		fx.deps.DaemonPing = func(context.Context) error { return errors.New("dial: no such file") }
		got := result(t, doctor.Run(context.Background(), fx.deps), "daemon")
		if got.Status != doctor.StatusFail {
			t.Fatalf("daemon check = %+v, want fail", got)
		}
	})
	t.Run("nil_error_ok", func(t *testing.T) {
		t.Parallel()
		fx := newFixture(t)
		fx.deps.DaemonPing = func(context.Context) error { return nil }
		got := result(t, doctor.Run(context.Background(), fx.deps), "daemon")
		if got.Status != doctor.StatusOK {
			t.Fatalf("daemon check = %+v, want ok", got)
		}
	})
}

// TestFixRepairsFiltersAndAttributes pins Fix's contract: it applies the
// idempotent repairs unconditionally, marks the repaired checks Fixed,
// and the re-run battery shows both clean afterward.
func TestFixRepairsFiltersAndAttributes(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	mustGit(t, fx.dir, "config", "--local", "filter.agentbrain.required", "false")
	if err := os.WriteFile(repo.NewLayout(fx.dir).AttributesFile(), []byte("corrupted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := doctor.Fix(context.Background(), fx.deps)
	if err != nil {
		t.Fatalf("Fix() error = %v", err)
	}
	filters := result(t, report, "filters")
	attributes := result(t, report, "attributes")
	if filters.Status != doctor.StatusOK || !filters.Fixed {
		t.Errorf("filters after Fix = %+v, want ok and Fixed", filters)
	}
	if attributes.Status != doctor.StatusOK || !attributes.Fixed {
		t.Errorf("attributes after Fix = %+v, want ok and Fixed", attributes)
	}
}

// TestFixIsIdempotent pins that running Fix twice in a row converges to
// the identical result — a repair applied to an already-healthy machine
// must be a safe no-op, never a second, different mutation.
func TestFixIsIdempotent(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	first, err := doctor.Fix(context.Background(), fx.deps)
	if err != nil {
		t.Fatalf("first Fix() error = %v", err)
	}
	second, err := doctor.Fix(context.Background(), fx.deps)
	if err != nil {
		t.Fatalf("second Fix() error = %v", err)
	}
	if len(first.Results) != len(second.Results) {
		t.Fatalf("Fix() result count changed across runs: %d vs %d", len(first.Results), len(second.Results))
	}
	for _, name := range []string{"filters", "attributes"} {
		a, b := result(t, first, name), result(t, second, name)
		if a.Status != b.Status || a.Detail != b.Detail {
			t.Errorf("%s changed across repeated Fix(): %+v vs %+v", name, a, b)
		}
	}
}

func TestFixSkipsCredentialHelperWithoutGH(t *testing.T) {
	t.Parallel()
	fx := newFixture(t) // deps.GH left nil
	report, err := doctor.Fix(context.Background(), fx.deps)
	if err != nil {
		t.Fatalf("Fix() error = %v", err)
	}
	if hasCheck(report, "credential-helper") {
		got := result(t, report, "credential-helper")
		if got.Fixed {
			t.Errorf("credential-helper marked Fixed with a nil GH client: %+v", got)
		}
	}
}

// TestFixDoesNotClaimAttributesFixedWithNilRegistry pins the rule:
// Fix skips repo.WriteAttributes when deps.Registry is nil (nothing to
// generate canonical content from), so Fixed must follow that same
// condition — exactly as credential-helper's Fixed already follows
// deps.GH != nil above. Reporting Fixed:true for a repair that did not run
// is a false claim to whoever reads the report (CLI operator or --json
// consumer).
func TestFixDoesNotClaimAttributesFixedWithNilRegistry(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	fx.deps.Registry = nil

	report, err := doctor.Fix(context.Background(), fx.deps)
	if err != nil {
		t.Fatalf("Fix() error = %v", err)
	}
	attributes := result(t, report, "attributes")
	if attributes.Fixed {
		t.Errorf("attributes marked Fixed with a nil Registry (WriteAttributes never ran): %+v", attributes)
	}
}
