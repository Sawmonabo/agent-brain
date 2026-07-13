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

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/actions"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/views"
	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/providertest"
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
// Code constant (2026-07-09). A "ctrl+x" name builds the modifier form
// (needed for ctrl+k, the open-palette shortcut) the same way msg.String()
// reports it back — verified against the ctrl+c quit path this suite already
// exercises.
func key(name string) tea.KeyPressMsg {
	switch name {
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	default:
		if rest, ok := strings.CutPrefix(name, "ctrl+"); ok {
			return tea.KeyPressMsg{Code: rune(rest[0]), Mod: tea.ModCtrl}
		}
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
	for _, want := range []string{"tab/1–4 switch", "↑/↓ select", "s sync", "u untrack", "ctrl+k palette", "? help", "q quit"} {
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
		for _, want := range []string{"tab/1–4 switch", "ctrl+k palette", "? help", "q quit"} {
			if !strings.Contains(otherFooter, want) {
				t.Errorf("%s footer %q missing %q", other.title(), otherFooter, want)
			}
		}
	}
}

// TestProjectsKeysStayDeadOffProjectsTab pins the behavior the old footer
// lied about: s/u on a non-Projects tab dispatch nothing and mutate nothing.
func TestProjectsKeysStayDeadOffProjectsTab(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	m := New(Config{Data: fake})
	m.active = tabConflicts

	m2, cmd := step(m, key("s"))
	if cmd != nil {
		t.Fatal("s on Conflicts produced a Cmd; want none")
	}
	_, cmd = step(m2, key("u"))
	if cmd != nil {
		t.Fatal("u on Conflicts produced a Cmd; want none")
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
// s/u/a/tab while the modal swallowed them (or routed them into a text input,
// so `q` typed a "q") is precisely the dead-key class the keymap contract
// exists to make impossible. ctrl+k and ? are genuinely dead here too — the
// modal-open check in handleKey runs before either global dispatch, so they
// must not leak into the modal footer any more than the tab-level keys do.
// The exact-equality check pins both the live set (render order included)
// and the absence of every other binding; the explicit tab-hint sweep
// documents the second half of the contract.
func TestFooterInModalStatesAdvertisesOnlyLiveKeys(t *testing.T) {
	t.Parallel()
	tabHints := []string{"s sync", "u untrack", "a add", "tab/1–4", "ctrl+k palette", "? help"}
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

// TestCtrlKOpensPaletteAndDispatches proves the palette and a direct
// keypress share one dispatch: ctrl+k opens the palette, typing "fleet"
// narrows to the sync-fleet action alone (its Title and ID are the only
// registry entries containing "fleet"), and enter both closes the palette
// and fires the exact same views.SyncCmd(data, "") a root-level fleet sync
// would use — recorded in the fake as a single call with an empty folder.
func TestCtrlKOpensPaletteAndDispatches(t *testing.T) {
	t.Parallel()
	fake := &fakeData{status: readyStatus(), syncResp: api.SyncResponse{Status: "completed"}}
	m := newTestModel(fake)
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})

	m = drive(t, m, key("ctrl+k"))
	if !m.paletteOpen {
		t.Fatal("ctrl+k did not open the palette")
	}

	for _, r := range "fleet" {
		m, _ = step(m, key(string(r)))
	}
	m = drive(t, m, key("enter"))

	if m.paletteOpen {
		t.Error("palette still open after choosing an action")
	}
	if diff := cmp.Diff([]string{""}, fake.syncCalls); diff != "" {
		t.Fatalf("sync-fleet via the palette did not fire a whole-fleet sync (-want +got):\n%s", diff)
	}
}

// TestCtrlKPaletteEscClosesWithoutDispatch proves esc inside the palette
// closes it without invoking anything — the negative half of "share one
// dispatch": a cancelled palette session must reach the daemon zero times.
func TestCtrlKPaletteEscClosesWithoutDispatch(t *testing.T) {
	t.Parallel()
	fake := &fakeData{status: readyStatus()}
	m := newTestModel(fake)
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})

	m = drive(t, m, key("ctrl+k"))
	m = drive(t, m, key("esc"))

	if m.paletteOpen {
		t.Error("esc did not close the palette")
	}
	if len(fake.syncCalls) != 0 || len(fake.untrackCalls) != 0 || len(fake.trackCalls) != 0 {
		t.Errorf("esc in the palette reached the daemon: sync=%v untrack=%v track=%v",
			fake.syncCalls, fake.untrackCalls, fake.trackCalls)
	}
}

// TestPaletteListsOnlyDispatchableActions is the completeness pin for the
// palette's availability gate: every action paletteAvailable admits must
// resolve to either dispatch's help special-case or a real entry in
// runners() — the two, and only two, things dispatch can actually run. This
// ranges over the REAL registry and the REAL predicate/runners map (not a
// synthetic stand-in), so a future action added to the registry without a
// runner would fail this test the moment it also became paletteAvailable.
// Both add-flow wirings run the invariant: with the closures absent,
// paletteAvailable skips add-project entirely, so only the wired variant
// walks that row — without it, deleting add-project's runner while keeping
// its paletteAvailable case would ship a dead palette row unnoticed.
func TestPaletteListsOnlyDispatchableActions(t *testing.T) {
	t.Parallel()
	discover := func(context.Context) ([]views.TrackCandidate, error) { return nil, nil }
	identify := func(context.Context, string, views.TrackRoot, string) (provider.Identity, error) {
		return provider.Identity{}, nil
	}
	variants := []struct {
		name            string
		model           Model
		wantAddAdmitted bool
	}{
		{name: "add flow unavailable", model: newTestModel(&fakeData{}), wantAddAdmitted: false},
		{
			name: "add flow wired",
			model: New(Config{
				Data:         &fakeData{},
				StartService: func() error { return nil },
				Discover:     discover,
				Identify:     identify,
			}),
			wantAddAdmitted: true,
		},
	}
	for _, tc := range variants {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.model.paletteAvailable("add-project"); got != tc.wantAddAdmitted {
				t.Errorf("paletteAvailable(%q) = %v, want %v for this wiring", "add-project", got, tc.wantAddAdmitted)
			}
			runners := tc.model.runners()
			for _, action := range actions.Registry() {
				if !tc.model.paletteAvailable(action.ID) {
					continue
				}
				if action.ID == "help" {
					continue // dispatch's one non-runner special case
				}
				if _, ok := runners[action.ID]; !ok {
					t.Errorf("paletteAvailable(%q) = true but dispatch has no runner for it (and it is not the help special-case)", action.ID)
				}
			}
		})
	}
}

// TestPaletteHidesDeadEndsFooterAndHelpStillAdvertiseThem pins the palette-
// scoped fix directly: switch-tabs and select have no runner and are never
// reachable through dispatch (handleKey consumes switch-tabs before the
// generic dispatch loop ever runs; select has no dispatch path at all), so
// choosing either from the palette used to close it and silently do
// nothing. Both must now be absent from the palette's own listing while the
// footer and help overlay — which advertise hints for keys the active
// view's own routing already honors directly, not dispatch — keep naming
// both (spec §2).
func TestPaletteHidesDeadEndsFooterAndHelpStillAdvertiseThem(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{})
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	m.active = tabProjects

	m = drive(t, m, key("ctrl+k"))
	paletteView := plain(m.palette.View())
	for _, deadEnd := range []string{"switch", "select"} {
		if strings.Contains(paletteView, deadEnd) {
			t.Errorf("palette view %q lists %q; it has no runner and dispatch can never reach it", paletteView, deadEnd)
		}
	}

	footer := plain(m.footer())
	for _, want := range []string{"tab/1–4 switch", "↑/↓ select"} {
		if !strings.Contains(footer, want) {
			t.Errorf("footer %q missing %q; hiding a dead end from the palette must not hide its footer hint", footer, want)
		}
	}

	help := plain(views.NewHelpModel(m.styles).View())
	for _, want := range []string{"switch", "select"} {
		if !strings.Contains(help, want) {
			t.Errorf("help overlay %q missing %q; help documents every registered action unconditionally", help, want)
		}
	}
}

// TestEscAtRootPromptsBeforeQuit pins spec §2's root chrome: esc with
// nothing else owning the keyboard asks before quitting rather than quitting
// outright (that stays q's job, still immediate); n dismisses the prompt and
// the model keeps running; esc again reopens the SAME prompt, and y then
// actually quits.
func TestEscAtRootPromptsBeforeQuit(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{status: readyStatus()})
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})

	m, cmd := step(m, key("esc"))
	if !m.quitPrompt {
		t.Fatal("esc at root did not open the quit prompt")
	}
	if cmd != nil {
		t.Error("opening the quit prompt produced a Cmd; want none")
	}
	if got := plain(m.footer()); !strings.Contains(got, "quit agent-brain? (y/n)") {
		t.Errorf("footer %q does not show the quit prompt", got)
	}

	m, _ = step(m, key("n"))
	if m.quitPrompt {
		t.Error("n did not dismiss the quit prompt")
	}
	if m.quitting {
		t.Error("n must not quit")
	}

	m, _ = step(m, key("esc"))
	if !m.quitPrompt {
		t.Fatal("esc did not reopen the quit prompt")
	}
	m, cmd = step(m, key("y"))
	if !m.quitting {
		t.Error("y at the quit prompt did not quit")
	}
	if cmd == nil {
		t.Fatal("expected a tea.Quit Cmd")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("Cmd did not produce a QuitMsg")
	}
}

// TestQuiescedMutationRefusedLocally pins spec §15's functional half of the
// Mutates gate: pressing u while the daemon is quiesced must refuse LOCALLY,
// before the untrack confirm even opens — never reaching the daemon and
// never leaving a confirm dialog open that a subsequent y would act on — and
// must toast the refusal so the user learns why nothing happened.
func TestQuiescedMutationRefusedLocally(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	fake := &fakeData{}
	m := newTestModel(fake)
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	m.now = now
	m.status = api.StatusResponse{State: "ready", QuiescedUntil: &future}
	m.active = tabProjects
	m.projects.SetUnits([]api.UnitInfo{{Provider: "claude", Folder: "agent-brain", LocalDir: "/home/u/.claude/projects/agent-brain/memory"}})

	next, cmd := step(m, key("u"))
	drain(cmd)

	if len(fake.untrackCalls) != 0 {
		t.Fatalf("untrack reached the daemon while quiesced: %v", fake.untrackCalls)
	}
	if next.projects.Confirming {
		t.Error("untrack confirm opened while quiesced; the refusal must happen before it")
	}
	if next.toast == nil {
		t.Fatal("no toast pushed for the refused mutation")
	}
	if !strings.Contains(next.toast.text, "quiesced") {
		t.Errorf("toast text = %q, want it to name the quiesce refusal", next.toast.text)
	}
}

// TestQuiescedMutationOffProjectsTabStaysSilent guards the false-positive
// side of the same gate: u is already a dead key off the Projects tab
// (TestProjectsKeysStayDeadOffProjectsTab), and quiescing must not start
// toasting a refusal for a key that was never going to do anything anyway.
func TestQuiescedMutationOffProjectsTabStaysSilent(t *testing.T) {
	t.Parallel()
	future := time.Now().Add(time.Hour)
	m := newTestModel(&fakeData{})
	m.status = api.StatusResponse{State: "ready", QuiescedUntil: &future}
	m.active = tabConflicts

	next, cmd := step(m, key("u"))
	if cmd != nil {
		t.Error("u off the Projects tab produced a Cmd")
	}
	if next.toast != nil {
		t.Errorf("toast = %+v, want none — u does nothing on this tab regardless of quiesce", next.toast)
	}
}

// TestToastExpiresOnTick pins the toast lifecycle the brief specifies: a 5s
// TTL checked on the existing 2s poll tick, no extra timer. A tick well
// inside the window leaves it up; a tick past it clears it.
func TestToastExpiresOnTick(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{})
	start := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	m.now = start
	m.pushToast("test toast")
	if m.toast == nil {
		t.Fatal("pushToast did not set a toast")
	}

	m, _ = step(m, tickMsg(start.Add(2*time.Second)))
	if m.toast == nil {
		t.Fatal("toast cleared before its 5s TTL elapsed")
	}

	m, _ = step(m, tickMsg(start.Add(6*time.Second)))
	if m.toast != nil {
		t.Errorf("toast still set after its TTL elapsed: %+v", m.toast)
	}
}

// TestToastRendersInStatusHeaderRegion pins spec §2's placement: "status
// bar: daemon state · version · update banner · toasts" groups a toast with
// the persistent header, not between the active view's body and the
// footer. A pushed toast must render after the header's own daemon-state
// text and before the tab bar (and therefore before the body/footer that
// follow it).
func TestToastRendersInStatusHeaderRegion(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{})
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	m.status = readyStatus()
	m.pushToast("local refusal notice")

	body := plain(m.View().Content)
	headerIdx := strings.Index(body, "daemon:")
	toastIdx := strings.Index(body, "local refusal notice")
	tabBarIdx := strings.Index(body, "[1 Projects]")

	if headerIdx == -1 || toastIdx == -1 || tabBarIdx == -1 {
		t.Fatalf("expected header, toast, and tab bar all present in the view; got:\n%s", body)
	}
	if headerIdx >= toastIdx || toastIdx >= tabBarIdx {
		t.Errorf("toast not grouped with the status header (spec §2): header@%d toast@%d tabBar@%d\nview:\n%s",
			headerIdx, toastIdx, tabBarIdx, body)
	}
}

// TestHelpOpensAndAnyKeyCloses pins the ? overlay's own tiny lifecycle: it
// replaces the whole body while open, and any key — not just esc — closes
// it, per spec §14's "any-key closes".
func TestHelpOpensAndAnyKeyCloses(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{status: readyStatus()})
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})

	m, _ = step(m, key("?"))
	if !m.helpOpen {
		t.Fatal("? did not open the help overlay")
	}
	if got := plain(m.View().Content); !strings.Contains(got, "Keymap") {
		t.Errorf("help view %q missing its title", got)
	}

	m, _ = step(m, key("x"))
	if m.helpOpen {
		t.Error("an arbitrary key did not close the help overlay")
	}
}

// browserRegistry builds a minimal one-provider registry good enough to
// exercise the root's enter-to-browse wiring (buildBrowserDeps →
// memoryfs.List) end to end. Duplicated from views' own browser_test.go
// fixture rather than exported for tests only — the same rule readyStatus
// above states: dashboard_test.go is a different package, and this fixture
// is small enough that duplicating it beats inventing a test-only export.
func browserRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	fake := providertest.New("claude", provider.ScopePerProject, nil)
	registry, err := provider.NewRegistry(fake)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

// TestEnterOnProjectsPushesBrowser pins the Screen stack's one entry point
// from a tab view and its own exit back out: enter on a selected Projects
// row cannot build a *views.Browser itself (views.ProjectsView has none of
// Registry/Styles/memoryfs/glamour) — it emits views.OpenFolderMsg, and the
// root, the only place with all of those, resolves it into an actual Screen
// and pushes it. This is the seam every later screen's own drill-in
// (Task 12's Reading, Task 14's History) will reuse. esc then pops the
// identical round trip back to the Projects tab, proving the stack is not
// just push-only.
func TestEnterOnProjectsPushesBrowser(t *testing.T) {
	t.Parallel()
	registry := browserRegistry(t)
	m := New(Config{Data: &fakeData{}, Registry: registry})
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	m.active = tabProjects
	m.projects.SetUnits([]api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: t.TempDir()}})

	m = drive(t, m, key("enter"))

	if len(m.stack) != 1 {
		t.Fatalf("stack depth = %d after enter, want 1", len(m.stack))
	}
	browser, ok := m.stack[0].(*views.Browser)
	if !ok {
		t.Fatalf("stack top = %T, want *views.Browser", m.stack[0])
	}
	if got := browser.Title(); got != "acme" {
		t.Errorf("pushed browser Title() = %q, want %q", got, "acme")
	}
	body := plain(m.View().Content)
	if !strings.Contains(body, "Projects ▸ acme") {
		t.Errorf("view missing the breadcrumb; got:\n%s", body)
	}
	if !strings.Contains(body, "Memory browser: acme") {
		t.Errorf("view missing the pushed browser's own body; got:\n%s", body)
	}

	// esc must pop the full round trip back to the Projects tab: the
	// pushed Browser's own esc key produces a Cmd whose message is
	// PopScreenMsg (Browser.updateKey, browser.go), and the root — the
	// only place that ever mutates m.stack — must act on that message,
	// not just accept the key.
	m = drive(t, m, key("esc"))
	if len(m.stack) != 0 {
		t.Fatalf("stack depth = %d after esc, want 0 (esc must pop back to the tab)", len(m.stack))
	}
	restored := plain(m.View().Content)
	if strings.Contains(restored, "Memory browser: acme") {
		t.Errorf("view still shows the browser after esc popped it; got:\n%s", restored)
	}
	if !strings.Contains(restored, "Projects") {
		t.Errorf("view missing the Projects tab body after esc popped back; got:\n%s", restored)
	}
}

// TestStackForwardsTick pins that the shared 2s poll keeps a pushed screen
// live: RefreshMsg reaches the top of the stack on every tick (proved here
// by a List call count that only a forwarded refresh can advance beyond
// construction's own first call), the root's own reload still runs
// alongside it, and forwarding neither pushes nor pops anything on its own.
func TestStackForwardsTick(t *testing.T) {
	t.Parallel()
	var listCalls int
	browser := views.NewBrowser(views.BrowserDeps{
		Folder:   "acme",
		Now:      time.Now,
		ReadBody: func(memoryfs.Memory) (string, error) { return "", nil },
		List: func() ([]memoryfs.Memory, error) {
			listCalls++
			return nil, nil
		},
	})
	afterConstruct := listCalls
	if afterConstruct == 0 {
		t.Fatal("setup: NewBrowser did not call List")
	}

	m := newTestModel(&fakeData{})
	m, cmd := step(m, views.PushScreenMsg{Screen: browser})
	if cmd != nil {
		t.Fatal("PushScreenMsg produced a Cmd; want none")
	}

	next, tickCmd := step(m, tickMsg(time.Now()))
	msgs := drain(tickCmd)
	if !containsMsg[tickMsg](msgs) {
		t.Error("tick while browsing did not reschedule the next tick")
	}
	if !containsMsg[statusMsg](msgs) {
		t.Error("tick while browsing did not still refresh root status")
	}
	if listCalls <= afterConstruct {
		t.Errorf("List calls = %d after a tick, want more than %d (construction alone) — RefreshMsg was not forwarded to the pushed screen", listCalls, afterConstruct)
	}
	if len(next.stack) != 1 {
		t.Errorf("stack depth = %d after a tick, want 1 (a tick must not itself push or pop)", len(next.stack))
	}
}

// TestPopScreenOnEmptyStackIsNoOp covers the brief's named edge case
// directly: PopScreenMsg arriving with nothing on the stack (a stray or
// duplicate pop, or simply a program that never pushed anything) must not
// panic on an out-of-range slice and must leave the stack empty.
func TestPopScreenOnEmptyStackIsNoOp(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{})
	if len(m.stack) != 0 {
		t.Fatalf("setup: stack depth = %d, want 0", len(m.stack))
	}

	next, cmd := step(m, views.PopScreenMsg{})
	if cmd != nil {
		t.Error("popping an empty stack produced a Cmd; want none")
	}
	if len(next.stack) != 0 {
		t.Errorf("stack depth = %d after popping an empty stack, want 0", len(next.stack))
	}
}

// TestBackgroundColorSwapsWhileBrowsing extends TestBackgroundColorSwapsPalette
// to a pushed screen: SetStyles/SetRender are not part of the Screen
// interface (Update/View/Title only), so the root reaches an
// already-pushed *Browser through applyStackTheme, the same explicit
// propagation withStyles already gives every tab view. The dark and light
// renders of the identical preview content must differ at the byte level
// (proving the swap actually reached the pushed browser's render seam, not
// just a freshly constructed one) while the visible text survives both.
func TestBackgroundColorSwapsWhileBrowsing(t *testing.T) {
	t.Parallel()
	browser := views.NewBrowser(views.BrowserDeps{
		Folder:   "acme",
		Now:      time.Now,
		ReadBody: func(memoryfs.Memory) (string, error) { return "# Heading", nil },
		List: func() ([]memoryfs.Memory, error) {
			return []memoryfs.Memory{{Provider: "claude", Name: "Note", RepoPath: "claude/note.md"}}, nil
		},
	})
	m := newTestModel(&fakeData{})
	m, _ = step(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m, cmd := step(m, views.PushScreenMsg{Screen: browser})
	if cmd != nil {
		t.Fatal("PushScreenMsg produced a Cmd; want none")
	}
	if len(m.stack) != 1 {
		t.Fatalf("setup: stack depth = %d, want 1", len(m.stack))
	}

	dark, cmd := step(m, tea.BackgroundColorMsg{Color: color.Black})
	if cmd != nil {
		t.Error("BackgroundColorMsg while browsing produced a Cmd; want none")
	}
	if len(dark.stack) != 1 {
		t.Fatal("BackgroundColorMsg dropped the pushed browser from the stack")
	}
	darkRaw := dark.View().Content
	if !strings.Contains(plain(darkRaw), "Heading") {
		t.Fatalf("dark view lost the preview content; got:\n%s", plain(darkRaw))
	}

	light, cmd := step(dark, tea.BackgroundColorMsg{Color: color.White})
	if cmd != nil {
		t.Error("BackgroundColorMsg while browsing produced a Cmd; want none")
	}
	lightRaw := light.View().Content
	if !strings.Contains(plain(lightRaw), "Heading") {
		t.Fatalf("light view lost the preview content; got:\n%s", plain(lightRaw))
	}

	if darkRaw == lightRaw {
		t.Error("dark and light preview renders are byte-identical; the theme swap did not reach the pushed browser's render seam")
	}
}

// TestStackFooterAdvertisesScopedKeys pins the footer's scope switch: while
// a screen is pushed, the footer must name exactly that screen's own keys
// (ScopeBrowser's o/(/)/esc) and nothing from the tab level or
// ScopeGlobal — a pushed screen intercepts every key before either would
// ever be reached (handleKey), so naming them would advertise dead keys,
// exactly the dishonesty TestFooterInModalStatesAdvertisesOnlyLiveKeys
// already forbids for a Projects modal.
func TestStackFooterAdvertisesScopedKeys(t *testing.T) {
	t.Parallel()
	browser := views.NewBrowser(views.BrowserDeps{
		Folder:   "acme",
		Now:      time.Now,
		ReadBody: func(memoryfs.Memory) (string, error) { return "", nil },
		List:     func() ([]memoryfs.Memory, error) { return nil, nil },
	})
	m := newTestModel(&fakeData{})
	m, _ = step(m, views.PushScreenMsg{Screen: browser})

	got := plain(m.footer())
	for _, want := range []string{"o order", "/ filter", "esc back"} {
		if !strings.Contains(got, want) {
			t.Errorf("stack footer %q missing %q", got, want)
		}
	}
	for _, deadHint := range []string{"tab/1–4", "ctrl+k palette", "? help", "s sync", "↑/↓ select"} {
		if strings.Contains(got, deadHint) {
			t.Errorf("stack footer %q leaks a tab-level or global hint %q; a pushed screen owns the whole keyboard", got, deadHint)
		}
	}
}
