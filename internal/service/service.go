// Package service manages the login-started daemon via
// kardianos/service (ADR 04), wrapped in a narrow interface so nothing
// else in the codebase touches the live service manager — and tests
// never do.
package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	kardianos "github.com/kardianos/service"
)

// ErrAlreadyInstalled and ErrNotInstalled are this package's own
// sentinels for the two lifecycle states kardianos/service does not
// expose uniformly across its OS backends: "already installed" is a
// bare formatted string that differs per backend (no typed sentinel
// upstream for it, unlike ErrNotInstalled). mapErr is the ONE place
// either shape is inspected; every caller branches with errors.Is
// against these instead of kardianos's own types or text.
var (
	ErrAlreadyInstalled = errors.New("service already installed")
	ErrNotInstalled     = errors.New("service not installed")
)

// Status is the coarse service state the CLI reports.
type Status int

// Service states surfaced to the CLI. StatusUnknown covers an
// undeterminable state; the rest map from kardianos's own enum.
const (
	StatusUnknown Status = iota
	StatusRunning
	StatusStopped
	StatusNotInstalled
)

func (s Status) String() string {
	switch s {
	case StatusRunning:
		return "running"
	case StatusStopped:
		return "stopped"
	case StatusNotInstalled:
		return "not installed"
	default:
		return "unknown"
	}
}

// Controller is the fake-able surface over the service manager.
type Controller interface {
	// Install installs the service. The returned string is a non-fatal
	// warning (Task 3c: a WSL2 systemd-lingering failure) — never an
	// error condition itself; err is nil (or ErrAlreadyInstalled) exactly
	// when the underlying install succeeded or was already done.
	Install() (warning string, err error)
	Uninstall() error
	Start() error
	Stop() error
	Status() (Status, error)
	// LingerStatus reports WSL2 systemd user-lingering state as an
	// advisory line for `service status` — "" whenever there is nothing
	// to report (non-WSL2, or the query itself failed).
	LingerStatus() string
}

// Result carries a finished loginctl invocation (same shape idiom as
// ghx.Result / gitx.Result).
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner executes loginctl. execRunner is process-global reality; tests
// script a fake Runner instead of touching a real login manager (mirrors
// internal/ghx's Runner pattern).
type Runner interface {
	Run(ctx context.Context, args ...string) (Result, error)
}

// lingerTimeout bounds the loginctl subprocess — a local D-Bus/systemd
// call, never a network one, but a hung system bus must not block
// Install or Status indefinitely (same rationale as migrate's
// config.MigrateSettings.PreflightTimeout, Task 3a).
const lingerTimeout = 5 * time.Second

// execRunner shells the real loginctl binary. args come only from this
// package's own call sites, never unsanitized user input.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, args ...string) (Result, error) {
	//nolint:gosec // G204: "loginctl" is a constant, args are internal to this package
	cmd := exec.CommandContext(ctx, "loginctl", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	// A canceled/expired context kills loginctl; report that rather than
	// mapping a signal-kill (ExitCode -1) to data a caller could misread
	// as real exit status. Checked before the exit code so a genuine
	// non-zero exit under a live context stays data.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, fmt.Errorf("loginctl %v: %w", args, ctxErr)
	}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		return result, nil
	case errors.As(runErr, &exitErr):
		if !exitErr.Exited() {
			return result, fmt.Errorf("loginctl %v terminated by signal: %w", args, runErr)
		}
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	default:
		return result, fmt.Errorf("spawn loginctl %v: %w", args, runErr)
	}
}

// currentUsername resolves the invoking user's login name for `loginctl
// enable-linger <user>` / `show-user <user>`: user.Current() first (works
// under most invocations without relying on env), falling back to $USER
// so a minimal environment missing NSS user lookups still works.
func currentUsername() (string, error) {
	if currentUser, err := user.Current(); err == nil && currentUser.Username != "" {
		return currentUser.Username, nil
	}
	if name := os.Getenv("USER"); name != "" {
		return name, nil
	}
	return "", errors.New("service: could not determine current username (user.Current failed and $USER is unset)")
}

// noopProgram satisfies kardianos.Interface; install/uninstall/start/
// stop never invoke it — the daemon process is self-sufficient
// (`agent-brain daemon run`), the service manager just launches it.
type noopProgram struct{}

func (noopProgram) Start(kardianos.Service) error { return nil }
func (noopProgram) Stop(kardianos.Service) error  { return nil }

type kardianosController struct {
	svc kardianos.Service

	// isWSL2, username, and runner are Task 3c's WSL2-lingering seams —
	// production wiring is IsWSL2/currentUsername/execRunner{}; tests
	// substitute all three so no test ever shells a real loginctl.
	isWSL2   func() bool
	username func() (string, error)
	runner   Runner
}

// NewController builds the launchd/systemd-user controller for the
// given absolute binary path (ADR 04: UserService; launchd RunAtLoad
// must be set explicitly — kardianos defaults it false).
func NewController(binaryPath string) (Controller, error) {
	if !filepath.IsAbs(binaryPath) {
		return nil, fmt.Errorf("service: binary path %q must be absolute", binaryPath)
	}
	cfg := &kardianos.Config{
		Name:        "agent-brain",
		DisplayName: "agent-brain memory sync",
		Description: "Syncs AI coding agents' per-project memory across machines.",
		Executable:  binaryPath,
		Arguments:   []string{"daemon", "run"},
		Option: kardianos.KeyValue{
			"UserService": true,
			"RunAtLoad":   true,
			"KeepAlive":   true,
		},
	}
	svc, err := kardianos.New(noopProgram{}, cfg)
	if err != nil {
		return nil, fmt.Errorf("service: %w", err)
	}
	return &kardianosController{
		svc:      svc,
		isWSL2:   IsWSL2,
		username: currentUsername,
		runner:   execRunner{},
	}, nil
}

// Install installs the service and, on WSL2, best-effort enables systemd
// user lingering (Task 3c) so the resident unit survives past this login
// session — WSL2 has no display manager or session keyring to do that
// itself, unlike launchd/native-systemd session management. Linger is
// attempted whenever the install itself succeeded OR was already done
// (ErrAlreadyInstalled: a user who installed before this feature existed
// should get lingering retroactively on a rerun); a genuine install
// failure skips it — there is no unit to keep alive. A linger failure
// is returned as a warning, never folded into err: the service unit
// itself is in place either way.
func (c *kardianosController) Install() (string, error) {
	err := mapErr(c.svc.Install())
	if err != nil && !errors.Is(err, ErrAlreadyInstalled) {
		return "", err
	}
	return c.ensureLinger(context.Background()), err
}

func (c *kardianosController) Uninstall() error { return mapErr(c.svc.Uninstall()) }
func (c *kardianosController) Start() error     { return mapErr(c.svc.Start()) }
func (c *kardianosController) Stop() error      { return mapErr(c.svc.Stop()) }

// ensureLinger is a no-op outside WSL2. On WSL2, it best-effort runs
// `loginctl enable-linger <user>`; any failure (no loginctl, permission
// denied, no polkit rule, unresolvable username) becomes a warning string
// naming the manual command rather than an error — Install must still
// succeed so the unit itself is in place.
func (c *kardianosController) ensureLinger(ctx context.Context) string {
	if !c.isWSL2() {
		return ""
	}
	username, err := c.username()
	if err != nil {
		return fmt.Sprintf("WARNING: could not determine the current username to enable systemd lingering (%v) — run `loginctl enable-linger <user>` by hand so the service survives logout", err)
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, lingerTimeout)
	defer cancel()
	result, runErr := c.runner.Run(timeoutCtx, "enable-linger", username)
	if runErr != nil {
		return fmt.Sprintf("WARNING: failed to enable systemd lingering for %s (%v) — the service will stop when this login session ends; run `loginctl enable-linger %s` by hand", username, runErr, username)
	}
	if result.ExitCode != 0 {
		return fmt.Sprintf("WARNING: failed to enable systemd lingering for %s (loginctl exited %d: %s) — the service will stop when this login session ends; run `loginctl enable-linger %s` by hand", username, result.ExitCode, strings.TrimSpace(result.Stderr), username)
	}
	return ""
}

// LingerStatus reports WSL2 systemd user-lingering state as an advisory
// line for `service status`. It is silent ("") outside WSL2 and on any
// query failure — this is advisory only, never load-bearing, so a
// transient loginctl failure must not surface as an error from Status.
func (c *kardianosController) LingerStatus() string {
	if !c.isWSL2() {
		return ""
	}
	username, err := c.username()
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), lingerTimeout)
	defer cancel()
	result, err := c.runner.Run(ctx, "show-user", username, "--property=Linger")
	if err != nil || result.ExitCode != 0 {
		return ""
	}
	switch strings.TrimSpace(result.Stdout) {
	case "Linger=yes":
		return "linger: enabled (service will survive logout)"
	case "Linger=no":
		return fmt.Sprintf("linger: disabled — the service stops when this login session ends; run `loginctl enable-linger %s` to fix", username)
	default:
		return ""
	}
}

// mapErr translates kardianos/service's own error shapes into this
// package's sentinels. "not installed" already has a typed upstream
// sentinel (kardianos.ErrNotInstalled); "already installed" does not —
// every OS backend formats it differently ("Init already exists: ...",
// "Manifest already exists: ..." on solaris, "service ... already
// exists" on Windows) so a substring match on the one word they all
// share is the only seam available without kardianos itself changing.
// Every other error passes through unchanged.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, kardianos.ErrNotInstalled) {
		return fmt.Errorf("%w: %w", ErrNotInstalled, err)
	}
	if strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("%w: %w", ErrAlreadyInstalled, err)
	}
	return err
}

func (c *kardianosController) Status() (Status, error) {
	status, err := c.svc.Status()
	if errors.Is(err, kardianos.ErrNotInstalled) {
		return StatusNotInstalled, nil
	}
	if err != nil {
		return StatusUnknown, err
	}
	switch status {
	case kardianos.StatusRunning:
		return StatusRunning, nil
	case kardianos.StatusStopped:
		return StatusStopped, nil
	default:
		return StatusUnknown, nil
	}
}
