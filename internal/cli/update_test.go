package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/selfupdate"
	"github.com/Sawmonabo/agent-brain/internal/service"
)

// fakeUpdateEngine scripts the updateEngine seam — the real mechanics
// (resolution, checksum, extraction, swap) live in internal/selfupdate and
// are covered there; these tests cover runUpdate's flow and messages.
type fakeUpdateEngine struct {
	decision selfupdate.Decision
	checkErr error

	applyCalls int
	applyTag   string
	applyErr   error
}

func (f *fakeUpdateEngine) Check(context.Context, selfupdate.Options) (selfupdate.Decision, error) {
	return f.decision, f.checkErr
}

func (f *fakeUpdateEngine) Apply(_ context.Context, _ selfupdate.Options, targetTag string) error {
	f.applyCalls++
	f.applyTag = targetTag
	return f.applyErr
}

// noRestartCalled is a restart stub that fails the test if invoked.
func noRestartCalled(t *testing.T) func(context.Context, io.Writer) error {
	return func(context.Context, io.Writer) error {
		t.Fatal("restart invoked, want none")
		return nil
	}
}

func TestRunUpdateAlreadyUpToDate(t *testing.T) {
	t.Parallel()
	engine := &fakeUpdateEngine{decision: selfupdate.Decision{Latest: "v2.0.0", UpdateNeeded: false}}
	var out bytes.Buffer
	opts := selfupdate.Options{CurrentVersion: "2.0.0"}
	if err := runUpdate(t.Context(), &out, engine, opts, false, false, noRestartCalled(t)); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if !strings.Contains(out.String(), "already up to date (2.0.0; latest release v2.0.0)") {
		t.Fatalf("output = %q, want the up-to-date line", out.String())
	}
	if engine.applyCalls != 0 {
		t.Fatalf("Apply called %d times, want 0", engine.applyCalls)
	}
}

// TestRunUpdateCheckOnlyReportsWithoutInstalling proves --check names the
// available version and never applies or restarts.
func TestRunUpdateCheckOnlyReportsWithoutInstalling(t *testing.T) {
	t.Parallel()
	engine := &fakeUpdateEngine{decision: selfupdate.Decision{Latest: "v2.1.0", UpdateNeeded: true}}
	var out bytes.Buffer
	opts := selfupdate.Options{CurrentVersion: "2.0.0"}
	if err := runUpdate(t.Context(), &out, engine, opts, true, false, noRestartCalled(t)); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if !strings.Contains(out.String(), "v2.1.0 available (running 2.0.0)") {
		t.Fatalf("output = %q, want the availability line", out.String())
	}
	if engine.applyCalls != 0 {
		t.Fatalf("Apply called %d times, want 0 in --check mode", engine.applyCalls)
	}
}

// TestRunUpdateAppliesAndRestarts proves the happy path: apply the checked
// decision's tag, report the transition, then hand off to restart.
func TestRunUpdateAppliesAndRestarts(t *testing.T) {
	t.Parallel()
	engine := &fakeUpdateEngine{decision: selfupdate.Decision{Latest: "v2.1.0", UpdateNeeded: true}}
	var out bytes.Buffer
	restartCalls := 0
	restart := func(context.Context, io.Writer) error {
		restartCalls++
		return nil
	}
	opts := selfupdate.Options{CurrentVersion: "2.0.0", TargetPath: "/home/user/.local/bin/agent-brain", GOOS: "linux", GOARCH: "arm64"}
	if err := runUpdate(t.Context(), &out, engine, opts, false, false, restart); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if engine.applyCalls != 1 || engine.applyTag != "v2.1.0" {
		t.Fatalf("Apply calls/tag = %d/%q, want 1/v2.1.0", engine.applyCalls, engine.applyTag)
	}
	if restartCalls != 1 {
		t.Fatalf("restart calls = %d, want 1", restartCalls)
	}
	if !strings.Contains(out.String(), "installed 2.0.0 -> v2.1.0 at /home/user/.local/bin/agent-brain") {
		t.Fatalf("output = %q, want the installed line", out.String())
	}
}

// TestRunUpdateNoRestartSkipsAndExplains proves --no-restart leaves the
// service alone and says how to restart it later.
func TestRunUpdateNoRestartSkipsAndExplains(t *testing.T) {
	t.Parallel()
	engine := &fakeUpdateEngine{decision: selfupdate.Decision{Latest: "v2.1.0", UpdateNeeded: true}}
	var out bytes.Buffer
	opts := selfupdate.Options{CurrentVersion: "2.0.0"}
	if err := runUpdate(t.Context(), &out, engine, opts, false, true, noRestartCalled(t)); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if !strings.Contains(out.String(), "old version (--no-restart)") {
		t.Fatalf("output = %q, want the no-restart explanation", out.String())
	}
}

// TestRunUpdateErrorsCarryContext proves both failure sites prefix
// "update:" so fang's error rendering names the command.
func TestRunUpdateErrorsCarryContext(t *testing.T) {
	t.Parallel()
	t.Run("check failure", func(t *testing.T) {
		t.Parallel()
		want := errors.New("gh release list: HTTP 500")
		engine := &fakeUpdateEngine{checkErr: want}
		err := runUpdate(t.Context(), io.Discard, engine, selfupdate.Options{}, false, false, noRestartCalled(t))
		if !errors.Is(err, want) || !strings.Contains(err.Error(), "update:") {
			t.Fatalf("runUpdate error = %v, want update-prefixed wrap of %v", err, want)
		}
	})
	t.Run("apply failure", func(t *testing.T) {
		t.Parallel()
		want := errors.New("checksum mismatch")
		engine := &fakeUpdateEngine{
			decision: selfupdate.Decision{Latest: "v2.1.0", UpdateNeeded: true},
			applyErr: want,
		}
		err := runUpdate(t.Context(), io.Discard, engine, selfupdate.Options{}, false, false, noRestartCalled(t))
		if !errors.Is(err, want) || !strings.Contains(err.Error(), "update:") {
			t.Fatalf("runUpdate error = %v, want update-prefixed wrap of %v", err, want)
		}
	})
}

// statusScriptedController extends the shared fake with a scriptable
// Status result — restartServiceForUpdate branches on StatusNotInstalled.
type statusScriptedController struct {
	fakeServiceController
	status service.Status
}

func (s *statusScriptedController) Status() (service.Status, error) {
	if s.statusErr != nil {
		return service.StatusUnknown, s.statusErr
	}
	return s.status, nil
}

func TestRestartServiceForUpdateSkipsWhenNotInstalled(t *testing.T) {
	t.Parallel()
	controller := &statusScriptedController{status: service.StatusNotInstalled}
	var out bytes.Buffer
	err := restartServiceForUpdate(t.Context(), &out, controller, func(context.Context) string {
		t.Fatal("daemon poll invoked, want none for a not-installed service")
		return ""
	})
	if err != nil {
		t.Fatalf("restartServiceForUpdate: %v", err)
	}
	if !strings.Contains(out.String(), "not installed — nothing to restart") {
		t.Fatalf("output = %q, want the skip explanation", out.String())
	}
	if controller.stopCalls != 0 || controller.startCalls != 0 {
		t.Fatalf("stop/start calls = %d/%d, want 0/0", controller.stopCalls, controller.startCalls)
	}
}

// TestRestartServiceForUpdateBouncesAndConfirms proves the full bounce:
// stop (tolerating not-running), start, then the daemon readiness line
// with the polled version.
func TestRestartServiceForUpdateBouncesAndConfirms(t *testing.T) {
	t.Parallel()
	controller := &statusScriptedController{status: service.StatusRunning}
	controller.stopErr = service.ErrNotRunning // lost race: daemon exited between Status and Stop
	var out bytes.Buffer
	err := restartServiceForUpdate(t.Context(), &out, controller, func(context.Context) string { return "2.1.0" })
	if err != nil {
		t.Fatalf("restartServiceForUpdate: %v", err)
	}
	if controller.startCalls != 1 {
		t.Fatalf("start calls = %d, want 1", controller.startCalls)
	}
	if !strings.Contains(out.String(), "daemon: ready (version 2.1.0)") {
		t.Fatalf("output = %q, want the readiness line", out.String())
	}
}

// TestRestartServiceForUpdateWarnsWhenDaemonSilent proves an unconfirmed
// daemon is a printed warning naming the next diagnostic steps, not a
// silent success and not an error — the binary update itself already
// landed.
func TestRestartServiceForUpdateWarnsWhenDaemonSilent(t *testing.T) {
	t.Parallel()
	controller := &statusScriptedController{status: service.StatusRunning}
	var out bytes.Buffer
	err := restartServiceForUpdate(t.Context(), &out, controller, func(context.Context) string { return "" })
	if err != nil {
		t.Fatalf("restartServiceForUpdate: %v", err)
	}
	if !strings.Contains(out.String(), "not confirmed ready") || !strings.Contains(out.String(), "service logs") {
		t.Fatalf("output = %q, want the unconfirmed-daemon warning with diagnostics", out.String())
	}
}

// TestRestartServiceForUpdateSurfacesStartFailure proves a genuine start
// failure after the swap reaches the caller with context — the one moment
// this command must never shrug through.
func TestRestartServiceForUpdateSurfacesStartFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("spawn failed")
	controller := &statusScriptedController{status: service.StatusStopped}
	controller.startErr = want
	err := restartServiceForUpdate(t.Context(), io.Discard, controller, func(context.Context) string { return "" })
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "start service") {
		t.Fatalf("restartServiceForUpdate error = %v, want start-service wrap of %v", err, want)
	}
}
