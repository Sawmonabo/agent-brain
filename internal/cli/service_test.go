package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/service"
)

// fakeServiceController is a scriptable service.Controller test double
// for CLI-layer tests — service install/uninstall must never touch a
// real system in tests, so callers here implement the interface
// directly (same pattern as fakeProvider in init_test.go) rather than
// reaching into internal/service's own fakes.
type fakeServiceController struct {
	installCalls   int
	installErr     error
	installWarning string

	uninstallCalls int
	uninstallErr   error

	startCalls int
	startErr   error
	stopCalls  int
	stopErr    error

	statusErr    error
	lingerStatus string
}

func (f *fakeServiceController) Install() (string, error) {
	f.installCalls++
	if f.installCalls > 1 {
		return f.installWarning, service.ErrAlreadyInstalled
	}
	return f.installWarning, f.installErr
}

func (f *fakeServiceController) Uninstall() error {
	f.uninstallCalls++
	if f.uninstallCalls > 1 {
		return service.ErrNotInstalled
	}
	return f.uninstallErr
}

func (f *fakeServiceController) Start() error { f.startCalls++; return f.startErr }
func (f *fakeServiceController) Stop() error  { f.stopCalls++; return f.stopErr }
func (f *fakeServiceController) Status() (service.Status, error) {
	if f.statusErr != nil {
		return service.StatusUnknown, f.statusErr
	}
	return service.StatusRunning, nil
}
func (f *fakeServiceController) LingerStatus() string { return f.lingerStatus }

// --- T3 review fix: shared install/status helpers ---
//
// installServiceAndReport and printServiceStatus are the ONE place the
// idempotency branch, warning print, and linger-line logic live —
// runServiceInstall/runServiceStatus (the standalone `service install`/
// `service status` commands) and stepService (init's own service step,
// internal/cli/initsteps.go) all delegate to these two rather than each
// hand-rolling the same branches. stepService itself constructs a REAL
// service.Controller (never fake-able without touching a live system —
// see internal/service's own "construction only" test discipline), so
// these direct, fake-controller tests are what actually covers "the
// stepService path": after the fix, stepService's own body is nothing
// but "construct controller, call these two helpers."

// TestInstallServiceAndReportFreshInstallPrintsOK proves a clean install
// prints the plain success line and returns nil.
func TestInstallServiceAndReportFreshInstallPrintsOK(t *testing.T) {
	t.Parallel()
	controller := &fakeServiceController{}
	var out bytes.Buffer
	if err := installServiceAndReport(controller, &out); err != nil {
		t.Fatalf("installServiceAndReport: %v, want nil", err)
	}
	if !strings.Contains(out.String(), "ok") {
		t.Fatalf("output = %q, want a success line", out.String())
	}
}

// TestInstallServiceAndReportIdempotentPrintsNothingToDo proves the
// second call against an already-installed unit prints the nothing-to-do
// message and returns the ErrAlreadyInstalled-wrapped error (never a
// string match — errors.Is) so a caller can still detect idempotency if
// it wants to.
func TestInstallServiceAndReportIdempotentPrintsNothingToDo(t *testing.T) {
	t.Parallel()
	controller := &fakeServiceController{}
	var firstOut bytes.Buffer
	if err := installServiceAndReport(controller, &firstOut); err != nil {
		t.Fatalf("first call: %v", err)
	}

	var secondOut bytes.Buffer
	err := installServiceAndReport(controller, &secondOut)
	if !errors.Is(err, service.ErrAlreadyInstalled) {
		t.Fatalf("second call error = %v, want errors.Is(_, ErrAlreadyInstalled)", err)
	}
	if !strings.Contains(secondOut.String(), "already installed") {
		t.Fatalf("second call output = %q, want the nothing-to-do message", secondOut.String())
	}
}

// TestInstallServiceAndReportHardFailurePrintsNothing proves a genuine
// install failure returns the raw error and prints NOTHING — there is no
// "ok"/"already installed" line to show for a failed install, and no
// warning either (there is no unit to keep alive).
func TestInstallServiceAndReportHardFailurePrintsNothing(t *testing.T) {
	t.Parallel()
	want := errors.New("permission denied")
	controller := &fakeServiceController{installErr: want}
	var out bytes.Buffer
	err := installServiceAndReport(controller, &out)
	if !errors.Is(err, want) {
		t.Fatalf("installServiceAndReport error = %v, want it to wrap %v", err, want)
	}
	if out.String() != "" {
		t.Fatalf("output = %q, want nothing printed on a hard failure", out.String())
	}
}

// TestInstallServiceAndReportPrintsWarningAfterMessage proves a non-empty
// warning (a WSL2 linger failure) is printed after the success
// line, never instead of it.
func TestInstallServiceAndReportPrintsWarningAfterMessage(t *testing.T) {
	t.Parallel()
	controller := &fakeServiceController{installWarning: "WARNING: enable-linger failed for testuser"}
	var out bytes.Buffer
	if err := installServiceAndReport(controller, &out); err != nil {
		t.Fatalf("installServiceAndReport: %v, want nil", err)
	}
	okIndex := strings.Index(out.String(), "ok")
	warningIndex := strings.Index(out.String(), "WARNING")
	if okIndex == -1 || warningIndex == -1 || warningIndex < okIndex {
		t.Fatalf("output = %q, want the success line before the warning", out.String())
	}
}

// TestPrintServiceStatusPrintsLingerLine proves the status line plus, on
// WSL2, the linger advisory print together.
func TestPrintServiceStatusPrintsLingerLine(t *testing.T) {
	t.Parallel()
	controller := &fakeServiceController{lingerStatus: "linger: enabled (service will survive logout)"}
	var out bytes.Buffer
	if err := printServiceStatus(&out, controller); err != nil {
		t.Fatalf("printServiceStatus: %v", err)
	}
	if !strings.Contains(out.String(), "running") {
		t.Fatalf("output = %q, want the plain status line", out.String())
	}
	if !strings.Contains(out.String(), "linger: enabled") {
		t.Fatalf("output = %q, want the linger advisory line", out.String())
	}
}

// TestPrintServiceStatusSilentWithoutLingerLine proves the advisory is
// omitted entirely (not printed empty) when LingerStatus reports "".
func TestPrintServiceStatusSilentWithoutLingerLine(t *testing.T) {
	t.Parallel()
	controller := &fakeServiceController{}
	var out bytes.Buffer
	if err := printServiceStatus(&out, controller); err != nil {
		t.Fatalf("printServiceStatus: %v", err)
	}
	if strings.Contains(out.String(), "linger") {
		t.Fatalf("output = %q, want no linger line", out.String())
	}
}

// TestPrintServiceStatusPropagatesStatusError proves a Status() failure
// is surfaced, not swallowed.
func TestPrintServiceStatusPropagatesStatusError(t *testing.T) {
	t.Parallel()
	want := errors.New("dial failed")
	controller := &fakeServiceController{statusErr: want}
	var out bytes.Buffer
	if err := printServiceStatus(&out, controller); !errors.Is(err, want) {
		t.Fatalf("printServiceStatus error = %v, want it to wrap %v", err, want)
	}
}

// --- goal-state idempotent start/stop UX ---
//
// startServiceAndReport/stopServiceAndReport are the ONE place the
// run-state idempotency branch and its message live — runServiceStart/
// runServiceStop (the standalone commands) and stepService (init's own
// service step) all delegate here, exactly as installServiceAndReport
// does for Install. The motivating failure: re-running `agent-brain
// init` against a healthy running daemon died on launchd's
// already-loaded EIO instead of no-opping (2026-07-10).

// TestStartServiceAndReportFreshStartPrintsOK proves a clean start
// prints the plain success line and returns nil.
func TestStartServiceAndReportFreshStartPrintsOK(t *testing.T) {
	t.Parallel()
	controller := &fakeServiceController{}
	var out bytes.Buffer
	if err := startServiceAndReport(controller, &out); err != nil {
		t.Fatalf("startServiceAndReport: %v, want nil", err)
	}
	if !strings.Contains(out.String(), "service start: ok") {
		t.Fatalf("output = %q, want the success line", out.String())
	}
}

// TestStartServiceAndReportAlreadyRunningPrintsNothingToDo proves the
// already-running sentinel becomes the nothing-to-do message and is
// still returned (errors.Is-matchable) so callers can branch on it.
func TestStartServiceAndReportAlreadyRunningPrintsNothingToDo(t *testing.T) {
	t.Parallel()
	controller := &fakeServiceController{startErr: service.ErrAlreadyRunning}
	var out bytes.Buffer
	err := startServiceAndReport(controller, &out)
	if !errors.Is(err, service.ErrAlreadyRunning) {
		t.Fatalf("startServiceAndReport error = %v, want errors.Is(_, ErrAlreadyRunning)", err)
	}
	if !strings.Contains(out.String(), "already running — nothing to do") {
		t.Fatalf("output = %q, want the nothing-to-do message", out.String())
	}
}

// TestStartServiceAndReportHardFailurePrintsNothing proves a genuine
// start failure returns the raw error and prints NOTHING — callers add
// their own context prefix.
func TestStartServiceAndReportHardFailurePrintsNothing(t *testing.T) {
	t.Parallel()
	want := errors.New("spawn failed: permission denied")
	controller := &fakeServiceController{startErr: want}
	var out bytes.Buffer
	err := startServiceAndReport(controller, &out)
	if !errors.Is(err, want) {
		t.Fatalf("startServiceAndReport error = %v, want it to wrap %v", err, want)
	}
	if out.String() != "" {
		t.Fatalf("output = %q, want nothing printed on a hard failure", out.String())
	}
}

// TestRunServiceStartSwallowsSentinelWrapsFailure proves the standalone
// command exits 0 on the idempotent case and prefixes a genuine failure
// with its own context.
func TestRunServiceStartSwallowsSentinelWrapsFailure(t *testing.T) {
	t.Parallel()
	t.Run("already running exits clean", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		if err := runServiceStart(&out, &fakeServiceController{startErr: service.ErrAlreadyRunning}); err != nil {
			t.Fatalf("runServiceStart error = %v, want nil", err)
		}
	})
	t.Run("hard failure carries context", func(t *testing.T) {
		t.Parallel()
		want := errors.New("spawn failed")
		var out bytes.Buffer
		err := runServiceStart(&out, &fakeServiceController{startErr: want})
		if !errors.Is(err, want) {
			t.Fatalf("runServiceStart error = %v, want it to wrap %v", err, want)
		}
		if !strings.Contains(err.Error(), "service start:") {
			t.Fatalf("runServiceStart error = %q, want the command's context prefix", err)
		}
	})
}

// TestStopServiceAndReportNotRunningPrintsNothingToDo proves the
// symmetric stop case: the not-running sentinel becomes the
// nothing-to-do message and stays errors.Is-matchable.
func TestStopServiceAndReportNotRunningPrintsNothingToDo(t *testing.T) {
	t.Parallel()
	controller := &fakeServiceController{stopErr: service.ErrNotRunning}
	var out bytes.Buffer
	err := stopServiceAndReport(controller, &out)
	if !errors.Is(err, service.ErrNotRunning) {
		t.Fatalf("stopServiceAndReport error = %v, want errors.Is(_, ErrNotRunning)", err)
	}
	if !strings.Contains(out.String(), "not running — nothing to do") {
		t.Fatalf("output = %q, want the nothing-to-do message", out.String())
	}
}

// TestRunServiceStopSwallowsSentinelWrapsFailure mirrors the start
// command's exit-code contract for stop.
func TestRunServiceStopSwallowsSentinelWrapsFailure(t *testing.T) {
	t.Parallel()
	t.Run("not running exits clean", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		if err := runServiceStop(&out, &fakeServiceController{stopErr: service.ErrNotRunning}); err != nil {
			t.Fatalf("runServiceStop error = %v, want nil", err)
		}
		if !strings.Contains(out.String(), "not running — nothing to do") {
			t.Fatalf("output = %q, want the nothing-to-do message", out.String())
		}
	})
	t.Run("hard failure carries context", func(t *testing.T) {
		t.Parallel()
		want := errors.New("signal not delivered")
		var out bytes.Buffer
		err := runServiceStop(&out, &fakeServiceController{stopErr: want})
		if !errors.Is(err, want) {
			t.Fatalf("runServiceStop error = %v, want it to wrap %v", err, want)
		}
		if !strings.Contains(err.Error(), "service stop:") {
			t.Fatalf("runServiceStop error = %q, want the command's context prefix", err)
		}
	})
}

// --- 3b: idempotent install/uninstall UX ---

// TestRunServiceInstallTwiceIsIdempotent proves a second `service install`
// against an already-installed unit is a nothing-to-do success (exit 0),
// branching on service.ErrAlreadyInstalled via errors.Is — never a
// string match on the CLI side either.
func TestRunServiceInstallTwiceIsIdempotent(t *testing.T) {
	t.Parallel()
	controller := &fakeServiceController{}

	var firstOut bytes.Buffer
	if err := runServiceInstall(&firstOut, controller); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if !strings.Contains(firstOut.String(), "ok") {
		t.Fatalf("first install output = %q, want a success line", firstOut.String())
	}

	var secondOut bytes.Buffer
	if err := runServiceInstall(&secondOut, controller); err != nil {
		t.Fatalf("second install: %v, want nil (idempotent no-op, exit 0)", err)
	}
	if !strings.Contains(secondOut.String(), "already installed") {
		t.Fatalf("second install output = %q, want the nothing-to-do message", secondOut.String())
	}
}

// TestRunServiceInstallPropagatesRealErrors proves a genuine install
// failure (not the already-installed sentinel) still fails the command.
func TestRunServiceInstallPropagatesRealErrors(t *testing.T) {
	t.Parallel()
	controller := &fakeServiceController{installErr: errors.New("permission denied")}
	var out bytes.Buffer
	if err := runServiceInstall(&out, controller); err == nil {
		t.Fatal("runServiceInstall: want the real error to propagate")
	}
}

// --- 3c: WSL2 systemd lingering ---

// TestRunServiceInstallPrintsLingerWarning proves a non-empty warning
// from Install (a WSL2 enable-linger failure) is printed after
// the success line — informational only, never turns a successful
// install into a failing command.
func TestRunServiceInstallPrintsLingerWarning(t *testing.T) {
	t.Parallel()
	controller := &fakeServiceController{installWarning: "WARNING: failed to enable systemd lingering for testuser — run `loginctl enable-linger testuser` by hand"}
	var out bytes.Buffer
	if err := runServiceInstall(&out, controller); err != nil {
		t.Fatalf("runServiceInstall: %v, want nil (a linger warning must not fail install)", err)
	}
	if !strings.Contains(out.String(), "loginctl enable-linger testuser") {
		t.Fatalf("runServiceInstall output = %q, want the linger warning printed", out.String())
	}
}

// TestRunServiceStatusPrintsLingerAdvisory proves `service status` prints
// the WSL2 linger advisory line after the plain status line when the
// controller reports one.
func TestRunServiceStatusPrintsLingerAdvisory(t *testing.T) {
	t.Parallel()
	controller := &fakeServiceController{lingerStatus: "linger: enabled (service will survive logout)"}
	var out bytes.Buffer
	if err := runServiceStatus(&out, controller); err != nil {
		t.Fatalf("runServiceStatus: %v", err)
	}
	if !strings.Contains(out.String(), "running") {
		t.Fatalf("runServiceStatus output = %q, want the plain status line", out.String())
	}
	if !strings.Contains(out.String(), "linger: enabled") {
		t.Fatalf("runServiceStatus output = %q, want the linger advisory line", out.String())
	}
}

// TestRunServiceStatusSilentWhenNoLingerAdvisory proves the advisory
// line is omitted entirely (not printed empty) on non-WSL2, where
// LingerStatus reports "".
func TestRunServiceStatusSilentWhenNoLingerAdvisory(t *testing.T) {
	t.Parallel()
	controller := &fakeServiceController{}
	var out bytes.Buffer
	if err := runServiceStatus(&out, controller); err != nil {
		t.Fatalf("runServiceStatus: %v", err)
	}
	if strings.Contains(out.String(), "linger") {
		t.Fatalf("runServiceStatus output = %q, want no linger line when LingerStatus() is empty", out.String())
	}
}

// TestRunServiceUninstallWhenAlreadyGoneIsIdempotent mirrors the install
// case for the symmetric "not installed" sentinel.
func TestRunServiceUninstallWhenAlreadyGoneIsIdempotent(t *testing.T) {
	t.Parallel()
	controller := &fakeServiceController{}

	var firstOut bytes.Buffer
	if err := runServiceUninstall(&firstOut, controller); err != nil {
		t.Fatalf("first uninstall: %v", err)
	}

	var secondOut bytes.Buffer
	if err := runServiceUninstall(&secondOut, controller); err != nil {
		t.Fatalf("second uninstall: %v, want nil (idempotent no-op, exit 0)", err)
	}
	if !strings.Contains(secondOut.String(), "not installed") {
		t.Fatalf("second uninstall output = %q, want the nothing-to-do message", secondOut.String())
	}
}

// TestServiceLogsPrintsTail proves `service logs -n 2` on a fabricated
// 5-line daemon.log prints only the last two lines plus a trailer naming
// the log path.
func TestServiceLogsPrintsTail(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir())
	t.Setenv("AGENT_BRAIN_DATA_DIR", dataDir)
	logPath := filepath.Join(dataDir, "daemon.log")
	content := "line1\nline2\nline3\nline4\nline5\n"
	if err := os.WriteFile(logPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, nil, "service", "logs", "-n", "2")
	if err != nil {
		t.Fatalf("service logs: %v", err)
	}
	got := string(out)
	for _, want := range []string{"line4", "line5", logPath} {
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Fatalf("service logs output missing %q:\n%s", want, got)
		}
	}
	if bytes.Contains([]byte(got), []byte("line3")) {
		t.Fatalf("service logs -n 2 printed more than 2 lines:\n%s", got)
	}
}

// TestServiceLogsDefaultLineCount proves the documented default of 100
// lines: a log shorter than that prints in full without a -n flag.
func TestServiceLogsDefaultLineCount(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir())
	t.Setenv("AGENT_BRAIN_DATA_DIR", dataDir)
	logPath := filepath.Join(dataDir, "daemon.log")
	if err := os.WriteFile(logPath, []byte("only line\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, nil, "service", "logs")
	if err != nil {
		t.Fatalf("service logs: %v", err)
	}
	if !bytes.Contains(out, []byte("only line")) {
		t.Fatalf("service logs (default -n) missing content:\n%s", out)
	}
}

// TestServiceLogsMissingFile proves logs works with the daemon down —
// exactly when logs matter most — and exits 0 with a friendly message
// rather than a raw stat error.
func TestServiceLogsMissingFile(t *testing.T) {
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir())
	t.Setenv("AGENT_BRAIN_DATA_DIR", t.TempDir())

	out, err := runCmd(t, nil, "service", "logs")
	if err != nil {
		t.Fatalf("service logs on a missing file must exit 0: %v", err)
	}
	if !bytes.Contains(out, []byte("no daemon log yet")) {
		t.Fatalf("missing-log message wrong:\n%s", out)
	}
}

// TestServiceLogsNotesRotationSibling proves the trailer names the .1
// rotation generation when the mid-run rotation has produced one.
func TestServiceLogsNotesRotationSibling(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir())
	t.Setenv("AGENT_BRAIN_DATA_DIR", dataDir)
	logPath := filepath.Join(dataDir, "daemon.log")
	if err := os.WriteFile(logPath, []byte("current\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath+".1", []byte("older\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, nil, "service", "logs")
	if err != nil {
		t.Fatalf("service logs: %v", err)
	}
	if !bytes.Contains(out, []byte(logPath+".1")) {
		t.Fatalf("service logs trailer must name the .1 sibling when present:\n%s", out)
	}
}
