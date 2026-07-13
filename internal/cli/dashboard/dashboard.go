package dashboard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	keybinding "charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/actions"
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
}

// Model is the root bubbletea model: a tab bar over four views, all refreshed
// by one shared tick.
type Model struct {
	data         views.DataSource
	startService func() error
	actions      views.TrackActions
	styles       theme.Styles

	active tab
	width  int
	height int
	now    time.Time

	status     api.StatusResponse
	statusErr  error
	daemonDown bool

	starting   bool // a service-start Cmd is in flight
	serviceErr error

	projects  views.ProjectsView
	conflicts views.ConflictsView
	activity  views.ActivityView
	doctor    views.DoctorView

	// Root chrome (spec §14/§2): the palette and help overlays each own the
	// whole screen and the keyboard while open; the quit prompt is an inline
	// footer state, not a full overlay. toast is the persistent status-area
	// notification dispatch uses to explain a local refusal.
	paletteOpen bool
	palette     views.PaletteModel
	helpOpen    bool
	quitPrompt  bool
	toast       *toast

	quitting bool
}

// New builds the root model.
func New(cfg Config) Model {
	m := Model{
		data:         cfg.Data,
		startService: cfg.StartService,
		actions:      views.TrackActions{Discover: cfg.Discover, Identify: cfg.Identify},
		now:          time.Now(),
		projects:     views.NewProjectsView(),
	}
	return m.withStyles(theme.Default(true)) // dark until the terminal answers (Init requests it)
}

// withStyles installs styles on the root and propagates them to every view
// in one call — construction and every tea.BackgroundColorMsg both route
// through here, so a palette swap is one place, not five. The root palette
// is included even before it is ever opened (a zero-value PaletteModel's
// SetStyles is harmless) so a background swap while it happens to be open
// is never missed.
func (m Model) withStyles(styles theme.Styles) Model {
	m.styles = styles
	m.projects.SetStyles(styles)
	m.conflicts.SetStyles(styles)
	m.activity.SetStyles(styles)
	m.doctor.SetStyles(styles)
	m.palette.SetStyles(styles)
	return m
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
		return m.withStyles(theme.Default(msg.IsDark())), nil

	case tickMsg:
		m.now = time.Time(msg)
		if m.toast != nil && !m.now.Before(m.toast.expiresAt) {
			m.toast = nil
		}
		return m, tea.Batch(m.reloadCmd(), m.tickCmd())

	case statusMsg:
		m.status, m.statusErr = msg.resp, msg.err
		m.daemonDown = errors.Is(msg.err, api.ErrDaemonNotRunning)
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

	case views.PaletteChoiceMsg:
		return m, m.dispatch(msg.ID)

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

	// Every other global action (quit, ctrl+k, ?, and — once Task 15 wires
	// it — /) shares the one dispatch a palette choice also runs through, so
	// a direct keypress and picking the same action from the palette can
	// never behave differently.
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
// (refuseIfQuiesced is the one function both call). It is scope-aware: a
// Projects-scoped action's key is only a real refusal when the Projects tab
// is actually active — off that tab the key was already a no-op, and
// quiescing must not start toasting about something that was never going to
// happen anyway.
func (m *Model) quiesceGate(msg tea.KeyPressMsg) bool {
	if !m.quiesced() {
		return false
	}
	for _, candidate := range actions.Registry() {
		if !candidate.Mutates || !keybinding.Matches(msg, actions.Binding(candidate)) {
			continue
		}
		if candidate.Scope == actions.ScopeProjects && m.active != tabProjects {
			continue // dead key off the Projects tab; nothing to refuse
		}
		return m.refuseIfQuiesced(candidate)
	}
	return false
}

// refuseIfQuiesced toasts and refuses action if it Mutates and the daemon is
// currently quiesced. It is the one place that decides a mutation is
// refused — called from quiesceGate (a direct keypress) and dispatch (a
// palette choice) alike, so the refusal itself cannot diverge between them.
func (m *Model) refuseIfQuiesced(action actions.Action) bool {
	if !action.Mutates || !m.quiesced() {
		return false
	}
	m.pushToast(fmt.Sprintf("daemon quiesced until %s — retry after", m.status.QuiescedUntil.Format("15:04:05")))
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
// and otherwise runs the action's registered runner. help is handled
// directly here rather than through runners(): it is the one action that is
// a pure Model state flip with no async work to schedule, so it has no
// tea.Cmd to produce — forcing it into runners()'s func() tea.Cmd shape just
// to return a constant nil is exactly the dead-weight-result smell the
// unparam linter exists to catch. An unknown id, or one with no registered
// runner (available's gate), does nothing — the registry stays honest about
// what actually works right now.
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
	runner, ok := m.runners()[id]
	if !ok {
		return nil
	}
	return runner()
}

// available reports whether action id can actually do something right now.
// It drives the footer (footerBindings) and dispatch's own pre-run gate —
// NOT the palette, which uses the stricter paletteAvailable below. switch-
// tabs and select are structural navigation handled by dedicated key-routing
// paths in handleKey rather than a runner — dispatch never actually reaches
// either — so they are unconditionally available here purely to keep
// advertising their footer/help hints (spec §2); help has no wiring
// precondition and is never hidden either, but IS genuinely dispatchable
// (dispatch special-cases it directly). add-project additionally needs both
// track closures wired (the existing AddAvailable contract, unchanged by
// this task); every other action is available exactly when it has a
// registered runner — the mechanism that keeps a not-yet-built feature's
// registry row (search, until Task 15) invisible in the footer while the
// help overlay still documents it.
func (m *Model) available(id string) bool {
	switch id {
	case "switch-tabs", "select", "help":
		return true
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
// map, plus the one case dispatch special-cases outside that map (help),
// means a future registry row added without a runner is automatically
// absent from the palette even if some later change ever gave available
// itself a new unconditional-true case — there is no hand-maintained
// exclusion list here to fall out of sync.
func (m *Model) paletteAvailable(id string) bool {
	switch id {
	case "help":
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

// View composes the tab bar and the active view, or the full-screen daemon-down
// notice. It runs in the alternate screen buffer so it never scrolls the
// terminal's scrollback.
func (m Model) View() tea.View {
	var body string
	switch {
	case m.daemonDown:
		body = m.daemonDownView()
	case m.helpOpen:
		body = views.NewHelpModel(m.styles).View()
	case m.paletteOpen:
		body = m.palette.View()
	default:
		header := m.statusHeader()
		if toastLine := m.toastLine(); toastLine != "" {
			// Grouped with the status header, not between body and
			// footer (spec §2: "status bar: daemon state · version ·
			// update banner · toasts").
			header = strings.Join([]string{header, toastLine}, "\n\n")
		}
		body = strings.Join([]string{header, m.tabBar(), m.activeBody(), m.footer()}, "\n\n")
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
// the quit prompt while it owns the keyboard, the active Projects modal's
// live subset while it owns the keyboard (unchanged — a modal is an input-
// owned state machine, not a set of dispatchable actions), or otherwise the
// registry-driven rows for the current scope (spec §14's single source, so
// this can never advertise a key the active surface actually ignores).
func (m Model) footer() string {
	switch {
	case m.quitPrompt:
		return m.styles.Warn.Render("quit agent-brain? (y/n)")
	case m.projects.ModalOpen():
		bindings := views.DashboardKeys.ForModal(m.projects.Confirming, m.projects.Adding)
		return m.styles.Dim.Render(views.HelpLine(bindings))
	default:
		return m.styles.Dim.Render(views.HelpLine(m.footerBindings()))
	}
}

// footerBindings renders the active scope's live keys straight from the
// action registry, in registry order: every global action plus the active
// tab's own scope, filtered to rows that both have a real key to advertise
// (sync-fleet does not — palette/help only) and are actually available right
// now (search is not, until Task 15) via the same available() the footer has
// always used. This deliberately differs from the palette's own listing
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
