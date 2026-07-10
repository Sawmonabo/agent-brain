// Package service manages the login-started daemon via
// kardianos/service (ADR 04), wrapped in a narrow interface so nothing
// else in the codebase touches the live service manager — and tests
// never do.
package service

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

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
	Install() error
	Uninstall() error
	Start() error
	Stop() error
	Status() (Status, error)
}

// noopProgram satisfies kardianos.Interface; install/uninstall/start/
// stop never invoke it — the daemon process is self-sufficient
// (`agent-brain daemon run`), the service manager just launches it.
type noopProgram struct{}

func (noopProgram) Start(kardianos.Service) error { return nil }
func (noopProgram) Stop(kardianos.Service) error  { return nil }

type kardianosController struct {
	svc kardianos.Service
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
	return &kardianosController{svc: svc}, nil
}

func (c *kardianosController) Install() error   { return mapErr(c.svc.Install()) }
func (c *kardianosController) Uninstall() error { return mapErr(c.svc.Uninstall()) }
func (c *kardianosController) Start() error     { return mapErr(c.svc.Start()) }
func (c *kardianosController) Stop() error      { return mapErr(c.svc.Stop()) }

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
