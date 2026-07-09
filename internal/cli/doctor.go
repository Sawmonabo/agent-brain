package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
	"github.com/Sawmonabo/agent-brain/internal/ghx"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func newDoctorCmd() *cobra.Command {
	var fix, jsonOut, offline bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check (and optionally repair) this machine's agent-brain wiring",
		RunE: func(cmd *cobra.Command, _ []string) error {
			deps, err := buildDoctorDeps(offline)
			if err != nil {
				return err
			}
			var report doctor.Report
			if fix {
				report, err = doctor.Fix(cmd.Context(), deps)
				if err != nil {
					return err
				}
			} else {
				report = doctor.Run(cmd.Context(), deps)
			}

			if jsonOut {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				if err := encoder.Encode(report); err != nil {
					return err
				}
			} else if err := printDoctorReport(cmd.OutOrStdout(), report); err != nil {
				return err
			}

			if report.Failed() {
				// A plain returned error is the established exit-code-1 signal
				// (mirrors service.go's WSL2-unsupported message): fang prints it
				// to stderr, so it never pollutes a --json stdout consumer.
				return errors.New("doctor: one or more checks failed")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "apply the idempotent wiring repairs (filters, attributes, credential helper), then re-check")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the report as JSON")
	cmd.Flags().BoolVar(&offline, "offline", false, "skip the network reachability check")
	return cmd
}

// buildDoctorDeps assembles doctor.Deps from the ambient machine — the same
// Paths → Settings → home → registry composition daemon.go's `daemon run`
// uses (registry.go: "daemon, doctor, init, and track must all see the
// identical registry"), except a bad config.toml is captured as SettingsErr
// (its own check, checks.go's checkSettings) rather than aborting before any
// check runs — doctor's whole point is reporting on a half-broken machine,
// not refusing to look at one. gh/daemon/local-registry are each best-effort:
// their own checks (or applicability guards) handle a nil/absent dependency.
func buildDoctorDeps(offline bool) (doctor.Deps, error) {
	paths, err := config.DefaultPaths()
	if err != nil {
		return doctor.Deps{}, err
	}
	settings, settingsErr := config.LoadSettings(paths.SettingsFile())
	if settingsErr != nil {
		settings = config.DefaultSettings()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return doctor.Deps{}, err
	}
	registry, err := buildRegistry(settings, home)
	if err != nil {
		return doctor.Deps{}, err
	}
	binaryPath, err := os.Executable()
	if err != nil {
		return doctor.Deps{}, err
	}

	gh, err := ghx.NewClient()
	if err != nil {
		gh = nil // the "gh" check reports ghx.ErrMissing itself
	}

	var enrolled []repo.Unit
	if localRegistry, err := repo.LoadLocalRegistry(paths.LocalRegistryFile()); err == nil {
		enrolled = localRegistry.Units
	}

	var daemonPing func(context.Context) error
	if client, err := newAPIClient(); err == nil {
		daemonPing = func(ctx context.Context) error {
			_, err := client.Status(ctx)
			return err
		}
	}

	return doctor.Deps{
		Paths:       paths,
		Settings:    settings,
		SettingsErr: settingsErr,
		Registry:    registry,
		GH:          gh,
		BinaryPath:  binaryPath,
		DaemonPing:  daemonPing,
		Enrolled:    enrolled,
		Home:        home,
		Offline:     offline,
	}, nil
}

// printDoctorReport renders the battery in its deterministic order, one row
// per check, with FAIL uppercased for visual emphasis and a fix/fixed line
// under anything that needs one.
func printDoctorReport(out io.Writer, report doctor.Report) error {
	w := &reportWriter{w: out}
	for _, result := range report.Results {
		label := result.Status.String()
		if result.Status == doctor.StatusFail {
			label = strings.ToUpper(label)
		}
		w.printf("%-4s  %-20s %s\n", label, result.Name, result.Detail)
		if result.Status != doctor.StatusOK && result.Fix != "" {
			w.printf("            %-20s fix: %s\n", "", result.Fix)
		}
		if result.Fixed {
			w.printf("            %-20s fixed\n", "")
		}
	}
	return w.err
}
