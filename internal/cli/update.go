package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/ghx"
	"github.com/Sawmonabo/agent-brain/internal/selfupdate"
	"github.com/Sawmonabo/agent-brain/internal/service"
)

// productRepo is the distribution repository `update` resolves releases
// from — the product's own home, not the per-user memories repo.
const productRepo = "Sawmonabo/agent-brain"

// updateDaemonPollTimeout/Interval bound the post-restart readiness poll —
// the same values init's own daemon wait uses (ensureDaemonClient).
const (
	updateDaemonPollTimeout  = 15 * time.Second
	updateDaemonPollInterval = 500 * time.Millisecond
)

// updateEngine is newUpdateCmd's seam over selfupdate.Updater so the
// command's flow (messages, restart orchestration) is testable without
// building release archives — the Updater's own mechanics are covered by
// internal/selfupdate's tests.
type updateEngine interface {
	Check(ctx context.Context, opts selfupdate.Options) (selfupdate.Decision, error)
	Apply(ctx context.Context, opts selfupdate.Options, targetTag string) error
}

func newUpdateCmd() *cobra.Command {
	var check, prerelease, noRestart bool
	cmd := &cobra.Command{
		Use:   "update [--flags]",
		Short: "Update agent-brain to the latest release and restart the service",
		Long: "Downloads the latest GitHub release for this platform through gh, verifies " +
			"it against the release's checksums file, sanity-runs the new binary, atomically " +
			"replaces the current one, and restarts the daemon service so it picks the new " +
			"binary up.\n\n" +
			"Stable releases only by default; --prerelease widens the channel to release " +
			"candidates. Homebrew-installed binaries are refused — use `brew upgrade " +
			"agent-brain` there instead. The command never downgrades.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			binaryPath, err := resolveBinary()
			if err != nil {
				return err
			}
			ghClient, err := ghx.NewClient()
			if err != nil {
				return err
			}
			updater := &selfupdate.Updater{Source: ghClient, Getenv: os.Getenv}
			opts := selfupdate.Options{
				Repo:           productRepo,
				CurrentVersion: Version,
				TargetPath:     binaryPath,
				Prerelease:     prerelease,
				GOOS:           runtime.GOOS,
				GOARCH:         runtime.GOARCH,
			}
			restart := func(ctx context.Context, out io.Writer) error {
				controller, err := service.NewController(binaryPath)
				if err != nil {
					return err
				}
				return restartServiceForUpdate(ctx, out, controller, func(pollCtx context.Context) string {
					if client := pollForDaemonClient(pollCtx, updateDaemonPollTimeout, updateDaemonPollInterval); client != nil {
						if status, err := client.Status(pollCtx); err == nil {
							return status.Version
						}
					}
					return ""
				})
			}
			return runUpdate(cmd.Context(), cmd.OutOrStdout(), updater, opts, check, noRestart, restart)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false,
		"only report whether an update is available; install nothing")
	cmd.Flags().BoolVar(&prerelease, "prerelease", false,
		"consider prerelease tags (release candidates), not just stable releases")
	cmd.Flags().BoolVar(&noRestart, "no-restart", false,
		"replace the binary but leave the running daemon service alone (it keeps the old version until restarted)")
	return cmd
}

// runUpdate is the update command's flow: check → report or apply →
// restart. Split from newUpdateCmd so tests drive it with a fake engine
// and a fake restart.
func runUpdate(ctx context.Context, out io.Writer, engine updateEngine, opts selfupdate.Options, checkOnly, noRestart bool, restart func(context.Context, io.Writer) error) error {
	decision, err := engine.Check(ctx, opts)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	if !decision.UpdateNeeded {
		_, err := fmt.Fprintf(out, "update: already up to date (%s; latest release %s)\n", opts.CurrentVersion, decision.Latest)
		return err
	}
	if checkOnly {
		_, err := fmt.Fprintf(out, "update: %s available (running %s) — run `agent-brain update` to install\n", decision.Latest, opts.CurrentVersion)
		return err
	}

	if _, err := fmt.Fprintf(out, "update: downloading and verifying %s for %s/%s\n", decision.Latest, opts.GOOS, opts.GOARCH); err != nil {
		return err
	}
	if err := engine.Apply(ctx, opts, decision.Latest); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	if _, err := fmt.Fprintf(out, "update: installed %s -> %s at %s\n", opts.CurrentVersion, decision.Latest, opts.TargetPath); err != nil {
		return err
	}

	if noRestart {
		_, err := fmt.Fprintln(out, "update: service left running on the old version (--no-restart) — restart with `agent-brain service stop && agent-brain service start`")
		return err
	}
	return restart(ctx, out)
}

// restartServiceForUpdate bounces the daemon service onto the freshly
// installed binary and reports readiness. A not-installed service is a
// legitimate posture (--skip-service init), reported and skipped — the
// binary update itself already succeeded. The stop/start tolerances lean
// on the goal-state sentinels: a stopped service just starts, and a start
// race lost to the manager is success.
func restartServiceForUpdate(ctx context.Context, out io.Writer, controller service.Controller, daemonVersion func(context.Context) string) error {
	status, err := controller.Status()
	if err == nil && status == service.StatusNotInstalled {
		_, printErr := fmt.Fprintln(out, "service: not installed — nothing to restart")
		return printErr
	}
	if err := stopServiceAndReport(controller, out); err != nil && !errors.Is(err, service.ErrNotRunning) {
		return fmt.Errorf("update: stop service: %w", err)
	}
	if err := startServiceAndReport(controller, out); err != nil && !errors.Is(err, service.ErrAlreadyRunning) {
		return fmt.Errorf("update: start service: %w", err)
	}
	if version := daemonVersion(ctx); version != "" {
		_, err := fmt.Fprintf(out, "daemon: ready (version %s)\n", version)
		return err
	}
	_, err = fmt.Fprintf(out, "daemon: not confirmed ready within %s — check `agent-brain status` and `agent-brain service logs`\n", updateDaemonPollTimeout)
	return err
}
