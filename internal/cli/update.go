package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"

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
	var check, noRestart, selectRelease, list, jsonOut bool
	cmd := &cobra.Command{
		Use:   "update [version]",
		Short: "Update agent-brain to a newer release and restart the service",
		Long: "Downloads a GitHub release for this platform through gh, verifies it " +
			"against the release's checksums file, sanity-runs the new binary, atomically " +
			"replaces the current one, and restarts the daemon service so it picks the new " +
			"binary up.\n\n" +
			"With no arguments the newest release wins, and the resolved version is " +
			"never older than the running one. Naming a version (`agent-brain update " +
			"v2.1.0` or `2.1.0`) pins that exact release instead, and an explicitly " +
			"named OLDER release is installed after a downgrade warning (state written " +
			"by the newer version may not load; run `agent-brain doctor` afterwards).\n\n" +
			"--list prints exactly the releases a version argument accepts (--json for " +
			"scripts); --select offers the same rows as an interactive picker on a " +
			"terminal.\n\n" +
			"Homebrew-installed binaries are refused — use `brew upgrade agent-brain` " +
			"there instead.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if list && len(args) > 0 {
				return errors.New("update: --list takes no version argument")
			}
			if jsonOut && !list {
				return errors.New("update: --json requires --list")
			}
			if selectRelease && len(args) > 0 {
				return errors.New("update: pass a version argument or --select, not both")
			}
			if list {
				ghClient, err := ghx.NewClient()
				if err != nil {
					return err
				}
				releases, err := ghClient.ListReleases(cmd.Context(), productRepo, selfupdate.ReleaseListLimit)
				if err != nil {
					return fmt.Errorf("update: %w", err)
				}
				return writeReleaseList(cmd.OutOrStdout(), releasePickerCandidates(releases, Version), jsonOut)
			}
			binaryPath, err := resolveBinary()
			if err != nil {
				return err
			}
			ghClient, err := ghx.NewClient()
			if err != nil {
				return err
			}
			updater := &selfupdate.Updater{Source: ghClient, Getenv: os.Getenv}
			opts := updateOptions(binaryPath)
			if len(args) == 1 {
				opts.RequestedVersion = args[0]
			}
			if selectRelease {
				tag, err := selectReleaseTag(cmd, ghClient)
				if err != nil {
					return err
				}
				if tag == "" {
					return nil // selection cancelled; already reported
				}
				opts.RequestedVersion = tag
			}
			return runUpdate(cmd.Context(), cmd.OutOrStdout(), updater, opts, check, noRestart, updateRestartFunc(binaryPath))
		},
	}
	cmd.Flags().BoolVar(&check, "check", false,
		"only report what would happen; install nothing")
	cmd.Flags().BoolVar(&noRestart, "no-restart", false,
		"replace the binary but leave the running daemon service alone (it keeps the old version until restarted)")
	cmd.Flags().BoolVar(&selectRelease, "select", false,
		"pick the release from an interactive list (terminal only)")
	cmd.Flags().BoolVar(&list, "list", false,
		"list the installable releases (newest first) and exit")
	cmd.Flags().BoolVar(&jsonOut, "json", false,
		"with --list, emit the releases as JSON")
	cmd.MarkFlagsMutuallyExclusive("list", "select")
	cmd.MarkFlagsMutuallyExclusive("list", "check")
	cmd.MarkFlagsMutuallyExclusive("list", "no-restart")
	return cmd
}

// updateOptions is the selfupdate.Options shared by every in-process update
// path: the product repo, the running version as the downgrade floor, and
// this platform's own binary as the atomic-replace target. Callers pin a
// specific release by setting RequestedVersion; the zero value resolves the
// newest.
func updateOptions(binaryPath string) selfupdate.Options {
	return selfupdate.Options{
		Repo:           productRepo,
		CurrentVersion: Version,
		TargetPath:     binaryPath,
		GOOS:           runtime.GOOS,
		GOARCH:         runtime.GOARCH,
	}
}

// updateRestartFunc is the post-install restart step: bounce the daemon
// service onto the freshly written binary and poll it back to ready. Shared
// by `agent-brain update` and the hub's one-key self-update so both restart
// the service identically.
func updateRestartFunc(binaryPath string) func(context.Context, io.Writer) error {
	return func(ctx context.Context, out io.Writer) error {
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
}

// newSelfUpdater builds the selfupdate engine over a fresh gh client — the
// same construction `update` runs, lifted so the hub's update closures share
// it. A gh-client failure surfaces to the caller (the hub swallows it on
// check, reports it as a toast on apply).
func newSelfUpdater() (*selfupdate.Updater, error) {
	ghClient, err := ghx.NewClient()
	if err != nil {
		return nil, err
	}
	return &selfupdate.Updater{Source: ghClient, Getenv: os.Getenv}, nil
}

// hubUpdateClosures builds the dashboard's update seams (spec §11) over the
// same machinery `agent-brain update` uses. checkUpdate resolves the newest
// release and reduces it to a banner tag (empty when up to date); applyUpdate
// installs a named tag and restarts the daemon by DELEGATING to runUpdate
// with the command's exact check→apply→restart composition, its progress
// output discarded (the hub speaks through its status line, not stdout).
func hubUpdateClosures(binaryPath string) (
	checkUpdate func(context.Context) (string, error),
	applyUpdate func(context.Context, string) error,
) {
	checkUpdate = func(ctx context.Context) (string, error) {
		updater, err := newSelfUpdater()
		if err != nil {
			return "", err
		}
		return adaptCheckDecision(updater.Check(ctx, updateOptions(binaryPath)))
	}
	applyUpdate = func(ctx context.Context, tag string) error {
		updater, err := newSelfUpdater()
		if err != nil {
			return err
		}
		opts := updateOptions(binaryPath)
		opts.RequestedVersion = tag
		return runUpdate(ctx, io.Discard, updater, opts, false, false, updateRestartFunc(binaryPath))
	}
	return checkUpdate, applyUpdate
}

// adaptCheckDecision reduces a selfupdate check to the hub banner's contract:
// the available release tag, or "" when there is nothing to offer. A check
// error propagates unchanged (the hub swallows it — no banner); "up to date"
// becomes the empty tag; otherwise the resolved latest tag is what the banner
// advertises and U installs.
func adaptCheckDecision(decision selfupdate.Decision, err error) (string, error) {
	if err != nil {
		return "", err
	}
	if !decision.UpdateNeeded {
		return "", nil
	}
	return decision.Latest, nil
}

// releaseListRow is `update --list --json`'s wire shape.
type releaseListRow struct {
	Tag        string `json:"tag"`
	Prerelease bool   `json:"prerelease"`
	Running    bool   `json:"running"`
}

// writeReleaseList prints the same rows the picker offers — one source of
// truth for what `update <version>` accepts, unlike raw `gh release list`
// output, which also shows drafts (to maintainers) and non-semver tags the
// pin would refuse. Plain output reuses the picker labels; --json emits
// the structured form for scripts.
func writeReleaseList(out io.Writer, choices []releaseChoice, asJSON bool) error {
	if len(choices) == 0 {
		return fmt.Errorf("update: %w in %s", selfupdate.ErrNoRelease, productRepo)
	}
	if asJSON {
		rows := make([]releaseListRow, len(choices))
		for i, choice := range choices {
			rows[i] = releaseListRow{Tag: choice.tag, Prerelease: choice.prerelease, Running: choice.running}
		}
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(rows)
	}
	for _, choice := range choices {
		if _, err := fmt.Fprintln(out, choice.label); err != nil {
			return err
		}
	}
	return nil
}

// selectReleaseTag runs the interactive release picker and returns the
// chosen tag, or "" when the user cancelled (already reported to out).
// The picker is TTY-only BY DESIGN: huh v2.0.3's accessible Select
// backend auto-accepts the first option on stdin EOF (a headless
// `--select` would silently install the newest release) and panics on an
// invalid line followed by EOF — so headless and ACCESSIBLE callers are
// refused here with the scriptable equivalent, which is `update <version>`.
func selectReleaseTag(cmd *cobra.Command, source selfupdate.ReleaseSource) (string, error) {
	if isAccessible() {
		return "", errors.New("update: --select needs an interactive terminal — run `agent-brain update <version>` instead " +
			"(`agent-brain update --list` shows what exists)")
	}
	releases, err := source.ListReleases(cmd.Context(), productRepo, selfupdate.ReleaseListLimit)
	if err != nil {
		return "", fmt.Errorf("update: %w", err)
	}
	choices := releasePickerCandidates(releases, Version)
	if len(choices) == 0 {
		return "", fmt.Errorf("update: %w in %s", selfupdate.ErrNoRelease, productRepo)
	}
	tag, err := pickReleaseInteractive(choices)
	return releaseSelectionResult(cmd.OutOrStdout(), tag, err)
}

// releaseSelectionResult turns pickReleaseInteractive's outcome into
// selectReleaseTag's return, split out so the cancel branch is testable
// without driving a real huh form.
func releaseSelectionResult(out io.Writer, tag string, err error) (string, error) {
	if formCancelled(err) {
		_, printErr := fmt.Fprintln(out, "update: selection cancelled — nothing changed")
		return "", printErr
	}
	if err != nil {
		return "", err
	}
	return tag, nil
}

// releaseChoice is one picker/list row: the release tag, its display
// label, and the structured facts the label encodes (for --list --json).
type releaseChoice struct {
	tag        string
	label      string
	prerelease bool
	running    bool
}

// releasePickerCandidates orders the picker: non-draft semver releases,
// newest first, each labeled with a prerelease badge where applicable and a
// marker on the running version.
func releasePickerCandidates(releases []ghx.ReleaseInfo, currentVersion string) []releaseChoice {
	current := "v" + currentVersion
	choices := make([]releaseChoice, 0, len(releases))
	for _, release := range releases {
		if release.IsDraft || !semver.IsValid(release.TagName) {
			continue
		}
		running := semver.Compare(release.TagName, current) == 0
		label := release.TagName
		if release.IsPrerelease {
			label += "  (prerelease)"
		}
		if running {
			label += "  ← running"
		}
		choices = append(choices, releaseChoice{
			tag:        release.TagName,
			label:      label,
			prerelease: release.IsPrerelease,
			running:    running,
		})
	}
	slices.SortFunc(choices, func(a, b releaseChoice) int {
		return semver.Compare(b.tag, a.tag)
	})
	return choices
}

// pickReleaseInteractive presents the choices as a huh select and returns
// the chosen tag. Only ever called on an interactive terminal — see
// selectReleaseTag for why the accessible backend is off-limits.
func pickReleaseInteractive(choices []releaseChoice) (string, error) {
	var tag string
	if err := buildReleasePickerForm(choices, &tag).Run(); err != nil {
		return "", err
	}
	if tag == "" {
		return "", errors.New("update: no release selected")
	}
	return tag, nil
}

// buildReleasePickerForm is pickReleaseInteractive's form construction,
// split out so a test can render it (Init/View) without ever running it —
// the render is the only way to pin that the cancel hint actually appears
// in the real production form, not a hand-built replica of it.
//
// The cancel hint is unconditional here (accessible hardcoded false, not
// threaded as a parameter): selectReleaseTag already refuses with
// isAccessible() before this is ever reached, so this form structurally
// never runs in accessible mode — there is no accessible value to thread.
func buildReleasePickerForm(choices []releaseChoice, tag *string) *huh.Form {
	options := make([]huh.Option[string], len(choices))
	for i, choice := range choices {
		options[i] = huh.NewOption(choice.label, choice.tag)
	}
	return huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title(titleWithCancelHint("Select the release to install", false)).
			Options(options...).
			Value(tag),
	)).WithKeyMap(cancellableKeyMap())
}

// runUpdate is the update command's flow: check → report or apply →
// restart. Split from newUpdateCmd so tests drive it with a fake engine
// and a fake restart.
func runUpdate(ctx context.Context, out io.Writer, engine updateEngine, opts selfupdate.Options, checkOnly, noRestart bool, restart func(context.Context, io.Writer) error) error {
	decision, err := engine.Check(ctx, opts)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	requested := opts.RequestedVersion != ""
	if !decision.UpdateNeeded {
		if requested {
			_, err := fmt.Fprintf(out, "update: already running the requested version (%s)\n", opts.CurrentVersion)
			return err
		}
		_, err := fmt.Fprintf(out, "update: already up to date (%s; latest release %s)\n", opts.CurrentVersion, decision.Latest)
		return err
	}
	if checkOnly {
		if decision.Downgrade {
			_, err := fmt.Fprintf(out, "update: %s is a DOWNGRADE from the running %s — `agent-brain update %s` installs it anyway\n",
				decision.Latest, opts.CurrentVersion, decision.Latest)
			return err
		}
		installHint := "`agent-brain update`"
		if requested {
			installHint = fmt.Sprintf("`agent-brain update %s`", decision.Latest)
		}
		_, err := fmt.Fprintf(out, "update: %s available (running %s) — run %s to install\n", decision.Latest, opts.CurrentVersion, installHint)
		return err
	}

	if decision.Downgrade {
		if _, err := fmt.Fprintf(out, "update: DOWNGRADING %s -> %s — state written by the newer version (config, manifests) may not load; run `agent-brain doctor` after the swap\n",
			opts.CurrentVersion, decision.Latest); err != nil {
			return err
		}
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
