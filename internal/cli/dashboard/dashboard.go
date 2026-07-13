package dashboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	keybinding "charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	glamour "charm.land/glamour/v2"
	glamourstyles "charm.land/glamour/v2/styles"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/actions"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/links"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/views"
	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
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
	serviceStartedMsg struct{ err error }
)

// toast is a transient status-area notification (spec §2's "status bar: …
// toasts"). expiresAt is computed from the model's current now at push time,
// so tests control expiry deterministically the same way they already
// control tickMsg — no wall-clock dependency in either the push or the
// expiry check.
type toast struct {
	text      string
	expiresAt time.Time
}

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
}

// Model is the root bubbletea model: a tab bar over four views, all refreshed
// by one shared tick.
type Model struct {
	data         views.DataSource
	startService func() error
	actions      views.TrackActions
	styles       theme.Styles
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
	// is an inline footer state, not a full overlay. toast is the persistent
	// status-area notification dispatch uses to explain a local refusal.
	paletteOpen bool
	palette     views.PaletteModel
	helpOpen    bool
	quitPrompt  bool
	toast       *toast

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

	quitting bool
}

// New builds the root model.
func New(cfg Config) Model {
	m := Model{
		data:         cfg.Data,
		startService: cfg.StartService,
		actions:      views.TrackActions{Discover: cfg.Discover, Identify: cfg.Identify},
		registry:     cfg.Registry,
		settings:     cfg.Settings,
		getenv:       os.Getenv,
		cacheRoot:    cfg.CacheRoot,
		now:          time.Now(),
		projects:     views.NewProjectsView(),
	}
	return m.withStyles(theme.Default(true), true) // dark until the terminal answers (Init requests it)
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

// themedScreen is the optional seam a stack screen exposes so a theme swap
// can reach it after construction: both *views.Browser and *views.Reading
// satisfy it, and a later screen that renders styled markdown (History)
// joins by implementing the same two setters — no per-type case to forget
// here.
type themedScreen interface {
	SetStyles(theme.Styles)
	SetRender(func(md string, width int) string)
}

// applyStackTheme pushes the CURRENT styles and markdown renderer into
// every themedScreen on the navigation stack. SetStyles/SetRender are
// deliberately not part of the Screen interface (Update/View/Title only,
// so the stack's own push/pop/forward plumbing never needs to know a
// screen's concrete type) — this is the one place that steps outside that
// abstraction, the same way withStyles already does for the four tab
// views, and for the identical reason: a background-color swap must reach
// state that already exists, not just state constructed after the swap.
func (m Model) applyStackTheme() {
	for _, screen := range m.stack {
		if themed, ok := screen.(themedScreen); ok {
			themed.SetStyles(m.styles)
			themed.SetRender(m.renderMarkdown)
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
	return tea.Batch(m.reloadCmd(), m.tickCmd(), tea.RequestBackgroundColor)
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

func (m Model) startServiceCmd() tea.Cmd {
	start := m.startService
	return func() tea.Msg {
		if start == nil {
			return serviceStartedMsg{err: errors.New("no service controller available")}
		}
		return serviceStartedMsg{err: start()}
	}
}

// Update is the root reducer. It owns the shared status, tab switching, and the
// daemon-down/service-start flow; view-specific data and keys are forwarded to
// the owning view.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.projects.SetSize(msg.Width, msg.Height)
		return m, nil

	case tea.BackgroundColorMsg:
		return m.withStyles(theme.Default(msg.IsDark()), msg.IsDark()), nil

	case tickMsg:
		m.now = time.Time(msg)
		if m.toast != nil && !m.now.Before(m.toast.expiresAt) {
			m.toast = nil
		}
		cmds := []tea.Cmd{m.reloadCmd(), m.tickCmd()}
		if _, ok := m.stackTop(); ok {
			var stackCmd tea.Cmd
			m, stackCmd = m.forwardToStack(views.RefreshMsg{Now: m.now})
			cmds = append(cmds, stackCmd)
		}
		return m, tea.Batch(cmds...)

	case statusMsg:
		m.status, m.statusErr = msg.resp, msg.err
		m.daemonDown = errors.Is(msg.err, api.ErrDaemonNotRunning)
		// The capture wait resolves off the same poll that carries every
		// other daemon fact — no dedicated timer (editflow.go).
		m.checkPendingCapture()
		return m, nil

	case projectsMsg:
		if msg.err != nil {
			m.projects.SetLoadErr(msg.err)
		} else {
			m.projects.SetUnits(msg.resp.Units)
		}
		return m, nil

	case conflictsMsg:
		m.conflicts.Set(msg.records, msg.err)
		return m, nil

	case doctorMsg:
		m.doctor.Set(msg.report, msg.err)
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
		failed := msg.Err != nil
		m.projects.OnTrackResult(msg)
		if failed {
			return m, m.projectsCmd()
		}
		// Track's HTTP reply returns BEFORE the daemon's post-admin cycle
		// (the same lesson track.go's syncAfterTrack records): an explicit
		// whole-fleet sync is what makes the enrollment's first mirror-in
		// visible here rather than landing silently later.
		return m, tea.Batch(m.projectsCmd(), m.statusCmd(), views.SyncCmd(m.data, ""))

	case views.OpenFolderMsg:
		// The one place a bare (Folder, Units) request becomes an actual
		// Screen: ProjectsView cannot build a *views.Browser itself (it has
		// none of Registry/Styles/memoryfs/glamour), so it emits this and
		// the root — the only place with all of those — constructs it and
		// pushes it, same as any other PushScreenMsg.
		m = m.pushScreen(views.NewBrowser(m.buildBrowserDeps(msg.Folder, msg.Units)))
		return m, nil

	case views.PushScreenMsg:
		m = m.pushScreen(msg.Screen)
		return m, nil

	case views.PopScreenMsg:
		m = m.popScreen()
		return m, nil

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

	case editorFinishedMsg:
		return m.finishEdit(msg), nil

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
		return m, m.projects.Update(msg, m.data, m.actions)
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
		m.quitPrompt = true
		return m, nil
	}

	// Every other global action (quit, ctrl+k, ?, /) shares the one dispatch
	// a palette choice also runs through, so a direct keypress and picking
	// the same action from the palette can never behave differently.
	for _, candidate := range actions.ForScope(actions.ScopeGlobal) {
		if keybinding.Matches(msg, actions.Binding(candidate)) {
			return m, m.dispatch(candidate.ID)
		}
	}

	// Everything else belongs to the active view (table nav, s/u/a on Projects).
	if m.active == tabProjects {
		return m, m.projects.Update(msg, m.data, m.actions)
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

// pushToast surfaces text in the persistent status area for toastTTL, expiry
// checked on the existing 2s poll tick rather than a dedicated timer.
// pointer receiver: every caller already holds an addressable *Model mid-
// mutation (dispatch, quiesceGate) and wants this folded in as one more
// field write, not a value threaded back out and reassigned.
func (m *Model) pushToast(text string) {
	m.toast = &toast{text: text, expiresAt: m.now.Add(toastTTL)}
}

// toastLine renders the active toast, if any and not yet expired. This is a
// belt-and-suspenders check alongside the tick-driven expiry in Update, so a
// View call between ticks never shows text past its TTL.
func (m Model) toastLine() string {
	if m.toast == nil || !m.now.Before(m.toast.expiresAt) {
		return ""
	}
	return m.styles.Toast.Render(m.toast.text)
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
// whose body is perfectly readable.
func (m Model) openSearchChoice(memory memoryfs.Memory) Model {
	var index *links.Index
	if memories, err := memoryfs.List(m.registry, unitsForFolder(m.projects.Units, memory.Folder)); err == nil {
		index = links.BuildIndex(memories, memoryfs.ReadBody)
	}
	return m.pushScreen(views.NewReading(views.ReadingDeps{
		Memory:   memory,
		Index:    index,
		ReadBody: memoryfs.ReadBody,
		Render:   m.renderMarkdown,
		Styles:   m.styles,
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

// stackBodyHeight computes a pushed screen's content budget: terminal
// height minus the header, breadcrumb, and footer chrome around it —
// mirroring the tab-level budget ProjectsView.SetSize already computes for
// the same chrome (status header, toast slot, footer), minus the one line
// the breadcrumb costs in place of the tab bar it replaces.
func (m Model) stackBodyHeight() int {
	const chromeLines = 8
	if height := m.height - chromeLines; height > 3 {
		return height
	}
	return 3
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
	if id == "help" {
		m.helpOpen = true
		return nil
	}
	if id == "search" {
		m.openSearchOverlay()
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
		"open-browser", "browser-read", "browser-order", "browser-filter", "browser-back",
		"reading-links", "reading-follow", "reading-backlinks", "reading-copy-path", "reading-back":
		return true
	case "browser-edit", "browser-new", "browser-rename", "browser-delete", "reading-edit":
		return m.flowAvailable(id)
	case "add-project":
		return m.actions.AddAvailable()
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
			return m.projects.Update(replayKey('s'), m.data, m.actions)
		},
		"untrack": func() tea.Cmd {
			m.active = tabProjects
			return m.projects.Update(replayKey('u'), m.data, m.actions)
		},
		"add-project": func() tea.Cmd {
			m.active = tabProjects
			return m.projects.Update(replayKey('a'), m.data, m.actions)
		},
		"sync-fleet": func() tea.Cmd {
			return views.SyncCmd(m.data, "")
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
			// would need to separately invalidate.
			body = strings.Join([]string{header, m.breadcrumb(), top.View(m.width, m.stackBodyHeight()), m.footer()}, "\n\n")
		} else {
			body = strings.Join([]string{header, m.tabBar(), m.activeBody(), m.footer()}, "\n\n")
		}
	}
	view := tea.NewView(body)
	view.AltScreen = true
	view.WindowTitle = "agent-brain dashboard"
	return view
}

func (m Model) activeBody() string {
	switch m.active {
	case tabProjects:
		return m.projects.View()
	case tabConflicts:
		return m.conflicts.View()
	case tabActivity:
		return m.activity.View(m.status, m.statusErr, m.projects.Units, m.now)
	case tabDoctor:
		return m.doctor.View()
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
	case m.quitPrompt:
		return m.styles.Warn.Render("quit agent-brain? (y/n)")
	case m.flowModal != nil:
		return m.flowModalFooterLine()
	case m.projects.ModalOpen():
		bindings := views.DashboardKeys.ForModal(m.projects.Confirming, m.projects.Adding)
		return m.styles.Dim.Render(views.HelpLine(bindings))
	default:
		if _, ok := m.stackTop(); ok {
			return m.stackFooterLine()
		}
		return m.styles.Dim.Render(views.HelpLine(m.footerBindings()))
	}
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

// activeScope maps the active tab to its actions.Scope. Activity has no
// tab-specific actions of its own yet, so it falls back to Global — its
// footer advertises exactly the always-on rows.
func (m Model) activeScope() actions.Scope {
	switch m.active {
	case tabProjects:
		return actions.ScopeProjects
	case tabDoctor:
		return actions.ScopeDoctor
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
