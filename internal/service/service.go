// Package service manages the login-started daemon via
// kardianos/service (ADR 04), wrapped in a narrow interface so nothing
// else in the codebase touches the live service manager — and tests
// never do.
package service

import (
	"errors"
	"fmt"
	"path/filepath"

	kardianos "github.com/kardianos/service"
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

func (c *kardianosController) Install() error   { return c.svc.Install() }
func (c *kardianosController) Uninstall() error { return c.svc.Uninstall() }
func (c *kardianosController) Start() error     { return c.svc.Start() }
func (c *kardianosController) Stop() error      { return c.svc.Stop() }

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
