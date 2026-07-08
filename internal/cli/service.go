package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/service"
)

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
			RunE: func(cmd *cobra.Command, args []string) error {
				if service.IsWSL2() {
					return fmt.Errorf("service install is not supported on WSL2 — WSL lacks a reliable login service manager; on-demand mode arrives in Phase 4. Run `agent-brain daemon run` in a terminal for now")
				}
				return run("install", service.Controller.Install)(cmd, args)
			},
		},
		&cobra.Command{Use: "uninstall", Short: "Remove the user service", RunE: run("uninstall", service.Controller.Uninstall)},
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
	)
	return serviceCmd
}
