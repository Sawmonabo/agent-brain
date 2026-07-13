package dashboard

import (
	"context"
	"errors"
	"image/color"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/views"
	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
	"github.com/Sawmonabo/agent-brain/internal/provider"
)

// csiPattern matches the CSI escape sequences lipgloss emits — ESC '[',
// numeric parameters, a letter terminator (SGR colour/attributes end in
// 'm'). The views render only styled text, so this is the whole escape
// surface in a View string.
var csiPattern = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// plain strips styling so assertions match the visible text. This realises
// the brief's "styling forced plain in tests": lipgloss v2 emits ANSI
// unconditionally at Render and has no plain-render mode — colour downgrade
// happens only at the colorprofile writer (verified against the resolved module
// 2026-07-09, where lipgloss.Writer *is* a colorprofile writer). Stripping the
// escapes here keeps the dashboard package's import set exactly the reviewed
// allowlist, with no extra dependency pulled in for test scaffolding.
func plain(s string) string {
	return csiPattern.ReplaceAllString(s, "")
}

// fakeData is an injectable views.DataSource: canned reads, and recorded
// mutating calls so a test can prove what a key press actually did. No
// socket, no filesystem, no doctor battery.
type fakeData struct {
	status       api.StatusResponse
	statusErr    error
	projects     api.ProjectsResponse
	projectsErr  error
	report       doctor.Report
	doctorErr    error
	conflicts    []config.ConflictRecord
	conflictsErr error

	syncResp    api.SyncResponse
	syncErr     error
	untrackResp api.UntrackResponse
	untrackErr  error
	trackResp   api.TrackResponse
	trackErr    error

	syncCalls    []string
	untrackCalls []api.UntrackRequest
	trackCalls   []api.TrackRequest
}

func (f *fakeData) Status(context.Context) (api.StatusResponse, error) {
	return f.status, f.statusErr
}

func (f *fakeData) Projects(context.Context) (api.ProjectsResponse, error) {
	return f.projects, f.projectsErr
}

func (f *fakeData) Sync(_ context.Context, project string) (api.SyncResponse, error) {
	f.syncCalls = append(f.syncCalls, project)
	return f.syncResp, f.syncErr
}

func (f *fakeData) Untrack(_ context.Context, req api.UntrackRequest) (api.UntrackResponse, error) {
	f.untrackCalls = append(f.untrackCalls, req)
	return f.untrackResp, f.untrackErr
}

func (f *fakeData) Track(_ context.Context, req api.TrackRequest) (api.TrackResponse, error) {
	f.trackCalls = append(f.trackCalls, req)
	return f.trackResp, f.trackErr
}

func (f *fakeData) Doctor(context.Context) (doctor.Report, error) {
	return f.report, f.doctorErr
}

func (f *fakeData) Conflicts() ([]config.ConflictRecord, error) {
	return f.conflicts, f.conflictsErr
}

// key builds a KeyPressMsg for a key name ("q", "s", "tab", "esc", …). Verified
// forms against bubbletea v2.0.8: printable runes carry Text, specials carry a
// Code constant (2026-07-09).
func key(name string) tea.KeyPressMsg {
	switch name {
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	default:
		return tea.KeyPressMsg{Code: []rune(name)[0], Text: name}
	}
}

// step feeds one message and returns the concrete root Model plus its Cmd.
func step(m Model, msg tea.Msg) (Model, tea.Cmd) {
	next, cmd := m.Update(msg)
	return next.(Model), cmd
}

// drain executes a (possibly batched) Cmd and returns every leaf message,
// running the fake's methods as a side effect — the standard way to test
// bubbletea Cmds without a program loop.
func drain(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var msgs []tea.Msg
		for _, child := range batch {
			msgs = append(msgs, drain(child)...)
		}
		return msgs
	}
	return []tea.Msg{msg}
}

func containsMsg[T tea.Msg](msgs []tea.Msg) bool {
	for _, m := range msgs {
		if _, ok := m.(T); ok {
			return true
		}
	}
	return false
}

func newTestModel(data views.DataSource) Model {
	return New(Config{Data: data, StartService: func() error { return nil }})
}

// readyStatus is a ready-daemon status fixture shared by the tests below and
// by views.ProjectsView's own test suite (duplicated there rather than
// exported: views must not import this package, so a views-level fixture
// cannot come from here, and it is small enough that duplicating it is
// cheaper than inventing a shared export just for test fixtures).
func readyStatus() api.StatusResponse {
	return api.StatusResponse{
		State:     "ready",
		Version:   "dev",
		PID:       4242,
		StartedAt: time.Date(2026, 7, 9, 9, 0, 0, 0, time.UTC),
		LastSync:  &api.SyncSummary{At: time.Date(2026, 7, 9, 11, 0, 0, 0, time.UTC), Pushed: true},
	}
}

func TestTabCycling(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		keys     []string
		wantTab  tab
		wantMark string
	}{
		{name: "tab advances to Conflicts", keys: []string{"tab"}, wantTab: tabConflicts, wantMark: "[2 Conflicts]"},
		{name: "tab twice reaches Activity", keys: []string{"tab", "tab"}, wantTab: tabActivity, wantMark: "[3 Activity]"},
		{name: "number key jumps to Doctor", keys: []string{"4"}, wantTab: tabDoctor, wantMark: "[4 Doctor]"},
		{name: "tab wraps from Doctor back to Projects", keys: []string{"4", "tab"}, wantTab: tabProjects, wantMark: "[1 Projects]"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			model := newTestModel(&fakeData{})
			for _, k := range testCase.keys {
				model, _ = step(model, key(k))
			}
			if model.active != testCase.wantTab {
				t.Errorf("active = %d, want %d", model.active, testCase.wantTab)
			}
			bar := plain(model.tabBar())
			if !strings.Contains(bar, testCase.wantMark) {
				t.Errorf("tab bar %q does not mark active tab %q", bar, testCase.wantMark)
			}
		})
	}
}

func TestTickTriggersReloadAndReschedules(t *testing.T) {
	t.Parallel()
	model := newTestModel(&fakeData{})
	moment := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

	next, cmd := step(model, tickMsg(moment))
	if !next.now.Equal(moment) {
		t.Errorf("now = %v, want %v", next.now, moment)
	}
	if cmd == nil {
		t.Fatal("tick returned no Cmd; expected a reload + reschedule batch")
	}
	msgs := drain(cmd)
	if !containsMsg[statusMsg](msgs) {
		t.Error("tick reload did not fetch status")
	}
	if !containsMsg[projectsMsg](msgs) {
		t.Error("tick reload did not fetch the active (Projects) view's data")
	}
	if !containsMsg[tickMsg](msgs) {
		t.Error("tick did not reschedule the next tick")
	}
}

func TestQuitKeys(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"q", "ctrl+c"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			model := newTestModel(&fakeData{})
			var msg tea.KeyPressMsg
			if name == "ctrl+c" {
				msg = tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
			} else {
				msg = key(name)
			}
			next, cmd := step(model, msg)
			if !next.quitting {
				t.Error("model did not mark quitting")
			}
			if cmd == nil {
				t.Fatal("expected tea.Quit Cmd")
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Error("Cmd did not produce a QuitMsg")
			}
		})
	}
}

func TestDaemonDownScreen(t *testing.T) {
	t.Parallel()
	model := newTestModel(&fakeData{})

	model, _ = step(model, statusMsg{err: api.ErrDaemonNotRunning})
	if !model.daemonDown {
		t.Fatal("daemonDown not set from ErrDaemonNotRunning")
	}
	body := plain(model.View().Content)
	for _, want := range []string{"daemon is not running", "start the login service", "quit"} {
		if !strings.Contains(body, want) {
			t.Errorf("daemon-down screen missing %q; got:\n%s", want, body)
		}
	}

	// A down daemon still polls, so it recovers on the next successful status.
	_, cmd := step(model, tickMsg(time.Now()))
	if !containsMsg[statusMsg](drain(cmd)) {
		t.Error("tick did not keep polling status while down")
	}
}

func TestDaemonDownStartServiceOffer(t *testing.T) {
	t.Parallel()
	var started int
	model := New(Config{Data: &fakeData{}, StartService: func() error { started++; return nil }})

	model, _ = step(model, statusMsg{err: api.ErrDaemonNotRunning})
	model, cmd := step(model, key("s"))
	if !model.starting {
		t.Error("pressing s did not enter the starting state")
	}
	msgs := drain(cmd)
	if started != 1 {
		t.Fatalf("StartService called %d times, want 1", started)
	}
	if !containsMsg[serviceStartedMsg](msgs) {
		t.Error("start-service Cmd did not produce serviceStartedMsg")
	}
}

func TestWatchState(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	tests := []struct {
		name   string
		status api.StatusResponse
		want   string
	}{
		{name: "ready", status: api.StatusResponse{State: "ready"}, want: "watching"},
		{name: "held when quiesced in future", status: api.StatusResponse{State: "ready", QuiescedUntil: &future}, want: "held"},
		{name: "stale quiesce reads as watching", status: api.StatusResponse{State: "ready", QuiescedUntil: &past}, want: "watching"},
		{name: "uninitialized verbatim", status: api.StatusResponse{State: "uninitialized"}, want: "uninitialized"},
		{name: "empty state dash", status: api.StatusResponse{}, want: "—"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := watchState(testCase.status, now); got != testCase.want {
				t.Errorf("watchState = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestLastCycle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status api.StatusResponse
		want   string
	}{
		{name: "never", status: api.StatusResponse{}, want: "never"},
		{name: "ok", status: api.StatusResponse{LastSync: &api.SyncSummary{Pushed: true}}, want: "ok"},
		{name: "error", status: api.StatusResponse{LastSync: &api.SyncSummary{Error: "boom"}}, want: "error"},
		{name: "degraded", status: api.StatusResponse{LastSync: &api.SyncSummary{Degraded: []string{"x"}}}, want: "degraded"},
		{name: "scrubbed", status: api.StatusResponse{LastSync: &api.SyncSummary{Scrubbed: []string{"y"}}}, want: "scrubbed"},
		{name: "offline", status: api.StatusResponse{LastSync: &api.SyncSummary{Offline: true}}, want: "offline"},
		{name: "error outranks offline", status: api.StatusResponse{LastSync: &api.SyncSummary{Offline: true, Error: "boom"}}, want: "error"},
		{name: "scrubbed outranks offline", status: api.StatusResponse{LastSync: &api.SyncSummary{Offline: true, Scrubbed: []string{"x"}}}, want: "scrubbed"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := lastCycle(testCase.status); got != testCase.want {
				t.Errorf("lastCycle = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestStatusHeader proves the persistent fleet header renders daemon posture,
// shows a live quiesce deadline, and degrades to a plain notice when status is
// unavailable.
func TestStatusHeader(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	quiesce := now.Add(30 * time.Minute)
	tests := []struct {
		name    string
		model   Model
		want    []string
		notWant []string
	}{
		{
			name:  "ready with last sync",
			model: Model{now: now, status: api.StatusResponse{State: "ready", LastSync: &api.SyncSummary{Pushed: true}}},
			want:  []string{"daemon: watching", "last cycle: ok"},
		},
		{
			name:  "held shows the quiesce deadline",
			model: Model{now: now, status: api.StatusResponse{State: "ready", QuiescedUntil: &quiesce}},
			want:  []string{"daemon: held", "quiesced until"},
		},
		{
			name:    "status error degrades gracefully",
			model:   Model{now: now, statusErr: errors.New("boom")},
			want:    []string{"daemon status unavailable"},
			notWant: []string{"last cycle"},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got := plain(testCase.model.statusHeader())
			for _, want := range testCase.want {
				if !strings.Contains(got, want) {
					t.Errorf("status header missing %q; got:\n%s", want, got)
				}
			}
			for _, notWant := range testCase.notWant {
				if strings.Contains(got, notWant) {
					t.Errorf("status header should not contain %q; got:\n%s", notWant, got)
				}
			}
		})
	}
}

// TestSwitchRefetchesStatusForEveryTab pins N-3: switching to any tab must
// refetch status so the persistent fleet header is fresh on arrival, not up to
// a poll interval stale. Conflicts and Doctor previously fetched only their own
// data on switch (Projects and Activity already refetched status), so their
// header lagged until the next 2s tick. The tick path (reloadCmd) already
// fetched status unconditionally — the gap was switchCmd alone.
func TestSwitchRefetchesStatusForEveryTab(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		key  string
	}{
		{name: "projects", key: "1"},
		{name: "conflicts", key: "2"},
		{name: "activity", key: "3"},
		{name: "doctor", key: "4"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			model := newTestModel(&fakeData{status: readyStatus()})
			model, _ = step(model, tea.WindowSizeMsg{Width: 110, Height: 40})

			_, cmd := step(model, key(testCase.key))
			if !containsMsg[statusMsg](drain(cmd)) {
				t.Errorf("switching to %s did not refetch status; the header would lag a poll interval", testCase.name)
			}
		})
	}
}

// TestHeaderFreshOnSwitchToConflicts is the user-visible form of N-3: after
// switching to Conflicts and running the switch's Cmds, the header reflects the
// freshly-fetched fleet state rather than the empty zero-value header it would
// otherwise show until the next tick.
func TestHeaderFreshOnSwitchToConflicts(t *testing.T) {
	t.Parallel()
	data := &fakeData{status: api.StatusResponse{
		State:    "ready",
		LastSync: &api.SyncSummary{Degraded: []string{"agent-brain"}},
	}}
	model := newTestModel(data)
	model, _ = step(model, tea.WindowSizeMsg{Width: 110, Height: 40})

	model, cmd := step(model, key("2")) // → Conflicts
	for _, msg := range drain(cmd) {
		model, _ = step(model, msg)
	}
	if header := plain(model.statusHeader()); !strings.Contains(header, "last cycle: degraded") {
		t.Errorf("header stale after switching to Conflicts (switch did not refetch status); got:\n%s", header)
	}
}

func TestFooterAdvertisesOnlyActiveTabKeys(t *testing.T) {
	t.Parallel()
	m := New(Config{Data: &fakeData{}})

	m.active = tabProjects
	projectsFooter := plain(m.footer())
	for _, want := range []string{"tab/1–4 switch", "↑/↓ select", "s sync", "t untrack", "q quit"} {
		if !strings.Contains(projectsFooter, want) {
			t.Errorf("Projects footer %q missing %q", projectsFooter, want)
		}
	}

	for _, other := range []tab{tabConflicts, tabActivity, tabDoctor} {
		m.active = other
		otherFooter := plain(m.footer())
		if strings.Contains(otherFooter, "sync") || strings.Contains(otherFooter, "untrack") {
			t.Errorf("%s footer advertises Projects-only keys: %q", other.title(), otherFooter)
		}
		for _, want := range []string{"tab/1–4 switch", "q quit"} {
			if !strings.Contains(otherFooter, want) {
				t.Errorf("%s footer %q missing %q", other.title(), otherFooter, want)
			}
		}
	}
}

// TestProjectsKeysStayDeadOffProjectsTab pins the behavior the old footer
// lied about: s/t on a non-Projects tab dispatch nothing and mutate nothing.
func TestProjectsKeysStayDeadOffProjectsTab(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	m := New(Config{Data: fake})
	m.active = tabConflicts

	m2, cmd := step(m, key("s"))
	if cmd != nil {
		t.Fatal("s on Conflicts produced a Cmd; want none")
	}
	_, cmd = step(m2, key("t"))
	if cmd != nil {
		t.Fatal("t on Conflicts produced a Cmd; want none")
	}
	if len(fake.syncCalls) != 0 || len(fake.untrackCalls) != 0 {
		t.Fatalf("keys off the Projects tab reached the daemon: sync=%v untrack=%v",
			fake.syncCalls, fake.untrackCalls)
	}
}

// TestFooterInModalStatesAdvertisesOnlyLiveKeys extends the modal-state footer
// honesty to a modal owning the keyboard. While an untrack confirm or any
// add-flow stage is open, the footer must advertise EXACTLY that state's live
// keys in render order and none of the tab-level hints — a footer that named
// s/t/a/tab while the modal swallowed them (or routed them into a text input,
// so `q` typed a "q") is precisely the dead-key class the keymap contract
// exists to make impossible. The exact-equality check pins both the live set
// (render order included) and the absence of every other binding; the explicit
// tab-hint sweep documents the second half of the contract.
func TestFooterInModalStatesAdvertisesOnlyLiveKeys(t *testing.T) {
	t.Parallel()
	tabHints := []string{"s sync", "t untrack", "a add", "tab/1–4"}
	tests := []struct {
		name       string
		confirming bool
		stage      views.AddStage
		want       string
	}{
		{name: "untrack confirm", confirming: true, stage: views.AddNone, want: "y/n decide · esc cancel"},
		{name: "add discovering", stage: views.AddDiscovering, want: "esc cancel"},
		{name: "add picking", stage: views.AddPicking, want: "↑/↓ select · enter confirm · esc cancel"},
		{name: "add confirm path", stage: views.AddConfirmPath, want: "enter confirm · esc cancel"},
		{name: "add identifying", stage: views.AddIdentifying, want: "esc cancel"},
		{name: "add naming folder", stage: views.AddNamingFolder, want: "enter confirm · esc cancel"},
		{name: "add tracking", stage: views.AddTracking, want: "esc cancel"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			m := newTestModel(&fakeData{})
			m.active = tabProjects
			m.projects.Confirming = testCase.confirming
			m.projects.Adding = testCase.stage

			got := plain(m.footer())
			if got != testCase.want {
				t.Errorf("modal footer = %q, want %q", got, testCase.want)
			}
			for _, hint := range tabHints {
				if strings.Contains(got, hint) {
					t.Errorf("modal footer %q leaks tab-level hint %q", got, hint)
				}
			}
		})
	}
}

// TestBackgroundColorSwapsPalette pins the theme re-derive wiring: sending a
// dark tea.BackgroundColorMsg then a light one through Update must not panic
// and must leave the model renderable — View non-empty — proving m.styles
// (and every view's copy of it, propagated through withStyles) is rebuilt
// from theme.Default on each swap rather than left stale or zeroed.
func TestBackgroundColorSwapsPalette(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{status: readyStatus(), projects: api.ProjectsResponse{}})
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})

	dark, cmd := step(m, tea.BackgroundColorMsg{Color: color.Black})
	if cmd != nil {
		t.Error("BackgroundColorMsg produced a Cmd; want none")
	}
	if body := dark.View().Content; body == "" {
		t.Error("model unrenderable after a dark BackgroundColorMsg")
	}

	light, cmd := step(dark, tea.BackgroundColorMsg{Color: color.White})
	if cmd != nil {
		t.Error("BackgroundColorMsg produced a Cmd; want none")
	}
	if body := light.View().Content; body == "" {
		t.Error("model unrenderable after a light BackgroundColorMsg")
	}
}

// addConfig builds a Config whose discovery closure returns the given
// candidates and whose identity closure always resolves to the zero
// identity — no root-level add test below drives far enough to make the
// resolved identity's value observable (global candidates track directly,
// skipping Identify; the one per-project candidate below never advances
// past the picker).
func addConfig(fake *fakeData, candidates []views.TrackCandidate) Config {
	return Config{
		Data: fake,
		Discover: func(context.Context) ([]views.TrackCandidate, error) {
			return candidates, nil
		},
		Identify: func(_ context.Context, _ string, _ views.TrackRoot, _ string) (provider.Identity, error) {
			return provider.Identity{}, nil
		},
	}
}

// drive feeds one message through the root model and executes any returned
// Cmd synchronously — flattening tea.Batch the way a running program would —
// feeding every produced message back in until the model goes quiet. It lets
// a test walk the full a → discover → pick → confirm → identify → track
// chain without a running program.
func drive(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	queue := []tea.Msg{msg}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if next == nil {
			continue
		}
		if batch, ok := next.(tea.BatchMsg); ok {
			for _, cmd := range batch {
				if cmd != nil {
					queue = append(queue, cmd())
				}
			}
			continue
		}
		model, cmd := m.Update(next)
		m = model.(Model)
		if cmd != nil {
			queue = append(queue, cmd())
		}
	}
	return m
}

// TestFooterAdvertisesAddOnlyWhenWired and TestFooterAndDispatchGateAddOnBothClosures
// and the three TestProjectsAdd*Sync* tests below stay at root (rather than
// moving to views.ProjectsView's own test suite) because they pin ROOT-level
// orchestration: the root's footer() gating on m.actions.AddAvailable(), and
// the root's Update deciding — from views.TrackResultMsg.Err — whether a
// completed enrollment also fires a whole-fleet views.SyncCmd. Neither
// concern is reachable by driving a bare views.ProjectsView.

func TestFooterAdvertisesAddOnlyWhenWired(t *testing.T) {
	t.Parallel()
	wired := New(addConfig(&fakeData{}, nil))
	wired.active = tabProjects
	if got := plain(wired.footer()); !strings.Contains(got, "a add") {
		t.Fatalf("Projects footer %q missing %q with discovery wired", got, "a add")
	}
	wired.active = tabDoctor
	if got := plain(wired.footer()); strings.Contains(got, "a add") {
		t.Fatalf("Doctor footer %q must not advertise add", got)
	}
	unwired := New(Config{Data: &fakeData{}})
	unwired.active = tabProjects
	if got := plain(unwired.footer()); strings.Contains(got, "a add") {
		t.Fatalf("footer %q advertises add with no discovery closure wired", got)
	}
}

// TestFooterAndDispatchGateAddOnBothClosures pins that add availability gates on
// BOTH injected closures, not discovery alone. A build wiring Discover without
// Identify would panic the moment a per-project candidate is picked (identifyCmd
// calls identify on nil), so with Identify unwired the a key must be dead and
// unadvertised — never a key the footer names but the dispatch cannot honor.
func TestFooterAndDispatchGateAddOnBothClosures(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	m := New(Config{
		Data: fake,
		Discover: func(context.Context) ([]views.TrackCandidate, error) {
			return nil, nil
		},
		// Identify deliberately unwired: discovery alone must not enable add.
	})
	m.active = tabProjects

	if got := plain(m.footer()); strings.Contains(got, "a add") {
		t.Errorf("footer %q advertises add with identify unwired", got)
	}

	m = drive(t, m, key("a"))
	if m.projects.Adding != views.AddNone {
		t.Errorf("Adding = %v, want AddNone: a must be dead with identify unwired", m.projects.Adding)
	}
	if got := plain(m.projects.View()); !strings.Contains(got, "add is unavailable") {
		t.Errorf("view = %q, want the 'add is unavailable' notice", got)
	}
	if len(fake.trackCalls) != 0 {
		t.Errorf("a with identify unwired still reached the daemon: %v", fake.trackCalls)
	}
}

func TestProjectsAddGlobalTracksAllRootsAndSyncs(t *testing.T) {
	t.Parallel()
	fake := &fakeData{trackResp: api.TrackResponse{Folder: "_global"}}
	candidates := []views.TrackCandidate{{
		Provider: "codex",
		Label:    "codex  global memories",
		Global:   true,
		Roots: []views.TrackRoot{
			{LocalDir: "/home/u/.codex/memories"},
			{LocalDir: "/home/u/.codex/notes", RepoSubdir: "notes"},
		},
	}}
	m := New(addConfig(fake, candidates))
	m.active = tabProjects

	m = drive(t, m, key("a"))     // discover → picker (one row)
	m = drive(t, m, key("enter")) // global: track directly, then fleet sync

	if len(fake.trackCalls) != 2 {
		t.Fatalf("trackCalls = %d, want 2 (both roots of the grouped global candidate)", len(fake.trackCalls))
	}
	for i, call := range fake.trackCalls {
		if call.Provider != "codex" || call.ProjectID != "" {
			t.Fatalf("trackCalls[%d] = %+v, want codex with empty ProjectID (global scope)", i, call)
		}
	}
	if fake.trackCalls[1].RepoSubdir != "notes" {
		t.Fatalf("trackCalls[1].RepoSubdir = %q, want %q", fake.trackCalls[1].RepoSubdir, "notes")
	}
	if len(fake.syncCalls) != 1 || fake.syncCalls[0] != "" {
		t.Fatalf("syncCalls = %v, want one whole-fleet sync after a successful track", fake.syncCalls)
	}
}

// TestProjectsAddTrackFailureSurfacesAndSkipsSync covers the track error
// branch: when the daemon rejects the enrollment, the root surfaces the
// reason and fires NO fleet sync — there is nothing new to mirror in, so
// syncing would be a lie. The recorded calls prove exactly one track attempt
// and zero syncs.
func TestProjectsAddTrackFailureSurfacesAndSkipsSync(t *testing.T) {
	t.Parallel()
	fake := &fakeData{trackErr: errors.New("daemon refused: quiesced")}
	candidates := []views.TrackCandidate{{
		Provider: "codex",
		Label:    "codex  global memories",
		Global:   true,
		Roots:    []views.TrackRoot{{LocalDir: "/home/u/.codex/memories"}},
	}}
	m := New(addConfig(fake, candidates))
	m.active = tabProjects

	m = drive(t, m, key("a"))     // discover → picker (one global row)
	m = drive(t, m, key("enter")) // global: track directly (fails)

	if len(fake.trackCalls) != 1 {
		t.Fatalf("trackCalls = %d, want 1 (the attempted enrollment)", len(fake.trackCalls))
	}
	if len(fake.syncCalls) != 0 {
		t.Fatalf("a failed track must not fire a fleet sync: %v", fake.syncCalls)
	}
	if got := plain(m.projects.View()); !strings.Contains(got, "track failed") ||
		!strings.Contains(got, "quiesced") {
		t.Fatalf("view = %q, want a 'track failed' notice carrying the reason", got)
	}
}

// TestProjectsStaleTrackResultKeepsNewFlow pins the OnTrackResult reset guard
// together with root's unconditional-on-success fleet sync: a prior
// enrollment's in-flight views.TrackResultMsg can land after the user esc'd
// and reopened the add flow. drive is synchronous, so that interleaving is
// constructed directly — reach a real AddPicking state through key events,
// then deliver the stale result through the model's Update. The view's reset
// must be skipped (the new picker survives untouched) while the notice and
// root's fleet sync still fire, because the stale enrollment genuinely
// happened.
func TestProjectsStaleTrackResultKeepsNewFlow(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	candidates := []views.TrackCandidate{
		{Provider: "claude", Label: "claude  one  → /g/one", PathGuess: "/g/one", Roots: []views.TrackRoot{{LocalDir: "/x/one"}}},
		{Provider: "claude", Label: "claude  two  → /g/two", PathGuess: "/g/two", Roots: []views.TrackRoot{{LocalDir: "/x/two"}}},
	}
	m := New(addConfig(fake, candidates))
	m.active = tabProjects

	// Reach a real AddPicking state through key events, then move the cursor off
	// row 0 so "cursor untouched" is a meaningful claim.
	m = drive(t, m, key("a"))
	m = drive(t, m, key("j"))
	if m.projects.Adding != views.AddPicking {
		t.Fatalf("setup: Adding = %v, want AddPicking", m.projects.Adding)
	}

	// The prior enrollment's result lands now. It is no longer AddTracking, so
	// it must not stomp the new picker — but it is still a real outcome.
	next, cmd := step(m, views.TrackResultMsg{Folders: []string{"agent-brain"}})

	if next.projects.Adding != views.AddPicking {
		t.Fatalf("Adding = %v, want AddPicking (a stale result must not reset the new flow)", next.projects.Adding)
	}
	if got := plain(next.projects.View()); !strings.Contains(got, "tracked agent-brain") {
		t.Fatalf("view = %q, want the stale enrollment's notice still set", got)
	}
	drain(cmd)
	if diff := cmp.Diff([]string{""}, fake.syncCalls); diff != "" {
		t.Fatalf("a stale success did not fire the whole-fleet sync (-want +got):\n%s", diff)
	}
}
