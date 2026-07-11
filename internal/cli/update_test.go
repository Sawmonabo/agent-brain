package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/ghx"
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

// TestRunUpdateExplicitEqualReportsRequested proves pinning the version
// already running is a clean no-op with its own message (the generic
// "latest release" line would be misleading for an explicit pin).
func TestRunUpdateExplicitEqualReportsRequested(t *testing.T) {
	t.Parallel()
	engine := &fakeUpdateEngine{decision: selfupdate.Decision{Latest: "v2.0.0", UpdateNeeded: false}}
	var out bytes.Buffer
	opts := selfupdate.Options{CurrentVersion: "2.0.0", RequestedVersion: "2.0.0"}
	if err := runUpdate(t.Context(), &out, engine, opts, false, false, noRestartCalled(t)); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if !strings.Contains(out.String(), "already running the requested version (2.0.0)") {
		t.Fatalf("output = %q, want the requested-version no-op line", out.String())
	}
	if engine.applyCalls != 0 {
		t.Fatalf("Apply called %d times, want 0", engine.applyCalls)
	}
}

// TestRunUpdateCheckOnlyDowngradeNamesEscapeHatch proves --check on an
// older explicit version reports the downgrade and the exact command that
// performs it — without installing anything.
func TestRunUpdateCheckOnlyDowngradeNamesEscapeHatch(t *testing.T) {
	t.Parallel()
	engine := &fakeUpdateEngine{decision: selfupdate.Decision{Latest: "v1.9.0", UpdateNeeded: true, Downgrade: true}}
	var out bytes.Buffer
	opts := selfupdate.Options{CurrentVersion: "2.0.0", RequestedVersion: "1.9.0"}
	if err := runUpdate(t.Context(), &out, engine, opts, true, false, noRestartCalled(t)); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if !strings.Contains(out.String(), "DOWNGRADE") || !strings.Contains(out.String(), "`agent-brain update v1.9.0`") {
		t.Fatalf("output = %q, want the downgrade report naming the escape hatch", out.String())
	}
	if engine.applyCalls != 0 {
		t.Fatalf("Apply called %d times, want 0 in --check mode", engine.applyCalls)
	}
}

// TestRunUpdateDowngradeWarnsAndApplies proves an explicit downgrade
// proceeds — the named version IS the operator's acknowledgment — but only
// after the warning that newer-version state may not load (config parsing
// is strict, ADR 17), pointing at doctor.
func TestRunUpdateDowngradeWarnsAndApplies(t *testing.T) {
	t.Parallel()
	engine := &fakeUpdateEngine{decision: selfupdate.Decision{Latest: "v1.9.0", UpdateNeeded: true, Downgrade: true}}
	var out bytes.Buffer
	restartCalls := 0
	restart := func(context.Context, io.Writer) error {
		restartCalls++
		return nil
	}
	opts := selfupdate.Options{CurrentVersion: "2.0.0", RequestedVersion: "1.9.0", TargetPath: "/home/user/.local/bin/agent-brain"}
	if err := runUpdate(t.Context(), &out, engine, opts, false, false, restart); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if !strings.Contains(out.String(), "DOWNGRADING 2.0.0 -> v1.9.0") || !strings.Contains(out.String(), "agent-brain doctor") {
		t.Fatalf("output = %q, want the downgrade warning naming doctor", out.String())
	}
	if engine.applyCalls != 1 || engine.applyTag != "v1.9.0" {
		t.Fatalf("Apply calls/tag = %d/%q, want 1/v1.9.0", engine.applyCalls, engine.applyTag)
	}
	if restartCalls != 1 {
		t.Fatalf("restart calls = %d, want 1", restartCalls)
	}
}

// TestRunUpdateCheckOnlyExplicitHintEchoesVersion proves --check on an
// explicitly requested newer version names that version in the install
// hint — a bare `agent-brain update` would install latest, not the pin.
func TestRunUpdateCheckOnlyExplicitHintEchoesVersion(t *testing.T) {
	t.Parallel()
	engine := &fakeUpdateEngine{decision: selfupdate.Decision{Latest: "v2.1.0", UpdateNeeded: true}}
	var out bytes.Buffer
	opts := selfupdate.Options{CurrentVersion: "2.0.0", RequestedVersion: "2.1.0"}
	if err := runUpdate(t.Context(), &out, engine, opts, true, false, noRestartCalled(t)); err != nil {
		t.Fatalf("runUpdate: %v", err)
	}
	if !strings.Contains(out.String(), "run `agent-brain update v2.1.0` to install") {
		t.Fatalf("output = %q, want the hint to echo the pinned version", out.String())
	}
}

// TestReleasePickerCandidates proves the picker rows: semver-descending
// order regardless of list order, drafts and non-semver tags dropped,
// channel badge on prereleases, and the running marker.
func TestReleasePickerCandidates(t *testing.T) {
	t.Parallel()
	releases := []ghx.ReleaseInfo{
		{TagName: "v2.0.0-rc.1", IsPrerelease: true},
		{TagName: "nightly-build"},
		{TagName: "v2.1.0"},
		{TagName: "v3.0.0", IsDraft: true},
		{TagName: "v2.0.0"},
	}
	got := releasePickerCandidates(releases, "2.0.0")
	want := []releaseChoice{
		{tag: "v2.1.0", label: "v2.1.0"},
		{tag: "v2.0.0", label: "v2.0.0  ← running", running: true},
		{tag: "v2.0.0-rc.1", label: "v2.0.0-rc.1  (prerelease)", prerelease: true},
	}
	if diff := cmp.Diff(want, got, cmp.AllowUnexported(releaseChoice{})); diff != "" {
		t.Fatalf("releasePickerCandidates mismatch (-want +got):\n%s", diff)
	}
}

// TestWriteReleaseList proves --list's two forms carry the same rows the
// picker offers: plain output is the picker labels verbatim, and --json is
// the structured equivalent.
func TestWriteReleaseList(t *testing.T) {
	t.Parallel()
	choices := []releaseChoice{
		{tag: "v2.1.0", label: "v2.1.0"},
		{tag: "v2.0.0", label: "v2.0.0  ← running", running: true},
		{tag: "v2.0.0-rc.1", label: "v2.0.0-rc.1  (prerelease)", prerelease: true},
	}
	t.Run("plain rows are the picker labels", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		if err := writeReleaseList(&out, choices, false); err != nil {
			t.Fatalf("writeReleaseList: %v", err)
		}
		want := "v2.1.0\nv2.0.0  ← running\nv2.0.0-rc.1  (prerelease)\n"
		if out.String() != want {
			t.Fatalf("output = %q, want %q", out.String(), want)
		}
	})
	t.Run("json is the structured equivalent", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		if err := writeReleaseList(&out, choices, true); err != nil {
			t.Fatalf("writeReleaseList: %v", err)
		}
		var got []releaseListRow
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		want := []releaseListRow{
			{Tag: "v2.1.0"},
			{Tag: "v2.0.0", Running: true},
			{Tag: "v2.0.0-rc.1", Prerelease: true},
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("json rows mismatch (-want +got):\n%s", diff)
		}
	})
	t.Run("empty list is ErrNoRelease", func(t *testing.T) {
		t.Parallel()
		err := writeReleaseList(io.Discard, nil, false)
		if !errors.Is(err, selfupdate.ErrNoRelease) {
			t.Fatalf("writeReleaseList error = %v, want errors.Is(_, ErrNoRelease)", err)
		}
	})
}

// TestUpdateFlagConflicts proves the surface stays unambiguous: every
// conflicting combination is refused at the command layer, before any
// binary resolution or network call.
func TestUpdateFlagConflicts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "list with a version argument", args: []string{"--list", "v2.0.0"}, wantErr: "--list takes no version argument"},
		{name: "json without list", args: []string{"--json"}, wantErr: "--json requires --list"},
		{name: "select with a version argument", args: []string{"--select", "v2.0.0"}, wantErr: "not both"},
		{name: "list with select", args: []string{"--list", "--select"}, wantErr: "list"},
		{name: "list with check", args: []string{"--list", "--check"}, wantErr: "list"},
		{name: "list with prerelease", args: []string{"--list", "--prerelease"}, wantErr: "list"},
		{name: "list with no-restart", args: []string{"--list", "--no-restart"}, wantErr: "list"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cmd := newUpdateCmd()
			cmd.SetArgs(test.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Execute(%v) error = %v, want it to contain %q", test.args, err, test.wantErr)
			}
		})
	}
}

// headlessRefusalSource fails the test if the picker lists releases after
// the TTY gate should have refused.
type headlessRefusalSource struct{ t *testing.T }

func (s *headlessRefusalSource) ListReleases(context.Context, string, int) ([]ghx.ReleaseInfo, error) {
	s.t.Fatal("ListReleases called, want the TTY gate to refuse first")
	return nil, nil
}

func (s *headlessRefusalSource) DownloadReleaseAssets(context.Context, string, string, string, ...string) error {
	s.t.Fatal("DownloadReleaseAssets called, want the TTY gate to refuse first")
	return nil
}

// TestSelectReleaseTagRefusedHeadless proves the picker's TTY gate: under
// a test process stdin is never a terminal, so --select must refuse with
// the scriptable alternative BEFORE any network call — huh v2.0.3's
// accessible select auto-accepts the first option on EOF and panics on
// invalid-input-then-EOF, so reaching it headless would be unsafe.
func TestSelectReleaseTagRefusedHeadless(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetOut(io.Discard)
	_, err := selectReleaseTag(cmd, &headlessRefusalSource{t: t})
	if err == nil || !strings.Contains(err.Error(), "agent-brain update <version>") {
		t.Fatalf("selectReleaseTag error = %v, want the headless refusal naming the alternative", err)
	}
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
