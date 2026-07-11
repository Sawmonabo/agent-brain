package dashboard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	keybinding "charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
)

// pollInterval is the shared refresh cadence. Tick-based polling is the
// idiomatic bubbletea pattern for a local daemon: no push channel exists, and
// inventing one would violate the no-new-seams rule (spec §7 / task brief).
const pollInterval = 2 * time.Second

// requestTimeout bounds each background data Cmd so a wedged daemon cannot hang
// a poll. It is well under the UDS client's own 120s ceiling so the UI stays
// responsive even when a request is doomed.
const requestTimeout = 10 * time.Second

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
// perform I/O directly (model purity, enforced by the Q3 gate).
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
	syncResultMsg struct {
		folder string
		resp   api.SyncResponse
		err    error
	}
	untrackResultMsg struct {
		folder string
		resp   api.UntrackResponse
		err    error
	}
	serviceStartedMsg struct{ err error }
)

// Config is what the cli root command supplies to build the root model.
type Config struct {
	Data dashboardData
	// StartService starts the login daemon service — the same
	// service.Controller.Start path the CLI uses — offered on the daemon-down
	// screen (spec §7). nil disables the offer.
	StartService func() error
}

// Model is the root bubbletea model: a tab bar over four views, all refreshed
// by one shared tick.
type Model struct {
	data         dashboardData
	startService func() error

	active tab
	width  int
	height int
	now    time.Time

	status     api.StatusResponse
	statusErr  error
	daemonDown bool

	starting   bool // a service-start Cmd is in flight
	serviceErr error

	projects  projectsView
	conflicts conflictsView
	activity  activityView
	doctor    doctorView

	quitting bool
}

// New builds the root model.
func New(cfg Config) Model {
	return Model{
		data:         cfg.Data,
		startService: cfg.StartService,
		now:          time.Now(),
		projects:     newProjectsView(),
	}
}

// Init loads the active view once and starts the shared tick.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.reloadCmd(), m.tickCmd())
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
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		resp, err := data.Status(ctx)
		return statusMsg{resp: resp, err: err}
	}
}

func (m Model) projectsCmd() tea.Cmd {
	data := m.data
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
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
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
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
		m.projects.setSize(msg.Width, msg.Height)
		return m, nil

	case tickMsg:
		m.now = time.Time(msg)
		return m, tea.Batch(m.reloadCmd(), m.tickCmd())

	case statusMsg:
		m.status, m.statusErr = msg.resp, msg.err
		m.daemonDown = errors.Is(msg.err, api.ErrDaemonNotRunning)
		return m, nil

	case projectsMsg:
		if msg.err != nil {
			m.projects.setLoadErr(msg.err)
		} else {
			m.projects.setUnits(msg.resp.Units)
		}
		return m, nil

	case conflictsMsg:
		m.conflicts.set(msg.records, msg.err)
		return m, nil

	case doctorMsg:
		m.doctor.set(msg.report, msg.err)
		return m, nil

	case syncResultMsg:
		m.projects.onSyncResult(msg)
		return m, m.projectsCmd() // reflect the post-sync fleet state

	case untrackResultMsg:
		m.projects.onUntrackResult(msg)
		return m, tea.Batch(m.projectsCmd(), m.statusCmd())

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

	// A modal confirm on the Projects view consumes keys before the globals,
	// so a `y`/`n` answer is never mistaken for a tab jump.
	if m.active == tabProjects && m.projects.confirming {
		return m, m.projects.update(msg, m.data)
	}

	switch {
	case keybinding.Matches(msg, dashboardKeys.Quit):
		m.quitting = true
		return m, tea.Quit
	case keybinding.Matches(msg, dashboardKeys.TabSwitch):
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
	}

	// Everything else belongs to the active view (table nav, s/t on Projects).
	if m.active == tabProjects {
		return m, m.projects.update(msg, m.data)
	}
	return m, nil
}

// View composes the tab bar and the active view, or the full-screen daemon-down
// notice. It runs in the alternate screen buffer so it never scrolls the
// terminal's scrollback.
func (m Model) View() tea.View {
	var body string
	if m.daemonDown {
		body = m.daemonDownView()
	} else {
		body = strings.Join([]string{m.statusHeader(), m.tabBar(), m.activeBody(), m.footer()}, "\n\n")
	}
	view := tea.NewView(body)
	view.AltScreen = true
	view.WindowTitle = "agent-brain dashboard"
	return view
}

func (m Model) activeBody() string {
	switch m.active {
	case tabProjects:
		return m.projects.view()
	case tabConflicts:
		return m.conflicts.view()
	case tabActivity:
		return m.activity.view(m.status, m.statusErr, m.projects.units, m.now)
	case tabDoctor:
		return m.doctor.view()
	default:
		return ""
	}
}

func (m Model) tabBar() string {
	parts := make([]string, tabCount)
	for t := range tabCount {
		label := fmt.Sprintf("%d %s", int(t)+1, t.title())
		if t == m.active {
			parts[t] = activeTabStyle.Render("[" + label + "]")
		} else {
			parts[t] = inactiveTabStyle.Render(" " + label + " ")
		}
	}
	return strings.Join(parts, " ")
}

// footer advertises exactly the keys that dispatch on the active tab,
// rendered from the same bindings handleKey matches (keymap.go).
func (m Model) footer() string {
	bindings := dashboardKeys.forTab(m.active)
	parts := make([]string, len(bindings))
	for i, binding := range bindings {
		help := binding.Help()
		parts[i] = help.Key + " " + help.Desc
	}
	return dimStyle.Render(strings.Join(parts, " · "))
}

// statusHeader renders the fleet-level facts once, persistently above the tab
// bar, so daemon posture is glanceable from every view. Projects rows carry the
// genuine per-unit telemetry (watch state, last cycle — Task 6.5); the header
// keeps only what is fleet-wide and cannot be broken down per unit (daemon
// state, quiesce, the last fleet cycle), never repeated down every identical row.
func (m Model) statusHeader() string {
	if m.statusErr != nil {
		return dimStyle.Render("daemon status unavailable")
	}
	segments := []string{"daemon: " + watchState(m.status, m.now)}
	if quiesce := m.status.QuiescedUntil; quiesce != nil && quiesce.After(m.now) {
		segments = append(segments, "quiesced until "+quiesce.Format("15:04:05"))
	}
	segments = append(segments, "last cycle: "+lastCycle(m.status))
	return dimStyle.Render(strings.Join(segments, " · "))
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
	default:
		return "ok"
	}
}

func (m Model) daemonDownView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("agent-brain daemon is not running"))
	b.WriteString("\n\n")
	b.WriteString("The dashboard reads a live daemon over its socket; nothing is answering.\n\n")
	switch {
	case m.starting:
		b.WriteString(dimStyle.Render("starting the service…"))
		b.WriteString("\n\n")
	case m.serviceErr != nil:
		b.WriteString(failStyle.Render(fmt.Sprintf("start failed: %v", m.serviceErr)))
		b.WriteString("\n\n")
	}
	b.WriteString("  s   start the login service, then re-check\n")
	b.WriteString("  q   quit\n")
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("(or start it yourself: `agent-brain service install` / `agent-brain daemon run`)"))
	return b.String()
}

// Shared styles. The visible glyphs and text — not colour — carry every signal
// a test asserts on: tests strip the styling to plain (see the test helper), and
// bubbletea downgrades colour for non-colour terminals in production.
var (
	titleStyle       = lipgloss.NewStyle().Bold(true)
	headerStyle      = lipgloss.NewStyle().Bold(true)
	dimStyle         = lipgloss.NewStyle().Faint(true)
	okStyle          = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	warnStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	failStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	infoStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	activeTabStyle   = lipgloss.NewStyle().Bold(true)
	inactiveTabStyle = lipgloss.NewStyle().Faint(true)
)

func sectionTitle(title string) string { return titleStyle.Render(title) }
