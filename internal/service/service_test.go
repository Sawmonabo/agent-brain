package service

import (
	"errors"
	"runtime"
	"testing"

	kardianos "github.com/kardianos/service"
)

var errFake = errors.New("fake")

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
	controller := &kardianosController{svc: svc}
	if err := controller.Install(); !errors.Is(err, ErrAlreadyInstalled) {
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
	controller := &kardianosController{svc: svc}
	err := controller.Install()
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
	controller := &kardianosController{svc: &fakeKardianosService{}}
	if err := controller.Install(); err != nil {
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
