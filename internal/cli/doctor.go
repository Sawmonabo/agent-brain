package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
	"github.com/Sawmonabo/agent-brain/internal/ghx"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// testBinaryPathEnv lets a test point doctor's filter/credential-helper
// wiring checks at a real built binary instead of os.Executable() — which,
// inside a test process, IS the compiled cli.test binary itself, the exact
// anti-pattern CLAUDE.md's fork-bomb rule (commit 8624631) forbids (Q3 gate
// finding I1). There is no per-invocation parameter to thread this through:
// newDoctorCmd is built once, argument-less, inside root.go's Root() — the
// same Root() every cli test drives via runCmd (filter_test.go) — so
// changing buildDoctorDeps' signature alone cannot reach a test; an env var
// is the seam that can, mirroring how config.DefaultPaths already resolves
// AGENT_BRAIN_CONFIG_DIR et al. for the identical class of problem. Unlike
// those, this is not a real user-facing setting — there is no legitimate
// production reason to override your own running binary's path — so the
// name says TEST explicitly. See testmain_test.go's testBinaryPath doc
// comment for the incident this exists to prevent.
const testBinaryPathEnv = "AGENT_BRAIN_TEST_BINARY_PATH"

func newDoctorCmd() *cobra.Command {
	var fix, jsonOut, offline bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check (and optionally repair) this machine's agent-brain wiring",
		RunE: func(cmd *cobra.Command, _ []string) error {
			deps, err := buildDoctorDeps(offline, os.Getenv(testBinaryPathEnv))
			if err != nil {
				return err
			}
			var report doctor.Report
			if fix {
				// --fix re-wires the checkout's git config and rewrites
				// .gitattributes; a resident daemon's cycle racing that surgery
				// contends on git locks (same Phase-3 F2 hazard init closes).
				// Hold its cycles best-effort — a daemon that is down or refuses
				// is the status quo, never a reason to fail the repair. A
				// refusal still gets an operator-visible note (stderr, so a
				// --json stdout consumer stays clean) mirroring init's
				// identical situation (initsteps.go's stepRepoState) — Task 4.5
				// absorbs this as a T2 residual Minor: doctor used to swallow
				// this case silently.
				if client := tryAPIClient(cmd.Context()); client != nil {
					if _, qerr := client.Quiesce(cmd.Context(), quiesceHoldForInit); qerr != nil {
						if _, werr := fmt.Fprintf(cmd.ErrOrStderr(), "doctor: could not quiesce the daemon (%v) — proceeding\n", qerr); werr != nil {
							return werr
						}
					} else {
						defer resumeQuietly(client)
					}
				}
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
	cmd.Flags().BoolVar(&fix, "fix", false, "apply the idempotent wiring repairs (filters, attributes, credential helper, maintenance posture), then re-check")
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
// binaryPath mirrors daemon.Config.BinaryPath's empty-means-default
// convention: "" resolves os.Executable() here, same as production always
// did; a non-empty value (RunE passes testBinaryPathEnv's value) is used
// as-is, letting a test inject a real built binary instead (Q3 gate finding
// I1) — never os.Executable(), which inside a test process is the compiled
// cli.test binary itself.
func buildDoctorDeps(offline bool, binaryPath string) (doctor.Deps, error) {
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
	if binaryPath == "" {
		resolved, err := os.Executable()
		if err != nil {
			return doctor.Deps{}, err
		}
		binaryPath = resolved
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
