package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/service"
)

// defaultLogLines bounds `service logs` output absent -n.
const defaultLogLines = 100

// invokedBinary returns this process's own executable path WITHOUT resolving
// symlinks — os.Executable() as invoked. This is the UPGRADE-STABLE spelling:
// on a Homebrew install it is /opt/homebrew/bin/agent-brain (the symlink-farm
// entry), which survives every `brew upgrade`, whereas the symlink's target
// (…/Cellar/agent-brain/<version>/bin/agent-brain) is version-scoped and
// vanishes the moment the version changes. Every site that RECORDS the binary
// path into durable state or re-execs the user's invocation uses this: the
// service definition's Executable (a Cellar path there dies on upgrade, leaving
// the launchd/systemd unit pointing at nothing), the git filter wiring init
// installs, the hub's post-update re-exec, and the in-TUI service controller.
// It is also the spelling every CHECKER already reads — daemon.go and
// buildDoctorDeps both take raw os.Executable() — so recording it here is what
// makes writer and checker converge on one spelling (doctor.checkFilters still
// equates spellings by file identity, but converging first avoids needless
// churn and dead-Cellar wiring).
func invokedBinary() (string, error) {
	return os.Executable()
}

// resolvedBinary returns this process's own executable path WITH symlinks
// resolved (os.Executable() + filepath.EvalSymlinks) — the real on-disk file,
// never a symlink or a go-run temp path. Exactly one caller needs this
// spelling: `update`'s selfupdate.Options.TargetPath (update.go). selfupdate
// detects a Homebrew install by a /Cellar/ segment in TargetPath and refuses
// there (brew owns that binary), and its atomic swap must replace the real
// file, never a symlink pointing at it — both require the resolved spelling.
// Options.TargetPath's own doc says "resolved (symlink-free)"; this keeps that
// true.
func resolvedBinary() (string, error) {
	executable, err := invokedBinary()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(executable)
}

func newServiceCmd() *cobra.Command {
	serviceCmd := &cobra.Command{
		Use:   "service",
		Short: "Install or control the login-started daemon service",
	}
	controllerFor := func() (service.Controller, error) {
		binaryPath, err := invokedBinary()
		if err != nil {
			return nil, err
		}
		return service.NewController(binaryPath)
	}

	serviceCmd.AddCommand(
		&cobra.Command{
			Use:   "install",
			Short: "Install the user service (launchd / systemd --user)",
			RunE: func(cmd *cobra.Command, _ []string) error {
				controller, err := controllerFor()
				if err != nil {
					return err
				}
				return runServiceInstall(cmd.OutOrStdout(), controller)
			},
		},
		&cobra.Command{
			Use:   "uninstall",
			Short: "Remove the user service",
			RunE: func(cmd *cobra.Command, _ []string) error {
				controller, err := controllerFor()
				if err != nil {
					return err
				}
				return runServiceUninstall(cmd.OutOrStdout(), controller)
			},
		},
		&cobra.Command{
			Use:   "start",
			Short: "Start the service",
			RunE: func(cmd *cobra.Command, _ []string) error {
				controller, err := controllerFor()
				if err != nil {
					return err
				}
				return runServiceStart(cmd.OutOrStdout(), controller)
			},
		},
		&cobra.Command{
			Use:   "stop",
			Short: "Stop the service",
			RunE: func(cmd *cobra.Command, _ []string) error {
				controller, err := controllerFor()
				if err != nil {
					return err
				}
				return runServiceStop(cmd.OutOrStdout(), controller)
			},
		},
		&cobra.Command{
			Use:   "status",
			Short: "Report service state",
			RunE: func(cmd *cobra.Command, _ []string) error {
				controller, err := controllerFor()
				if err != nil {
					return err
				}
				return runServiceStatus(cmd.OutOrStdout(), controller)
			},
		},
		newServiceLogsCmd(),
	)
	return serviceCmd
}

// installServiceAndReport installs the service and prints the outcome —
// the idempotency branch (a second install against an already-installed
// unit, service.ErrAlreadyInstalled matched with errors.Is, never a
// string match), the ok/nothing-to-do message, and any non-fatal
// WSL2 linger warning all live here ONCE: runServiceInstall
// (the standalone `service install` command) and stepService (init's own
// service step, internal/cli/initsteps.go) both delegate to this rather
// than hand-rolling the same three branches. A genuine
// install failure (anything but ErrAlreadyInstalled) is returned
// unwrapped and prints nothing — callers add their own context prefix.
func installServiceAndReport(controller service.Controller, out io.Writer) error {
	warning, err := controller.Install()
	if err != nil && !errors.Is(err, service.ErrAlreadyInstalled) {
		return err
	}
	message := "service install: ok"
	if errors.Is(err, service.ErrAlreadyInstalled) {
		message = "service install: already installed — nothing to do"
	}
	if _, printErr := fmt.Fprintln(out, message); printErr != nil {
		return printErr
	}
	if warning != "" {
		if _, printErr := fmt.Fprintln(out, warning); printErr != nil {
			return printErr
		}
	}
	return err
}

// runServiceInstall installs the service and reports the outcome,
// wrapping a genuine (non-idempotent) failure with command-specific
// context; see installServiceAndReport for the shared idempotency/
// warning logic.
func runServiceInstall(out io.Writer, controller service.Controller) error {
	err := installServiceAndReport(controller, out)
	if err != nil && !errors.Is(err, service.ErrAlreadyInstalled) {
		return fmt.Errorf("service install: %w", err)
	}
	return nil
}

// startServiceAndReport starts the service and prints the outcome — the
// idempotency branch (starting an already-running service,
// service.ErrAlreadyRunning matched with errors.Is, never a string
// match) and the ok/nothing-to-do message live here ONCE, the same
// shape installServiceAndReport gives Install: runServiceStart (the
// standalone `service start` command) and stepService (init's own
// service step, internal/cli/initsteps.go) both delegate to this.
// Before this branch existed, re-running init against a healthy running
// daemon died on launchd's EIO ("Load failed: 5: Input/output error").
// A genuine start failure is returned unwrapped and prints nothing —
// callers add their own context prefix.
func startServiceAndReport(controller service.Controller, out io.Writer) error {
	err := controller.Start()
	if err != nil && !errors.Is(err, service.ErrAlreadyRunning) {
		return err
	}
	message := "service start: ok"
	if errors.Is(err, service.ErrAlreadyRunning) {
		message = "service start: already running — nothing to do"
	}
	if _, printErr := fmt.Fprintln(out, message); printErr != nil {
		return printErr
	}
	return err
}

// runServiceStart starts the service and reports the outcome, wrapping
// a genuine (non-idempotent) failure with command-specific context; see
// startServiceAndReport for the shared idempotency logic.
func runServiceStart(out io.Writer, controller service.Controller) error {
	err := startServiceAndReport(controller, out)
	if err != nil && !errors.Is(err, service.ErrAlreadyRunning) {
		return fmt.Errorf("service start: %w", err)
	}
	return nil
}

// stopServiceAndReport mirrors startServiceAndReport for the symmetric
// "already stopped" case (service.ErrNotRunning).
func stopServiceAndReport(controller service.Controller, out io.Writer) error {
	err := controller.Stop()
	if err != nil && !errors.Is(err, service.ErrNotRunning) {
		return err
	}
	message := "service stop: ok"
	if errors.Is(err, service.ErrNotRunning) {
		message = "service stop: not running — nothing to do"
	}
	if _, printErr := fmt.Fprintln(out, message); printErr != nil {
		return printErr
	}
	return err
}

// runServiceStop stops the service and reports the outcome, wrapping a
// genuine (non-idempotent) failure with command-specific context; see
// stopServiceAndReport for the shared idempotency logic.
func runServiceStop(out io.Writer, controller service.Controller) error {
	err := stopServiceAndReport(controller, out)
	if err != nil && !errors.Is(err, service.ErrNotRunning) {
		return fmt.Errorf("service stop: %w", err)
	}
	return nil
}

// printServiceStatus writes the plain status line plus, on WSL2, the
// systemd user-lingering advisory line — runServiceStatus (the
// standalone `service status` command) and stepService (init's own
// service step) both delegate to this rather than hand-rolling the same
// linger-line branch. LingerStatus returns "" when there
// is nothing to report (non-WSL2, or the query itself failed), so the
// advisory line is silently omitted rather than printed empty.
func printServiceStatus(out io.Writer, controller service.Controller) error {
	status, err := controller.Status()
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "service: %s\n", status); err != nil {
		return err
	}
	linger := controller.LingerStatus()
	if linger == "" {
		return nil
	}
	_, err = fmt.Fprintln(out, linger)
	return err
}

// runServiceStatus reports service state plus, on WSL2, the systemd
// user-lingering advisory line; see printServiceStatus for the shared
// logic.
func runServiceStatus(out io.Writer, controller service.Controller) error {
	return printServiceStatus(out, controller)
}

// runServiceUninstall mirrors runServiceInstall's idempotent treatment
// for the symmetric "already gone" case (service.ErrNotInstalled).
func runServiceUninstall(out io.Writer, controller service.Controller) error {
	err := controller.Uninstall()
	if err != nil && !errors.Is(err, service.ErrNotInstalled) {
		return fmt.Errorf("service uninstall: %w", err)
	}
	message := "service uninstall: ok"
	if errors.Is(err, service.ErrNotInstalled) {
		message = "service uninstall: not installed — nothing to do"
	}
	_, printErr := fmt.Fprintln(out, message)
	return printErr
}

// newServiceLogsCmd is a pure file read over paths.DaemonLogFile() — no
// controller, no socket. That is deliberate: logs matter most exactly when
// the daemon is down, so this command must work then too. No follow mode
// in v1 (spec §7 surface, never built).
func newServiceLogsCmd() *cobra.Command {
	var lines int
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Print the daemon's log file (works even when the daemon is down)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.DefaultPaths()
			if err != nil {
				return err
			}
			return printDaemonLogs(cmd, paths, lines)
		},
	}
	cmd.Flags().IntVarP(&lines, "lines", "n", defaultLogLines, "number of trailing lines to print")
	return cmd
}

func printDaemonLogs(cmd *cobra.Command, paths config.Paths, lines int) error {
	logPath := paths.DaemonLogFile()
	data, err := os.ReadFile(logPath) //nolint:gosec // G304: logPath is the program-derived daemon-log location (config.Paths), not untrusted input
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			_, printErr := fmt.Fprintf(cmd.OutOrStdout(), "no daemon log yet at %s\n", logPath)
			return printErr
		}
		return err
	}

	report := &reportWriter{w: cmd.OutOrStdout()}
	for _, line := range lastLines(data, lines) {
		report.printf("%s\n", line)
	}
	report.printf("--- %s", logPath)
	if _, err := os.Stat(logPath + ".1"); err == nil {
		report.printf(" (older entries rotated to %s.1)", logPath)
	}
	report.println()
	return report.err
}

// lastLines returns the trailing n lines of data (n <= 0 means all of it).
func lastLines(data []byte, n int) []string {
	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}
