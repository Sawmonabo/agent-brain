package service

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"strings"
	"testing"

	kardianos "github.com/kardianos/service"
)

var errFake = errors.New("fake")

// fakeRunner is an ordered, scripted Runner test double for loginctl
// invocations — same contract as ghxtest.Fake (internal/ghx/ghxtest):
// calls must arrive in exactly the scripted order, and Cleanup fails the
// test if fewer calls than expected were made. Kept unexported here,
// unlike ghxtest.Fake: nothing outside this package fakes Runner, since
// every other package only ever sees the Controller interface.
type fakeRunner struct {
	t     *testing.T
	calls []runnerCall
	next  int
}

type runnerCall struct {
	args   []string
	result Result
	err    error
}

func newFakeRunner(t *testing.T, calls ...runnerCall) *fakeRunner {
	t.Helper()
	fake := &fakeRunner{t: t, calls: calls}
	t.Cleanup(func() {
		if fake.next != len(fake.calls) {
			t.Errorf("fakeRunner: %d of %d expected calls were made", fake.next, len(fake.calls))
		}
	})
	return fake
}

func (f *fakeRunner) Run(_ context.Context, args ...string) (Result, error) {
	f.t.Helper()
	if f.next >= len(f.calls) {
		f.t.Fatalf("fakeRunner: unexpected call loginctl %v (all %d expectations already consumed)", args, len(f.calls))
		return Result{}, nil
	}
	expected := f.calls[f.next]
	f.next++
	if !slices.Equal(expected.args, args) {
		f.t.Fatalf("fakeRunner: call %d args = loginctl %v, want loginctl %v", f.next, args, expected.args)
	}
	return expected.result, expected.err
}

// fakeKardianosService is a minimal kardianos.Service test double. This
// file lives in package service (an internal test), so it wires directly
// into kardianosController's unexported svc field — no exported seam
// needs to leak into the public API for this. Only Install/Uninstall/
// Start/Stop/Status are exercised by Controller; the rest satisfy the
// interface with inert zero-value returns.
type fakeKardianosService struct {
	installErr   error
	uninstallErr error
	startErr     error
	stopErr      error
}

func (f *fakeKardianosService) Run() error     { return nil }
func (f *fakeKardianosService) Start() error   { return f.startErr }
func (f *fakeKardianosService) Stop() error    { return f.stopErr }
func (f *fakeKardianosService) Restart() error { return nil }
func (f *fakeKardianosService) Install() error { return f.installErr }
func (f *fakeKardianosService) Uninstall() error {
	return f.uninstallErr
}

func (f *fakeKardianosService) Logger(chan<- error) (kardianos.Logger, error) { return nil, nil }
func (f *fakeKardianosService) SystemLogger(chan<- error) (kardianos.Logger, error) {
	return nil, nil
}
func (f *fakeKardianosService) String() string   { return "agent-brain" }
func (f *fakeKardianosService) Platform() string { return "test" }
func (f *fakeKardianosService) Status() (kardianos.Status, error) {
	return kardianos.StatusUnknown, nil
}

// --- 3b: typed sentinels ---

// TestControllerInstallAlreadyInstalledYieldsSentinel proves the ONE
// place this package string-matches kardianos's per-OS-backend "already
// installed" wording ("Init already exists: ...", "Manifest already
// exists: ...", "service ... already exists" on Windows — no typed
// upstream sentinel exists for this, unlike ErrNotInstalled): callers
// branch on the sentinel via errors.Is, never on that text themselves.
func TestControllerInstallAlreadyInstalledYieldsSentinel(t *testing.T) {
	t.Parallel()
	svc := &fakeKardianosService{installErr: errors.New("Init already exists: /some/path.plist")}
	controller := &kardianosController{svc: svc, isWSL2: func() bool { return false }}
	if _, err := controller.Install(); !errors.Is(err, ErrAlreadyInstalled) {
		t.Fatalf("Install() error = %v, want errors.Is(_, ErrAlreadyInstalled)", err)
	}
}

// TestControllerInstallPassesThroughUnrelatedErrors proves mapErr never
// over-matches: a real install failure (e.g. permission denied) must
// reach the caller as-is, not get silently absorbed as "already
// installed".
func TestControllerInstallPassesThroughUnrelatedErrors(t *testing.T) {
	t.Parallel()
	want := errors.New("permission denied")
	svc := &fakeKardianosService{installErr: want}
	controller := &kardianosController{svc: svc, isWSL2: func() bool { return false }}
	_, err := controller.Install()
	if !errors.Is(err, want) {
		t.Fatalf("Install() error = %v, want it to wrap %v unchanged", err, want)
	}
	if errors.Is(err, ErrAlreadyInstalled) {
		t.Fatalf("Install() error = %v, want it NOT to match ErrAlreadyInstalled", err)
	}
}

// TestControllerUninstallNotInstalledYieldsSentinel proves Uninstall maps
// kardianos's OWN typed sentinel (kardianos.ErrNotInstalled) to this
// package's own ErrNotInstalled — callers never need to import kardianos
// directly to branch on it.
func TestControllerUninstallNotInstalledYieldsSentinel(t *testing.T) {
	t.Parallel()
	svc := &fakeKardianosService{uninstallErr: kardianos.ErrNotInstalled}
	controller := &kardianosController{svc: svc}
	if err := controller.Uninstall(); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("Uninstall() error = %v, want errors.Is(_, ErrNotInstalled)", err)
	}
}

// TestControllerStartStopMapNotInstalled proves Start/Stop get the same
// treatment as Uninstall — the mapping lives in one shared helper, not
// duplicated per method.
func TestControllerStartStopMapNotInstalled(t *testing.T) {
	t.Parallel()
	t.Run("start", func(t *testing.T) {
		t.Parallel()
		controller := &kardianosController{svc: &fakeKardianosService{startErr: kardianos.ErrNotInstalled}}
		if err := controller.Start(); !errors.Is(err, ErrNotInstalled) {
			t.Fatalf("Start() error = %v, want errors.Is(_, ErrNotInstalled)", err)
		}
	})
	t.Run("stop", func(t *testing.T) {
		t.Parallel()
		controller := &kardianosController{svc: &fakeKardianosService{stopErr: kardianos.ErrNotInstalled}}
		if err := controller.Stop(); !errors.Is(err, ErrNotInstalled) {
			t.Fatalf("Stop() error = %v, want errors.Is(_, ErrNotInstalled)", err)
		}
	})
}

// TestControllerInstallSucceedsCleanly proves the ordinary, no-error
// path still reports plain success.
func TestControllerInstallSucceedsCleanly(t *testing.T) {
	t.Parallel()
	controller := &kardianosController{svc: &fakeKardianosService{}, isWSL2: func() bool { return false }}
	if _, err := controller.Install(); err != nil {
		t.Fatalf("Install() error = %v, want nil", err)
	}
}

func TestDetectWSL2(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		content string
		readErr bool
		want    bool
	}{
		{"wsl2 kernel", "Linux version 5.15.167.4-microsoft-standard-WSL2", false, true},
		{"wsl1 kernel", "Linux version 4.4.0-19041-Microsoft", false, true},
		{"native linux", "Linux version 6.8.0-45-generic (buildd@lcy02)", false, false},
		{"unreadable", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			read := func(string) ([]byte, error) {
				if tc.readErr {
					return nil, errFake
				}
				return []byte(tc.content), nil
			}
			if got := detectWSL2(read); got != tc.want {
				t.Fatalf("detectWSL2 = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewControllerConstructsWithoutTouchingSystem(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("phase 2 targets darwin/linux")
	}
	controller, err := NewController("/usr/local/bin/agent-brain")
	if err != nil {
		t.Fatal(err)
	}
	if controller == nil {
		t.Fatal("nil controller")
	}
	// Construction only — Install/Start would touch the live system and
	// are exercised manually (exit criteria), never in tests.
}

func TestNewControllerRejectsRelativePath(t *testing.T) {
	t.Parallel()
	if _, err := NewController("agent-brain"); err == nil {
		t.Fatal("relative binary path accepted")
	}
}

// --- 3c: WSL2 systemd lingering ---

// TestControllerInstallOnWSL2EnablesLinger proves a successful install on
// WSL2 best-effort enables systemd user lingering so the resident unit
// (kardianos's UserService option) survives past this login session —
// WSL2 has no display manager or session keyring to do that itself.
func TestControllerInstallOnWSL2EnablesLinger(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner(t, runnerCall{
		args:   []string{"enable-linger", "testuser"},
		result: Result{ExitCode: 0},
	})
	controller := &kardianosController{
		svc:      &fakeKardianosService{},
		isWSL2:   func() bool { return true },
		username: func() (string, error) { return "testuser", nil },
		runner:   runner,
	}
	warning, err := controller.Install()
	if err != nil {
		t.Fatalf("Install() error = %v, want nil", err)
	}
	if warning != "" {
		t.Fatalf("Install() warning = %q, want empty (linger succeeded)", warning)
	}
}

// TestControllerInstallOnWSL2LingerFailureIsWarningNotError proves a
// failed enable-linger call degrades to a warning string that names the
// manual fix — it must never fail Install: the service unit itself was
// installed successfully.
func TestControllerInstallOnWSL2LingerFailureIsWarningNotError(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner(t, runnerCall{
		args:   []string{"enable-linger", "testuser"},
		result: Result{ExitCode: 1, Stderr: "Failed to enable linger: Access denied"},
	})
	controller := &kardianosController{
		svc:      &fakeKardianosService{},
		isWSL2:   func() bool { return true },
		username: func() (string, error) { return "testuser", nil },
		runner:   runner,
	}
	warning, err := controller.Install()
	if err != nil {
		t.Fatalf("Install() error = %v, want nil (linger failure must not fail Install)", err)
	}
	if !strings.Contains(warning, "loginctl enable-linger testuser") {
		t.Fatalf("Install() warning = %q, want it to name the manual fix command", warning)
	}
}

// TestControllerInstallOnNonWSL2NeverInvokesLoginctl proves the non-WSL2
// path never touches the Runner at all — newFakeRunner with zero scripted
// calls fails the test (via its Cleanup) if Install invokes it anyway.
func TestControllerInstallOnNonWSL2NeverInvokesLoginctl(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner(t)
	controller := &kardianosController{
		svc:    &fakeKardianosService{},
		isWSL2: func() bool { return false },
		runner: runner,
	}
	warning, err := controller.Install()
	if err != nil {
		t.Fatalf("Install() error = %v, want nil", err)
	}
	if warning != "" {
		t.Fatalf("Install() warning = %q, want empty on non-WSL2", warning)
	}
}

// TestControllerInstallAlreadyInstalledStillAttemptsLinger proves the
// idempotent "already installed" path (Task 3b) still best-effort enables
// lingering: a user who installed before this feature existed and reruns
// `service install` should retroactively get a lingering unit.
func TestControllerInstallAlreadyInstalledStillAttemptsLinger(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner(t, runnerCall{
		args:   []string{"enable-linger", "testuser"},
		result: Result{ExitCode: 0},
	})
	svc := &fakeKardianosService{installErr: errors.New("Init already exists: /some/path.plist")}
	controller := &kardianosController{
		svc:      svc,
		isWSL2:   func() bool { return true },
		username: func() (string, error) { return "testuser", nil },
		runner:   runner,
	}
	warning, err := controller.Install()
	if !errors.Is(err, ErrAlreadyInstalled) {
		t.Fatalf("Install() error = %v, want errors.Is(_, ErrAlreadyInstalled)", err)
	}
	if warning != "" {
		t.Fatalf("Install() warning = %q, want empty (linger succeeded)", warning)
	}
}

// TestControllerInstallHardFailureSkipsLinger proves a genuine install
// failure (not the idempotent already-installed sentinel) skips linger
// entirely — there is no unit to keep alive, so attempting it would be
// noise at best. The zero-call fakeRunner enforces this.
func TestControllerInstallHardFailureSkipsLinger(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner(t)
	svc := &fakeKardianosService{installErr: errors.New("permission denied")}
	controller := &kardianosController{
		svc:    svc,
		isWSL2: func() bool { return true },
		runner: runner,
	}
	warning, err := controller.Install()
	if err == nil || errors.Is(err, ErrAlreadyInstalled) {
		t.Fatalf("Install() error = %v, want the unrelated hard failure", err)
	}
	if warning != "" {
		t.Fatalf("Install() warning = %q, want empty — a hard install failure must skip linger", warning)
	}
}

// TestControllerInstallUsernameLookupFailureIsWarning proves a failure to
// resolve the current username also degrades to a warning (there is no
// user to pass loginctl), never an Install error.
func TestControllerInstallUsernameLookupFailureIsWarning(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner(t)
	controller := &kardianosController{
		svc:      &fakeKardianosService{},
		isWSL2:   func() bool { return true },
		username: func() (string, error) { return "", errFake },
		runner:   runner,
	}
	warning, err := controller.Install()
	if err != nil {
		t.Fatalf("Install() error = %v, want nil", err)
	}
	if !strings.Contains(warning, "username") {
		t.Fatalf("Install() warning = %q, want it to mention the username lookup failure", warning)
	}
}

// TestLingerStatusReportsEnabled proves `service status`'s WSL2 advisory
// line reflects a "Linger=yes" loginctl query.
func TestLingerStatusReportsEnabled(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner(t, runnerCall{
		args:   []string{"show-user", "testuser", "--property=Linger"},
		result: Result{Stdout: "Linger=yes\n"},
	})
	controller := &kardianosController{
		isWSL2:   func() bool { return true },
		username: func() (string, error) { return "testuser", nil },
		runner:   runner,
	}
	if got := controller.LingerStatus(); !strings.Contains(got, "enabled") {
		t.Fatalf("LingerStatus() = %q, want it to report enabled", got)
	}
}

// TestLingerStatusReportsDisabled proves the "Linger=no" case names the
// manual fix command, same as the install-time warning does.
func TestLingerStatusReportsDisabled(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner(t, runnerCall{
		args:   []string{"show-user", "testuser", "--property=Linger"},
		result: Result{Stdout: "Linger=no\n"},
	})
	controller := &kardianosController{
		isWSL2:   func() bool { return true },
		username: func() (string, error) { return "testuser", nil },
		runner:   runner,
	}
	got := controller.LingerStatus()
	if !strings.Contains(got, "disabled") {
		t.Fatalf("LingerStatus() = %q, want it to report disabled", got)
	}
	if !strings.Contains(got, "loginctl enable-linger testuser") {
		t.Fatalf("LingerStatus() = %q, want it to name the manual fix command", got)
	}
}

// TestLingerStatusEmptyOnNonWSL2 proves the advisory is silent (empty
// string, nothing printed) outside WSL2 — native macOS/Linux session
// management already keeps the service running, so there is nothing to
// report and the Runner must never be touched.
func TestLingerStatusEmptyOnNonWSL2(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner(t)
	controller := &kardianosController{
		isWSL2: func() bool { return false },
		runner: runner,
	}
	if got := controller.LingerStatus(); got != "" {
		t.Fatalf("LingerStatus() = %q, want empty on non-WSL2", got)
	}
}

// TestLingerStatusEmptyOnQueryFailure proves a failed loginctl query
// degrades to silence, not an error — this is an advisory line, not
// load-bearing.
func TestLingerStatusEmptyOnQueryFailure(t *testing.T) {
	t.Parallel()
	runner := newFakeRunner(t, runnerCall{
		args: []string{"show-user", "testuser", "--property=Linger"},
		err:  errFake,
	})
	controller := &kardianosController{
		isWSL2:   func() bool { return true },
		username: func() (string, error) { return "testuser", nil },
		runner:   runner,
	}
	if got := controller.LingerStatus(); got != "" {
		t.Fatalf("LingerStatus() = %q, want empty when the query itself fails (advisory only)", got)
	}
}
