package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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
	"github.com/Sawmonabo/agent-brain/internal/provider/claude"
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

	checkUpdate, applyUpdate := hubUpdateClosures(binaryPath)

	model := dashboard.New(dashboard.Config{
		Data:     dashboard.NewData(client, offlineDoctorRunner()),
		Discover: dashboardDiscover(),
		Identify: dashboardIdentify(),
		// The migrate flow's three closures (spec §10), composed here in package
		// cli for the same reason Discover/Identify are: the dashboard tree cannot
		// import the provider adapters or the bash-era importer's helpers. Identify
		// above is REUSED as the migrate flow's identity resolver (dashboard.New
		// wires the one closure into both the track and migrate action bundles), so
		// enroll and migrate can never disagree about a project's identity.
		LegacyDiscover:   dashboardLegacyDiscover(),
		LiveDirFor:       dashboardLiveDirFor(),
		MigratePreflight: dashboardMigratePreflight(),
		Registry:         deps.registry,
		Settings:         settings,
		CacheRoot:        cacheRoot,
		// The build version stamped into cli.Version (-ldflags), rendered in the
		// Projects fleet header (spec §9). "dev" for an unstamped local build.
		Version: Version,
		// The hub's self-update seams (spec §11): CheckUpdate resolves the
		// newest release for the status-header banner; ApplyUpdate installs a
		// chosen tag and restarts the daemon. Both run the same selfupdate
		// machinery `agent-brain update` uses (hubUpdateClosures), so the hub
		// and the command can never drift in what they install or how.
		CheckUpdate: checkUpdate,
		ApplyUpdate: applyUpdate,
		// The Doctor tab's action seams (spec §11/§12). RunDoctorFix is the
		// exact quiesce-aware `doctor --fix` the command runs, passing offline=true
		// (the hub's whole doctor posture is offline — offlineDoctorRunner — so it
		// never probes the network from the TUI) and discarding stderr (the hub
		// reports through the Doctor view, not stderr), so the command and the hub
		// can never drift in how they hold the daemon or what they repair. Scan is
		// the advisory gitleaks sweep, composed here in package cli because its
		// provider/gitleaks composition sits outside the dashboard package's import
		// allowlist — the same edge-composition reason the doctor runner is injected.
		RunDoctorFix: func(ctx context.Context) (doctor.Report, error) {
			return runDoctorFixWithQuiesce(ctx, true, io.Discard)
		},
		Scan: hubScanRunner(),
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
	finalModel, err := program.Run()
	if err != nil {
		// A context-cancelled run (external signal) is a clean user exit,
		// not a CLI failure to report as exit code 1.
		if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	// On a clean quit, hand off to the just-installed binary if the hub
	// latched the R restart after a self-update (spec §11). When no re-exec
	// was requested this returns nil and the process exits normally; on
	// success syscall.Exec never returns — this invocation becomes the new
	// binary, same argv and environment.
	return maybeReExec(finalModel, syscall.Exec)
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

// hubScanRunner composes the advisory gitleaks sweep the Doctor tab's s action
// runs (spec §12) — the same local-registry read + per-unit gitleaks shell-out
// `agent-brain scan` performs — and maps each finding to the hub's
// views.ScanFinding. It lives in package cli because the provider/registry
// composition and the gitleaks runner sit outside the dashboard package's
// import allowlist; the dashboard only ever sees the mapped findings. folder ""
// scans every enrolled unit, a non-empty folder narrows to that project.
// redaction is ALWAYS on: the hub struct carries only a finding's LOCATION
// (file/rule/line), never its secret text, so gitleaks --redact scrubs
// Secret/Match before the report reaches this process and no plaintext secret
// ever enters hub memory even transiently (plaintext-never-logged is a repo
// hard constraint, spec §12).
func hubScanRunner() func(context.Context, string) ([]views.ScanFinding, error) {
	return func(ctx context.Context, folder string) ([]views.ScanFinding, error) {
		paths, err := config.DefaultPaths()
		if err != nil {
			return nil, err
		}
		localRegistry, err := repo.LoadLocalRegistry(paths.LocalRegistryFile())
		if err != nil {
			return nil, err
		}
		units := localRegistry.Units
		if folder != "" {
			units = filterUnitsByFolder(units, folder)
		}
		binaryPath, err := exec.LookPath("gitleaks")
		if err != nil {
			return nil, errGitleaksMissing
		}
		runner := &gitleaksExecRunner{binaryPath: binaryPath}
		findings, err := scanUnits(ctx, runner, units, true)
		if err != nil {
			return nil, err
		}
		hits := make([]views.ScanFinding, len(findings))
		for i, finding := range findings {
			hits[i] = views.ScanFinding{
				Folder: finding.Folder,
				// gitleaks `dir` mode reports File as the scanned root joined with
				// the in-unit path — i.e. LocalDir-prefixed and absolute (verified
				// model, scan_test.go). The view renders Folder/File:line, so File
				// must be UNIT-RELATIVE or the location doubles (work//Users/…). Trim
				// the LocalDir prefix; TrimPrefix returns the path untouched if it
				// somehow isn't prefixed (symlinked root, future gitleaks change), so
				// a mismatch degrades to the raw path rather than a wrong join.
				File: strings.TrimPrefix(finding.Finding.File, finding.LocalDir+"/"),
				Rule: finding.Finding.RuleID,
				Line: finding.Finding.StartLine,
			}
		}
		return hits, nil
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

// dashboardLegacyDiscover enumerates the un-imported bash-era stores the hub's
// migrate flow offers (spec §10), reusing runMigrate's own discovery verbatim —
// legacyRoot → enumerateLegacySlugs → the per-slug hasRealContent filter — so
// the hub and the `migrate` command can never disagree about what is a
// migratable store. Every slug with real content becomes a candidate; the
// daemon's Skipped marker (not a client-side probe) is what makes an
// already-imported slug idempotent, so those DO appear and resolve through the
// "already imported" wording. Deps are rebuilt per call so each `m` press sees
// the current legacy tree. PathGuess binds the SAME os.Stat closure migrateOne
// binds, and LiveDir is precomputed for the guess (the flow recomputes it via
// dashboardLiveDirFor only if the user corrects the path).
func dashboardLegacyDiscover() func(context.Context) ([]views.MigrateCandidate, error) {
	return func(_ context.Context) ([]views.MigrateCandidate, error) {
		deps, err := buildTrackDeps()
		if err != nil {
			return nil, err
		}
		root := legacyRoot(deps.home)
		slugs, err := enumerateLegacySlugs(root)
		if err != nil {
			return nil, err
		}
		statDir := func(p string) bool {
			info, err := os.Stat(p)
			return err == nil && info.IsDir()
		}
		var candidates []views.MigrateCandidate
		for _, slug := range slugs {
			hasContent, err := hasRealContent(filepath.Join(root, slug))
			if err != nil {
				return nil, err
			}
			if !hasContent {
				continue
			}
			pathGuess := claude.GuessPath(slug, statDir)
			candidates = append(candidates, views.MigrateCandidate{
				Provider:  "claude",
				Slug:      slug,
				SeedDir:   filepath.Join(root, slug),
				PathGuess: pathGuess,
				LiveDir:   claude.MemoryDirFor(deps.home, pathGuess),
			})
		}
		return candidates, nil
	}
}

// dashboardLiveDirFor maps a confirmed project path to the live claude memory
// dir the daemon enrolls (spec §10 step 4) — claude.MemoryDirFor, the exact
// LocalDir migrateOne submits. claude is the bash-era importer's only domain, so
// the provider name is asserted rather than dispatched; the error return exists
// for the deps/home resolution MemoryDirFor itself never fails at.
func dashboardLiveDirFor() func(providerName, projectPath string) (string, error) {
	return func(providerName, projectPath string) (string, error) {
		if providerName != "claude" {
			return "", fmt.Errorf("migrate: live dir for provider %q is not supported (spec §10 imports claude only)", providerName)
		}
		deps, err := buildTrackDeps()
		if err != nil {
			return "", err
		}
		return claude.MemoryDirFor(deps.home, projectPath), nil
	}
}

// dashboardMigratePreflight runs spec §10's chezmoi gate bound to ambient config
// — the cobra command's own else-branch verbatim (settings-resolved timeout,
// the ConfigDir/chezmoi.toml path, runMigratePreflight). The CLI's
// --skip-preflight escape is CLI-only; the hub ALWAYS runs the real gate, once
// per session (the flow latches migratePreflighted after the first pass).
func dashboardMigratePreflight() func(context.Context) error {
	return func(ctx context.Context) error {
		deps, err := buildTrackDeps()
		if err != nil {
			return err
		}
		settings, err := config.LoadSettings(deps.paths.SettingsFile())
		if err != nil {
			return err
		}
		chezmoiConfigPath := filepath.Join(deps.paths.ConfigDir, "chezmoi.toml")
		timeout := time.Duration(settings.Migrate.PreflightTimeout)
		return runMigratePreflight(ctx, chezmoiConfigPath, timeout)
	}
}
