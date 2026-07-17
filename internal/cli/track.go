package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/claude"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// trackDeps is the ambient composition track and migrate both need —
// the same Paths → Settings → home → registry chain buildDoctorDeps
// resolves for doctor (registry.go: "daemon, doctor, init, and track
// must all see the identical registry").
type trackDeps struct {
	paths    config.Paths
	registry *provider.Registry
	home     string
}

func buildTrackDeps() (trackDeps, error) {
	paths, err := config.DefaultPaths()
	if err != nil {
		return trackDeps{}, err
	}
	settings, err := config.LoadSettings(paths.SettingsFile())
	if err != nil {
		return trackDeps{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return trackDeps{}, err
	}
	registry, err := buildRegistry(settings, home)
	if err != nil {
		return trackDeps{}, err
	}
	return trackDeps{paths: paths, registry: registry, home: home}, nil
}

func newTrackCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "track [path]",
		Short: "Enroll a memory root for cross-machine sync",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if all && len(args) == 1 {
				return errors.New("track: a path argument and --all are mutually exclusive")
			}
			deps, err := buildTrackDeps()
			if err != nil {
				return err
			}
			client, err := newAPIClient()
			if err != nil {
				return err
			}

			mode := ""
			if all {
				mode = "all"
			}
			callbacks := resolveEnrollCallbacks(mode, false, isAccessible())

			out := cmd.OutOrStdout()
			var enrolledAny bool
			if len(args) == 1 {
				enrolledAny, err = runTrackPath(cmd.Context(), deps, client, callbacks, out, args[0])
			} else {
				enrolledAny, err = runTrackDiscover(cmd.Context(), deps, client, callbacks, out)
			}
			if err != nil {
				return explainDown(err)
			}
			if !enrolledAny {
				return nil
			}
			// Track's HTTP reply returns BEFORE the daemon's post-admin cycle
			// runs (daemon.go's loop replies first, then calls runCycle) — an
			// explicit Sync is what makes the mirror-in actually visible here,
			// rather than landing silently sometime after this command exits.
			return syncAfterTrack(cmd.Context(), client, out)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false,
		"enroll every discovered-but-unenrolled root without prompting (`init --enroll all` semantics: remoteless projects are skipped with a warning)")
	return cmd
}

// runTrackDiscover is the full discovery-picker flow: every
// discovered-but-unenrolled root across every registered provider, offered
// through callbacks.pickEnrollUnits (interactive, or --all's "select
// everything"). Shared with init's step 9 (stepEnrollment) via enroll.go's
// buildEnrollCandidates/enrollOne — this function is track's own thin
// driver over the same primitives.
func runTrackDiscover(ctx context.Context, deps trackDeps, client *api.Client, callbacks enrollCallbacks, out io.Writer) (bool, error) {
	local, err := repo.LoadLocalRegistry(deps.paths.LocalRegistryFile())
	if err != nil {
		return false, err
	}
	enrolled := enrolledSet(local.Units)

	candidates, err := buildEnrollCandidates(ctx, deps.registry, enrolled)
	if err != nil {
		return false, err
	}
	if len(candidates) == 0 {
		_, err = fmt.Fprintln(out, "track: no new memory roots discovered")
		return false, err
	}

	chosen, err := callbacks.pickEnrollUnits(candidates)
	if err != nil {
		return false, err
	}
	if len(chosen) == 0 {
		_, err = fmt.Fprintln(out, "track: nothing selected")
		return false, err
	}

	target := enrollTarget{out: out, confirmProjectPath: callbacks.confirmProjectPath, nameRemotelessFolder: callbacks.nameRemotelessFolder}
	var enrolledAny bool
	for _, index := range chosen {
		candidate := candidates[index]
		for _, discovered := range candidate.discovered {
			err := enrollOne(ctx, target, client, candidate.provider, discovered)
			if errors.Is(err, errSkipRemoteless) {
				if _, err := fmt.Fprintf(out, "track: skipped %s (remoteless; needs a folder name — re-run interactively)\n", discovered.LocalDir); err != nil {
					return enrolledAny, err
				}
				continue
			}
			if formCancelled(err) {
				if _, err := fmt.Fprintf(out, "enroll: cancelled — nothing enrolled for %s\n", discovered.LocalDir); err != nil {
					return enrolledAny, err
				}
				continue
			}
			if err != nil {
				return enrolledAny, err
			}
			enrolledAny = true
		}
	}
	return enrolledAny, nil
}

// runTrackPath resolves path to exactly one candidate (skipping the
// multi-select picker) and enrolls it through the same enrollOne primitive
// discovery uses.
func runTrackPath(ctx context.Context, deps trackDeps, client *api.Client, callbacks enrollCallbacks, out io.Writer, path string) (bool, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}

	local, err := repo.LoadLocalRegistry(deps.paths.LocalRegistryFile())
	if err != nil {
		return false, err
	}
	enrolled := enrolledSet(local.Units)

	p, discovered, err := resolveTrackPath(ctx, deps.registry, deps.home, abs)
	if err != nil {
		return false, err
	}
	if enrolled[[2]string{p.Name(), discovered.LocalDir}] {
		_, err := fmt.Fprintf(out, "track: %s is already tracked\n", discovered.LocalDir)
		return false, err
	}

	target := enrollTarget{out: out, confirmProjectPath: callbacks.confirmProjectPath, nameRemotelessFolder: callbacks.nameRemotelessFolder}
	err = enrollOne(ctx, target, client, p, discovered)
	if errors.Is(err, errSkipRemoteless) {
		_, err := fmt.Fprintf(out, "track: skipped %s (remoteless; needs a folder name — re-run interactively)\n", discovered.LocalDir)
		return false, err
	}
	if formCancelled(err) {
		_, err := fmt.Fprintf(out, "enroll: cancelled — nothing enrolled for %s\n", discovered.LocalDir)
		return false, err
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// resolveTrackPath finds the (provider, Discovered) pair for abs, an
// already-absolute project path. It first looks for an exact PathGuess
// match across every registered per-project provider's Discover results,
// then — for claude specifically — falls back to probing the forward-only
// SlugFor encoding directly against the filesystem: GuessPath's lossy
// reverse (used to compute PathGuess) can guess WRONG for a hyphenated
// leaf like "agent-brain" even when the memory dir genuinely exists (the
// residual ambiguity its own doc comment describes), so this fallback
// never relies on Discover having found the right answer.
func resolveTrackPath(ctx context.Context, registry *provider.Registry, home, abs string) (provider.Provider, provider.Discovered, error) {
	for _, p := range registry.All() {
		if p.Scope() != provider.ScopePerProject {
			continue
		}
		discovered, err := p.Discover(ctx)
		if err != nil {
			return nil, provider.Discovered{}, fmt.Errorf("discover %s: %w", p.Name(), err)
		}
		for _, d := range discovered {
			if d.PathGuess == abs {
				return p, d, nil
			}
		}
	}

	if p, ok := registry.Get("claude"); ok {
		memoryDir := claude.MemoryDirFor(home, abs)
		if info, err := os.Stat(memoryDir); err == nil && info.IsDir() {
			return p, provider.Discovered{LocalDir: memoryDir, Label: filepath.Base(abs), PathGuess: abs}, nil
		}
	}

	return nil, provider.Discovered{}, fmt.Errorf(
		"track: no memory root found for %s — checked every provider's discovery plus ~/.claude/projects/<slug>/memory directly; confirm the path and that the agent has a session there", abs,
	)
}

// syncAfterTrack triggers one cycle and prints its outcome — see
// newTrackCmd's RunE for why an explicit call is needed here. The
// daemon replies to Track/Migrate BEFORE running its own cycle, so by
// the time this explicit cycle executes (queued behind the same single
// engine goroutine) the enrollment's mirror-in and push have usually
// already happened — this cycle finding nothing left is the SUCCESS
// shape, and printing its zeros as if they were the enrollment's result
// misreads success as a no-op. An all-quiet
// summary is therefore reported as up-to-date instead of itemized.
func syncAfterTrack(ctx context.Context, client *api.Client, out io.Writer) error {
	response, err := client.Sync(ctx, "")
	if err != nil {
		return explainDown(err)
	}
	report := &reportWriter{w: out}
	switch {
	case response.Status == "running":
		report.println("sync still running — check `agent-brain status`")
	case summaryIsNoOp(response.Summary):
		report.println("sync: up to date (the daemon already synced this in its own cycle)")
	default:
		report.println("sync completed")
		printSummary(report, response.Summary)
	}
	return report.err
}

// summaryIsNoOp reports whether a cycle did literally nothing worth
// itemizing: no error, no commits, no mirrored files, no push (done or
// queued), nothing degraded, nothing scrubbed. A nil summary is NOT a
// no-op — absence of evidence keeps the conservative "sync completed"
// path rather than claiming up-to-date without a report.
func summaryIsNoOp(summary *api.SyncSummary) bool {
	if summary == nil {
		return false
	}
	return summary.Error == "" &&
		len(summary.Commits) == 0 &&
		len(summary.Degraded) == 0 &&
		len(summary.Scrubbed) == 0 &&
		!summary.Pushed && !summary.PushQueued &&
		summary.MirrorIn == (api.Stats{}) &&
		summary.MirrorOut == (api.Stats{})
}

func newUntrackCmd() *cobra.Command {
	var purge, yes bool
	cmd := &cobra.Command{
		Use:   "untrack <path|folder>",
		Short: "Remove an enrollment (optionally purging its repo folder)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newAPIClient()
			if err != nil {
				return err
			}
			confirmPurge := func(folder string) (bool, error) { return confirmPurgeInteractive(folder, isAccessible()) }
			return runUntrack(cmd.Context(), client, cmd.OutOrStdout(), args[0], purge, yes, confirmPurge)
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "also remove the project folder from the repo (history retains it)")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the typed purge confirmation")
	return cmd
}

// runUntrack resolves target to one enrolled unit (by repo folder name or
// LocalDir path), confirms a purge unless --yes was given, then submits
// the Untrack call. api.UnitInfo carries no ProjectID/PathGuess (unlike
// provider.Discovered), so — unlike track <path> — this cannot reverse a
// claude project path back to its LocalDir; folder name (from
// `agent-brain projects`) is the reliable way to name a claude unit here.
func runUntrack(ctx context.Context, client *api.Client, out io.Writer, target string, purge, yes bool, confirmPurge func(folder string) (bool, error)) error {
	projects, err := client.Projects(ctx)
	if err != nil {
		return explainDown(err)
	}
	unit, err := resolveUntrackTarget(projects.Units, target)
	if err != nil {
		return err
	}

	if purge && !yes {
		confirmed, err := confirmPurge(unit.Folder)
		if err != nil {
			return err
		}
		if !confirmed {
			_, err := fmt.Fprintln(out, "untrack: purge not confirmed — aborted (the enrollment itself was NOT removed)")
			return err
		}
	}

	response, err := client.Untrack(ctx, api.UntrackRequest{Provider: unit.Provider, LocalDir: unit.LocalDir, Purge: purge})
	if err != nil {
		return explainDown(err)
	}

	report := &reportWriter{w: out}
	// Removed is false when the daemon found no such enrollment in the local
	// registry — a race with another untrack, or a stale `projects` listing.
	// Saying "removed" regardless would report work that never happened.
	if response.Removed {
		report.printf("untrack: %s (%s) removed\n", unit.LocalDir, unit.Folder)
	} else {
		report.printf("untrack: %s (%s) was not enrolled — nothing to remove\n", unit.LocalDir, unit.Folder)
	}
	switch {
	case response.Purged:
		report.printf("untrack: %s purged from the repo\n", unit.Folder)
	case purge:
		report.printf("untrack: %s not purged — another machine still tracks it\n", unit.Folder)
	}
	report.println("untrack: repo history retains everything regardless of purge")
	if report.err != nil {
		return report.err
	}
	return syncAfterTrack(ctx, client, out)
}

// resolveUntrackTarget finds the ONE enrolled unit target names: an exact
// repo folder name, or a LocalDir path match (exact, a subpath within it,
// or target being a broader ancestor LocalDir nests under — e.g. codex's
// ~/.codex/memories under ~/.codex). Ambiguity (two units share the
// matched folder — always true for codex's shared "_global", or two
// LocalDirs both plausibly matching) is an error listing every candidate
// rather than guessing.
func resolveUntrackTarget(units []api.UnitInfo, target string) (api.UnitInfo, error) {
	var byFolder, byPath []api.UnitInfo
	abs, absErr := filepath.Abs(target)
	for _, u := range units {
		switch {
		case u.Folder == target:
			byFolder = append(byFolder, u)
		case absErr == nil && isPathMatch(abs, u.LocalDir):
			byPath = append(byPath, u)
		}
	}
	if len(byFolder) == 1 {
		return byFolder[0], nil
	}
	if len(byFolder) > 1 {
		return api.UnitInfo{}, fmt.Errorf("%q is ambiguous; candidates: %s", target, formatUnitCandidates(byFolder))
	}
	if len(byPath) == 1 {
		return byPath[0], nil
	}
	if len(byPath) > 1 {
		return api.UnitInfo{}, fmt.Errorf("%q is ambiguous; candidates: %s", target, formatUnitCandidates(byPath))
	}
	return api.UnitInfo{}, fmt.Errorf("no enrolled unit matches %q; enrolled folders: %s", target, strings.Join(folderNames(units), ", "))
}

func isPathMatch(target, localDir string) bool {
	if target == localDir {
		return true
	}
	return strings.HasPrefix(target, localDir+string(filepath.Separator)) ||
		strings.HasPrefix(localDir, target+string(filepath.Separator))
}

func folderNames(units []api.UnitInfo) []string {
	names := make([]string, len(units))
	for i, u := range units {
		names[i] = u.Folder
	}
	return names
}

func formatUnitCandidates(units []api.UnitInfo) string {
	parts := make([]string, len(units))
	for i, u := range units {
		parts[i] = fmt.Sprintf("%s (%s)", u.Folder, u.LocalDir)
	}
	return strings.Join(parts, ", ")
}

// confirmPurgeInteractive requires typing the folder name back exactly —
// a purge deletes the repo folder, so confirmation should name the
// specific thing being removed, not just answer yes/no. A cancel is
// folded into the same "not confirmed" outcome the typed-mismatch case
// already produces: runUntrack's existing message for it ("purge not
// confirmed — aborted, the enrollment itself was NOT removed") is already
// honest and specific, so a cancel needs no new copy of its own.
func confirmPurgeInteractive(folder string, accessible bool) (bool, error) {
	var typed string
	err := buildPurgeConfirmForm(folder, accessible, &typed).Run()
	return purgeConfirmationResult(typed, folder, err)
}

// buildPurgeConfirmForm is confirmPurgeInteractive's form construction,
// split out so a test can render it (Init/View) without ever running it —
// the render is the only way to pin that the cancel hint actually appears
// in the real production form, not a hand-built replica of it.
func buildPurgeConfirmForm(folder string, accessible bool, typed *string) *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title(titleWithCancelHint(fmt.Sprintf("Type %q to confirm purging this folder from the repo (history retains it)", folder), accessible)).
			Value(typed),
	)).WithAccessible(accessible).WithKeyMap(cancellableKeyMap())
}

// purgeConfirmationResult turns confirmPurgeInteractive's form outcome into
// its return, split out so the cancel branch is testable without driving a
// real huh form.
func purgeConfirmationResult(typed, folder string, err error) (bool, error) {
	if formCancelled(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return typed == folder, nil
}
