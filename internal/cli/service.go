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

// resolveBinary pins the service definition to the real installed
// binary, not a symlink or a go-run temp path.
func resolveBinary() (string, error) {
	executable, err := os.Executable()
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
		binaryPath, err := resolveBinary()
		if err != nil {
			return nil, err
		}
		return service.NewController(binaryPath)
	}
	run := func(action string, act func(service.Controller) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, _ []string) error {
			controller, err := controllerFor()
			if err != nil {
				return err
			}
			if err := act(controller); err != nil {
				return fmt.Errorf("service %s: %w", action, err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "service %s: ok\n", action)
			return err
		}
	}

	serviceCmd.AddCommand(
		&cobra.Command{
			Use:   "install",
			Short: "Install the user service (launchd / systemd --user)",
			RunE: func(cmd *cobra.Command, _ []string) error {
				if service.IsWSL2() {
					return fmt.Errorf("service install is not supported on WSL2 — WSL lacks a reliable login service manager; on-demand mode arrives in Phase 4. Run `agent-brain daemon run` in a terminal for now")
				}
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
		&cobra.Command{Use: "start", Short: "Start the service", RunE: run("start", service.Controller.Start)},
		&cobra.Command{Use: "stop", Short: "Stop the service", RunE: run("stop", service.Controller.Stop)},
		&cobra.Command{
			Use:   "status",
			Short: "Report service state",
			RunE: func(cmd *cobra.Command, _ []string) error {
				controller, err := controllerFor()
				if err != nil {
					return err
				}
				status, err := controller.Status()
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "service: %s\n", status)
				return err
			},
		},
		newServiceLogsCmd(),
	)
	return serviceCmd
}

// runServiceInstall installs the service and reports the outcome. A
// second, idempotent install against an already-installed unit
// (service.ErrAlreadyInstalled, matched with errors.Is — never a string
// match, Task 3b) prints a nothing-to-do line and exits 0 rather than
// failing; any other error still fails the command.
func runServiceInstall(out io.Writer, controller service.Controller) error {
	err := controller.Install()
	if err != nil && !errors.Is(err, service.ErrAlreadyInstalled) {
		return fmt.Errorf("service install: %w", err)
	}
	message := "service install: ok"
	if errors.Is(err, service.ErrAlreadyInstalled) {
		message = "service install: already installed — nothing to do"
	}
	_, printErr := fmt.Fprintln(out, message)
	return printErr
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
// in v1 (spec §7 surface Phase 2 never shipped).
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
