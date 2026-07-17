package dashboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	keybinding "charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	glamour "charm.land/glamour/v2"
	glamourstyles "charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/actions"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/links"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/views"
	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
	"github.com/Sawmonabo/agent-brain/internal/ghx"
	"github.com/Sawmonabo/agent-brain/internal/provider"
)

// pollInterval is the shared refresh cadence. Tick-based polling is the
// idiomatic bubbletea pattern for a local daemon: no push channel exists, and
// inventing one would violate the no-new-seams rule (spec §7 / task brief).
const pollInterval = 2 * time.Second

// toastTTL is how long a pushed toast stays visible. Expiry is checked on
// the existing 2s poll tick (tickMsg) rather than a dedicated timer — one
// fewer moving part, and toasts are never so time-critical that a sub-2s
// clear matters.
const toastTTL = 5 * time.Second

// tab identifies the active view, in tab-bar order (spec §7).
type tab int

const (
	tabProjects tab = iota
	tabConflicts
	tabActivity
	tabDoctor
	tabCount
)

func (t tab) title() string {
	switch t {
	case tabProjects:
		return "Projects"
	case tabConflicts:
		return "Conflicts"
	case tabActivity:
		return "Activity"
	case tabDoctor:
		return "Doctor"
	default:
		return ""
	}
}

// Messages. Every one is produced by a background Cmd; Update and View never
// perform I/O directly (model purity, enforced by the Q3 gate). Sync/untrack/
// discover/identify/track results are views.SyncResultMsg etc. — produced by
// Cmds that live with the Projects view (spec §15) — switched on below
// alongside these root-owned messages.
type (
	tickMsg   time.Time
	statusMsg struct {
		resp api.StatusResponse
		err  error
	}
	projectsMsg struct {
		resp api.ProjectsResponse
		err  error
	}
	conflictsMsg struct {
		records []config.ConflictRecord
		err     error
	}
	doctorMsg struct {
		report doctor.Report
		err    error
	}
	// doctorFixedMsg carries the quiesce-aware `doctor --fix` outcome (spec
	// §11): err nil re-renders the re-checked report and info-toasts; a non-nil
	// err renders inline in the Doctor view (the in-screen convention), never a
	// toast.
	doctorFixedMsg struct {
		report doctor.Report
		err    error
	}
	// doctorScannedMsg carries the advisory gitleaks sweep result (spec §12):
	// the findings (possibly empty) or an error, both rendered in the Doctor
	// view's findings section.
	doctorScannedMsg struct {
		findings []views.ScanFinding
		err      error
	}
	serviceStartedMsg struct{ err error }
	// updateCheckedMsg carries CheckUpdate's verdict (spec §11): tag is the
	// newer release, "" when current. err is best-effort — the hub shows
	// nothing on a failed check, so an errored result never surfaces a banner.
	updateCheckedMsg struct {
		tag string
		err error
	}
	// updateAppliedMsg is ApplyUpdate's outcome: nil err → the banner offers R;
	// a non-nil err is toasted verbatim (sticky) and the banner returns to
	// offering a retry.
	updateAppliedMsg struct{ err error }
)

// toast is a transient status-area notification (spec §2's "status bar: …
// toasts"). The info slot expires on toastTTL; the sticky slot (error /
// action-required) never does. The TTL measures VISIBILITY, not age:
// visibleSince is when the toast first had a clear status area (no chrome
// overlay covering it) and stays zero until then, so a toast pushed under an
// open overlay cannot expire unseen behind it. Expiry is derived from
// visibleSince rather than stored alongside it, so the two can never
// disagree; the model clock (m.now) drives both the stamp and the check, so
// tests control the whole lifecycle deterministically with no wall-clock
// dependency.
type toast struct {
	text         string
	visibleSince time.Time
}

// expired reports whether an info-slot toast has exhausted its visible TTL:
// visibleSince must be set (the toast has actually been on screen) AND
// toastTTL must have elapsed since. A zero visibleSince (still hidden under
// chrome) is never expired. The sticky slot never calls this — it does not
// expire on time.
func (t *toast) expired(now time.Time) bool {
	return !t.visibleSince.IsZero() && !now.Before(t.visibleSince.Add(toastTTL))
}

// updatePhase is the update banner's lifecycle (spec §11, Task 18). It advances
// strictly forward on user action: an available release is OFFERED (banner: "U
// to update"), U opens the CONFIRM footer prompt, y starts APPLYING (the binary
// swap + daemon restart are in flight; every key but ctrl+c is frozen), and a
// clean apply lands on INSTALLED (banner: "R to restart"). A failed apply drops
// back to OFFERED so the user can retry. updateTag names the release across
// every non-idle phase; the two are kept in lockstep (idle iff updateTag == "").
type updatePhase int

const (
	updateIdle      updatePhase = iota // no update known (updateTag == "")
	updateOffered                      // a newer release is available: banner offers U
	updateConfirm                      // U pressed: the confirm prompt owns the footer
	updateApplying                     // confirmed: ApplyUpdate Cmd in flight, inputs frozen
	updateInstalled                    // applied cleanly: banner offers R (re-exec)
)

// Config is what the cli root command supplies to build the root model.
type Config struct {
	Data views.DataSource
	// StartService starts the login daemon service — the same
	// service.Controller.Start path the CLI uses — offered on the daemon-down
	// screen (spec §7). nil disables the offer.
	StartService func() error
	// Discover lists discovered-but-unenrolled memory roots; Identify
	// resolves a confirmed project path to its cross-machine identity. Both
	// are injected by the cli root command (the same composition-at-the-edge
	// pattern as the doctor runner) because provider/registry composition
	// lives outside this package's import allowlist. nil disables the
	// Projects tab's add action.
	Discover func(context.Context) ([]views.TrackCandidate, error)
	Identify func(ctx context.Context, providerName string, root views.TrackRoot, projectPath string) (provider.Identity, error)
	// LegacyDiscover enumerates the un-imported bash-era stores the Projects
	// tab's m flow offers (spec §10); LiveDirFor maps a confirmed project path
	// to the live provider dir to enroll; MigratePreflight is the once-per-
	// session chezmoi gate. All three are injected by the cli root command (the
	// same composition-at-the-edge reason as Discover/Identify — the bash-era
	// importer's helpers and the provider adapters live outside this package's
	// import allowlist). Identify above is REUSED as the migrate flow's identity
	// resolver, so enroll and migrate never disagree on a project's identity. Any
	// nil disables the m action (MigrateAvailable gates on all four closures).
	LegacyDiscover   func(context.Context) ([]views.MigrateCandidate, error)
	LiveDirFor       func(providerName, projectPath string) (string, error)
	MigratePreflight func(context.Context) error
	// Registry is the shared provider registry (buildTrackDeps().registry in
	// the cli command) — memoryfs classification needs it to resolve each
	// enrolled unit's pattern table (Task 6).
	Registry *provider.Registry
	// Settings is the loaded config.toml (cli's own loadDashboardSettings,
	// independent of buildTrackDeps — the cli root command composes it at
	// the edge, same as Registry above). buildBrowserDeps reads
	// Settings.Lint.StaleAfterDays; the edit flow reads Settings.Editor
	// (editorx.Resolve's first source).
	Settings config.Settings
	// CacheRoot is where the edit flow creates its per-session scratch
	// directories (editorx.NewScratchDir) — the cli root command passes an
	// os.UserCacheDir()-derived root, so the editor only ever sees a
	// disposable copy outside every watched provider tree (ADR 20 D2).
	// "" falls back to os.UserCacheDir() itself inside editorx.
	CacheRoot string
	// Version is the build's own version string (cli.Version), rendered in the
	// Projects fleet header (spec §9). The "vs latest" comparison joins in Task
	// 18 once the release check exists; until then this is shown plain, no
	// placeholder. Empty in tests that do not set it.
	Version string
	// CheckUpdate returns the newer release tag ("v2.1.0"), "" when the running
	// binary is current, or an error the hub SWALLOWS — the update banner is
	// best-effort and never noise (spec §11). Wired in the cli root command to
	// selfupdate.Updater.Check via the same gh composition `update` uses (ADR
	// 18); the hub never talks to gh itself. nil disables the banner entirely.
	CheckUpdate func(context.Context) (string, error)
	// ApplyUpdate runs the Check→Apply→service-restart sequence for tag — the
	// exact runUpdate pipeline minus its stdout prose (the hub reports via its
	// modal and toasts). Wired in the cli root command through the existing
	// updateEngine seam + restartServiceForUpdate with an io.Discard writer.
	// nil disables the one-key self-update.
	ApplyUpdate func(ctx context.Context, tag string) error
	// RunDoctorFix is the quiesce-aware `doctor --fix` (quiesce best-effort →
	// doctor.Fix → resume): cli's runDoctorFixWithQuiesce with stderr routed to
	// io.Discard, since the hub reports the outcome through the Doctor view, not
	// stderr. The Doctor tab's f action runs it (spec §11); nil disables that
	// action (available("doctor-fix") gates on it).
	RunDoctorFix func(context.Context) (doctor.Report, error)
	// Scan runs the advisory gitleaks sweep for one folder ("" = every enrolled
	// unit), mapping cli's scanFinding rows to the hub's views.ScanFinding. The
	// Doctor tab's s action runs it (spec §12). Advisory only — a finding never
	// joins SafetyGate. nil disables the action. Its return type is a views type
	// (like Discover's TrackCandidate) because the Doctor view renders it.
	Scan func(ctx context.Context, folder string) ([]views.ScanFinding, error)

	// ReauthGH and ProbeGHAuth are the gh-auth attention remedy, injected
	// by the cli root because gh binary resolution lives outside this package's
	// import allowlist. ReauthGH builds the interactive `gh auth login -h
	// github.com` command the Doctor tab's f hands the terminal to when gh auth is
	// the invalid piece — an *exec.Cmd so it rides the SAME tea.ExecProcess
	// suspend/resume seam the $EDITOR flow uses. ProbeGHAuth re-checks auth after
	// that handoff (a `gh auth status`). A nil ReauthGH disables the handoff (the
	// f key falls back to the standard doctor --fix); a nil ProbeGHAuth keeps the
	// handoff and only degrades its return to an honest probe-unavailable failure,
	// since there is then nothing to re-verify the token with. No token ever enters
	// this process: gh owns its own credential storage (ADR 08).
	ReauthGH    func() *exec.Cmd
	ProbeGHAuth func(context.Context) error
}

// Model is the root bubbletea model: a tab bar over four views, all refreshed
// by one shared tick.
type Model struct {
	data         views.DataSource
	startService func() error
	actions      views.TrackActions
	// migrateActions bundles the migrate flow's closures (spec §10) — the
	// migrate twin of actions above. Identify is the SAME closure both bundles
	// hold, so enroll and migrate resolve identity identically.
	migrateActions views.MigrateActions
	styles         theme.Styles
	// registry resolves an enrolled unit's provider pattern table — needed
	// to list a project folder's memories when a Screen is pushed
	// (buildBrowserDeps); nil disables the Projects tab's enter-to-browse
	// action the same way a nil Discover disables add.
	registry *provider.Registry
	// settings is the loaded config.toml (Config.Settings) — buildBrowserDeps
	// reads settings.Lint.StaleAfterDays for the pushed Browser's staleness
	// threshold; a zero-value Settings (e.g. a test that never sets Config.
	// Settings) means StaleAfterDays is 0, spec §8's own "disabled" value,
	// not config.DefaultSettings()'s 90 — a real config.toml is only ever
	// absent in a test, and production wiring always sources this through
	// LoadSettings, which itself defaults to DefaultSettings() when
	// config.toml does not exist on disk (config/settings.go).
	settings config.Settings
	// renderMarkdown is the glamour seam every pushed Screen renders
	// markdown through (buildBrowserDeps' Render, threaded onward into each
	// Reading the browser pushes) — rebuilt by withStyles whenever the
	// theme changes, see newMarkdownRenderer's own doc for why a
	// value-semantics Model can hold a closure with private mutable
	// memoization state safely.
	renderMarkdown func(markdown string, width int) string
	// version is the build's own version string (Config.Version), rendered in
	// the Projects fleet header (spec §9) — static for the process lifetime, so
	// it is seeded once in New and never re-derived.
	version string

	active tab
	width  int
	height int
	now    time.Time

	// stack is the drill-in navigation stack (spec §2): empty on every tab
	// view, one level after Projects' enter-to-browse, one more per reading
	// view a link jump pushes, deeper again once Task 14 (History) lands
	// its own push. Every mutation flows through pushScreen/popScreen/
	// replaceStackTop — see withStack's doc for why none of the three ever
	// writes into a shared backing array in place.
	stack []views.Screen

	// mouseCaptureOff, when true, holds the browser preview's cell-motion mouse
	// capture disarmed so the terminal's own drag-select works in the pane.
	// Capture is terminal-global — scoped capture is not expressible — so the
	// only honest fix is to turn it off entirely: View's arming gate reads this
	// and keeps mouseWanted false on the one frame it would otherwise arm, and
	// the renderer diffs MouseMode back to None with no explicit teardown (the
	// same path every non-browser frame already takes). The browser's m toggles
	// it (handleKey), and the footer discloses the state every frame it is off
	// (mouseCaptureDisclosure). Root-level and persistent across navigation on
	// purpose — it is a preference about the terminal, not a per-screen mode, so
	// leaving and re-entering the browser preserves it.
	mouseCaptureOff bool

	status     api.StatusResponse
	statusErr  error
	daemonDown bool

	starting   bool // a service-start Cmd is in flight
	serviceErr error

	projects  views.ProjectsView
	conflicts views.ConflictsView
	activity  views.ActivityView
	doctor    views.DoctorView

	// Root chrome (spec §14/§2/§7): the palette, help, and search overlays
	// each own the whole screen and the keyboard while open; the quit prompt
	// is an inline footer state, not a full overlay. The status area carries
	// two toast slots: toast is the info slot (transient feedback, TTL-expired)
	// and stickyToast is the error/action-required slot (a preserved scratch,
	// an unresolved failure) that persists until esc dismisses it or a newer
	// sticky replaces it. Both follow the replace-only pointer discipline — a
	// handler installs a fresh pointer (or nil), never mutating through a
	// stored one.
	paletteOpen bool
	palette     views.PaletteModel
	helpOpen    bool
	quitPrompt  bool
	toast       *toast
	stickyToast *toast

	// Edit-flow state (spec §5, editflow.go). All three pointer fields
	// follow the toast field's replace-only discipline under Model's value
	// semantics: a handler installs a fresh pointer (or nil) and never
	// mutates through a stored one — flowModal, whose textinput must change
	// per keystroke, goes copy-on-write (updateFlowModal). getenv and
	// cacheRoot are the flow's two environment seams: os.Getenv and
	// Config.CacheRoot in production, scripted in tests so a developer
	// machine's real $EDITOR can never leak into an assertion.
	editing        *editSession
	pendingCapture *pendingCapture
	flowModal      *flowModal
	getenv         func(string) string
	cacheRoot      string

	// searchOverlay is spec §7's global search. Unlike the palette (a value
	// plus a paletteOpen flag), the pointer itself is the open flag — nil is
	// closed — so the two can never disagree; forwardToSearchOverlay is the
	// one place that drops it once the overlay latches Closed.
	searchOverlay *views.SearchOverlay

	// Update banner + self-update state (spec §11, Task 18). checkUpdate/
	// applyUpdate are the cli-injected closures (nil in tests that do not wire
	// them, which simply never surfaces a banner — the best-effort posture).
	// updateCheckFired guards the at-most-once CheckUpdate, flipped true the
	// first time a successful statusMsg schedules the check. updateTag is the
	// release the banner names; updatePhase drives the U→confirm→apply→installed
	// progression; reExec latches R so launchHub re-execs onto the new binary.
	checkUpdate      func(context.Context) (string, error)
	applyUpdate      func(ctx context.Context, tag string) error
	updateCheckFired bool
	updateTag        string
	updatePhase      updatePhase
	reExec           bool

	// Doctor action seams (spec §11/§12), injected by the cli root command
	// because the doctor.Deps / gitleaks composition they need lives outside
	// this package's import allowlist (same edge-composition pattern as the
	// doctor runner). runDoctorFix is the quiesce-aware `doctor --fix`; scan is
	// the advisory gitleaks sweep. nil disables the corresponding f/s action
	// (available/paletteAvailable gate on it).
	runDoctorFix func(context.Context) (doctor.Report, error)
	scan         func(ctx context.Context, folder string) ([]views.ScanFinding, error)

	// gh-auth attention (logic in ghauth.go). authInvalid is the sticky flag the
	// status header renders loudly when any gh call classifies as auth-invalid
	// (the update-check's ErrAuthInvalid, or the doctor gh row): STICKY by design
	// — cleared only by a gh probe that succeeds (a passing doctor gh row, or the
	// re-auth handoff's re-probe), never by time, since an invalid token stays
	// invalid until a human re-auths. reauthGH/probeGHAuth are the Config closures
	// the Doctor tab's f uses to run the handoff and re-probe; nil disables the
	// handoff (f then runs the standard doctor --fix for every failure).
	authInvalid bool
	reauthGH    func() *exec.Cmd
	probeGHAuth func(context.Context) error

	quitting bool
}

// New builds the root model.
func New(cfg Config) Model {
	m := Model{
		data:         cfg.Data,
		startService: cfg.StartService,
		actions:      views.TrackActions{Discover: cfg.Discover, Identify: cfg.Identify},
		// Identify is shared with actions above by design — one resolver, so a
		// project enrolled through add and imported through migrate can never map
		// to two different identities (spec §10).
		migrateActions: views.MigrateActions{
			Preflight:  cfg.MigratePreflight,
			Discover:   cfg.LegacyDiscover,
			Identify:   cfg.Identify,
			LiveDirFor: cfg.LiveDirFor,
		},
		registry:     cfg.Registry,
		settings:     cfg.Settings,
		version:      cfg.Version,
		checkUpdate:  cfg.CheckUpdate,
		applyUpdate:  cfg.ApplyUpdate,
		runDoctorFix: cfg.RunDoctorFix,
		scan:         cfg.Scan,
		reauthGH:     cfg.ReauthGH,
		probeGHAuth:  cfg.ProbeGHAuth,
		getenv:       os.Getenv,
		cacheRoot:    cfg.CacheRoot,
		now:          time.Now(),
		projects:     views.NewProjectsView(),
		// Doctor and Activity own a scroll pane whose viewport needs its keymap
		// installed before any scroll key reaches it, so they are constructed
		// here rather than left zero-valued (NewDoctorView/NewActivityView).
		doctor:   views.NewDoctorView(),
		activity: views.NewActivityView(),
	}
	return m.withStyles(theme.Default(true), true) // dark until the terminal answers (Init requests it)
}

// ReExecRequested reports whether the hub latched a re-exec (the R key after a
// successful self-update, spec §11). launchHub reads it off the final model and
// replaces the process image with a fresh exec of the newly installed binary.
func (m Model) ReExecRequested() bool {
	return m.reExec
}

// withStyles installs styles (and the matching glamour renderer) on the
// root and propagates both to every view in one call — construction and
// every tea.BackgroundColorMsg both route through here, so a palette swap
// is one place, not five or six. The root palette is included even before
// it is ever opened (a zero-value PaletteModel's SetStyles is harmless) so
// a background swap while it happens to be open is never missed; a pushed
// Screen gets the same treatment via applyStackTheme so a browser opened
// under one background color is never left rendering the other one's
// palette or preview style after a swap.
func (m Model) withStyles(styles theme.Styles, isDark bool) Model {
	m.styles = styles
	m.projects.SetStyles(styles)
	m.conflicts.SetStyles(styles)
	m.activity.SetStyles(styles)
	m.doctor.SetStyles(styles)
	m.palette.SetStyles(styles)
	if m.searchOverlay != nil {
		m.searchOverlay.SetStyles(styles)
	}
	m.renderMarkdown = newMarkdownRenderer(styleName(isDark))
	m.applyStackTheme()
	return m
}

// styledScreen is the seam a stack screen exposes so a theme swap can reach it
// after construction: Browser/Reading/History/ConflictDetail/Insights all
// satisfy it. renderedScreen is the ADDITIONAL seam a screen that renders
// markdown exposes to also receive the theme-matched glamour renderer. The two
// are split because Insights renders no markdown — it re-themes through
// SetStyles alone, so it implements styledScreen without carrying a dead
// SetRender just to be reachable here. A markdown screen implements both.
type styledScreen interface {
	SetStyles(theme.Styles)
}

type renderedScreen interface {
	SetRender(func(md string, width int) string)
}

// applyStackTheme pushes the CURRENT styles into every stack screen, and the
// CURRENT markdown renderer into the ones that render markdown. SetStyles/
// SetRender are deliberately not part of the Screen interface (Update/View/
// Title only, so the stack's own push/pop/forward plumbing never needs to know
// a screen's concrete type) — this is the one place that steps outside that
// abstraction, the same way withStyles already does for the four tab views, and
// for the identical reason: a background-color swap must reach state that
// already exists, not just state constructed after the swap.
func (m Model) applyStackTheme() {
	for _, screen := range m.stack {
		if styled, ok := screen.(styledScreen); ok {
			styled.SetStyles(m.styles)
		}
		if rendered, ok := screen.(renderedScreen); ok {
			rendered.SetRender(m.renderMarkdown)
		}
	}
}

// styleName picks glamour's built-in style matching the terminal's
// background — the same isDark bool theme.Default already branches on, so
// the preview pane's markdown colors and the rest of the chrome's palette
// never disagree about light vs. dark.
func styleName(isDark bool) string {
	if isDark {
		return glamourstyles.DarkStyle
	}
	return glamourstyles.LightStyle
}

// newMarkdownRenderer returns a Render func for BrowserDeps (and the later
// Reading screen's identical seam): it rebuilds glamour's TermRenderer only
// when the requested width changes from the last call, since constructing
// one re-parses a full style sheet — real cost at the once-per-render rate
// a preview pane calls this at, wasted work if paid again on every render
// at a steady width. style is fixed at closure-creation time (withStyles
// creates a fresh closure exactly when isDark changes), so the closure
// itself never needs to re-derive it.
//
// The returned func closes over cachedWidth/cachedRenderer, mutable state
// private to this one closure's environment. Model holds it as a plain
// value-semantics field (renderMarkdown func(string, int) string), but
// copying a func value never duplicates what it closes over — every copy
// of the SAME closure shares the SAME cache, so the memoization survives
// across the many Model copies Update produces, exactly like a pointer
// field would, without Model itself ever needing pointer semantics.
// Reassigning the field (withStyles, on a theme change) simply starts a
// fresh closure with an empty cache, which is correct: the old cache was
// keyed to a style that no longer applies.
func newMarkdownRenderer(style string) func(markdown string, width int) string {
	var (
		cachedWidth    int
		cachedRenderer *glamour.TermRenderer
	)
	return func(markdown string, width int) string {
		if width <= 0 {
			width = 80
		}
		if cachedRenderer == nil || width != cachedWidth {
			renderer, err := glamour.NewTermRenderer(
				glamour.WithStandardStyle(style),
				glamour.WithWordWrap(width),
			)
			if err != nil {
				// style is always one of the two package constants above —
				// a construction error here means a broken glamour build,
				// not a bad input. Render raw rather than lose the preview
				// pane entirely.
				return markdown
			}
			cachedRenderer, cachedWidth = renderer, width
		}
		rendered, err := cachedRenderer.Render(markdown)
		if err != nil {
			return markdown
		}
		return rendered
	}
}

// Init loads the active view once, starts the shared tick, and requests the
// terminal's background color so the theme can pick Mocha vs Latte
// (tea.BackgroundColorMsg, handled in Update; default dark until it answers).
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.reloadCmd(), m.tickCmd(), tea.RequestBackgroundColor}
	if m.settings.Dashboard.AlternateScroll {
		// Set once for the whole session; the terminal keeps the mode across
		// the editor handoff's altscreen exit/re-entry (it is dormant outside
		// the alternate screen), and editorFinishedMsg re-asserts it in case
		// the child editor reset it. Save and set travel as one Raw payload,
		// save first: tea.Batch gives no ordering guarantee between separate
		// commands, and the save must reach the terminal before the set arms
		// the mode — reversed or split across two commands, we would risk
		// saving our own just-armed 1007 instead of whatever the terminal
		// held before the hub started.
		cmds = append(cmds, tea.Raw(saveAlternateScrollState+setAlternateScroll))
	}
	return tea.Batch(cmds...)
}

func (m Model) tickCmd() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// reloadCmd fetches the status (always — it drives the header, the fleet
// columns, and daemon-down detection) plus the active view's own data.
// Non-active views are not polled: you cannot see them, and the doctor battery
// in particular is too heavy to run every 2s unseen. When the daemon is down
// nothing but the status is worth fetching.
func (m Model) reloadCmd() tea.Cmd {
	if m.daemonDown {
		return m.statusCmd()
	}
	cmds := []tea.Cmd{m.statusCmd()}
	switch m.active {
	case tabProjects:
		cmds = append(cmds, m.projectsCmd())
	case tabConflicts:
		cmds = append(cmds, m.conflictsCmd())
	case tabDoctor:
		cmds = append(cmds, m.doctorCmd())
	case tabActivity:
		// Activity's fleet watch-trigger total is the max of the per-unit WatchTriggers
		// (Task 6.5), so it needs the projects payload alongside the status above.
		cmds = append(cmds, m.projectsCmd())
	}
	return tea.Batch(cmds...)
}

// switchCmd fetches the newly active view's data immediately on a tab switch,
// so it does not sit blank for up to a full poll interval. It always refetches
// status too — the persistent fleet header is on every view, so it must be
// fresh on arrival at any tab; fetching only the view's own data (as Conflicts
// and Doctor once did) left the header up to a poll interval stale until the
// next tick (N-3). Mirrors reloadCmd's shape; status is the same cheap UDS GET.
func (m Model) switchCmd() tea.Cmd {
	cmds := []tea.Cmd{m.statusCmd()}
	switch m.active {
	case tabProjects:
		cmds = append(cmds, m.projectsCmd())
	case tabConflicts:
		cmds = append(cmds, m.conflictsCmd())
	case tabDoctor:
		cmds = append(cmds, m.doctorCmd())
	case tabActivity:
		// Activity's fleet watch-trigger total is the max of the per-unit WatchTriggers
		// (Task 6.5), so fetch the projects payload on arrival too, not just status.
		cmds = append(cmds, m.projectsCmd())
	}
	return tea.Batch(cmds...)
}

func (m Model) statusCmd() tea.Cmd {
	data := m.data
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), views.RequestTimeout)
		defer cancel()
		resp, err := data.Status(ctx)
		return statusMsg{resp: resp, err: err}
	}
}

func (m Model) projectsCmd() tea.Cmd {
	data := m.data
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), views.RequestTimeout)
		defer cancel()
		resp, err := data.Projects(ctx)
		return projectsMsg{resp: resp, err: err}
	}
}

func (m Model) conflictsCmd() tea.Cmd {
	data := m.data
	return func() tea.Msg {
		records, err := data.Conflicts()
		return conflictsMsg{records: records, err: err}
	}
}

func (m Model) doctorCmd() tea.Cmd {
	data := m.data
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), views.RequestTimeout)
		defer cancel()
		report, err := data.Doctor(ctx)
		return doctorMsg{report: report, err: err}
	}
}

// doctorFixCmd runs the injected quiesce-aware `doctor --fix` off the UI thread
// (spec §11). Like applyUpdateCmd, it uses a background context rather than the
// 2s-poll RequestTimeout: the fix holds a daemon quiesce and rewrites the
// checkout's git config, work whose own steps carry their bounds (the quiesce
// TTL, gitx's operations), and a hard deadline mid-surgery is worse than
// letting the idempotent repair finish. The Doctor view shows "fixing…"
// meanwhile, so the UI stays responsive without a timer. Reached only when
// runDoctorFix is wired (available("doctor-fix")).
func (m Model) doctorFixCmd() tea.Cmd {
	run := m.runDoctorFix
	return func() tea.Msg {
		report, err := run(context.Background())
		return doctorFixedMsg{report: report, err: err}
	}
}

// doctorScanCmd runs the injected advisory gitleaks sweep over every enrolled
// unit (folder "") off the UI thread (spec §12). Background context for the
// same reason the cli `scan` command leaves it to the process signal context:
// a sweep shells gitleaks over potentially many files and must not be capped at
// the 2s-poll bound; the child carries its own WaitDelay. The Doctor view shows
// "scanning…" meanwhile. Reached only when scan is wired (available("scan")).
func (m Model) doctorScanCmd() tea.Cmd {
	scan := m.scan
	return func() tea.Msg {
		findings, err := scan(context.Background(), "")
		return doctorScannedMsg{findings: findings, err: err}
	}
}

func (m Model) startServiceCmd() tea.Cmd {
	start := m.startService
	return func() tea.Msg {
		if start == nil {
			return serviceStartedMsg{err: errors.New("no service controller available")}
		}
		return serviceStartedMsg{err: start()}
	}
}

// checkUpdateCmd runs the injected release check off the poll and adapts its
// (tag, err) into an updateCheckedMsg. Best-effort: a nil closure yields no Cmd,
// and the message handler swallows an errored result (the banner is never
// noise, spec §11). The 10s RequestTimeout bounds the gh round trip the check
// makes; a timeout simply yields no banner.
func (m Model) checkUpdateCmd() tea.Cmd {
	check := m.checkUpdate
	if check == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), views.RequestTimeout)
		defer cancel()
		tag, err := check(ctx)
		return updateCheckedMsg{tag: tag, err: err}
	}
}

// applyUpdateCmd runs the injected self-update for tag and reports the outcome.
// It uses a background context, NOT the 2s poll's RequestTimeout: the download/
// verify/sanity/restart steps carry their own timeouts (internal/selfupdate,
// restartServiceForUpdate), and a hard deadline mid-swap is worse than letting
// the atomic replace finish. A nil closure — a misconfiguration, since the cli
// wires check and apply together — reports an error rather than nil-calling.
func (m Model) applyUpdateCmd(tag string) tea.Cmd {
	apply := m.applyUpdate
	return func() tea.Msg {
		if apply == nil {
			return updateAppliedMsg{err: errors.New("self-update is not available in this build")}
		}
		return updateAppliedMsg{err: apply(context.Background(), tag)}
	}
}

// Update is the root reducer. It owns the shared status, tab switching, and the
// daemon-down/service-start flow; view-specific data and keys are forwarded to
// the owning view.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resizeProjects()
		return m, nil

	case tea.BackgroundColorMsg:
		return m.withStyles(theme.Default(msg.IsDark()), msg.IsDark()), nil

	case tickMsg:
		m.now = time.Time(msg)
		m.advanceToasts()
		cmds := []tea.Cmd{m.reloadCmd(), m.tickCmd()}
		if _, ok := m.stackTop(); ok {
			var stackCmd tea.Cmd
			m, stackCmd = m.forwardToStack(views.RefreshMsg{Now: m.now})
			cmds = append(cmds, stackCmd)
		}
		return m, tea.Batch(cmds...)

	case statusMsg:
		m.status, m.statusErr = msg.resp, msg.err
		// A daemon we restart ourselves mid-update is EXPECTED to be transiently
		// unreachable; that must not flip the alarming daemon-down screen (which
		// the applying gate would also freeze). Hold the current view — the
		// installing banner — until the apply resolves.
		if m.updatePhase != updateApplying {
			m.daemonDown = errors.Is(msg.err, api.ErrDaemonNotRunning)
		}
		// Activity scrolls its snapshot: feed the fresh status through so a
		// materially changed status resets its scroll to the top while an
		// unchanged 2s poll leaves the reader where they are (OnData's contract).
		// Done here, in the message handler, so the reset persists — View runs on
		// a value copy and cannot.
		m.activity.OnData(m.status, m.statusErr, m.projects.Units, m.now)
		// The capture wait resolves off the same poll that carries every
		// other daemon fact — no dedicated timer (editflow.go).
		m.checkPendingCapture()
		// The release check fires at most once per session, only after the
		// FIRST successful status (spec §11): a status error never triggers it,
		// and updateCheckFired makes every later success a no-op.
		if msg.err == nil && !m.updateCheckFired && m.checkUpdate != nil {
			m.updateCheckFired = true
			return m, m.checkUpdateCmd()
		}
		return m, nil

	case projectsMsg:
		if msg.err != nil {
			m.projects.SetLoadErr(msg.err)
		} else {
			m.projects.SetUnits(msg.resp.Units)
		}
		// Activity's fleet trigger count comes from the unit list, so a projects
		// refresh feeds its scroll pane too (OnData) — the same reset-on-real-
		// change discipline the status poll uses.
		m.activity.OnData(m.status, m.statusErr, m.projects.Units, m.now)
		return m, nil

	case conflictsMsg:
		m.conflicts.Set(msg.records, msg.err)
		return m, nil

	case doctorMsg:
		m.doctor.Set(msg.report, msg.err)
		// The gh row is a live probe: a passing row clears the sticky attention,
		// an auth-invalid one arms it — so visiting the Doctor tab both surfaces
		// and resolves the state, through the same ghx classifier the update-check
		// feeds (ghauth.go).
		m.applyGHAuthSignal(msg.report)
		return m, nil

	case doctorFixedMsg:
		// The re-checked report (or the failure, rendered inline) lands in the
		// view; only a clean fix earns the info toast — immediate feedback with
		// the re-rendered battery right there, so INFO, not sticky (spec §11).
		m.doctor.SetFixResult(msg.report, msg.err)
		// A standard fix re-runs the battery, so its fresh gh row updates the
		// attention the same way a plain doctor poll does.
		m.applyGHAuthSignal(msg.report)
		if msg.err == nil {
			m.pushToast("fix applied — re-checked")
		}
		return m, nil

	case doctorScannedMsg:
		m.doctor.SetScanResult(msg.findings, msg.err)
		return m, nil

	case views.SyncResultMsg:
		m.projects.OnSyncResult(msg)
		return m, m.projectsCmd() // reflect the post-sync fleet state

	case views.UntrackResultMsg:
		m.projects.OnUntrackResult(msg)
		return m, tea.Batch(m.projectsCmd(), m.statusCmd())

	case views.DiscoverMsg:
		m.projects.OnDiscover(msg)
		return m, nil

	case views.IdentifyMsg:
		return m, m.projects.OnIdentify(msg, m.data)

	case views.TrackResultMsg:
		next := m.projects.OnTrackResult(msg, m.data)
		if msg.Err != nil {
			// A hard failure abandoned the rest of the multi-select queue
			// (OnTrackResult named what landed before it); refresh the fleet to
			// reflect whatever DID enroll before the rejection.
			return m, m.projectsCmd()
		}
		if next != nil {
			// The queue has more candidates: advance to the next one WITHOUT the
			// whole-fleet sync yet, so a multi-project enrollment fires one sync at
			// the end (below), not one per candidate.
			return m, next
		}
		// The queue drained cleanly. Track's HTTP reply returns BEFORE the daemon's
		// post-admin cycle (the same lesson track.go's syncAfterTrack records): an
		// explicit whole-fleet sync is what makes the enrollments' first mirror-in
		// visible here rather than landing silently later.
		return m, tea.Batch(m.projectsCmd(), m.statusCmd(), views.SyncCmd(m.data, ""))

	case views.MigratePreflightMsg:
		if msg.Err != nil {
			// The chezmoi gate refused (spec §10's point-of-no-return guard). The
			// view is parked on a stage waiting for this Cmd and cannot self-reset,
			// so the root clears the flow and surfaces the refusal verbatim —
			// sticky, because it is an action-required failure the user must
			// reconcile (adjudicate the orphans) before a retry can pass.
			m.projects.ResetMigrate()
			m.pushStickyToast(msg.Err.Error())
			return m, nil
		}
		return m, m.projects.OnMigratePreflightOK(m.migrateActions)

	case views.MigrateDiscoverMsg:
		// Empty/errored/loaded are all decided inside the view (it sets its own
		// notice and resets on a dead end), exactly like OnDiscover for add.
		m.projects.OnMigrateDiscover(msg)
		return m, nil

	case views.MigrateIdentifyMsg:
		return m, m.projects.OnMigrateIdentify(msg, m.data)

	case views.MigrateResultMsg:
		m.projects.OnMigrateResult(msg)
		if msg.Err != nil {
			// The daemon rejected the import; nothing enrolled, so refresh the
			// fleet only and surface the failure (sticky — action-required).
			m.pushStickyToast(fmt.Sprintf("migrate failed: %v", msg.Err))
			return m, m.projectsCmd()
		}
		// Mirror the add flow's post-track idiom: the seed + enrollment landed, so
		// an explicit whole-fleet sync makes the imported memory's first mirror-in
		// visible now rather than on the next poll (spec §10).
		m.pushToast(msg.Toast())
		return m, tea.Batch(m.projectsCmd(), m.statusCmd(), views.SyncCmd(m.data, ""))

	case views.OpenFolderMsg:
		// The one place a bare (Folder, Units) request becomes an actual
		// Screen: ProjectsView cannot build a *views.Browser itself (it has
		// none of Registry/Styles/memoryfs/glamour), so it emits this and
		// the root — the only place with all of those — constructs it and
		// pushes it, same as any other PushScreenMsg.
		m = m.pushScreen(views.NewBrowser(m.buildBrowserDeps(msg.Folder, msg.Units)))
		return m, nil

	case views.OpenConflictMsg:
		// The conflicts-tab twin of OpenFolderMsg: ConflictsView cannot build a
		// *views.ConflictDetail itself (it holds none of Registry/Units/
		// memoryfs/glamour), so it emits the selected record and the root — the
		// only place with all of those — resolves the record's path to a live
		// memory and pushes the detail screen.
		m = m.pushScreen(views.NewConflictDetail(m.buildConflictDetailDeps(msg.Record)))
		return m, nil

	case views.PushScreenMsg:
		// A pushed screen that needs an initial async load (the History screen's
		// version fetch) exposes InitCmd; issuing it here — right after the push,
		// in the same Update — rather than batching it into the message that
		// asked for the push keeps the screen on the stack before its own first
		// result can arrive (screen.go's initScreen seam).
		m = m.pushScreen(msg.Screen)
		return m, initScreenCmd(msg.Screen)

	case views.PopScreenMsg:
		m = m.popScreen()
		return m, nil

	case views.MouseCaptureToggleMsg:
		// The browser matched m in its mode dispatch (never in the filter, where m
		// stays a typable query letter) and asked the root to flip its terminal-
		// global mouse-capture veto. The flip lives HERE, not in the browser,
		// because View's arming gate — the only reader of mouseCaptureOff — is root
		// state the browser cannot reach. Both directions ride one message: off
		// disarms the preview's cell-motion capture so native drag-select works
		// across the browser, on re-arms it. The disclosure (mouseCaptureDisclosure)
		// then shows every frame it is off, from this same flag.
		m.mouseCaptureOff = !m.mouseCaptureOff
		return m, nil

	case views.HistoryVersionsMsg, views.HistoryBlobMsg, views.InsightsDataMsg:
		// The History screen's async fetches (and the browser's folder-wide
		// deleted scan), plus the Insights screen's one folder-wide history
		// fetch, resolve as Cmds whose results the root forwards to the stack
		// top, exactly like the tick's RefreshMsg — the top screen matches each
		// on its own keys (Folder/RepoPath, or Folder for insights) and drops any
		// not its own, so a fetch that outlived its screen never lands anywhere
		// it does not belong. InsightsDataMsg is a distinct type from
		// HistoryVersionsMsg precisely so the browser's folder-wide deleted scan
		// and the insights fetch — both path "" — can never adopt each other's
		// result.
		m, cmd := m.forwardToStack(msg)
		return m, cmd

	case views.ToastMsg:
		// The generic screen→root notice channel (screen.go): the reading
		// view's dangling-link refusal today, any pushed screen's local
		// notice tomorrow.
		m.pushToast(msg.Text)
		return m, nil

	case views.CopyPathMsg:
		// spec §4's y (copy provider-file path), in two halves: the toast
		// prints the absolute path verbatim — the guaranteed affordance,
		// visible in every terminal — and tea.SetClipboard issues the OSC52
		// clipboard write as best effort alongside it. OSC52 support varies
		// by terminal and the write has no delivery ack, which is why the
		// toast is unconditional rather than a fallback: the path on screen
		// is the one outcome the binding can promise.
		m.pushToast("path: " + msg.Path)
		return m, tea.SetClipboard(msg.Path)

	case views.CopyMemoryMsg:
		// The feature-full companion to copy-path's y: the browser's y (or the
		// reading view's Y) copies the memory's RAW markdown body to the system
		// clipboard via OSC52 — an app-level copy that carries over SSH/tmux/WSL2,
		// the remedy for the browser's mouse mode suppressing native drag-select.
		// Same best-effort split as CopyPathMsg: the toast names the memory (the
		// affordance every terminal shows), tea.SetClipboard issues the OSC52 write
		// alongside it (support varies, no delivery ack), so the toast — not the
		// silent escape — is what the binding promises.
		m.pushToast(fmt.Sprintf("copied %q to clipboard", msg.Label))
		return m, tea.SetClipboard(msg.Body)

	case views.EditRequestMsg:
		// The four flow-request messages follow the CopyPathMsg pattern:
		// views emit, the root — the only holder of editor settings, the
		// scratch cache root, ExecProcess, and the toast/modal chrome —
		// runs the flow (editflow.go).
		return m.startEditFlow(msg.Memory)

	case views.NewRequestMsg:
		return m.startNewFlow(msg)

	case views.RenameRequestMsg:
		return m.startRenameFlow(msg.Memory)

	case views.DeleteRequestMsg:
		return m.startDeleteFlow(msg.Memory), nil

	case views.RestoreRequestMsg:
		// spec §6's restore: land a historical blob the History screen already
		// fetched as a new version. Same emit-only split as the four edit-flow
		// requests — the root owns the write path, the class gate, and the
		// capture wait (editflow.go).
		return m.startRestoreFlow(msg), nil

	case editorFinishedMsg:
		updated := m.finishEdit(msg)
		// Re-assert 1007 on the editor's exit: the in-terminal $EDITOR handoff
		// hands the terminal to a child that may reset private modes, so the mode
		// is set again here. Keyed off the exit itself, not the land outcome —
		// idempotent when the child left it alone (4 bytes), and harmless on the
		// GUI path, which never released the terminal. reassertAlternateScrollCmd
		// is the shared seam the gh re-auth handoff also returns through (set only,
		// no paired save — see its doc).
		return updated, updated.reassertAlternateScrollCmd()

	case views.PaletteChoiceMsg:
		return m, m.dispatch(msg.ID)

	case views.SearchDebounceMsg, views.SearchResultsMsg:
		// The search overlay's own async plumbing (debounce ticks, completed
		// queries) surfaces at the root because the overlay is chrome, not a
		// stacked Screen. After a dismissal an in-flight message lands here
		// with the overlay already gone — dropped by the forward's nil
		// check, exactly like a stale generation inside the overlay.
		return m.forwardToSearchOverlay(msg)

	case views.SearchChoiceMsg:
		// spec §7's enter: the chosen memory opens its reading view. The
		// overlay latched Closed when it emitted this, so the key forward
		// already dropped it and the pushed screen owns the keyboard next.
		return m.openSearchChoice(msg.Memory), nil

	case serviceStartedMsg:
		m.starting = false
		m.serviceErr = msg.err
		return m, m.statusCmd() // re-poll to see whether it came up

	case updateCheckedMsg:
		// A check that failed because the gh token is dead is exactly the silent
		// gap this task closes: today the banner simply never appears (the user's
		// live symptom). Classify the error at this one seam and arm the sticky
		// attention instead of dropping it. This rides the existing at-most-once
		// check cadence — no new timer, and no retry, since an invalid token stays
		// invalid until a human re-auths (the daemon must never storm gh).
		if errors.Is(msg.err, ghx.ErrAuthInvalid) {
			m.authInvalid = true
		}
		// Best-effort (spec §11): a failed check or an "already current" (empty
		// tag) result surfaces nothing — no banner, no toast. Only a newer tag
		// opens the offer, and only from idle (the check fires once, so this
		// can never clobber a later phase).
		if msg.err == nil && msg.tag != "" && m.updatePhase == updateIdle {
			m.updateTag = msg.tag
			m.updatePhase = updateOffered
		}
		return m, nil

	case updateAppliedMsg:
		if msg.err != nil {
			// A failed apply is an unresolved failure the user must act on
			// (ErrBrewManaged/ErrDevBuild are self-remediating and surface at
			// CHECK time, so they never reach here; a download/verify/restart
			// failure lands verbatim). Sticky, never a 5s info toast — then the
			// banner returns to offering a retry.
			m.pushStickyToast(msg.err.Error())
			m.updatePhase = updateOffered
			return m, nil
		}
		m.updatePhase = updateInstalled
		return m, nil

	case ghAuthFinishedMsg:
		// The interactive `gh auth login` child exited and the terminal is ours
		// again. Re-assert 1007 exactly like the editor return (ADR 21). gh's own
		// exit code is not authoritative — a user may abandon the device flow yet
		// have fixed it in the browser, or vice versa — so a clean exit fires the
		// re-probe (the sole truth about whether the token is live). A launch
		// failure keeps the attention and names the manual path: there was nothing
		// to re-probe.
		reassert := m.reassertAlternateScrollCmd()
		if msg.err != nil {
			m.pushStickyToast("gh auth login did not run: " + msg.err.Error() + " — run `gh auth login -h github.com` manually")
			return m, reassert
		}
		return m, tea.Batch(reassert, m.probeGHAuthCmd())

	case ghAuthProbedMsg:
		if msg.err == nil {
			// The token is live again — clear the sticky attention and confirm.
			m.authInvalid = false
			m.pushToast("gh authentication restored")
			return m, nil
		}
		// Still invalid (or the probe itself failed): the attention stays and the
		// toast names the one command that fixes it, so the user is never stranded.
		m.pushStickyToast("gh auth still invalid — run `gh auth login -h github.com`")
		return m, nil

	case tea.MouseWheelMsg, tea.MouseClickMsg:
		// Mouse reporting is enabled only while a browser preview is on screen
		// (View's MouseMode gate), so a mouse event means that browser is the stack
		// top; rebase it to that screen's own coordinates (translateStackMouse) and
		// forward it there. With no stack — the mode was never on, but a stray event
		// could still arrive — it is a harmless no-op rather than something the tabs
		// try to interpret (native selection stays theirs).
		if _, ok := m.stackTop(); ok {
			return m.forwardToStack(m.translateStackMouse(msg))
		}
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		m.quitting = true
		return m, tea.Quit
	}

	// While an update is applying the binary is being swapped and the daemon
	// restarted under us; every key but ctrl+c is ignored until ApplyUpdate
	// resolves (spec §11). Checked above the daemon-down gate so the expected
	// mid-restart unreachability cannot pull in that screen's s/q keys either.
	if m.updatePhase == updateApplying {
		return m, nil
	}

	// The daemon-down screen owns the keyboard until the daemon answers again.
	if m.daemonDown {
		switch msg.String() {
		case "q":
			m.quitting = true
			return m, tea.Quit
		case "s":
			if m.startService != nil && !m.starting {
				m.starting = true
				m.serviceErr = nil
				return m, m.startServiceCmd()
			}
		}
		return m, nil
	}

	// The help overlay owns the keyboard while open: any key closes it
	// (spec §14) — it has no other state to react to.
	if m.helpOpen {
		m.helpOpen = false
		return m, nil
	}

	// The palette owns the keyboard while open. Its availability predicate is
	// re-bound from the live model on every forwarded keypress, not just at
	// open time: paletteAvailable is a bound method value on the root's
	// value-semantics Model, so the copy captured once when the palette
	// opened would otherwise freeze at whatever was true that instant —
	// harmless today (nothing paletteAvailable reads changes while the
	// palette is open) but fragile the moment availability ever depends on
	// state that does.
	if m.paletteOpen {
		m.palette.SetAvailable(m.paletteAvailable)
		next, cmd := m.palette.Update(msg)
		m.palette = next
		if next.Closed {
			m.paletteOpen = false
		}
		return m, cmd
	}

	// The search overlay owns the keyboard while open (spec §7): every key
	// is the query's or the result cursor's, and esc closes the overlay and
	// nothing else — reaching neither the quit prompt below nor any global
	// binding.
	if m.searchOverlay != nil {
		return m.forwardToSearchOverlay(msg)
	}

	// The update confirm owns the keyboard while open (spec §11): y/Y applies
	// the pending release, n/N/esc back out to the offer. Like the quit prompt
	// below, any other key is inert. It can only be entered on a bare tab — U
	// reaches the global dispatch only with no chrome and no stack open — so no
	// overlay or modal ever coexists with it, and refuseFlowStart refuses a
	// raced-in flow request the same way it does for the chrome above.
	if m.updatePhase == updateConfirm {
		switch {
		case keybinding.Matches(msg, views.DashboardKeys.ConfirmDecision):
			switch msg.String() {
			case "y", "Y":
				m.updatePhase = updateApplying
				return m, m.applyUpdateCmd(m.updateTag)
			default: // n / N
				m.updatePhase = updateOffered
				return m, nil
			}
		case keybinding.Matches(msg, views.DashboardKeys.Cancel):
			m.updatePhase = updateOffered
			return m, nil
		}
		return m, nil
	}

	// The quit prompt owns the keyboard while open (spec §2): y/Y actually
	// quits; n/N/esc dismiss it and the model keeps running. Any other key
	// — including q — matches neither ConfirmDecision (y/Y/n/N) nor Cancel
	// (esc) and falls through to this block's own final return: q is inert
	// while the prompt is open, answered only by y/n/esc.
	if m.quitPrompt {
		switch {
		case keybinding.Matches(msg, views.DashboardKeys.ConfirmDecision):
			switch msg.String() {
			case "y", "Y":
				m.quitting = true
				return m, tea.Quit
			default:
				m.quitPrompt = false
				return m, nil
			}
		case keybinding.Matches(msg, views.DashboardKeys.Cancel):
			m.quitPrompt = false
			return m, nil
		}
		return m, nil
	}

	// An open flow modal (the edit flow's name input / rename input /
	// delete confirm, editflow.go) owns the keyboard before the stack it
	// floats over: esc must abort the MODAL — consumed there — never fall
	// through to pop the screen or open the quit prompt, and y/n must
	// answer the confirm, never reach a screen binding that shares the key.
	if m.flowModal != nil {
		return m.updateFlowModal(msg)
	}

	// A pushed Screen owns the keyboard exactly the way a Projects modal
	// does (the two states are mutually exclusive in practice: reaching
	// enter-to-browse or a screen's own drill-in keys requires NOT being
	// inside an untrack confirm or the add flow) — checked before the
	// modal test, the quiesce gate, and the tab-switch/esc globals, so esc
	// backs out of a screen instead of opening the quit prompt, and a
	// screen's own keys are never shadowed by a global binding that
	// happens to share the same key. See ScopeBrowser's registry rows
	// (actions.go) and stackFooterRows for the matching footer/help
	// surface of this same priority.
	if _, ok := m.stackTop(); ok {
		return m.forwardToStack(msg)
	}

	// A modal confirm on the Projects view consumes keys before the globals,
	// so a `y`/`n` answer is never mistaken for a tab jump, and so the
	// quiesce gate below never fires on a key a text input would otherwise
	// have swallowed as a literal character.
	if m.active == tabProjects && m.projects.ModalOpen() {
		return m, m.projects.Update(msg, m.data, m.actions, m.migrateActions)
	}

	// A Mutates action reachable from here is refused locally while
	// quiesced (spec §15) — before it ever reaches ProjectsView.Update, so
	// e.g. pressing u never even opens the untrack confirm if the answer is
	// already no.
	if m.quiesceGate(msg) {
		return m, nil
	}

	switch {
	case keybinding.Matches(msg, views.DashboardKeys.TabSwitch):
		// The binding is the membership gate; the concrete key picks the
		// direction. "1"–"4" are the only single-rune members left after the
		// named cases, so the default is exact, not a catch-all.
		switch msg.String() {
		case "tab", "right", "l":
			m.active = (m.active + 1) % tabCount
		case "shift+tab", "left", "h":
			m.active = (m.active + tabCount - 1) % tabCount
		default:
			m.active = tab(msg.String()[0] - '1')
		}
		return m, m.switchCmd()
	case msg.String() == "esc":
		// esc consumes internal state first, the rule used everywhere else
		// (flow modal, screen stack, chrome all consume esc before it can
		// reach here): a sticky error toast is dismissed before esc escalates
		// to the quit prompt. q still quits directly regardless — a sticky
		// informs, it never traps.
		if m.stickyToast != nil {
			m.stickyToast = nil
			// The sticky's header line cleared, freeing a tab body row — grow the
			// Projects table back into it (resizeProjects' doc).
			m.resizeProjects()
			return m, nil
		}
		m.quitPrompt = true
		return m, nil
	case m.updatePhase == updateInstalled && msg.String() == "R":
		// The installed banner's "R to restart" (spec §11): latch the re-exec
		// and quit so launchHub replaces the process image with the freshly
		// installed binary. Reached only on a bare tab — a pushed screen
		// consumes keys first, so a History screen's own R still means restore
		// there — and only in the installed phase, so R is otherwise inert.
		m.reExec = true
		m.quitting = true
		return m, tea.Quit
	}

	// Every other global action (quit, ctrl+k, ?, /) shares the one dispatch
	// a palette choice also runs through, so a direct keypress and picking
	// the same action from the palette can never behave differently.
	for _, candidate := range actions.ForScope(actions.ScopeGlobal) {
		if keybinding.Matches(msg, actions.Binding(candidate)) {
			return m, m.dispatch(candidate.ID)
		}
	}

	// Everything else belongs to the active view: Projects' table nav + s/u/a,
	// the Conflicts list's cursor + enter-to-detail, or the Doctor tab's
	// r/f/s actions. All route here, after the globals, exactly as they do on
	// the Projects tab today.
	switch m.active {
	case tabProjects:
		return m, m.projects.Update(msg, m.data, m.actions, m.migrateActions)
	case tabConflicts:
		return m, m.conflicts.Update(msg)
	case tabActivity:
		// Activity's only keys are scroll; a non-scroll key is inert (Scroll
		// reports the miss, which Activity has nothing to fall through to). Reached
		// only on a bare Activity tab — every overlay/prompt/stack owned the key
		// first (handleKey's precedence chain) — so the pane never contends with a
		// modal for ctrl+d/G. Scroll mutates m.activity in place; returning m
		// carries the advanced offset back.
		m.activity.Scroll(msg, m.status, m.statusErr, m.projects.Units, m.now, m.width, m.tabBodyHeight())
		return m, nil
	case tabDoctor:
		// Scroll first (ctrl+d/u, pgup/pgdown, g/G bound the battery); on a miss
		// the key belongs to the tab's own r/f/s (handleDoctorKey). Same bare-tab
		// gating as Activity above.
		if m.doctor.Scroll(msg, m.width, m.tabBodyHeight()) {
			return m, nil
		}
		return m.handleDoctorKey(msg)
	}
	return m, nil
}

// handleDoctorKey routes the Doctor tab's own keys (spec §11/§12). r re-runs
// the read-only battery on demand — matched directly (no runner), reusing the
// existing doctorCmd, so like the Projects/Conflicts cursor rows it never lists
// in the palette. f (fix) and s (scan) dispatch through the registry so a
// direct key and a palette choice run the identical runner and honor the same
// availability/quiesce gates (spec §14). Reached only while the Doctor tab is
// active and nothing else owns the keyboard (handleKey's precedence chain);
// a quiesced f is already refused upstream by quiesceGate, so it never reaches
// dispatch here.
func (m Model) handleDoctorKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	for _, candidate := range actions.ForScope(actions.ScopeDoctor) {
		if !keybinding.Matches(msg, actions.Binding(candidate)) {
			continue
		}
		if candidate.ID == "doctor-rerun" {
			return m, m.doctorCmd()
		}
		return m, m.dispatch(candidate.ID)
	}
	return m, nil
}

// quiesced reports whether a quiesce hold is currently active.
func (m Model) quiesced() bool {
	return m.status.QuiescedUntil != nil && m.status.QuiescedUntil.After(m.now)
}

// quiesceGate refuses — before it reaches ProjectsView.Update or any runner
// — a bare-tab keypress that maps to a Mutates action while the daemon is
// quiesced, toasting the same refusal dispatch uses for the palette path
// (refuseIfQuiesced is the one function both call). It is scope-aware: an
// action's key is only a real refusal when its scope is the surface the
// key would actually reach — anywhere else the key was already a no-op,
// and quiescing must not start toasting about something that was never
// going to happen anyway (scopeActiveAtRoot).
func (m *Model) quiesceGate(msg tea.KeyPressMsg) bool {
	if !m.quiesced() {
		return false
	}
	for _, candidate := range actions.Registry() {
		if !candidate.Mutates || !keybinding.Matches(msg, actions.Binding(candidate)) {
			continue
		}
		if !m.scopeActiveAtRoot(candidate.Scope) {
			continue // dead key on this surface; nothing to refuse
		}
		return m.refuseIfQuiesced(candidate)
	}
	return false
}

// scopeActiveAtRoot reports whether scope's keys are live on the current
// BARE-TAB surface. quiesceGate only ever runs with an empty navigation
// stack — handleKey forwards every key to a pushed screen first — so the
// stack scopes (Browser/Reading/History) are never live here: their
// mutation keys (the edit flow's e/n/r/d, all Mutates) pressed on a bare
// tab are dead keys, and their own quiesce refusal happens where the keys
// are real, in the flow-request handlers (refuseFlowStart, editflow.go).
func (m *Model) scopeActiveAtRoot(scope actions.Scope) bool {
	switch scope {
	case actions.ScopeGlobal:
		return true
	case actions.ScopeProjects:
		return m.active == tabProjects
	case actions.ScopeDoctor:
		return m.active == tabDoctor
	case actions.ScopeActivity:
		return m.active == tabActivity
	case actions.ScopeConflicts:
		return m.active == tabConflicts
	default:
		return false
	}
}

// refuseIfQuiesced toasts and refuses action if it Mutates and the daemon is
// currently quiesced. It decides the refusal for every registry-dispatched
// mutation — quiesceGate (a direct keypress) and dispatch (a palette
// choice) alike — while the flow-request handlers apply the identical
// verdict through refuseFlowStart; all three share toastQuiesceRefusal
// (editflow.go), so the wording cannot diverge between any of them.
func (m *Model) refuseIfQuiesced(action actions.Action) bool {
	if !action.Mutates || !m.quiesced() {
		return false
	}
	m.toastQuiesceRefusal()
	return true
}

// pushToast surfaces text in the INFO slot of the status area for toastTTL of
// on-screen visibility. Text is sanitized to a single line (sanitizeToastText)
// before storing — see its doc for why. visibleSince is stamped now only if
// the status area is currently clear; pushed under chrome it stays zero and
// the tick handler stamps it once the chrome closes, so the TTL measures
// visibility rather than age. pointer receiver: every caller already holds
// an addressable *Model mid-mutation (dispatch, quiesceGate) and folds this
// in as one field write, not a value threaded back out and reassigned.
func (m *Model) pushToast(text string) {
	m.toast = &toast{text: sanitizeToastText(text), visibleSince: m.visibilityStamp()}
	// The info slot now occupies a header line it may not have before, shrinking
	// the tab body budget — reflow the Projects table so its own frame stays
	// exact (resizeProjects' doc).
	m.resizeProjects()
}

// pushStickyToast replaces the STICKY (error / action-required) slot; the
// newest sticky wins. Text is sanitized to a single line (sanitizeToastText)
// before storing, the same as pushToast. It carries the same visibility
// stamp as pushToast so a future visibility-gated behaviour reads a
// truthful value, but the sticky slot never expires on time regardless — it
// clears only on esc (handleKey) or when a newer sticky replaces it.
func (m *Model) pushStickyToast(text string) {
	m.stickyToast = &toast{text: sanitizeToastText(text), visibleSince: m.visibilityStamp()}
	// A newly populated sticky slot grows the header the same way the info slot
	// does, so the Projects table reflows to the shrunk budget (resizeProjects' doc).
	m.resizeProjects()
}

// sanitizeToastText collapses any embedded line breaks in text to single
// spaces, enforcing the toast slots' single-line-per-toast contract:
// headerBlockHeight counts exactly one rendered line per populated slot, and
// frameChromeLines' monotonicity guarantee (stackBodyHeight/tabBodyHeight
// never reserving less than the old two-toast-maximum constant) depends on
// that count being true. An un-sanitized multi-line payload — e.g. a raw
// err.Error() with embedded newlines passed straight to pushToast — would
// make one toast occupy more than its one accounted-for header line,
// inflating the reservation and reintroducing the clamp/overflow risk the
// monotonicity argument rules out for a well-behaved toast. "\r\n" collapses
// to one space, not two, so a Windows-style line ending does not double-count.
func sanitizeToastText(text string) string {
	return strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(text)
}

// visibilityStamp is m.now when the status area is currently clear, else the
// zero time — the one place the push-time stamp rule for both slots lives.
func (m *Model) visibilityStamp() time.Time {
	if m.chromeCoversStatus() {
		return time.Time{}
	}
	return m.now
}

// chromeCoversStatus reports whether an open chrome overlay is covering the
// status area, so a toast's visible TTL must not run (it is not actually on
// screen). The set is exactly the View branches that render something other
// than the status-header region — the daemon-down screen, the help overlay,
// the palette, and the search overlay each own the whole body and never
// render toastLine — so this predicate and View's branch list state the same
// truth once.
func (m Model) chromeCoversStatus() bool {
	return m.daemonDown || m.helpOpen || m.paletteOpen || m.searchOverlay != nil
}

// advanceToasts runs the visibility-measured toast lifecycle on each tick: it
// stamps a slot's visibleSince the first tick the status area is clear (so a
// toast pushed under chrome starts its TTL only once the chrome closes), then
// expires the INFO slot once its visible TTL elapses. The sticky slot is
// stamped the same way but never expires on time. Both writes replace the
// slot pointer with an updated copy rather than mutating through it — the
// Model-wide replace-only pointer discipline, so a retained earlier Model
// copy never sees its toast rewritten underneath it.
func (m *Model) advanceToasts() {
	covered := m.chromeCoversStatus()
	if m.stickyToast != nil && m.stickyToast.visibleSince.IsZero() && !covered {
		m.stickyToast = &toast{text: m.stickyToast.text, visibleSince: m.now}
	}
	if m.toast == nil {
		return
	}
	if m.toast.visibleSince.IsZero() && !covered {
		m.toast = &toast{text: m.toast.text, visibleSince: m.now}
	}
	if m.toast.expired(m.now) {
		m.toast = nil
		// The info slot's header line just cleared, freeing a tab body row — grow
		// the Projects table back into it (resizeProjects' doc), the reflow in the
		// clearing direction.
		m.resizeProjects()
	}
}

// toastLine renders the populated toast slots for the status area: the sticky
// (error) line first, then the info line, joined the same way the header
// joins the toast block. The info line carries a belt-and-suspenders expiry
// check so a View between ticks never shows it past its visible TTL; the
// sticky line renders whenever present (it has no TTL). Empty when both slots
// are clear.
func (m Model) toastLine() string {
	var lines []string
	if m.stickyToast != nil {
		lines = append(lines, m.styles.ToastSticky.Render(m.stickyToast.text))
	}
	if m.toast != nil && !m.toast.expired(m.now) {
		lines = append(lines, m.styles.Toast.Render(m.toast.text))
	}
	return strings.Join(lines, "\n\n")
}

// withStack replaces m.stack with stack. It is the SOLE place that assigns
// the field — pushScreen, popScreen, and replaceStackTop each build their
// desired new slice value with its own fresh clone (never a re-slice or an
// in-place element write against the existing backing array) and hand it
// here. Model has value semantics everywhere else; without this rule,
// growing, shrinking, or replacing the stack's top could reuse spare
// backing-array capacity — or overwrite a still-valid index — that an
// earlier Model copy (a retained pre-push/pre-pop snapshot, most concretely
// in a test) still reads through, silently changing what that
// supposedly-immutable older value reports back. Stack depth is at most a
// handful of drill-in levels, so paying one small allocation per mutation is
// immaterial next to the correctness it buys.
func (m Model) withStack(stack []views.Screen) Model {
	m.stack = stack
	return m
}

// pushScreen appends screen to the navigation stack.
func (m Model) pushScreen(screen views.Screen) Model {
	return m.withStack(append(slices.Clone(m.stack), screen))
}

// popScreen removes the top of the navigation stack, or is a no-op on an
// already-empty stack — esc with nothing left to back out of must never
// panic on m.stack[:len-1].
func (m Model) popScreen() Model {
	if len(m.stack) == 0 {
		return m
	}
	return m.withStack(slices.Clone(m.stack[:len(m.stack)-1]))
}

// stackTop reports the screen currently on top of the navigation stack, if
// any.
func (m Model) stackTop() (views.Screen, bool) {
	if len(m.stack) == 0 {
		return nil, false
	}
	return m.stack[len(m.stack)-1], true
}

// conflictDetailHistoryAvailable answers available("conflictdetail-history"):
// the stack top is a conflict detail whose record resolved to an enrolled unit
// (mapped or enrolled-but-deleted). The detail owns the resolution — it already
// walked the folder in NewConflictDetail — so this only reaches for the concrete
// type and asks, keeping the footer's struck/lit state exactly the h key's own
// inert/live behavior (openHistory self-gates on the same bit).
func (m *Model) conflictDetailHistoryAvailable() bool {
	top, ok := m.stackTop()
	if !ok {
		return false
	}
	detail, ok := top.(*views.ConflictDetail)
	if !ok {
		return false
	}
	return detail.HistoryAvailable()
}

// replaceStackTop swaps the top of the stack for screen. A Screen's Update
// usually returns itself — the common case is a pure in-place mutation, so
// this is a harmless rewrite of the same value — but the interface
// documents "usually itself," not always, so a genuine self-replacement
// goes through the same clone-before-store discipline as every other stack
// change rather than a bare index assignment.
func (m Model) replaceStackTop(screen views.Screen) Model {
	if len(m.stack) == 0 {
		return m
	}
	stack := slices.Clone(m.stack)
	stack[len(stack)-1] = screen
	return m.withStack(stack)
}

// initScreen is the optional seam a pushed screen exposes to run an initial
// async load once, right after it lands on the stack — the History screen's
// version fetch (its blobs then load on demand). A screen whose first frame
// comes from a cheap synchronous read instead (Browser, Reading) simply does
// not implement it, and initScreenCmd returns nil for it.
type initScreen interface {
	InitCmd() tea.Cmd
}

// initScreenCmd returns a freshly pushed screen's initial-load Cmd, or nil if
// it needs none — the one place the PushScreenMsg handler steps outside the
// Screen interface, the same way stackScope and applyStackTheme do.
func initScreenCmd(screen views.Screen) tea.Cmd {
	if init, ok := screen.(initScreen); ok {
		return init.InitCmd()
	}
	return nil
}

// forwardToStack sends msg to the top of the navigation stack and installs
// its replacement screen, returning the Cmd it produced. Shared by the
// tick's RefreshMsg forward and handleKey's key forward so both honor the
// exact same Screen contract (screen.go) in one place: the root never
// inspects what msg was, only what Update handed back.
func (m Model) forwardToStack(msg tea.Msg) (Model, tea.Cmd) {
	top, ok := m.stackTop()
	if !ok {
		return m, nil
	}
	next, cmd := top.Update(msg)
	return m.replaceStackTop(next), cmd
}

// headerBlockHeight is the status header block's ACTUAL height for this
// frame: the status header line (the update banner included, inline —
// updateBanner's doc) plus however many toast lines are populated right now —
// 0 extra when both slots are clear, 1 when exactly one is populated, 3 when
// both are (the sticky and info lines plus the blank toastLine joins between
// them). Shared by mousePrefixLines (mouse Y translation) and frameChromeLines
// (the body budget) so the offset that places the mouse cursor and the
// reservation that sizes the body read the same header the same way, rather
// than one of them assuming a fixed worst case the other measures for real.
func (m Model) headerBlockHeight() int {
	header := m.statusHeader()
	if toastLine := m.toastLine(); toastLine != "" {
		header = strings.Join([]string{header, toastLine}, "\n\n")
	}
	return lipgloss.Height(header)
}

// mousePrefixLines is how many terminal rows the root composes above a stack
// screen's first line: the header block, one blank line from the "\n\n" join,
// the breadcrumb, and its blank join line. Built from the same strings View
// joins (headerBlockHeight/breadcrumb) so the offset and the pixels cannot
// drift; mouse reporting is only ever armed on the stack-top browser frame
// (View's MouseMode gate), so mouse events are generated under the default
// branch's shape — header, breadcrumb, screen, footer. An event already in
// flight when an overlay opens can still land one message cycle late,
// translated against this same default-branch prefix — benign, worst case an
// invisible cursor move under the overlay.
func (m Model) mousePrefixLines() int {
	return m.headerBlockHeight() + 1 + lipgloss.Height(m.breadcrumb()) + 1
}

// translateStackMouse rebases a mouse event from terminal-absolute rows to
// screen-local rows before it is forwarded down the stack — the seam the browser's
// click-to-select contract needs (updateMouseClick's row mapping), left unbuilt
// when hover-scroll landed. X passes through untouched: the root adds no
// horizontal chrome (overPreview's documented invariant). The wheel's Y is unused
// today (updateMouseWheel routes on X alone), but is rebased anyway so every
// stack-bound mouse event reaches a screen in one coordinate space.
func (m Model) translateStackMouse(msg tea.Msg) tea.Msg {
	offset := m.mousePrefixLines()
	switch mouse := msg.(type) {
	case tea.MouseClickMsg:
		mouse.Y -= offset
		return mouse
	case tea.MouseWheelMsg:
		mouse.Y -= offset
		return mouse
	}
	return msg
}

// forwardToSearchOverlay routes msg to the open search overlay — keys while
// it owns the keyboard, plus its own SearchDebounceMsg/SearchResultsMsg
// plumbing — re-binding Collect from the LIVE model first (the palette's
// SetAvailable discipline, for the identical reason: a closure bound once
// at open time would freeze that instant's fleet snapshot forever, because
// Model has value semantics, and the whole point of Collect is enumerating
// the CURRENT fleet). A nil overlay drops the message: a debounce tick or
// query result that outlived its overlay is stale by definition. Once the
// overlay latches Closed (esc, or enter emitting its choice) this is the
// one place that drops it.
func (m Model) forwardToSearchOverlay(msg tea.Msg) (Model, tea.Cmd) {
	if m.searchOverlay == nil {
		return m, nil
	}
	m.searchOverlay.SetCollect(m.collectFleetMemories)
	cmd := m.searchOverlay.Update(msg)
	if m.searchOverlay.Closed {
		m.searchOverlay = nil
	}
	return m, cmd
}

// collectFleetMemories enumerates every tracked project's memories fresh —
// the search overlay's Collect seam (spec §7: results across every tracked
// project's provider dirs). One memoryfs.List over the ENTIRE latest fleet
// snapshot: List resolves each unit through the provider registry and
// returns entries sorted (Folder, RepoPath), i.e. already grouped by
// folder. A bound method value freezes this model copy's snapshot, which is
// why forwardToSearchOverlay re-binds it on every forwarded message.
func (m Model) collectFleetMemories() ([]memoryfs.Memory, error) {
	return memoryfs.List(m.registry, m.projects.Units)
}

// openSearchChoice pushes the chosen memory's reading view (spec §7's
// enter, root half). The links Index is built lazily — only now, only over
// the chosen memory's own folder: search itself never needs link graphs,
// and indexing every tracked folder up front on `/` would read every body
// in the fleet before the first keystroke. Folder scope matches the Index
// the browser would have handed the same Reading (links resolve within a
// project, spec §4). The synchronous List/BuildIndex here is construction
// I/O under the same documented local-read exception as OpenFolderMsg's
// NewBrowser above (screen.go's Screen.Update doc). A List failure degrades
// to a nil Index — every link renders dangling, the posture Reading already
// documents for a missing index — rather than refusing to open a memory
// whose body is perfectly readable; the degrade is SAID, though: a toast
// names why links won't resolve, instead of leaving the user to discover it
// link by dangling link.
func (m Model) openSearchChoice(memory memoryfs.Memory) Model {
	var index *links.Index
	if memories, err := memoryfs.List(m.registry, unitsForFolder(m.projects.Units, memory.Folder)); err == nil {
		index = links.BuildIndex(memories, memoryfs.ReadBody)
	} else {
		m.pushToast(fmt.Sprintf("link index unavailable: %v", err))
	}
	return m.pushScreen(views.NewReading(views.ReadingDeps{
		Memory:   memory,
		Index:    index,
		ReadBody: memoryfs.ReadBody,
		Render:   m.renderMarkdown,
		Styles:   m.styles,
		Data:     m.data,
		Now:      m.now,
	}))
}

// unitsForFolder filters the fleet snapshot down to folder's own units —
// the same subset openFolderCmd (views/projects.go) computes when a browser
// is pushed, duplicated as a four-line loop rather than exported across the
// views boundary just for this.
func unitsForFolder(units []api.UnitInfo, folder string) []api.UnitInfo {
	matching := make([]api.UnitInfo, 0, len(units))
	for _, unit := range units {
		if unit.Folder == folder {
			matching = append(matching, unit)
		}
	}
	return matching
}

// stackScope maps a concrete Screen type to the actions.Scope its footer
// hints and quiesce/available checks belong to — the one place a footer or
// help render needs a stack screen's scope, since the Screen interface
// itself stays exactly Update/View/Title. A later task's History screen
// adds its own case here as it lands, the same way activeScope grows a
// case per tab.
func (m Model) stackScope(screen views.Screen) actions.Scope {
	switch screen.(type) {
	case *views.Browser:
		return actions.ScopeBrowser
	case *views.Reading:
		return actions.ScopeReading
	case *views.History:
		return actions.ScopeHistory
	case *views.Insights:
		return actions.ScopeInsights
	case *views.ConflictDetail:
		return actions.ScopeConflictDetail
	default:
		return actions.ScopeGlobal
	}
}

// breadcrumb renders the navigation stack's header line in place of the tab
// bar: "Projects ▸ acme" one level deep (spec §2's own separator), extending
// with one more " ▸ " segment per further level a later task's screens push
// (Reading, History). The tab you drilled in from stays the first segment,
// so it is always clear which tab popping every level lands back on.
// Title() is each Screen's own single breadcrumb segment (screen.go).
func (m Model) breadcrumb() string {
	segments := make([]string, 0, len(m.stack)+1)
	segments = append(segments, m.active.title())
	for _, screen := range m.stack {
		segments = append(segments, screen.Title())
	}
	return m.styles.Title.Render(strings.Join(segments, " ▸ "))
}

// frameChromeLines computes a composed frame's ACTUAL non-body chrome height
// for this render: the header block at its current toast occupancy (not a
// two-toast-blind maximum), one nav line (navLine — the breadcrumb for a
// pushed screen, the tab bar for a tab body), the footer, and the blank-line
// separator the "\n\n" join costs between each of the four blocks joined —
// header, navLine, body, footer, three separators. Reserving the header's
// actual height rather than its worst case is what keeps the composed frame
// at exactly m.height on every frame regardless of toast occupancy: when a
// toast arrives the header grows by that many lines and this reservation
// grows with it, so the body budget shrinks by the same amount and the total
// never changes — the footer never leaves the terminal's last row. Shared by
// stackBodyHeight and tabBodyHeight: the two root layouts differ only in
// which nav line replaces the other, both exactly one line today.
func (m Model) frameChromeLines(navLine string) int {
	return m.headerBlockHeight() + 1 + lipgloss.Height(navLine) + 1 + 1 + lipgloss.Height(m.footer())
}

// bodyHeightFloor clamps a computed body height to a 3-line minimum — a
// terminal small enough to hit this is already overflowing regardless, and a
// readable few lines beats an empty pane.
func bodyHeightFloor(height int) int {
	if height > 3 {
		return height
	}
	return 3
}

// stackBodyHeight computes a pushed screen's content budget: terminal height
// minus the frame's actual current chrome — the header at its real toast
// occupancy, the breadcrumb, and the footer (frameChromeLines). This mirrors
// the tab-level budget's own shape (tabBodyHeight); ProjectsView.SetSize
// reserves separately for the Projects tab's own extra chrome (its own doc).
func (m Model) stackBodyHeight() int {
	return bodyHeightFloor(m.height - m.frameChromeLines(m.breadcrumb()))
}

// tabBodyHeight is the vertical budget for a tab body: the same
// frameChromeLines reservation as stackBodyHeight, measured against the tab
// bar instead of the breadcrumb — the one nav line that differs between the
// two root layouts. Kept a named helper of its own rather than a bare call so
// the two layouts' budgets read as the distinct concepts they are, each
// measuring its own nav line rather than assuming they coincide — even
// though both cost exactly one line today.
func (m Model) tabBodyHeight() int {
	return bodyHeightFloor(m.height - m.frameChromeLines(m.tabBar()))
}

// resizeProjects re-sizes the Projects table to the tab body's CURRENT budget
// (tabBodyHeight, which measures the status header at its actual toast
// occupancy). The Projects table is the one tab body that holds persistent
// bubbles viewport state, so — unlike Conflicts/Activity/Doctor, which re-derive
// their window from tabBodyHeight on every value-receiver View — it must be
// re-sized explicitly at each seam that moves the budget: the window resize and
// every toast push/expiry. Without the toast-transition calls the table would
// stay sized for whatever occupancy the last WindowSizeMsg saw, leaving a blank
// band above the footer whenever fewer toasts are up than it reserved for (the
// live-hub defect this closes), or clipping a real row when more are.
func (m *Model) resizeProjects() {
	m.projects.SetSize(m.width, m.tabBodyHeight())
}

// fitAndFillHeight clamps body to at most exact lines, keeping the FIRST
// exact (the alt-screen renders from the top and never scrolls, so the top
// rows are exactly the visible ones — a bottom-anchored keep would discard
// what the user actually sees), and pads a SHORTER body with trailing blank
// lines up to exactly exact lines. Line counting is ANSI-safe by
// construction: an SGR escape never contains a newline, so splitting on "\n"
// counts display rows regardless of the colour/attribute codes woven through
// them.
//
// The pad half is what pins the footer to the terminal's last row on every
// frame, not just the ones whose body happens to fill its budget: a screen
// like the browser preview deliberately sizes itself to its own content
// (renderPreviewPane's doc) rather than space-filling out to the height it
// was handed, so without padding here a short preview left the composed
// frame — header + breadcrumb/tabBar + body + footer — shorter than the
// terminal, and the footer floated mid-screen with blank rows beneath it
// instead of anchored to the bottom (the live-hub defect this fixes). Filling
// every call site to the identical budget stackBodyHeight/tabBodyHeight
// already compute keeps the total frame height invariant regardless of how
// short or tall the content underneath turns out to be.
//
// The clamp half is unchanged from the prior fitHeight: the root's defense-
// in-depth backstop, not the primary bound — every pushed screen and tab body
// already sizes its content to the height View hands it. This is what still
// holds the frame when one of them regresses or a future screen forgets, so
// a single over-tall body can never again shove the option keys past the
// fold.
func fitAndFillHeight(body string, exact int) string {
	if exact < 0 {
		exact = 0
	}
	lines := strings.Split(body, "\n")
	if len(lines) > exact {
		return strings.Join(lines[:exact], "\n")
	}
	if missing := exact - len(lines); missing > 0 {
		return body + strings.Repeat("\n", missing)
	}
	return body
}

// stackFooterRow is one advertised key on a pushed screen's footer.
// disabled marks a row whose action cannot run right now — it renders
// visibly struck rather than vanishing (stackFooterLine), the crush-style
// honesty rule: the user must see that e exists and learn why it is dead
// (the toast on pressing it says), never wonder whether the key exists at
// all. Contrast footerBindings, which HIDES an unavailable row: at tab
// level "unavailable" means not-built-yet (search until Task 15) or
// unwired (add without closures) — surfaces where a struck row would
// advertise something that may never work on this build — while a stack
// row's gates (editor resolution, selection class, an active handoff,
// quiesce) are all live, momentary state worth explaining.
type stackFooterRow struct {
	binding  keybinding.Binding
	disabled bool
}

// stackFooterRows lists the top stack screen's own scope, in registry
// order, every row with a real key — available or not (see stackFooterRow).
// A Mutates row additionally greys while the daemon is quiesced (spec §15's
// grey-out, matching the refusal its request handler would answer with).
// Unlike footerBindings, ScopeGlobal is deliberately NOT included: a pushed
// Screen intercepts every key before the global dispatch loop ever runs
// (handleKey — the same modal-priority rule ProjectsView's own confirm/add
// flow already establishes), so a global hint here would name a key the
// active surface actually ignores.
func (m Model) stackFooterRows() []stackFooterRow {
	top, ok := m.stackTop()
	if !ok {
		return nil
	}
	scope := m.stackScope(top)
	// While the browser's preview pane holds keyboard focus, the list-scope keys
	// are all swallowed by the browser's focused block, so the footer swaps to the
	// focused set — exactly the keys that do something in that mode — the same way
	// the Projects modal footers swap to their owned-input binding set. This
	// mirrors that idiom rather than inventing a new one: a state the active
	// surface owns selects a different, complete binding set for the footer.
	// PreviewFocused() is the browser's own effective-focus gate (previewFocused
	// AND the pane on screen), so a focus stranded off-screen by a narrow resize
	// keeps the normal browser footer, matching where the keys actually route.
	if browser, isBrowser := top.(*views.Browser); isBrowser && browser.PreviewFocused() {
		scope = actions.ScopeBrowserPreviewFocused
	}
	var rows []stackFooterRow
	for _, action := range actions.Registry() {
		if len(action.Keys) == 0 || action.Scope != scope {
			continue
		}
		disabled := !m.available(action.ID) || (action.Mutates && m.quiesced())
		rows = append(rows, stackFooterRow{binding: actions.Binding(action), disabled: disabled})
	}
	return rows
}

// stackFooterLine renders stackFooterRows in views.HelpLine's exact "key
// desc · key desc" shape, but styled per row: enabled rows dim (the whole
// footer's usual treatment), disabled rows dim + struck through — the
// visible half of the availability gate. Segments are styled individually
// because lipgloss terminates each Render with a reset, so one outer
// Dim.Render over a line containing an inner strikethrough render would
// lose the dim for everything after the inner reset.
func (m Model) stackFooterLine() string {
	rows := m.stackFooterRows()
	segments := make([]string, len(rows))
	for i, row := range rows {
		help := row.binding.Help()
		segment := help.Key + " " + help.Desc
		if row.disabled {
			segments[i] = m.styles.Dim.Strikethrough(true).Render(segment)
		} else {
			segments[i] = m.styles.Dim.Render(segment)
		}
	}
	return strings.Join(segments, m.styles.Dim.Render(" · "))
}

// buildBrowserDeps assembles a views.BrowserDeps for folder from the root's
// own composition-at-the-edge dependencies (spec §6's registry/memoryfs
// seam), binding List/ReadBody to units so the browser and its tests never
// touch Registry or the fleet snapshot directly (BrowserDeps' own doc in
// browser.go). Now seeds the pushed Browser's clock with m.now at push
// time only — it is a plain time.Time, not a func() time.Time closure, on
// purpose: a closure captured here would close over THIS call's Model copy
// forever (Model has value semantics), never observing a later tick's
// advanced clock. Every render after the first reads the Browser's own
// stored field instead, kept current by the live Now RefreshMsg carries on
// every tick (screen.go's RefreshMsg doc) — this seed only covers the
// window between push and the first tick.
func (m Model) buildBrowserDeps(folder string, units []api.UnitInfo) views.BrowserDeps {
	registry := m.registry
	return views.BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   folder,
		Styles:   m.styles,
		Now:      m.now,
		ReadBody: memoryfs.ReadBody,
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
		// StaleAfterDays reads the configured lint.stale_after_days setting
		// (config/settings.go, default 90) — m.settings is whatever the cli
		// root command's own loadDashboardSettings loaded, never a value
		// this package invents.
		StaleAfterDays: m.settings.Lint.StaleAfterDays,
		Render:         m.renderMarkdown,
		// Data is the read-only version surface (spec §6) the browser threads
		// into every History screen it opens and uses for the deleted-memories
		// scan. m.data is the full DataSource; it satisfies the narrower
		// HistoryDataSource the browser and History screen actually consume.
		Data: m.data,
	}
}

// buildConflictDetailDeps assembles a views.ConflictDetailDeps from the root's
// own composition-at-the-edge dependencies, the same registry/memoryfs/glamour
// seams buildBrowserDeps threads. Units is the whole fleet snapshot — the
// detail resolves the record's repo-relative path down to the one enrolled
// unit that still carries it (or renders the untracked notice when none does).
// Data and Now are the same history seam and clock every history-capable screen
// receives, threaded so the detail's h can push a live History screen.
func (m Model) buildConflictDetailDeps(record config.ConflictRecord) views.ConflictDetailDeps {
	return views.ConflictDetailDeps{
		Record:   record,
		Units:    m.projects.Units,
		Registry: m.registry,
		ReadBody: memoryfs.ReadBody,
		Render:   m.renderMarkdown,
		Styles:   m.styles,
		Data:     m.data,
		Now:      m.now,
	}
}

// findAction looks up a registry row by ID. Registry() is a handful of
// entries, so a linear scan costs nothing next to a keypress or a render.
func findAction(id string) (actions.Action, bool) {
	for _, a := range actions.Registry() {
		if a.ID == id {
			return a, true
		}
	}
	return actions.Action{}, false
}

// dispatch is the single entry point a matched key press and a chosen
// palette row both funnel through (spec §14): it resolves the action's
// metadata, applies the identical quiesce refusal a direct keypress gets,
// and otherwise runs the action's registered runner. help and search are
// handled directly here rather than through runners(): each is a pure
// synchronous Model mutation (flipping helpOpen; constructing the overlay)
// with no async work to schedule, so neither has a tea.Cmd to produce —
// forcing either into runners()'s func() tea.Cmd shape just to return a
// constant nil is exactly the dead-weight-result smell the unparam linter
// exists to catch. An unknown id, or one with no registered runner
// (available's gate), does nothing — the registry stays honest about what
// actually works right now.
func (m *Model) dispatch(id string) tea.Cmd {
	action, ok := findAction(id)
	if !ok || !m.available(id) {
		return nil
	}
	if m.refuseIfQuiesced(action) {
		return nil
	}
	// A palette choice reaches dispatch as a Cmd message (PaletteChoiceMsg),
	// which — unlike a keystroke, which handleKey routes to an open flow modal
	// before any global — can land AFTER a flow-request message opened a
	// modal. Refuse a chrome-opening choice here so no message path can layer
	// help, the search overlay, or the palette OVER a modal that owns the
	// screen (handleKey checks all three before the flow modal, so the chrome
	// would starve it); the key path to these is already closed by handleKey's
	// modal priority. The set is exactly dispatch's chrome openers: the help
	// and search special-cases below, and the open-palette runner.
	if m.flowModal != nil && (id == "help" || id == "search" || id == "open-palette" || id == "update-agent-brain") {
		m.pushToast("a prompt is already open — finish or esc it first")
		return nil
	}
	if id == "help" {
		m.helpOpen = true
		return nil
	}
	if id == "search" {
		m.openSearchOverlay()
		return nil
	}
	if id == "update-agent-brain" {
		// A pure state flip into the confirm prompt (no Cmd), like help/search
		// above — kept out of runners() so it needs no dead nil-returning
		// runner. available() has already gated it on the offered phase.
		m.updatePhase = updateConfirm
		return nil
	}
	runner, ok := m.runners()[id]
	if !ok {
		return nil
	}
	return runner()
}

// openSearchOverlay constructs and installs the global search overlay (spec
// §7). Styles and the Collect binding are both snapshots of THIS model
// copy — forwardToSearchOverlay re-binds Collect on every subsequent
// message and withStyles re-propagates styles on a theme swap, so neither
// snapshot outlives its accuracy.
func (m *Model) openSearchOverlay() {
	m.searchOverlay = views.NewSearchOverlay(views.SearchOverlayDeps{
		Collect:  m.collectFleetMemories,
		ReadBody: memoryfs.ReadBody,
		Styles:   m.styles,
	})
}

// available reports whether action id can actually do something right now.
// It drives the footer (footerBindings, stackFooterRows) and dispatch's
// own pre-run gate — NOT the palette, which uses the stricter
// paletteAvailable below. switch-tabs and select are structural navigation
// handled by dedicated key-routing paths in handleKey rather than a runner
// — dispatch never actually reaches either — so they are unconditionally
// available here purely to keep advertising their footer/help hints (spec
// §2); help and search have no wiring precondition and are never hidden
// either, and both ARE genuinely dispatchable (dispatch special-cases each
// directly). open-browser and the browser-*/reading-* rows are the
// identical shape one level down the stack: ProjectsView.Update,
// Browser.updateKey, and Reading.updateKey match their bindings directly
// (see actions.go's row comments), so dispatch never reaches any of them
// either, and they stay unconditionally available so the Projects, browser,
// and reading footers keep naming them — except the edit-flow rows, whose
// availability is the live gate flowAvailable computes (editor resolves ∧
// fact-class selection ∧ no active handoff, editflow.go): false renders
// them struck in the stack footer, never hidden. add-project additionally
// needs both track closures wired (the existing AddAvailable contract);
// every other action is available exactly when it has a registered runner —
// the mechanism that kept the search row invisible until its overlay
// landed, and does the same for any future row declared ahead of its
// runner.
func (m *Model) available(id string) bool {
	switch id {
	case "switch-tabs", "select", "help", "search",
		"open-browser", "browser-read", "browser-order", "browser-filter",
		"browser-history", "browser-show-deleted", "browser-insights", "browser-copy",
		"browser-scroll-preview", "browser-focus-preview", "mouse-capture-toggle", "browser-back",
		"browser-preview-list", "browser-preview-scroll", "browser-preview-half-page",
		"browser-preview-page", "browser-preview-ends", "browser-preview-copy",
		"browser-preview-mouse-capture",
		"reading-scroll", "reading-links", "reading-follow", "reading-backlinks", "reading-copy-path", "reading-copy-body",
		"reading-history", "reading-back",
		"history-view", "history-diff", "history-diff-older", "history-back",
		"insights-back",
		"conflicts-select", "conflicts-open", "conflictdetail-back",
		"doctor-rerun", "doctor-scroll", "activity-scroll":
		// doctor-rerun is a read-only refetch matched directly by
		// handleDoctorKey (no runner), always offerable so the Doctor footer
		// keeps naming it — the same shape as select/conflicts-select.
		// doctor-scroll/activity-scroll are the bounded tab bodies' scroll keys,
		// routed straight to the pane in the tab-key dispatch (like
		// browser-scroll-preview): always available so the footer keeps naming
		// them, their effect situational (only a body that overflows scrolls).
		// The browser-preview-* rows are the preview-focused footer set, matched
		// directly by the browser's focused block (list/scroll/half-page/page/ends/
		// copy/mouse-capture): always available so that swapped footer renders every
		// key lit, its whole reason for existing being to name the keys that DO work
		// in that mode.
		// mouse-capture-toggle (and its focused-scope twin browser-preview-mouse-
		// capture) is the same shape — matched directly by the browser's mode
		// dispatch, which emits MouseCaptureToggleMsg for the root to act on, never a
		// runner — so it is always offerable, its effect situational (only a shown
		// preview has a captured mouse to hand back), and renders lit rather than
		// struck.
		return true
	case "scan":
		// Advisory gitleaks sweep — live exactly when its runner is wired.
		return m.scan != nil
	case "doctor-fix":
		// The quiesce-aware `doctor --fix`: wired AND the battery is in a
		// fixable state (report failed with a fixable row) — the same gate
		// CanFix computes, so the footer, palette, and the f key agree.
		return m.runDoctorFix != nil && m.doctor.CanFix()
	case "browser-edit", "browser-new", "browser-rename", "browser-delete", "reading-edit", "conflictdetail-edit":
		return m.flowAvailable(id)
	case "history-restore":
		return m.historyRestoreAvailable()
	case "conflictdetail-read":
		// Unlike the browser/reading read rows — always live because their
		// screen always has a memory — a conflict detail can render a record
		// whose path no longer maps to a live file, so read is honest only when
		// one resolved. flowTarget reports exactly that for the detail on top.
		_, ok := m.flowTarget()
		return ok
	case "conflictdetail-history":
		// Wider than read: history is honest for a since-deleted file too — an
		// enrolled-but-deleted record still owns a version chain to browse and
		// restore from — so this asks the detail's own resolution (mapped OR
		// enrolled-but-deleted) rather than flowTarget's mapped-only report.
		return m.conflictDetailHistoryAvailable()
	case "add-project":
		return m.actions.AddAvailable()
	case "migrate":
		// Live only when every migrate closure is wired (the same shape as
		// add-project's AddAvailable): a build without them keeps the m row inert
		// in the footer and palette rather than advertising a dead key.
		return m.migrateActions.MigrateAvailable()
	case "update-agent-brain":
		// Live only while a newer release is offered — not idle (nothing to do)
		// and not after install (already applied; U must not re-open the
		// confirm). This refines the brief's "updateTag != """ to the phase, so
		// the installed state never re-advertises U.
		return m.updatePhase == updateOffered
	default:
		_, ok := m.runners()[id]
		return ok
	}
}

// paletteAvailable reports whether choosing action id from the palette would
// actually run something. It is stricter than available: switch-tabs and
// select are dead ends here even though the footer keeps advertising them,
// because dispatch never reaches a runner for either (handleKey consumes
// switch-tabs before the generic dispatch loop ever runs, and select has no
// dispatch path at all) — choosing either from the palette used to close it
// and silently do nothing. This is deliberately independent of available
// (not a filtered wrapper around it): deriving it straight from the runners
// map, plus the cases dispatch special-cases outside that map (help,
// search), means a future registry row added without a runner is
// automatically absent from the palette even if some later change ever gave
// available itself a new unconditional-true case — there is no
// hand-maintained exclusion list here to fall out of sync.
func (m *Model) paletteAvailable(id string) bool {
	switch id {
	case "help", "search":
		return true
	case "add-project":
		return m.actions.AddAvailable()
	case "migrate":
		// Same gate as available(): the palette lists m only on a build that
		// wired the migrate closures, never as a dead row.
		return m.migrateActions.MigrateAvailable()
	case "update-agent-brain":
		// A dispatch special-case (no runner), like help/search — offered only
		// while a newer release is available, so the palette lists it exactly
		// then and never as a dead row.
		return m.updatePhase == updateOffered
	case "scan":
		return m.scan != nil
	case "doctor-fix":
		// Mirrors available's gate: the palette lists fix only when it would
		// actually run — wired and the battery is fixable — never as a dead row
		// on a clean report. doctor-rerun is deliberately absent (no runner),
		// so the default below keeps it out of the palette entirely.
		return m.runDoctorFix != nil && m.doctor.CanFix()
	default:
		_, ok := m.runners()[id]
		return ok
	}
}

// runners maps a registered action ID to the Cmd-producing function that
// performs it, rebuilt fresh on every call (never cached on Model) so each
// closure closes over the CURRENT model state — the selected unit, the
// current tab — rather than a stale snapshot from some earlier point.
// sync-project/untrack/add-project replay the exact keypress
// ProjectsView.Update already handles (switching to the Projects tab first,
// so a palette choice made from elsewhere lands somewhere the user can
// actually see it happen) — the palette and a direct keypress run through
// the identical view-level code, not a second copy of "what s/u/a do." help
// is deliberately absent: dispatch handles it directly (see above) since it
// never produces a Cmd.
func (m *Model) runners() map[string]func() tea.Cmd {
	return map[string]func() tea.Cmd{
		"sync-project": func() tea.Cmd {
			m.active = tabProjects
			return m.projects.Update(replayKey('s'), m.data, m.actions, m.migrateActions)
		},
		"untrack": func() tea.Cmd {
			m.active = tabProjects
			return m.projects.Update(replayKey('u'), m.data, m.actions, m.migrateActions)
		},
		"add-project": func() tea.Cmd {
			m.active = tabProjects
			return m.projects.Update(replayKey('a'), m.data, m.actions, m.migrateActions)
		},
		// migrate replays m the same way add-project replays a: switch to the
		// Projects tab (so a palette choice lands where the user can watch the
		// flow) and drive ProjectsView.Update, which opens the spec §10 importer.
		"migrate": func() tea.Cmd {
			m.active = tabProjects
			return m.projects.Update(replayKey('m'), m.data, m.actions, m.migrateActions)
		},
		"sync-fleet": func() tea.Cmd {
			return views.SyncCmd(m.data, "")
		},
		// doctor-fix/scan switch to the Doctor tab first (a palette choice made
		// from elsewhere lands where the user can watch it), latch the in-flight
		// indicator, and schedule the work — the same shape sync-project uses,
		// so a direct f/s key and a palette choice run the identical path.
		"doctor-fix": func() tea.Cmd {
			m.active = tabDoctor
			// When gh auth is the invalid piece, f re-authenticates through the
			// interactive handoff the header directs the user to (ghauth.go) — the
			// quiesce-aware `doctor --fix` cannot re-mint a token, since GitHub's
			// device/browser flow needs the human. Every other fixable failure runs
			// the standard fix.
			if m.authInvalid && m.reauthGH != nil {
				return ghReauthCmd(m.reauthGH())
			}
			m.doctor.SetFixing()
			return m.doctorFixCmd()
		},
		"scan": func() tea.Cmd {
			m.active = tabDoctor
			m.doctor.SetScanning()
			return m.doctorScanCmd()
		},
		"open-palette": func() tea.Cmd {
			m.paletteOpen = true
			palette, cmd := views.NewPaletteModel(m.styles, m.paletteAvailable, m.quiesced())
			m.palette = palette
			return cmd
		},
		"quit": func() tea.Cmd {
			m.quitting = true
			return tea.Quit
		},
	}
}

// replayKey builds the KeyPressMsg a real keyboard produces for a single
// printable-rune shortcut, so a palette-invoked Projects action runs through
// the exact same ProjectsView.Update path a direct keypress does — that view
// is unaware of, and indifferent to, whether its caller was the keyboard or
// the palette.
func replayKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

// View composes the tab bar and the active view, the navigation stack's
// breadcrumb and top screen once one is pushed, or the full-screen
// daemon-down notice. It runs in the alternate screen buffer so it never
// scrolls the terminal's scrollback.
func (m Model) View() tea.View {
	var body string
	// Armed below for exactly one surface — a browser whose preview pane is on
	// screen — and left None for every daemon-down/help/palette/search-overlay
	// frame and every tab and non-browser screen, so native terminal selection
	// stays intact everywhere the wheel/click are not wanted (spec §3).
	mouseWanted := false
	switch {
	case m.daemonDown:
		body = m.daemonDownView()
	case m.helpOpen:
		body = views.NewHelpModel(m.styles).View()
	case m.paletteOpen:
		body = m.palette.View()
	case m.searchOverlay != nil:
		// Full-screen like the palette; unlike it, the overlay budgets its
		// result rows against the real terminal height, so it gets both
		// dimensions (searchoverlay.go's View doc states the floor).
		body = m.searchOverlay.View(m.width, m.height)
	default:
		header := m.statusHeader()
		if toastLine := m.toastLine(); toastLine != "" {
			// Grouped with the status header, not between body and
			// footer (spec §2: "status bar: daemon state · version ·
			// update banner · toasts").
			header = strings.Join([]string{header, toastLine}, "\n\n")
		}
		if top, ok := m.stackTop(); ok {
			// A pushed screen replaces the tab bar with the breadcrumb —
			// width/height are computed fresh on every call from the
			// current m.width/m.height, so a resize is handled purely by
			// construction (screen.go's View doc): there is no cached
			// dimension on the root or the screen that a WindowSizeMsg
			// would need to separately invalidate. fitAndFillHeight is the
			// backstop (see its doc): the screen sizes itself to the budget,
			// but a regressed one must still never grow the frame past the
			// terminal and shove the footer off the alt-screen — and a
			// screen that sizes SHORTER than its budget (the browser preview,
			// by design) still leaves the footer pinned to the last row
			// rather than floating above unfilled space.
			screen := fitAndFillHeight(top.View(m.width, m.stackBodyHeight()), m.stackBodyHeight())
			// top.View just ran, so a browser's previewShown — and thus WantsMouse —
			// now reflects this exact frame: read it here, before the tea.View is
			// built, to arm the mouse only while the preview pane the wheel/click act
			// on is actually drawn. mouseCaptureOff vetoes the arm: the runtime
			// toggle that hands the pane back to the terminal's own drag-select
			// (mouse capture is terminal-global — there is no scoped capture to arm
			// instead), disclosed in the footer every frame it holds.
			if browser, isBrowser := top.(*views.Browser); isBrowser && browser.WantsMouse() && !m.mouseCaptureOff {
				mouseWanted = true
			}
			body = strings.Join([]string{header, m.breadcrumb(), screen, m.footer()}, "\n\n")
		} else {
			// fitAndFillHeight's pad half matters here too: the Projects table
			// self-fills its configured height (bubbles table.Model pads its own
			// blank rows), but Conflicts/Activity/Doctor render an unbounded body
			// with no height budget of their own, so without padding here a tab
			// with little content would leave the same short-frame, floating-
			// footer symptom the pushed-screen path had.
			tabBody := fitAndFillHeight(m.activeBody(), m.tabBodyHeight())
			body = strings.Join([]string{header, m.tabBar(), tabBody, m.footer()}, "\n\n")
		}
	}
	view := tea.NewView(body)
	view.AltScreen = true
	view.WindowTitle = "agent-brain dashboard"
	if mouseWanted {
		// Cell-motion mode delivers wheel + click; the renderer diffs this back to
		// None on the first frame that leaves the preview (and on close), so leaving
		// the browser — or quitting mid-preview — restores native selection with no
		// explicit teardown here.
		view.MouseMode = tea.MouseModeCellMotion
	}
	return view
}

func (m Model) activeBody() string {
	switch m.active {
	case tabProjects:
		return m.projects.View(m.fleetHeaderLine())
	case tabConflicts:
		return m.conflicts.View(m.tabBodyHeight())
	case tabActivity:
		return m.activity.View(m.status, m.statusErr, m.projects.Units, m.now, m.width, m.tabBodyHeight())
	case tabDoctor:
		return m.doctor.View(m.width, m.tabBodyHeight())
	default:
		return ""
	}
}

func (m Model) tabBar() string {
	parts := make([]string, tabCount)
	for t := range tabCount {
		label := fmt.Sprintf("%d %s", int(t)+1, t.title())
		if t == m.active {
			parts[t] = m.styles.ActiveTab.Render("[" + label + "]")
		} else {
			parts[t] = m.styles.InactiveTab.Render(" " + label + " ")
		}
	}
	return strings.Join(parts, " ")
}

// footer advertises exactly the keys that dispatch on the active surface:
// the quit prompt or an edit-flow modal while one owns the keyboard (both
// single-line footer states — the flow modal's whole chrome fits the slot
// this budget already reserves, editflow.go), the active Projects modal's
// live subset while it owns the keyboard (unchanged — a modal is an input-
// owned state machine, not a set of dispatchable actions), the top of the
// navigation stack's own scope while a screen is pushed, or otherwise the
// registry-driven rows for the current tab's scope (spec §14's single
// source, so this can never advertise a key the active surface actually
// ignores).
func (m Model) footer() string {
	switch {
	case m.updatePhase == updateConfirm:
		return m.styles.Warn.Render("update to " + m.updateTag + "? (y/n)")
	case m.updatePhase == updateApplying:
		// Inputs are frozen except ctrl+c (handleKey); the footer says so rather
		// than advertising tab keys that do nothing right now.
		return m.styles.Dim.Render("installing " + m.updateTag + "… — ctrl+c aborts")
	case m.quitPrompt:
		return m.styles.Warn.Render("quit agent-brain? (y/n)")
	case m.flowModal != nil:
		return m.flowModalFooterLine()
	case m.projects.ModalOpen():
		// The migrate modal advertises its own stage subset (single-select, no
		// space-toggle); the untrack confirm and the add flow share ForModal. The
		// three modal states are mutually exclusive, so this branch names exactly
		// the one that owns the keyboard.
		var bindings []keybinding.Binding
		if m.projects.Migrating != views.MigrateNone {
			bindings = views.DashboardKeys.ForMigrateModal(m.projects.Migrating)
		} else {
			bindings = views.DashboardKeys.ForModal(m.projects.Confirming, m.projects.Adding)
		}
		return m.styles.Dim.Render(views.HelpLine(bindings))
	default:
		if top, ok := m.stackTop(); ok {
			footer := m.stackFooterLine()
			if cue := m.mouseCaptureDisclosure(); cue != "" {
				// The cue joins the SAME footer line (a state cue, not a new row) so
				// the frame keeps its exact height and the footer stays on the literal
				// last row (spec §2): it reads as one more " · "-joined segment, the
				// separator stackFooterLine already uses between hints. WHERE on the
				// line it sits depends on scope. The LIST footer overflows the
				// canonical PTY width (188 cols even armed) and the v2 compositor
				// CLIPS rather than wraps, so a cue appended last would begin past the
				// right edge and never render — the disclosed state would be invisible
				// at the one width that matters. So in list scope the cue LEADS,
				// surviving the clip. The focused footer is the short swapped preview
				// set and comfortably fits, so there the cue trails as the last
				// segment, leaving the return/scroll keys reading first. (The general
				// list-footer overflow — even "esc back" clips — is a separate
				// width-aware-fitting task, not this cue placement.)
				separator := m.styles.Dim.Render(" · ")
				if browser, isBrowser := top.(*views.Browser); isBrowser && browser.PreviewFocused() {
					footer = strings.Join([]string{footer, cue}, separator)
				} else {
					footer = strings.Join([]string{cue, footer}, separator)
				}
			}
			return footer
		}
		return m.styles.Dim.Render(views.HelpLine(m.footerBindings()))
	}
}

// mouseCaptureDisclosure is the footer state cue shown while the mouse-capture
// toggle is off and a browser owns the stack — the sole surface the mouse is
// ever armed for. It names the current state (native drag-select works) and the
// key that restores capture, derived purely from the persistent mouseCaptureOff
// field, so it renders on EVERY frame the toggle is off, not only the frame m
// flipped it. Empty (the footer says nothing about the mouse) whenever capture
// is armed or a non-browser screen owns the stack, where m neither toggles nor
// has anything to disclose.
func (m Model) mouseCaptureDisclosure() string {
	if !m.mouseCaptureOff {
		return ""
	}
	top, ok := m.stackTop()
	if !ok {
		return ""
	}
	if _, isBrowser := top.(*views.Browser); !isBrowser {
		return ""
	}
	return m.styles.Dim.Render("mouse: native select (m re-arms)")
}

// footerBindings renders the active scope's live keys straight from the
// action registry, in registry order: every global action plus the active
// tab's own scope, filtered to rows that both have a real key to advertise
// (sync-fleet does not — palette/help only) and are actually available right
// now via the same available() the footer has always used. This
// deliberately differs from the palette's own listing
// (paletteAvailable): switch-tabs and select stay advertised here because
// the active view's own key-routing honors them directly, even though
// neither is ever reachable through dispatch — which is exactly why the
// palette, unlike the footer, must hide both.
func (m Model) footerBindings() []keybinding.Binding {
	scope := m.activeScope()
	var bindings []keybinding.Binding
	for _, action := range actions.Registry() {
		if len(action.Keys) == 0 {
			continue
		}
		if action.Scope != actions.ScopeGlobal && action.Scope != scope {
			continue
		}
		if !m.available(action.ID) {
			continue
		}
		bindings = append(bindings, actions.Binding(action))
	}
	return bindings
}

// activeScope maps the active tab to its actions.Scope, so the footer
// advertises that tab's own rows alongside the always-on globals.
func (m Model) activeScope() actions.Scope {
	switch m.active {
	case tabProjects:
		return actions.ScopeProjects
	case tabDoctor:
		return actions.ScopeDoctor
	case tabActivity:
		return actions.ScopeActivity
	case tabConflicts:
		return actions.ScopeConflicts
	default:
		return actions.ScopeGlobal
	}
}

// statusHeader renders the fleet-level facts once, persistently above the tab
// bar, so daemon posture is glanceable from every view. Projects rows carry the
// genuine per-unit telemetry (watch state, last cycle — Task 6.5); the header
// keeps only what is fleet-wide and cannot be broken down per unit (daemon
// state, quiesce, the last fleet cycle), never repeated down every identical row.
func (m Model) statusHeader() string {
	header := m.statusHeaderBase()
	// Both the gh-auth attention and the update banner are status-bar segments
	// (spec §2: "daemon state · version · gh-auth alert · update banner ·
	// toasts"): appended on the SAME line, so they add no header row and every
	// frame budget that measures the header (headerBlockHeight and its callers
	// frameChromeLines/mousePrefixLines) or derives its own budget from it
	// (ProjectsView sizes its table from tabBodyHeight minus the view's named
	// chrome, not the old static reservation) stays put — the invariant the
	// exact-fill frames depend on. Both render
	// even when the base is the status-error placeholder, so the "installing…" line
	// holds through the self-managed daemon restart an apply performs, and the
	// attention persists when the daemon itself is unreachable.
	//
	// The attention leads the banner because it is the louder, action-required
	// signal; in practice the two never coexist (an invalid token is why the
	// update check failed, so no banner was ever offered), but the order is
	// defined rather than incidental.
	if m.authInvalid {
		header += m.styles.Dim.Render(" · ") + m.authAttentionSegment()
	}
	if banner := m.updateBanner(); banner != "" {
		header += m.styles.Dim.Render(" · ") + banner
	}
	return header
}

// statusHeaderBase renders the fleet-level daemon facts (spec §7) — everything
// on the status line except the update banner statusHeader appends.
func (m Model) statusHeaderBase() string {
	if m.statusErr != nil {
		return m.styles.Dim.Render("daemon status unavailable")
	}
	segments := []string{"daemon: " + watchState(m.status, m.now)}
	if m.quiesced() {
		segments = append(segments, "quiesced until "+m.status.QuiescedUntil.Format("15:04:05"))
	}
	segments = append(segments, "last cycle: "+lastCycle(m.status))
	return m.styles.Dim.Render(strings.Join(segments, " · "))
}

// updateBanner is the status header's update segment (spec §11), empty until a
// release check offers something. Its text tracks the phase: the U offer, the
// in-flight install, then the R restart offer. Info (blue) for the available
// and installing states, OK (green) for the completed install — both Styles
// fields, so a background swap recolors the banner with the rest of the chrome.
func (m Model) updateBanner() string {
	switch m.updatePhase {
	case updateOffered, updateConfirm:
		return m.styles.Info.Render(m.updateTag + " available — U to update")
	case updateApplying:
		return m.styles.Info.Render("installing " + m.updateTag + "…")
	case updateInstalled:
		return m.styles.OK.Render("installed " + m.updateTag + " — R to restart the hub on it (or restart manually)")
	default:
		return ""
	}
}

// watchState derives the fleet's watch posture from the daemon status. A live
// hold (QuiescedUntil in the future) wins; otherwise a "ready" daemon is
// watching and any other state (e.g. "uninitialized") is surfaced verbatim. It
// takes now so a stale hold (a quiesce whose deadline has passed) reads as
// watching, matching the Activity view's guard.
func watchState(status api.StatusResponse, now time.Time) string {
	if status.QuiescedUntil != nil && status.QuiescedUntil.After(now) {
		return "held"
	}
	switch status.State {
	case "ready":
		return "watching"
	case "":
		return "—"
	default:
		return status.State
	}
}

// fleetHeaderLine renders the Projects tab's one-line fleet summary (spec §9):
// "N units · watching M/N · last sync <outcome+relative> · vX.Y.Z". The count
// and watching tally read the current fleet snapshot; the outcome reuses
// lastCycle — the exact verdict the status header shows — with the cycle's
// relative age appended when there has been one; the version is the build's own
// (Config.Version). The "vs latest" comparison joins in Task 18 once the release
// check exists; until then the version is shown plain, with no placeholder.
func (m Model) fleetHeaderLine() string {
	units := m.projects.Units
	total := len(units)
	watching := 0
	for _, unit := range units {
		if unit.WatchState == "watching" {
			watching++
		}
	}
	return fmt.Sprintf("%s · watching %d/%d · last sync %s · %s",
		unitsLabel(total), watching, total, m.lastSyncLabel(), m.version)
}

// unitsLabel pluralises the fleet's unit count ("1 unit", "2 units", "0 units").
func unitsLabel(total int) string {
	if total == 1 {
		return "1 unit"
	}
	return fmt.Sprintf("%d units", total)
}

// lastSyncLabel is the fleet header's "<outcome+relative>" segment: the same
// lastCycle verdict the status header renders ("ok"/"error"/"degraded"/…) with
// the cycle's relative age appended, or a bare "never" before the fleet has
// ever cycled (lastCycle's own nil-LastSync verdict, so the two never diverge).
func (m Model) lastSyncLabel() string {
	outcome := lastCycle(m.status)
	if m.status.LastSync == nil {
		return outcome // "never" — no cycle to age
	}
	return outcome + " " + relativeAgo(m.status.LastSync.At, m.now)
}

// relativeAgo renders t as a coarse "X ago" relative to now — the fleet
// header's age suffix. It mirrors views.relativeTime across the package
// boundary (that helper is unexported): a stable, generic formatter small
// enough that duplicating it beats exporting a views helper for this one
// caller, the same duplication-over-coupling call browser.go's isSubsequence
// documents. A t at or after now (clock skew on a just-recorded cycle) falls in
// the sub-minute arm as "just now" rather than a negative age.
func relativeAgo(t, now time.Time) string {
	if t.IsZero() {
		return "—"
	}
	switch d := now.Sub(t); {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
}

// lastCycle summarises the last fleet cycle's outcome for the status header.
func lastCycle(status api.StatusResponse) string {
	switch {
	case status.LastSync == nil:
		return "never"
	case status.LastSync.Error != "":
		return "error"
	case len(status.LastSync.Degraded) > 0:
		return "degraded"
	case len(status.LastSync.Scrubbed) > 0:
		return "scrubbed"
	case status.LastSync.Offline:
		return "offline"
	default:
		return "ok"
	}
}

func (m Model) daemonDownView() string {
	var b strings.Builder
	b.WriteString(m.styles.Title.Render("agent-brain daemon is not running"))
	b.WriteString("\n\n")
	b.WriteString("The dashboard reads a live daemon over its socket; nothing is answering.\n\n")
	switch {
	case m.starting:
		b.WriteString(m.styles.Dim.Render("starting the service…"))
		b.WriteString("\n\n")
	case m.serviceErr != nil:
		b.WriteString(m.styles.Fail.Render(fmt.Sprintf("start failed: %v", m.serviceErr)))
		b.WriteString("\n\n")
	}
	b.WriteString("  s   start the login service, then re-check\n")
	b.WriteString("  q   quit\n")
	b.WriteString("\n")
	b.WriteString(m.styles.Dim.Render("(or start it yourself: `agent-brain service install` / `agent-brain daemon run`)"))
	return b.String()
}
