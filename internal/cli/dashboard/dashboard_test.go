package dashboard

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
)

// csiPattern matches the CSI escape sequences lipgloss emits — ESC '[', numeric
// parameters, a letter terminator (SGR colour/attributes end in 'm'). The views
// render only styled text, so this is the whole escape surface in a View string.
var csiPattern = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// plain strips styling so assertions match the visible text. This realises the
// brief's "styling forced plain in tests": lipgloss v2 emits ANSI
// unconditionally at Render and has no plain-render mode — colour downgrade
// happens only at the colorprofile writer (verified against the resolved module
// 2026-07-09, where lipgloss.Writer *is* a colorprofile writer). Stripping the
// escapes here keeps the dashboard package's import set exactly the reviewed
// allowlist, with no extra dependency pulled in for test scaffolding.
func plain(s string) string {
	return csiPattern.ReplaceAllString(s, "")
}

// fakeData is an injectable dashboardData: canned reads, and recorded mutating
// calls so a test can prove what a key press actually did. No socket, no
// filesystem, no doctor battery.
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

func newTestModel(data dashboardData) Model {
	return New(Config{Data: data, StartService: func() error { return nil }})
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
