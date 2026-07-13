package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/editorx"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/views"
	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/repo"
	"github.com/Sawmonabo/agent-brain/internal/service"
)

// newDashboardCmd wires `agent-brain dashboard` (spec §7): the bubbletea v2 TUI
// in internal/cli/dashboard. This command and that package are the only two
// places in the repo permitted to import bubbletea/lipgloss directly (ADR 05
// amendment); everything else stays huh/fang.
func newDashboardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dashboard",
		Short: "Live TUI over the running daemon: projects, conflicts, activity, doctor",
		Long: "Live terminal dashboard over the running daemon — enrolled projects, " +
			"retained conflicts, daemon activity, and doctor checks, refreshed every " +
			"couple of seconds.\n\n" +
			"It requires an interactive terminal. For scripted or non-interactive use " +
			"the equivalents are `agent-brain status --json` and `agent-brain projects --json`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return launchHub(cmd)
		},
	}
}

// launchHub builds and runs the bubbletea hub program: `agent-brain
// dashboard`'s own body (spec §7), and the bare root's initialized+TTY path
// (spec §1; ADR 20 decision 1) both call it, so the two entry points can
// never diverge — including this same TTY refusal, which the bare root's
// initialized+non-TTY case relies on verbatim rather than duplicating.
func launchHub(cmd *cobra.Command) error {
	if !isInteractiveTTY(cmd) {
		return errors.New("dashboard requires an interactive terminal (for scripting use `agent-brain status --json` or `agent-brain projects --json`)")
	}
	client, err := newAPIClient()
	if err != nil {
		return err
	}
	binaryPath, err := resolveBinary()
	if err != nil {
		return err
	}
	controller, err := service.NewController(binaryPath)
	if err != nil {
		return err
	}
	deps, err := buildTrackDeps()
	if err != nil {
		return err
	}
	settings, err := loadDashboardSettings()
	if err != nil {
		return err
	}
	// The edit flow's scratch dirs nest under the app's own cache dir
	// (~/Library/Caches/agent-brain, ~/.cache/agent-brain) — outside every
	// watched provider tree, per ADR 20 D2. UserCacheDir only fails when
	// the home dir cannot resolve, which loadDashboardSettings' own
	// DefaultPaths call has already ruled out by this line; treat a failure
	// like any other composition error rather than inventing a fallback.
	userCacheDir, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	cacheRoot := filepath.Join(userCacheDir, "agent-brain")
	// Reclaim scratch dirs orphaned by earlier sessions (quit or crash
	// mid-edit, abandoned failure preservations) before the hub starts:
	// they hold plaintext memory copies, which must never persist forever
	// (editorx.ScratchStaleAfter documents the window and its rationale).
	// The error is deliberately dropped — reclamation is opportunistic
	// housekeeping, anything unswept waits for the next launch, and
	// refusing to launch the dashboard over it would invert priorities.
	_ = editorx.SweepStaleScratch(cacheRoot, time.Now())

	model := dashboard.New(dashboard.Config{
		Data:      dashboard.NewData(client, offlineDoctorRunner()),
		Discover:  dashboardDiscover(),
		Identify:  dashboardIdentify(),
		Registry:  deps.registry,
		Settings:  settings,
		CacheRoot: cacheRoot,
		// The build version stamped into cli.Version (-ldflags), rendered in the
		// Projects fleet header (spec §9). "dev" for an unstamped local build.
		Version: Version,
		// The start offer only appears on the daemon-down screen. A
		// service that probes as already running there means a daemon
		// that is up-but-unresponsive or crash-looping — starting
		// cannot help, so name the next move instead of relaying the
		// sentinel as a bare "start failed: service already running".
		StartService: func() error {
			err := controller.Start()
			if errors.Is(err, service.ErrAlreadyRunning) {
				return errors.New("service already running but the daemon is not responding — check `agent-brain service logs`")
			}
			return err
		},
	})
	program := tea.NewProgram(
		model,
		tea.WithContext(cmd.Context()),
		tea.WithInput(cmd.InOrStdin()),
		tea.WithOutput(cmd.OutOrStdout()),
	)
	if _, err := program.Run(); err != nil {
		// A context-cancelled run (external signal) is a clean user exit,
		// not a CLI failure to report as exit code 1.
		if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
}

// loadDashboardSettings loads config.toml independently of buildTrackDeps
// (which loads its own copy internally, only to feed buildRegistry — it
// never keeps the Settings value around): the dashboard package composes
// its own Config at the edge, the same as Registry above, and a later
// task's memory-browser staleness threshold and $EDITOR choice both read
// from Settings, not from anything buildTrackDeps exposes. A failure here
// propagates exactly like buildTrackDeps' own internal LoadSettings call —
// this command already treats an unparseable config.toml as fatal one line
// above, so a second, more lenient policy for the identical file would be
// inconsistent, not more robust.
func loadDashboardSettings() (config.Settings, error) {
	paths, err := config.DefaultPaths()
	if err != nil {
		return config.Settings{}, err
	}
	return config.LoadSettings(paths.SettingsFile())
}

// offlineDoctorRunner assembles the full doctor.Deps (the same registry/gh
// composition `doctor` itself uses) and runs the battery read-only with
// --offline semantics — the 2s dashboard poll must never make a network
// ls-remote. Deps is built here, in package cli, because the provider/ghx/
// registry packages it needs sit outside the dashboard package's import
// allowlist (ADR 05 amendment); the dashboard only ever sees the doctor.Report.
func offlineDoctorRunner() func(context.Context) (doctor.Report, error) {
	return func(ctx context.Context) (doctor.Report, error) {
		deps, err := buildDoctorDeps(true, os.Getenv(testBinaryPathEnv))
		if err != nil {
			return doctor.Report{}, err
		}
		return doctor.Run(ctx, deps), nil
	}
}

// isInteractiveTTY reports whether the command's stdin AND stdout are real
// terminals. A bubbletea program needs both: stdin to read keys, stdout to
// render the alternate screen. In tests (and any piped invocation) the cobra
// in/out are byte buffers, not *os.File, so this returns false and the command
// refuses cleanly instead of hanging on a non-tty (task brief EOF/TTY contract).
func isInteractiveTTY(cmd *cobra.Command) bool {
	in, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		return false
	}
	out, ok := cmd.OutOrStdout().(*os.File)
	if !ok || !term.IsTerminal(int(out.Fd())) {
		return false
	}
	return true
}

// dashboardDiscover mirrors track's discovery flow (runTrackDiscover): the
// same buildTrackDeps composition and buildEnrollCandidates filter, mapped to
// the dashboard's provider-name candidate shape — the dashboard package
// cannot import cli or compose providers itself (ADR 05 amendment). Deps are
// rebuilt per call so every `a` press sees the current registry and
// enrollment; a root tracked since the last press disappears from the offer.
func dashboardDiscover() func(context.Context) ([]views.TrackCandidate, error) {
	return func(ctx context.Context) ([]views.TrackCandidate, error) {
		deps, err := buildTrackDeps()
		if err != nil {
			return nil, err
		}
		local, err := repo.LoadLocalRegistry(deps.paths.LocalRegistryFile())
		if err != nil {
			return nil, err
		}
		candidates, err := buildEnrollCandidates(ctx, deps.registry, enrolledSet(local.Units))
		if err != nil {
			return nil, err
		}
		out := make([]views.TrackCandidate, 0, len(candidates))
		for _, candidate := range candidates {
			roots := make([]views.TrackRoot, len(candidate.discovered))
			for i, discovered := range candidate.discovered {
				roots[i] = views.TrackRoot{LocalDir: discovered.LocalDir, RepoSubdir: discovered.RepoSubdir}
			}
			global := candidate.provider.Scope() == provider.ScopeGlobal
			pathGuess := ""
			if !global {
				pathGuess = candidate.discovered[0].PathGuess
			}
			out = append(out, views.TrackCandidate{
				Provider:  candidate.provider.Name(),
				Label:     candidate.label,
				PathGuess: pathGuess,
				Global:    global,
				Roots:     roots,
			})
		}
		return out, nil
	}
}

// dashboardIdentify resolves one candidate root's cross-machine identity for
// a human-confirmed project path — the enrollOne Identify step, reached
// through the registry so the dashboard names providers by string only.
func dashboardIdentify() func(context.Context, string, views.TrackRoot, string) (provider.Identity, error) {
	return func(ctx context.Context, providerName string, root views.TrackRoot, projectPath string) (provider.Identity, error) {
		deps, err := buildTrackDeps()
		if err != nil {
			return provider.Identity{}, err
		}
		registered, ok := deps.registry.Get(providerName)
		if !ok {
			return provider.Identity{}, fmt.Errorf("provider %q is not registered", providerName)
		}
		discovered := provider.Discovered{LocalDir: root.LocalDir, RepoSubdir: root.RepoSubdir}
		return registered.Identify(ctx, discovered, projectPath)
	}
}
