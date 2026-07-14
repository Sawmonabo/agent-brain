package dashboard

import (
	"context"
	"errors"
	"image/color"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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
	migrateResp api.MigrateResponse
	migrateErr  error

	syncCalls    []string
	untrackCalls []api.UntrackRequest
	trackCalls   []api.TrackRequest
	migrateCalls []api.MigrateRequest
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

func (f *fakeData) Migrate(_ context.Context, req api.MigrateRequest) (api.MigrateResponse, error) {
	f.migrateCalls = append(f.migrateCalls, req)
	return f.migrateResp, f.migrateErr
}

func (f *fakeData) Doctor(context.Context) (doctor.Report, error) {
	return f.report, f.doctorErr
}

func (f *fakeData) Conflicts() ([]config.ConflictRecord, error) {
	return f.conflicts, f.conflictsErr
}

// History and Blob satisfy the grown DataSource surface (Task 14). Root-level
// tests here drive the restore LAND path directly (RestoreRequestMsg already
// carries the fetched blob), so these answer empty; the History screen's own
// suite exercises the read funnel through a dedicated fake.
func (f *fakeData) History(context.Context, string, string, int) (api.HistoryResponse, error) {
	return api.HistoryResponse{}, nil
}

func (f *fakeData) Blob(context.Context, string, string, string) (api.BlobResponse, error) {
	return api.BlobResponse{}, nil
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
	case "space":
		// The multi-select picker's toggle: Code 0x20 stringifies to "space",
		// which is what DashboardKeys.Toggle matches — the rune-default arm would
		// wrongly build an 's' press for the name "space", so it is explicit here.
		return tea.KeyPressMsg{Code: ' ', Text: " "}
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
		{name: "add picking", stage: views.AddPicking, want: "↑/↓ select · space toggle · enter confirm · esc cancel"},
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
	if got := plain(m.projects.View("")); !strings.Contains(got, "add is unavailable") {
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
	m = drive(t, m, key("space")) // select the global candidate
	m = drive(t, m, key("enter")) // confirm the set → global: track directly, then fleet sync

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
	m = drive(t, m, key("space")) // select the global candidate
	m = drive(t, m, key("enter")) // confirm the set → global: track directly (fails)

	if len(fake.trackCalls) != 1 {
		t.Fatalf("trackCalls = %d, want 1 (the attempted enrollment)", len(fake.trackCalls))
	}
	if len(fake.syncCalls) != 0 {
		t.Fatalf("a failed track must not fire a fleet sync: %v", fake.syncCalls)
	}
	if got := plain(m.projects.View("")); !strings.Contains(got, "track failed") ||
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
	if got := plain(next.projects.View("")); !strings.Contains(got, "tracked agent-brain") {
		t.Fatalf("view = %q, want the stale enrollment's notice still set", got)
	}
	drain(cmd)
	if diff := cmp.Diff([]string{""}, fake.syncCalls); diff != "" {
		t.Fatalf("a stale success did not fire the whole-fleet sync (-want +got):\n%s", diff)
	}
}

// migrateConfig builds a Config wiring all four migrate closures (spec §10):
// preflightErr gates the flow, candidates feed discovery, identity resolves the
// confirmed path, and LiveDirFor is inert (the root tests accept the guessed
// path unchanged, so the candidate's own LiveDir is used). Identify is the
// SHARED closure New threads into both the track and migrate bundles.
func migrateConfig(fake *fakeData, preflightErr error, candidates []views.MigrateCandidate, identity provider.Identity) Config {
	return Config{
		Data:             fake,
		MigratePreflight: func(context.Context) error { return preflightErr },
		LegacyDiscover: func(context.Context) ([]views.MigrateCandidate, error) {
			return candidates, nil
		},
		Identify: func(_ context.Context, _ string, _ views.TrackRoot, _ string) (provider.Identity, error) {
			return identity, nil
		},
		LiveDirFor: func(_, _ string) (string, error) { return "", nil },
	}
}

// migrateCandidateFixture is the root migrate tests' shared candidate — a
// bash-era store whose guessed path resolves to a remote-backed project.
func migrateCandidateFixture() []views.MigrateCandidate {
	return []views.MigrateCandidate{{
		Provider: "claude", Slug: "-g-acme", SeedDir: "/home/u/.agent-brain/-g-acme",
		PathGuess: "/g/acme", LiveDir: "/home/u/.claude/projects/-g-acme/memory",
	}}
}

// TestMigrateSuccessToastsAndFleetSyncs pins the root's post-migrate
// orchestration (spec §10): a completed import surfaces the success toast in the
// info slot AND fires the whole-fleet sync (the add flow's post-track idiom), so
// the seeded memory's first mirror-in is visible immediately.
func TestMigrateSuccessToastsAndFleetSyncs(t *testing.T) {
	t.Parallel()
	fake := &fakeData{migrateResp: api.MigrateResponse{Folder: "acme", Files: 4}}
	identity := provider.Identity{ProjectID: "github.com/o/acme", PreferredFolder: "acme"}
	m := New(migrateConfig(fake, nil, migrateCandidateFixture(), identity))
	m.active = tabProjects

	m = drive(t, m, key("m"))     // preflight → discover → picker
	m = drive(t, m, key("enter")) // pick → path confirm
	m = drive(t, m, key("enter")) // accept path → identify → migrate → toast + fleet sync

	if len(fake.migrateCalls) != 1 {
		t.Fatalf("migrateCalls = %v, want exactly one", fake.migrateCalls)
	}
	if m.toast == nil || !strings.Contains(m.toast.text, "migrated -g-acme → acme (4 files)") {
		t.Fatalf("toast = %v, want the migrate success text", m.toast)
	}
	if diff := cmp.Diff([]string{""}, fake.syncCalls); diff != "" {
		t.Fatalf("a successful migrate did not fire the whole-fleet sync (-want +got):\n%s", diff)
	}
}

// TestMigrateSkippedResultToastsEnrolledOnly covers the daemon's idempotency
// marker (spec §10): an already-imported store re-seeds nothing (Skipped) but
// still enrolls the live dir, so the toast says so and the fleet sync still
// fires.
func TestMigrateSkippedResultToastsEnrolledOnly(t *testing.T) {
	t.Parallel()
	fake := &fakeData{migrateResp: api.MigrateResponse{Folder: "acme", Skipped: true}}
	identity := provider.Identity{ProjectID: "github.com/o/acme", PreferredFolder: "acme"}
	m := New(migrateConfig(fake, nil, migrateCandidateFixture(), identity))
	m.active = tabProjects

	m = drive(t, m, key("m"))
	m = drive(t, m, key("enter"))
	m = drive(t, m, key("enter"))

	if m.toast == nil || !strings.Contains(m.toast.text, "already imported — enrolled only") {
		t.Fatalf("toast = %v, want the Skipped 'already imported' wording", m.toast)
	}
	if diff := cmp.Diff([]string{""}, fake.syncCalls); diff != "" {
		t.Fatalf("a skipped migrate did not fire the fleet sync (-want +got):\n%s", diff)
	}
}

// TestMigratePreflightFailureStickyAborts pins the point-of-no-return gate's
// refusal (spec §10): a non-empty chezmoi diff aborts the flow before discovery
// and surfaces the error VERBATIM in the sticky (action-required) slot — never a
// 5s info toast — so the user must reconcile it before retrying.
func TestMigratePreflightFailureStickyAborts(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	preflightErr := errors.New("migrate: pre-flight chezmoi diff is NOT empty (spec §10)")
	m := New(migrateConfig(fake, preflightErr, migrateCandidateFixture(), provider.Identity{}))
	m.active = tabProjects

	m = drive(t, m, key("m")) // preflight fails → sticky verbatim, flow aborts

	if m.projects.Migrating != views.MigrateNone {
		t.Fatalf("Migrating = %v, want MigrateNone after a failed preflight", m.projects.Migrating)
	}
	if m.stickyToast == nil || m.stickyToast.text != preflightErr.Error() {
		t.Fatalf("stickyToast = %v, want the verbatim preflight error", m.stickyToast)
	}
	if len(fake.migrateCalls) != 0 {
		t.Fatalf("a failed preflight still migrated: %v", fake.migrateCalls)
	}
}

// TestFooterAndDispatchGateMigrateOnClosures pins that migrate availability
// gates on the injected closures (MigrateAvailable): fully wired, the footer
// advertises m and the key drives the flow; unwired, m is dead and unadvertised
// — never a key the footer names but the dispatch cannot honor.
func TestFooterAndDispatchGateMigrateOnClosures(t *testing.T) {
	t.Parallel()
	wired := New(migrateConfig(&fakeData{}, nil, migrateCandidateFixture(), provider.Identity{}))
	wired.active = tabProjects
	if got := plain(wired.footer()); !strings.Contains(got, "m migrate") {
		t.Fatalf("footer %q missing %q with migrate wired", got, "m migrate")
	}
	wired.active = tabDoctor
	if got := plain(wired.footer()); strings.Contains(got, "m migrate") {
		t.Fatalf("Doctor footer %q must not advertise migrate", got)
	}

	fake := &fakeData{}
	unwired := New(Config{Data: fake})
	unwired.active = tabProjects
	if got := plain(unwired.footer()); strings.Contains(got, "m migrate") {
		t.Fatalf("footer %q advertises migrate with no closures wired", got)
	}
	unwired = drive(t, unwired, key("m"))
	if unwired.projects.Migrating != views.MigrateNone {
		t.Errorf("Migrating = %v, want MigrateNone: m must be dead unwired", unwired.projects.Migrating)
	}
	if got := plain(unwired.projects.View("")); !strings.Contains(got, "migrate is unavailable") {
		t.Errorf("view = %q, want the 'migrate is unavailable' notice", got)
	}
	if len(fake.migrateCalls) != 0 {
		t.Errorf("m with closures unwired still reached the daemon: %v", fake.migrateCalls)
	}
}

// TestFooterInMigrateModalAdvertisesLiveKeys is the migrate twin of the add
// modal-footer honesty test: while a migrate stage owns the keyboard the footer
// advertises EXACTLY that stage's live keys (single-select — no space-toggle)
// and none of the tab-level hints.
func TestFooterInMigrateModalAdvertisesLiveKeys(t *testing.T) {
	t.Parallel()
	tabHints := []string{"s sync", "u untrack", "a add", "m migrate", "tab/1–4", "ctrl+k palette", "? help"}
	tests := []struct {
		name  string
		stage views.MigrateStage
		want  string
	}{
		{name: "preflighting", stage: views.MigratePreflighting, want: "esc cancel"},
		{name: "discovering", stage: views.MigrateDiscovering, want: "esc cancel"},
		{name: "picking", stage: views.MigratePicking, want: "↑/↓ select · enter confirm · esc cancel"},
		{name: "confirm path", stage: views.MigrateConfirmPath, want: "enter confirm · esc cancel"},
		{name: "identifying", stage: views.MigrateIdentifying, want: "esc cancel"},
		{name: "naming folder", stage: views.MigrateNamingFolder, want: "enter confirm · esc cancel"},
		{name: "migrating", stage: views.MigrateMigrating, want: "esc cancel"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			m := newTestModel(&fakeData{})
			m.active = tabProjects
			m.projects.Migrating = testCase.stage

			got := plain(m.footer())
			if got != testCase.want {
				t.Errorf("migrate modal footer = %q, want %q", got, testCase.want)
			}
			for _, hint := range tabHints {
				if strings.Contains(got, hint) {
					t.Errorf("migrate modal footer %q leaks tab-level hint %q", got, hint)
				}
			}
		})
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
// resolve to either one of dispatch's direct special-cases (help, search —
// pure state flips with no Cmd) or a real entry in runners() — the only
// things dispatch can actually run. This
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
		// migrate wired (add's Discover deliberately absent, so add-project stays
		// unavailable): this variant makes migrate paletteAvailable so the invariant
		// below actually walks its row and proves it has a runner — the same reason
		// the add-flow-wired variant exists.
		{
			name:            "migrate flow wired",
			model:           New(migrateConfig(&fakeData{}, nil, nil, provider.Identity{})),
			wantAddAdmitted: false,
		},
		// An offered update makes update-agent-brain paletteAvailable — it is a
		// dispatch special-case (no runner), so this variant walks that row
		// through the invariant below rather than leaving the special-case
		// untested (the same reason the add-flow-wired variant exists).
		{name: "update offered", model: offeredUpdateModel(), wantAddAdmitted: false},
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
				if action.ID == "help" || action.ID == "search" || action.ID == "update-agent-brain" {
					continue // dispatch's non-runner special cases
				}
				if _, ok := runners[action.ID]; !ok {
					t.Errorf("paletteAvailable(%q) = true but dispatch has no runner for it (and it is not a dispatch special-case)", action.ID)
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

// TestStickyToastPersistsAcrossTicks pins the sticky slot's defining
// property: an error/action-required toast outlives the info TTL. Both slots
// are pushed, then ticks are driven well past toastTTL — the info line
// expires on schedule, the sticky line stays until it is dismissed.
func TestStickyToastPersistsAcrossTicks(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{status: readyStatus()})
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	start := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	m.now = start
	m.pushStickyToast("save failed — your edit is kept at /scratch/x.md")
	m.pushToast("path: /home/u/.claude/x.md")
	if m.stickyToast == nil || m.toast == nil {
		t.Fatal("setup: both toast slots should be populated")
	}

	m, _ = step(m, tickMsg(start.Add(2*time.Second)))
	m, _ = step(m, tickMsg(start.Add(8*time.Second))) // past the 5s info TTL

	if m.toast != nil {
		t.Errorf("info toast survived past its TTL: %+v", m.toast)
	}
	if m.stickyToast == nil {
		t.Fatal("sticky toast expired on a tick; it must persist until dismissed")
	}
	if got := plain(m.toastLine()); !strings.Contains(got, "save failed") {
		t.Errorf("toastLine = %q, want the sticky line still rendered", got)
	}
}

// TestEscAtRootDismissesStickyBeforeQuitPrompt pins the esc ordering: esc
// consumes internal state first, so a present sticky toast is dismissed
// before esc escalates to the quit prompt; only a second esc (no sticky
// left) opens the prompt. q still quits directly regardless — a sticky
// informs, it never traps.
func TestEscAtRootDismissesStickyBeforeQuitPrompt(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{status: readyStatus()})
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	m.pushStickyToast("capture failed: push: remote hung up")

	m, _ = step(m, key("esc"))
	if m.stickyToast != nil {
		t.Error("esc did not dismiss the sticky toast")
	}
	if m.quitPrompt {
		t.Error("esc opened the quit prompt while a sticky toast was present; it must dismiss the sticky first")
	}

	m, _ = step(m, key("esc"))
	if !m.quitPrompt {
		t.Error("esc with no sticky present did not open the quit prompt")
	}

	// q quits directly even with a sticky toast present.
	q := newTestModel(&fakeData{status: readyStatus()})
	q, _ = step(q, tea.WindowSizeMsg{Width: 110, Height: 40})
	q.pushStickyToast("save failed — your edit is kept at /scratch/x.md")
	q, cmd := step(q, key("q"))
	if !q.quitting {
		t.Error("q did not quit while a sticky toast was present")
	}
	if cmd == nil {
		t.Fatal("q produced no Cmd; want tea.Quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("q's Cmd did not produce a QuitMsg")
	}
}

// TestInfoToastTTLMeasuresVisibility pins the visibility-measured TTL: an
// info toast pushed while chrome covers the status area does not start its
// clock until the chrome closes, so it can never expire unseen. The mirror
// case (no chrome at push) behaves exactly like the pre-two-slot toast —
// stamped at push, expiring five seconds later.
func TestInfoToastTTLMeasuresVisibility(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	t.Run("hidden under chrome does not expire", func(t *testing.T) {
		t.Parallel()
		m := newTestModel(&fakeData{status: readyStatus()})
		m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
		m.now = start
		m.openSearchOverlay() // chrome now covers the status area
		m.pushToast("path: /home/u/x.md")
		if m.toast == nil || !m.toast.visibleSince.IsZero() {
			t.Fatalf("info toast pushed under chrome must have a zero visibleSince, got %+v", m.toast)
		}

		// Ticks well past the TTL while still covered: no stamp, no expiry.
		m, _ = step(m, tickMsg(start.Add(3*time.Second)))
		m, _ = step(m, tickMsg(start.Add(9*time.Second)))
		if m.toast == nil {
			t.Fatal("info toast expired while hidden under chrome; the TTL must measure visibility, not age")
		}
		if !m.toast.visibleSince.IsZero() {
			t.Errorf("visibleSince stamped while chrome still covered the status area: %+v", m.toast)
		}

		// Close the overlay; the next tick stamps visibleSince from m.now.
		m.searchOverlay = nil
		stampAt := start.Add(10 * time.Second)
		m, _ = step(m, tickMsg(stampAt))
		if m.toast == nil || m.toast.visibleSince != stampAt {
			t.Fatalf("first uncovered tick did not stamp visibleSince=%v: %+v", stampAt, m.toast)
		}

		// Survives until stampAt+TTL, gone after — measured from the stamp.
		m, _ = step(m, tickMsg(stampAt.Add(toastTTL-time.Second)))
		if m.toast == nil {
			t.Error("info toast expired before its visible TTL elapsed")
		}
		m, _ = step(m, tickMsg(stampAt.Add(toastTTL)))
		if m.toast != nil {
			t.Errorf("info toast survived past visibleSince+TTL: %+v", m.toast)
		}
	})

	t.Run("no chrome behaves like today", func(t *testing.T) {
		t.Parallel()
		m := newTestModel(&fakeData{status: readyStatus()})
		m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
		m.now = start
		m.pushToast("path: /home/u/x.md")
		if m.toast == nil || m.toast.visibleSince != start {
			t.Fatalf("info toast pushed with no chrome must stamp visibleSince at push: %+v", m.toast)
		}
		m, _ = step(m, tickMsg(start.Add(toastTTL-time.Second)))
		if m.toast == nil {
			t.Error("info toast expired before its TTL")
		}
		m, _ = step(m, tickMsg(start.Add(toastTTL)))
		if m.toast != nil {
			t.Errorf("info toast survived past its TTL: %+v", m.toast)
		}
	})
}

// TestStickyAndInfoRenderTogether pins the two-line render: both slots
// populated render two lines, sticky first, and the sticky line goes through
// the ToastSticky style. A sentinel-Render theme (ToastSticky wraps its
// output in a marker) makes the style routing observable through the CSI
// strip.
func TestStickyAndInfoRenderTogether(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{status: readyStatus()})
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	const stickySentinel = "<<STICKY>>"
	m.styles.ToastSticky = lipgloss.NewStyle().Transform(func(s string) string { return stickySentinel + s })
	m.pushStickyToast("capture failed: push: remote hung up")
	m.pushToast("path: /home/u/x.md")

	line := m.toastLine()
	plainLine := plain(line)
	stickyIdx := strings.Index(plainLine, "capture failed")
	infoIdx := strings.Index(plainLine, "path: /home/u/x.md")
	if stickyIdx == -1 || infoIdx == -1 {
		t.Fatalf("toastLine is missing a slot: %q", plainLine)
	}
	if stickyIdx >= infoIdx {
		t.Errorf("sticky line not rendered before info line: sticky@%d info@%d in %q", stickyIdx, infoIdx, plainLine)
	}
	if count := strings.Count(line, stickySentinel); count != 1 {
		t.Errorf("ToastSticky style applied to %d lines, want exactly the sticky one: %q", count, line)
	}
}

// TestNewStickyReplacesOldSticky pins replace-only, newest-wins semantics for
// the sticky slot: a second push discards the first (two slots, never a
// queue).
func TestNewStickyReplacesOldSticky(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{status: readyStatus()})
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	m.pushStickyToast("first sticky")
	m.pushStickyToast("second sticky")
	if m.stickyToast == nil {
		t.Fatal("no sticky toast after two pushes")
	}
	if m.stickyToast.text != "second sticky" {
		t.Errorf("sticky text = %q, want the newest %q", m.stickyToast.text, "second sticky")
	}
	if got := plain(m.toastLine()); strings.Contains(got, "first sticky") {
		t.Errorf("toastLine still shows the replaced sticky: %q", got)
	}
}

// TestStackFrameExactFillAtEveryToastOccupancy pins the pushed-screen chrome
// reservation EXACTLY, in both directions. A fixed-height stub screen fills
// its whole stackBodyHeight budget deterministically (a real Browser windows
// its content and may under-fill), so the composed frame's line count is a
// pure function of the reservation — no non-determinism, so every occupancy
// gets an exact count, never a loose `<=`.
//
// The frame is header + breadcrumb + screen + footer joined by three
// blank-line separators, so its line count is headerLines + screenLines + 5
// (breadcrumb 1 + footer 1 + 3 separators). screenLines is stackBodyHeight()
// = height - 10 (well above the floor at height 40). The header block is 1
// line bare, 3 with one toast (status + blank + line), 5 with two (status +
// blank + sticky + blank + info). So the total is height-4 / height-2 /
// height for zero / one / two toasts — the reservation holds room for the
// two-line max, so fewer toasts leave the frame short (blank rows at the
// bottom), and full occupancy fills the terminal exactly. Asserting `==`
// (not `<=`) at the max is what makes an OVER-reservation regression — a
// screen budget one too small, so the frame never reaches the bottom — fail
// here rather than pass unseen.
func TestStackFrameExactFillAtEveryToastOccupancy(t *testing.T) {
	t.Parallel()
	const height = 40
	tests := []struct {
		name      string
		sticky    string
		info      string
		wantLines int
	}{
		{name: "zero toasts", wantLines: height - 4},
		{name: "info only", info: "path: /home/u/x.md", wantLines: height - 2},
		{name: "sticky only", sticky: "save failed — kept at /scratch/x.md", wantLines: height - 2},
		{name: "both slots", sticky: "save failed — kept at /scratch/x.md", info: "path: /home/u/x.md", wantLines: height},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			m := newTestModel(&fakeData{status: readyStatus()})
			m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: height})
			m.status = readyStatus()
			m = m.pushScreen(fixedHeightScreen{title: "stub"})
			if testCase.sticky != "" {
				m.pushStickyToast(testCase.sticky)
			}
			if testCase.info != "" {
				m.pushToast(testCase.info)
			}

			body := plain(m.View().Content)
			if gotLines := strings.Count(body, "\n") + 1; gotLines != testCase.wantLines {
				t.Errorf("stack frame is %d lines, want exactly %d — reservation off (over- or under-reserved)", gotLines, testCase.wantLines)
			}
			if want := plain(m.footer()); want == "" || !strings.Contains(body, want) {
				t.Errorf("stack footer missing from the composed frame:\n%s", body)
			}
		})
	}
}

// fixedHeightScreen is a test double for views.Screen that fills exactly the
// height it is handed (unlike a real Browser, whose fill depends on content),
// so a layout test can assert the root's frame arithmetic deterministically.
type fixedHeightScreen struct{ title string }

func (s fixedHeightScreen) Update(tea.Msg) (views.Screen, tea.Cmd) { return s, nil }

func (s fixedHeightScreen) View(_, height int) string {
	lines := make([]string, max(height, 1))
	for i := range lines {
		lines[i] = "x"
	}
	return strings.Join(lines, "\n")
}

func (s fixedHeightScreen) Title() string { return s.title }

// TestFleetHeaderLine pins the Projects fleet-header string (spec §9):
// "N units · watching M/N · last sync <outcome+relative> · vX.Y.Z". The unit
// count and watching tally come from the fleet snapshot; the last-sync outcome
// reuses the same lastCycle verdict the status header renders, with the
// relative age of the cycle appended; the version comes from Config. A mixed
// watching/failed fleet reads truthfully, and the never-synced and empty-fleet
// edges stay well-formed (singular "unit", "watching 0/0", "last sync never").
func TestFleetHeaderLine(t *testing.T) {
	t.Parallel()
	synced := readyStatus() // LastSync.At = 2026-07-09 11:00, outcome ok
	neverSynced := api.StatusResponse{State: "ready"}
	tests := []struct {
		name    string
		status  api.StatusResponse
		now     time.Time
		version string
		units   []api.UnitInfo
		want    string
	}{
		{
			name:    "mixed watching and failed, synced two hours ago",
			status:  synced,
			now:     time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC),
			version: "v1.2.3",
			units: []api.UnitInfo{
				{Provider: "claude", Folder: "agent-brain", WatchState: "watching"},
				{Provider: "codex", Folder: "_global", WatchState: "failed: watch /x: too many open files"},
			},
			want: "2 units · watching 1/2 · last sync ok 2h ago · v1.2.3",
		},
		{
			name:    "single watching unit never synced",
			status:  neverSynced,
			now:     time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC),
			version: "v9",
			units:   []api.UnitInfo{{Provider: "claude", Folder: "solo", WatchState: "watching"}},
			want:    "1 unit · watching 1/1 · last sync never · v9",
		},
		{
			name:    "empty fleet",
			status:  neverSynced,
			now:     time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC),
			version: "vdev",
			units:   nil,
			want:    "0 units · watching 0/0 · last sync never · vdev",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			m := newTestModel(&fakeData{})
			m.version = testCase.version
			m.status = testCase.status
			m.now = testCase.now
			m.projects.SetUnits(testCase.units)
			if got := m.fleetHeaderLine(); got != testCase.want {
				t.Errorf("fleetHeaderLine() = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestProjectsTabFrameExactFillWithFleetHeader pins the Projects TAB frame's
// chrome reservation EXACTLY, in both directions, now that the fleet header
// (spec §9) adds one row above the table. ProjectsView.SetSize reserves for
// every non-table line the frame carries at full chrome — status header, both
// toast slots, tab bar, section title, fleet header, action notice, footer, and
// the blank separators between them — and the table space-fills the remainder,
// so the composed frame's line count is a pure function of that reservation. At
// full chrome (both toasts + a notice) the frame fills the terminal exactly;
// fewer toasts leave it short. Asserting == at the max makes BOTH an
// under-reservation (the table a row too tall, a real row shoved off the bottom)
// and an over-reservation (the header eating a table row's height, the frame
// never reaching the bottom) fail here — the both-directions discipline the
// toast-tiers stack-frame pin established, extended to the tab that gained the
// header.
func TestProjectsTabFrameExactFillWithFleetHeader(t *testing.T) {
	t.Parallel()
	const height = 40
	tests := []struct {
		name      string
		sticky    string
		info      string
		wantLines int
	}{
		{name: "zero toasts", wantLines: height - 4},
		{name: "info only", info: "path: /home/u/x.md", wantLines: height - 2},
		{name: "sticky only", sticky: "save failed — kept at /scratch/x.md", wantLines: height - 2},
		{name: "both slots", sticky: "save failed — kept at /scratch/x.md", info: "path: /home/u/x.md", wantLines: height},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			m := newTestModel(&fakeData{status: readyStatus(), syncResp: api.SyncResponse{Status: "completed"}})
			m.version = "vtest"
			m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: height})
			m.status = readyStatus()
			m.now = time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC)
			m.projects.SetUnits([]api.UnitInfo{
				{Provider: "claude", Folder: "agent-brain", LocalDir: "/l/a", WatchState: "watching"},
				{Provider: "codex", Folder: "_global", LocalDir: "/l/c", WatchState: "failed: x"},
			})
			m, _ = step(m, key("s")) // an action notice — the reservation's last chrome line
			if testCase.sticky != "" {
				m.pushStickyToast(testCase.sticky)
			}
			if testCase.info != "" {
				m.pushToast(testCase.info)
			}

			body := plain(m.View().Content)
			if gotLines := strings.Count(body, "\n") + 1; gotLines != testCase.wantLines {
				t.Errorf("projects tab frame is %d lines, want exactly %d — reservation off (over- or under-reserved)", gotLines, testCase.wantLines)
			}
			// The fleet header renders above the table, not buried inside it.
			header := "2 units · watching 1/2 · last sync ok 2h ago · vtest"
			if !strings.Contains(body, header) {
				t.Errorf("fleet header %q missing from the Projects frame:\n%s", header, body)
			}
			if headerIdx, tableIdx := strings.Index(body, header), strings.Index(body, "PROVIDER"); headerIdx < 0 || tableIdx < 0 || headerIdx > tableIdx {
				t.Errorf("fleet header not rendered above the table (header index %d, table index %d)", headerIdx, tableIdx)
			}
		})
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

// TestEnterOnConflictsPushesDetail pins the Conflicts tab's drill-in, the twin
// of enter-to-browse: the tab footer advertises its own select/open rows, enter
// on the selected record emits views.OpenConflictMsg, the root resolves it into
// a *views.ConflictDetail and pushes it under a "Conflicts ▸ …" breadcrumb with
// the detail's own read/edit/back footer, and esc pops the whole round trip
// back to the tab.
func TestEnterOnConflictsPushesDetail(t *testing.T) {
	t.Parallel()
	registry := browserRegistry(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("# Notes\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(Config{Data: &fakeData{}, Registry: registry, Settings: terminalEditorSettings()})
	m.getenv = func(string) string { return "" }
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	m.active = tabConflicts
	m.projects.SetUnits([]api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}})
	m.conflicts.Set([]config.ConflictRecord{{Time: "2026-07-09T11:00:00Z", Path: "acme/claude/notes.md", Mode: "fact"}}, nil)

	tabFooter := plain(m.View().Content)
	for _, want := range []string{"select", "open"} {
		if !strings.Contains(tabFooter, want) {
			t.Errorf("Conflicts tab footer missing %q; got:\n%s", want, tabFooter)
		}
	}

	m = drive(t, m, key("enter"))

	if len(m.stack) != 1 {
		t.Fatalf("stack depth = %d after enter, want 1", len(m.stack))
	}
	if _, ok := m.stack[0].(*views.ConflictDetail); !ok {
		t.Fatalf("stack top = %T, want *views.ConflictDetail", m.stack[0])
	}
	pushed := plain(m.View().Content)
	if !strings.Contains(pushed, "Conflicts ▸ notes") {
		t.Errorf("view missing the detail breadcrumb; got:\n%s", pushed)
	}
	for _, want := range []string{"read", "edit", "back"} {
		if !strings.Contains(pushed, want) {
			t.Errorf("detail footer missing %q; got:\n%s", want, pushed)
		}
	}

	m = drive(t, m, key("esc"))
	if len(m.stack) != 0 {
		t.Fatalf("stack depth = %d after esc, want 0 (esc must pop back to the tab)", len(m.stack))
	}
	if restored := plain(m.View().Content); !strings.Contains(restored, "Conflicts") {
		t.Errorf("view missing the Conflicts tab after esc popped back; got:\n%s", restored)
	}
}

// TestConflictDetailAvailabilityTracksMapping pins the availability gate the
// detail footer renders from: a mapped fact record with a resolvable editor
// lights both read and edit, while an unmapped record strikes both (the brief's
// "offers nothing"). The wiring is flowTarget's *views.ConflictDetail case plus
// the read/edit entries in available().
func TestConflictDetailAvailabilityTracksMapping(t *testing.T) {
	t.Parallel()
	registry := browserRegistry(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("# Notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(Config{Data: &fakeData{}, Registry: registry, Settings: terminalEditorSettings()})
	m.getenv = func(string) string { return "" }
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	m.projects.SetUnits([]api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}})

	m = drive(t, m, views.OpenConflictMsg{Record: config.ConflictRecord{Time: "t", Path: "acme/claude/notes.md", Mode: "fact"}})
	if !m.available("conflictdetail-read") {
		t.Error("read unavailable over a mapped record, want available")
	}
	if !m.available("conflictdetail-edit") {
		t.Error("edit unavailable over a mapped fact record with a resolvable editor, want available")
	}

	m = m.popScreen()
	m = drive(t, m, views.OpenConflictMsg{Record: config.ConflictRecord{Time: "t", Path: "ghost/claude/gone.md", Mode: "fact"}})
	if m.available("conflictdetail-read") {
		t.Error("read available over an unmapped record, want struck")
	}
	if m.available("conflictdetail-edit") {
		t.Error("edit available over an unmapped record, want struck")
	}
}

// TestConflictDetailHistoryAvailabilityTracksResolution pins the h footer row's
// gate, which is wider than read/edit: history is honest wherever the record
// resolves to an enrolled unit — the mapped case AND the enrolled-but-deleted
// case (a since-deleted file keeps its version chain) — and struck only when the
// project is untracked. The wiring is conflictDetailHistoryAvailable plus the
// "conflictdetail-history" arm in available(); making h unconditionally
// available (the availability mutation) kills the untracked assertion below.
func TestConflictDetailHistoryAvailabilityTracksResolution(t *testing.T) {
	t.Parallel()
	registry := browserRegistry(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("# Notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(Config{Data: &fakeData{}, Registry: registry, Settings: terminalEditorSettings()})
	m.getenv = func(string) string { return "" }
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	m.projects.SetUnits([]api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}})

	// Mapped: the file resolves, so history is live.
	m = drive(t, m, views.OpenConflictMsg{Record: config.ConflictRecord{Time: "t", Path: "acme/claude/notes.md", Mode: "fact"}})
	if !m.available("conflictdetail-history") {
		t.Error("history unavailable over a mapped record, want available")
	}

	// Enrolled-but-deleted: the unit still carries the path but the file is gone;
	// history stays live where read (nothing live to read) strikes.
	m = m.popScreen()
	m = drive(t, m, views.OpenConflictMsg{Record: config.ConflictRecord{Time: "t", Path: "acme/claude/deleted.md", Mode: "fact"}})
	if !m.available("conflictdetail-history") {
		t.Error("history unavailable over an enrolled-but-deleted record, want available")
	}
	if m.available("conflictdetail-read") {
		t.Error("read available over an enrolled-but-deleted record, want struck (nothing live to read)")
	}

	// Untracked: no enrolled unit carries the path; history strikes.
	m = m.popScreen()
	m = drive(t, m, views.OpenConflictMsg{Record: config.ConflictRecord{Time: "t", Path: "ghost/claude/gone.md", Mode: "fact"}})
	if m.available("conflictdetail-history") {
		t.Error("history available over an untracked record, want struck")
	}
}

// TestConflictDetailHistoryEscPopsToDetail pins the h round trip at the root: h
// on a mapped detail pushes the History screen onto the stack above the detail,
// and esc pops History back off to the SAME intact detail beneath — the stack
// discipline every pushed screen shares, proven end-to-end through the real
// ConflictDetail and History screens rather than a stubbed push.
func TestConflictDetailHistoryEscPopsToDetail(t *testing.T) {
	t.Parallel()
	registry := browserRegistry(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("# Notes\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(Config{Data: &fakeData{}, Registry: registry, Settings: terminalEditorSettings()})
	m.getenv = func(string) string { return "" }
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	m.projects.SetUnits([]api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}})
	m = drive(t, m, views.OpenConflictMsg{Record: config.ConflictRecord{Time: "t", Path: "acme/claude/notes.md", Mode: "fact"}})

	if len(m.stack) != 1 {
		t.Fatalf("stack depth = %d after opening the detail, want 1", len(m.stack))
	}
	detail, ok := m.stack[0].(*views.ConflictDetail)
	if !ok {
		t.Fatalf("stack[0] = %T, want *views.ConflictDetail", m.stack[0])
	}

	// h pushes History above the detail.
	m = drive(t, m, key("h"))
	if len(m.stack) != 2 {
		t.Fatalf("stack depth = %d after h, want 2 (History pushed above the detail)", len(m.stack))
	}
	if _, ok := m.stack[1].(*views.History); !ok {
		t.Fatalf("stack top = %T after h, want *views.History", m.stack[1])
	}

	// esc pops History back to the same intact detail.
	m = drive(t, m, key("esc"))
	if len(m.stack) != 1 {
		t.Fatalf("stack depth = %d after esc, want 1 (History popped, detail intact)", len(m.stack))
	}
	if m.stack[0] != detail {
		t.Error("detail beneath History was replaced; want the same instance intact")
	}
	if _, mapped := detail.Memory(); !mapped {
		t.Error("detail no longer resolves to its memory after the History round trip")
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
		Now:      time.Now(),
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
// propagation withStyles already gives every tab view.
//
// This asserts on the pushed *Browser's OWN View() output, not the root's
// overall rendered frame: the root's header/footer/border chrome recolors
// on every BackgroundColorMsg regardless of whether applyStackTheme's
// browser case runs at all, so comparing m.View().Content dark vs light
// would still pass even with that case deleted entirely — root chrome
// alone supplies the byte difference. Reaching m.stack[0] directly and
// comparing its own View() output isolates the seam actually under test:
// the preview pane's glamour render, which only changes across the swap
// if SetStyles/SetRender actually reached this specific *Browser instance.
func TestBackgroundColorSwapsWhileBrowsing(t *testing.T) {
	t.Parallel()
	browser := views.NewBrowser(views.BrowserDeps{
		Folder:   "acme",
		Now:      time.Now(),
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
	darkBrowser, ok := dark.stack[0].(*views.Browser)
	if !ok {
		t.Fatalf("setup: stack[0] is %T, want *views.Browser", dark.stack[0])
	}
	darkRaw := darkBrowser.View(dark.width, dark.stackBodyHeight())
	if !strings.Contains(plain(darkRaw), "Heading") {
		t.Fatalf("dark browser view lost the preview content; got:\n%s", plain(darkRaw))
	}

	light, cmd := step(dark, tea.BackgroundColorMsg{Color: color.White})
	if cmd != nil {
		t.Error("BackgroundColorMsg while browsing produced a Cmd; want none")
	}
	lightBrowser, ok := light.stack[0].(*views.Browser)
	if !ok {
		t.Fatalf("setup: stack[0] is %T, want *views.Browser", light.stack[0])
	}
	lightRaw := lightBrowser.View(light.width, light.stackBodyHeight())
	if !strings.Contains(plain(lightRaw), "Heading") {
		t.Fatalf("light browser view lost the preview content; got:\n%s", plain(lightRaw))
	}

	if darkRaw == lightRaw {
		t.Error("dark and light renders of the pushed *Browser's own View() are byte-identical; the theme swap did not reach this *Browser's SetStyles/SetRender")
	}
}

// TestApplyStackThemeInvalidatesPushedBrowserPreviewCache isolates a seam
// the test above does not actually cover: it never calls View before the
// first theme swap, so the preview cache is never warmed beforehand, and
// the byte difference it asserts on can come entirely from the list
// pane's own SetStyles-driven chrome — proving nothing about whether the
// PREVIEW region specifically re-rendered. TestBrowserPreviewRenderIsCached
// (views/browser_test.go) never calls SetRender at all. Deleting
// SetRender's own `b.preview.valid = false` line left the entire
// dashboard+views suite green, including both of those tests — proof
// neither one covered it.
//
// Warms the cache with an initial renderer, then drives the real
// applyStackTheme wiring (not a simulated call) with a distinctly marked
// "sentinel" Render installed as the incoming theme's renderer, and
// asserts the very next View contains the sentinel's output. That
// requires BOTH that applyStackTheme actually propagated the new Render
// func to this specific pushed *Browser AND that SetRender's own
// invalidation cleared the cache so the next render re-runs it instead of
// serving the pre-swap cached string — failing if either breaks.
func TestApplyStackThemeInvalidatesPushedBrowserPreviewCache(t *testing.T) {
	t.Parallel()
	const sentinelMarker = "SENTINEL-RENDER-OUTPUT"
	browser := views.NewBrowser(views.BrowserDeps{
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: func(memoryfs.Memory) (string, error) { return "# Heading", nil },
		List: func() ([]memoryfs.Memory, error) {
			return []memoryfs.Memory{{Provider: "claude", Name: "Note", RepoPath: "claude/note.md"}}, nil
		},
		Render: func(md string, _ int) string { return "ORIGINAL:" + md },
	})
	m := newTestModel(&fakeData{})
	m, _ = step(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = step(m, views.PushScreenMsg{Screen: browser})

	warm := plain(browser.View(m.width, m.stackBodyHeight()))
	if !strings.Contains(warm, "ORIGINAL:") {
		t.Fatalf("setup: initial render did not go through the original Render func; got:\n%s", warm)
	}

	m.renderMarkdown = func(md string, _ int) string { return sentinelMarker + ":" + md }
	m.applyStackTheme()

	got := plain(browser.View(m.width, m.stackBodyHeight()))
	if !strings.Contains(got, sentinelMarker) {
		t.Errorf("pushed browser's preview was not re-rendered through the theme's newly installed Render func; got:\n%s", got)
	}
}

// TestApplyStackThemePropagatesStylesToPushedInsights is the root-level sentinel
// that keeps applyStackTheme's styledScreen arm alive. Insights implements only
// styledScreen — it renders no markdown, so it has no renderedScreen arm — making
// it the one pushed screen whose theme propagation rides SOLELY on the styledScreen
// arm. Drives the real applyStackTheme wiring (not a direct SetStyles call) with a
// sentinel Header installed on the incoming styles, and asserts the very next View
// carries it; deleting the styledScreen arm leaves the whole suite green except
// here.
func TestApplyStackThemePropagatesStylesToPushedInsights(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	insights := views.NewInsights(views.InsightsDeps{
		Folder:   "acme",
		Memories: []memoryfs.Memory{{Provider: "claude", Folder: "acme", RepoPath: "claude/a.md", ModTime: base.Add(-time.Hour), Size: 100}},
		Now:      base,
	})
	m := newTestModel(&fakeData{})
	m, _ = step(m, tea.WindowSizeMsg{Width: 100, Height: 40})
	m, _ = step(m, views.PushScreenMsg{Screen: insights})

	m.styles.Header = lipgloss.NewStyle().Transform(func(s string) string { return "STYLED<" + s + ">" })
	m.applyStackTheme()

	got := plain(insights.View(m.width, m.stackBodyHeight()))
	if !strings.Contains(got, "STYLED<Memories>") {
		t.Errorf("applyStackTheme did not propagate the swapped styles to the pushed Insights (styledScreen arm); got:\n%s", got)
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
		Now:      time.Now(),
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

// TestInsightsScreenWiring pins the root plumbing the insights screen depends on
// that lives in this package, not views: stackScope must map a pushed *Insights
// to ScopeInsights (so its footer names esc-back and nothing tab-level), and the
// root must forward an InsightsDataMsg to the stack top (so the screen's one
// folder-wide fetch actually reaches it). The machine tally the injected message
// produces is a fact no filesystem section could fabricate, so its appearance
// proves the forward landed.
func TestInsightsScreenWiring(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	insights := views.NewInsights(views.InsightsDeps{
		Folder:   "acme",
		Memories: []memoryfs.Memory{{Provider: "claude", Folder: "acme", RepoPath: "claude/a.md", ModTime: base.Add(-time.Hour), Size: 100}},
		Now:      base,
	})
	m := newTestModel(&fakeData{})
	m, _ = step(m, tea.WindowSizeMsg{Width: 100, Height: 40})
	m, _ = step(m, views.PushScreenMsg{Screen: insights})

	footer := plain(m.footer())
	if !strings.Contains(footer, "esc back") {
		t.Errorf("insights footer missing 'esc back'; got %q", footer)
	}
	for _, dead := range []string{"tab/1–4", "ctrl+k palette", "? help", "o order"} {
		if strings.Contains(footer, dead) {
			t.Errorf("insights footer leaks %q; a pushed screen owns the keyboard", dead)
		}
	}

	captured := base.Add(-30 * time.Minute)
	m, _ = step(m, views.InsightsDataMsg{
		Folder:   "acme",
		Versions: []api.HistoryVersion{{Rev: "r1", Host: "workstation", Timestamp: &captured, Paths: []string{"claude/a.md"}}},
	})
	top, ok := m.stack[len(m.stack)-1].(*views.Insights)
	if !ok {
		t.Fatalf("stack top is %T, want *views.Insights", m.stack[len(m.stack)-1])
	}
	if body := plain(top.View(m.width, m.stackBodyHeight())); !strings.Contains(body, "workstation  1") {
		t.Errorf("InsightsDataMsg was not forwarded to the pushed screen; got:\n%s", body)
	}
}

// TestBuildBrowserDepsThreadsConfiguredStaleAfterDays pins that
// buildBrowserDeps reads the REAL configured lint.stale_after_days
// (Config.Settings.Lint.StaleAfterDays) rather than any hardcoded
// fallback: two Models built with different Settings values, pushed
// against the identical 6-day-old memory, must disagree about whether it
// is stale — a fixed constant (0, or DefaultSettings' 90) could never
// produce that disagreement, so the difference itself proves the
// configured value actually reached lint.Check.
func TestBuildBrowserDepsThreadsConfiguredStaleAfterDays(t *testing.T) {
	t.Parallel()
	registry := browserRegistry(t)
	dir := t.TempDir()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	memoryPath := filepath.Join(dir, "old.md")
	if err := os.WriteFile(memoryPath, []byte("---\nname: Old\ndescription: aging memory\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(memoryPath, base, base); err != nil {
		t.Fatal(err)
	}
	sixDaysLater := base.Add(6 * 24 * time.Hour)

	pushedView := func(t *testing.T, staleAfterDays int) string {
		t.Helper()
		m := New(Config{
			Data:     &fakeData{},
			Registry: registry,
			Settings: config.Settings{Lint: config.LintSettings{StaleAfterDays: staleAfterDays}},
		})
		m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
		m, _ = step(m, tickMsg(sixDaysLater))
		m.active = tabProjects
		m.projects.SetUnits([]api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}})
		m = drive(t, m, key("enter"))
		return plain(m.View().Content)
	}

	strict := pushedView(t, 5) // 6 days old, 5-day threshold: stale
	if !strings.Contains(strict, "⚠") {
		t.Errorf("StaleAfterDays=5 with a 6-day-old memory did not flag it; view:\n%s", strict)
	}

	lenient := pushedView(t, 90) // 6 days old, 90-day threshold: not stale
	if strings.Contains(lenient, "⚠") {
		t.Errorf("StaleAfterDays=90 with the identical 6-day-old memory wrongly flagged it; view:\n%s", lenient)
	}
}

// pushedReading builds a minimal *views.Reading over a canned body — the
// dashboard-level seams below (theme propagation, footer scope, CopyPathMsg)
// need a real pushed Reading but none of the link fixtures the views-level
// suite exercises.
func pushedReading(render func(md string, width int) string) *views.Reading {
	return views.NewReading(views.ReadingDeps{
		Memory: memoryfs.Memory{
			Provider: "claude",
			Folder:   "acme",
			LocalDir: "/enrolled/acme/claude",
			RelPath:  "note.md",
			RepoPath: "claude/note.md",
			Name:     "Note",
		},
		ReadBody: func(memoryfs.Memory) (string, error) { return "# Heading", nil },
		Render:   render,
	})
}

// TestCopyPathMsgToastsAndWritesClipboard pins the root's half of spec §4's
// y: the toast names the absolute provider-file path verbatim (the
// guaranteed affordance — visible in every terminal), and the same update
// issues bubbletea's OSC52 clipboard write as the best-effort half (not
// every terminal honors OSC52, and there is no delivery ack, which is why
// the toast is not conditional on it).
func TestCopyPathMsgToastsAndWritesClipboard(t *testing.T) {
	t.Parallel()
	const wantPath = "/enrolled/acme/claude/note.md"
	m := newTestModel(&fakeData{})

	m, cmd := step(m, views.CopyPathMsg{Path: wantPath})

	if got := plain(m.toastLine()); !strings.Contains(got, "path: "+wantPath) {
		t.Errorf("toast = %q, want it to contain %q", got, "path: "+wantPath)
	}
	if cmd == nil {
		t.Fatal("CopyPathMsg produced no Cmd; want the OSC52 clipboard write")
	}
	if !slices.Contains(drain(cmd), tea.SetClipboard(wantPath)()) {
		t.Errorf("CopyPathMsg's Cmd did not carry tea.SetClipboard(%q)", wantPath)
	}
}

// TestToastMsgSurfacesInStatusArea pins the generic screen→root toast
// channel a pushed screen's local refusal rides (the reading view's
// enter-on-a-dangling-link, and any later screen's equivalent).
func TestToastMsgSurfacesInStatusArea(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{})
	m, cmd := step(m, views.ToastMsg{Text: "dangling link: no memory named \"ghost\""})
	if cmd != nil {
		t.Error("ToastMsg produced a Cmd; want none")
	}
	if got := plain(m.toastLine()); !strings.Contains(got, "dangling link") {
		t.Errorf("toast = %q, want the ToastMsg text surfaced", got)
	}
}

// TestApplyStackThemeInvalidatesPushedReadingRenderCache is the reading
// view's version of the browser seam test above, and isolates the same two
// halves for the SAME reason: the render cache is warmed first, so the only
// way the sentinel can appear in the next View's body is applyStackTheme
// actually reaching this pushed *Reading's SetRender AND that setter
// invalidating the warmed cache — a whole-View byte diff would be vacuous
// (root chrome alone differs across a styles swap).
func TestApplyStackThemeInvalidatesPushedReadingRenderCache(t *testing.T) {
	t.Parallel()
	const sentinelMarker = "SENTINEL-RENDER-OUTPUT"
	reading := pushedReading(func(md string, _ int) string { return "ORIGINAL:" + md })
	m := newTestModel(&fakeData{})
	m, _ = step(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = step(m, views.PushScreenMsg{Screen: reading})

	warm := plain(reading.View(m.width, m.stackBodyHeight()))
	if !strings.Contains(warm, "ORIGINAL:") {
		t.Fatalf("setup: initial render did not go through the original Render func; got:\n%s", warm)
	}

	m.renderMarkdown = func(md string, _ int) string { return sentinelMarker + ":" + md }
	m.applyStackTheme()

	got := plain(reading.View(m.width, m.stackBodyHeight()))
	if !strings.Contains(got, sentinelMarker) {
		t.Errorf("pushed reading's body was not re-rendered through the theme's newly installed Render func; got:\n%s", got)
	}
}

// TestStackFooterAdvertisesReadingScopedKeys pins the footer's scope switch
// for a pushed Reading: exactly ScopeReading's own keys, nothing from the
// tab level or ScopeGlobal — the same honesty rule
// TestStackFooterAdvertisesScopedKeys already pins for the browser.
func TestStackFooterAdvertisesReadingScopedKeys(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{})
	m, _ = step(m, views.PushScreenMsg{Screen: pushedReading(nil)})

	got := plain(m.footer())
	for _, want := range []string{"tab links", "enter follow", "b backlinks", "y copy path", "esc back"} {
		if !strings.Contains(got, want) {
			t.Errorf("stack footer %q missing %q", got, want)
		}
	}
	for _, deadHint := range []string{"tab/1–4", "ctrl+k palette", "? help", "s sync", "o order"} {
		if strings.Contains(got, deadHint) {
			t.Errorf("stack footer %q leaks a tab-level, global, or browser hint %q", got, deadHint)
		}
	}
}

// TestReadingBreadcrumbExtendsPerLevel pins spec §2's stack breadcrumb one
// level deeper than the browser test above: link-to-link navigation pushes
// one more segment per reading view, so the trail always shows how the
// current memory was reached.
func TestReadingBreadcrumbExtendsPerLevel(t *testing.T) {
	t.Parallel()
	registry := browserRegistry(t)
	m := New(Config{Data: &fakeData{}, Registry: registry})
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	m.active = tabProjects
	m.projects.SetUnits([]api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: t.TempDir()}})

	m = drive(t, m, key("enter"))
	m, _ = step(m, views.PushScreenMsg{Screen: pushedReading(nil)})

	if got := plain(m.breadcrumb()); !strings.Contains(got, "Projects ▸ acme ▸ Note") {
		t.Errorf("breadcrumb = %q, want the reading segment appended", got)
	}
}

// TestMarkdownRendererPreservesReadingSubstitutionMarkers pins the reading
// view's substitution contract THROUGH the real pinned glamour renderer,
// for both background flavours: the pre-render forms substituteLinks emits
// (views/reading.go) must survive rendering — ▶target◀ and [[target]]
// verbatim, and ~~target~~ struck via the theme's own strikethrough style
// (SGR 9) with no literal tildes left behind. The committed views-level
// suite drives nil/fake Render seams, so without this a glamour bump that
// started mangling any of these forms would degrade the reading view with
// every test still green.
func TestMarkdownRendererPreservesReadingSubstitutionMarkers(t *testing.T) {
	t.Parallel()
	strikethroughPattern := regexp.MustCompile(`\x1b\[(?:[0-9]+;)*9m`)
	const displayBody = "read ▶one◀ then [[two]] then ~~ghost~~ (dangling) end"

	tests := []struct {
		name   string
		isDark bool
	}{
		{"dark", true},
		{"light", false},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			render := newMarkdownRenderer(styleName(testCase.isDark))
			rendered := render(displayBody, 80)

			visible := plain(rendered)
			for _, want := range []string{"▶one◀", "[[two]]", "ghost (dangling)"} {
				if !strings.Contains(visible, want) {
					t.Errorf("rendered output lost %q; got:\n%s", want, visible)
				}
			}
			if strings.Contains(visible, "~~") {
				t.Errorf("strikethrough markers rendered as literal tildes; got:\n%s", visible)
			}
			if !strikethroughPattern.MatchString(rendered) {
				t.Errorf("no SGR strikethrough (CSI …9m) in the rendered output:\n%q", rendered)
			}
		})
	}
}

// TestSlashOpensSearchOverlayOnEveryRootTab pins spec §7's entry point: `/`
// from ANY root view — stack empty, no modal, no other overlay — opens the
// global search overlay through the same registry dispatch a palette choice
// runs, and the footer advertises the key on every tab now that the action
// is genuinely dispatchable.
func TestSlashOpensSearchOverlayOnEveryRootTab(t *testing.T) {
	t.Parallel()
	for _, activeTab := range []tab{tabProjects, tabConflicts, tabActivity, tabDoctor} {
		t.Run(activeTab.title(), func(t *testing.T) {
			t.Parallel()
			m := newTestModel(&fakeData{})
			m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
			m.active = activeTab

			if footer := plain(m.footer()); !strings.Contains(footer, "/ search") {
				t.Errorf("footer %q does not advertise search", footer)
			}

			m, cmd := step(m, key("/"))
			if m.searchOverlay == nil {
				t.Fatal("/ did not open the search overlay")
			}
			if cmd != nil {
				t.Error("opening the overlay produced a Cmd; want none (a pure state flip, like help)")
			}
			if got := plain(m.View().Content); !strings.Contains(got, "Global search") {
				t.Errorf("view does not render the overlay; got:\n%s", got)
			}
		})
	}
}

// TestSlashWithScreenStackedReachesBrowserFilter pins the other direction of
// the `/` gate: while a Screen is stacked the key belongs to the screen —
// the browser's own filter — because handleKey's stack forward runs before
// the global dispatch loop can ever see the key. Opening the overlay from
// inside a stacked screen would breach the screen's keyboard ownership the
// stack footer already promises.
func TestSlashWithScreenStackedReachesBrowserFilter(t *testing.T) {
	t.Parallel()
	browser := views.NewBrowser(views.BrowserDeps{
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: func(memoryfs.Memory) (string, error) { return "", nil },
		List: func() ([]memoryfs.Memory, error) {
			return []memoryfs.Memory{{Provider: "claude", Name: "note", RepoPath: "claude/note.md"}}, nil
		},
	})
	m := newTestModel(&fakeData{})
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	m, _ = step(m, views.PushScreenMsg{Screen: browser})

	m, _ = step(m, key("/"))
	if m.searchOverlay != nil {
		t.Fatal("/ with a Screen stacked opened the global overlay; it must reach the browser filter instead")
	}
	if got := plain(m.View().Content); !strings.Contains(got, "filter:") {
		t.Errorf("browser filter did not engage; view:\n%s", got)
	}
}

// TestSearchChoicePushesReadingWithLazyFolderIndex pins the root's half of
// spec §7's enter: a SearchChoiceMsg pushes the chosen memory's reading
// view, and the links Index handed to it is built lazily — at choice time,
// over the chosen memory's own folder — proven by following a [[link]]
// from the pushed reading: with a nil or empty index the link would dangle
// and enter would only toast instead of pushing the target.
func TestSearchChoicePushesReadingWithLazyFolderIndex(t *testing.T) {
	t.Parallel()
	registry := browserRegistry(t)
	folderDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(folderDir, "alpha.md"), []byte("see [[beta]]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folderDir, "beta.md"), []byte("# beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(Config{Data: &fakeData{}, Registry: registry})
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: folderDir}}
	m.projects.SetUnits(units)

	memories, err := memoryfs.List(registry, units)
	if err != nil {
		t.Fatal(err)
	}
	var alpha memoryfs.Memory
	found := false
	for _, memory := range memories {
		if memory.Name == "alpha" {
			alpha, found = memory, true
		}
	}
	if !found {
		t.Fatalf("setup: alpha not in listing %v", memories)
	}

	m = drive(t, m, views.SearchChoiceMsg{Memory: alpha})

	if len(m.stack) != 1 {
		t.Fatalf("stack depth = %d after a search choice, want 1", len(m.stack))
	}
	reading, ok := m.stack[0].(*views.Reading)
	if !ok {
		t.Fatalf("stack top = %T, want *views.Reading", m.stack[0])
	}
	if got := reading.Title(); got != "alpha" {
		t.Errorf("pushed reading Title() = %q, want %q", got, "alpha")
	}
	if got := plain(m.View().Content); !strings.Contains(got, "Projects ▸ alpha") {
		t.Errorf("view missing the breadcrumb; got:\n%s", got)
	}

	m = drive(t, m, key("tab"))
	m = drive(t, m, key("enter"))
	if len(m.stack) != 2 {
		t.Fatalf("stack depth = %d after following [[beta]], want 2 — the lazily built folder index did not resolve the link", len(m.stack))
	}
	followed, ok := m.stack[1].(*views.Reading)
	if !ok {
		t.Fatalf("stack top = %T, want *views.Reading", m.stack[1])
	}
	if got := followed.Title(); got != "beta" {
		t.Errorf("followed reading Title() = %q, want %q", got, "beta")
	}
}

// TestSearchEndToEndFindsNeedleAcrossProjectsAndOpensRightMemory drives spec
// §17's acceptance criterion through the REAL pipeline: `/`, one real
// keystroke whose actual 250ms tea.Tick drive waits out, a genuine
// memoryfs.List over two on-disk projects, and enter on the second row —
// proving the debounce wiring, the cross-project Collect, and the
// choice→Reading push agree end to end with no hand-built messages.
func TestSearchEndToEndFindsNeedleAcrossProjectsAndOpensRightMemory(t *testing.T) {
	t.Parallel()
	registry := browserRegistry(t)
	acmeDir, zenithDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(acmeDir, "apple.md"), []byte("alpha line\nthe needle hides here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(zenithDir, "zebra.md"), []byte("needle again\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := New(Config{Data: &fakeData{}, Registry: registry})
	m, _ = step(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m.projects.SetUnits([]api.UnitInfo{
		{Provider: "claude", Folder: "acme", LocalDir: acmeDir},
		{Provider: "claude", Folder: "zenith", LocalDir: zenithDir},
	})

	m = drive(t, m, key("/"))
	if m.searchOverlay == nil {
		t.Fatal("/ did not open the search overlay")
	}
	m = drive(t, m, key("n")) // real debounce: drive waits out the tick and runs the query

	view := plain(m.View().Content)
	for _, want := range []string{"acme", "zenith", "needle again"} {
		if !strings.Contains(view, want) {
			t.Errorf("overlay after searching %q misses %q; view:\n%s", "n", want, view)
		}
	}

	// Both hits are body-tier with no name match, so they rank by Name:
	// apple (acme) first, zebra (zenith) second — down+enter opens zebra.
	m = drive(t, m, key("down"))
	m = drive(t, m, key("enter"))

	if m.searchOverlay != nil {
		t.Error("choosing a result did not close the overlay")
	}
	if len(m.stack) != 1 {
		t.Fatalf("stack depth = %d after enter, want 1", len(m.stack))
	}
	reading, ok := m.stack[0].(*views.Reading)
	if !ok {
		t.Fatalf("stack top = %T, want *views.Reading", m.stack[0])
	}
	if got := reading.Title(); got != "zebra" {
		t.Errorf("opened Title() = %q, want %q (the cursor row's memory, across projects)", got, "zebra")
	}
	if got := plain(m.View().Content); !strings.Contains(got, "needle again") {
		t.Errorf("opened reading does not show the needle body; got:\n%s", got)
	}
}

// TestSearchOverlayEscClosesOverlayNotQuitPrompt pins the esc contract both
// ways: inside the overlay, esc dismisses it and NOTHING else — no quit
// prompt — and once dismissed, the root's ordinary esc behavior is back.
func TestSearchOverlayEscClosesOverlayNotQuitPrompt(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{})
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	m = drive(t, m, key("/"))
	if m.searchOverlay == nil {
		t.Fatal("setup: / did not open the search overlay")
	}

	m, cmd := step(m, key("esc"))
	if m.searchOverlay != nil {
		t.Fatal("esc did not close the overlay")
	}
	if cmd != nil {
		t.Error("closing the overlay produced a Cmd; want none")
	}
	if m.quitPrompt {
		t.Error("esc inside the overlay must not open the quit prompt")
	}

	m, _ = step(m, key("esc"))
	if !m.quitPrompt {
		t.Error("esc after dismissal did not reopen the root quit prompt")
	}
}

// TestSearchMessagesAfterDismissalAreInert covers the in-flight race the
// debounce leaves behind: a tick or query result whose overlay was
// dismissed before it landed must be dropped — no panic, no reopen, no Cmd.
func TestSearchMessagesAfterDismissalAreInert(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{})
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	m = drive(t, m, key("/"))
	m, _ = step(m, key("esc"))
	if m.searchOverlay != nil {
		t.Fatal("setup: esc did not close the overlay")
	}

	m, cmd := step(m, views.SearchDebounceMsg{Generation: 1})
	if cmd != nil {
		t.Error("a debounce tick after dismissal produced a Cmd")
	}
	if m.searchOverlay != nil {
		t.Error("a debounce tick after dismissal re-opened the overlay")
	}

	m, cmd = step(m, views.SearchResultsMsg{Generation: 1})
	if cmd != nil {
		t.Error("a query result after dismissal produced a Cmd")
	}
	if m.searchOverlay != nil {
		t.Error("a query result after dismissal re-opened the overlay")
	}
}

// TestPaletteChoiceSearchOpensOverlay pins spec §14's cannot-diverge rule
// for the new action: choosing search from the palette runs the identical
// dispatch the `/` key does.
func TestPaletteChoiceSearchOpensOverlay(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{})
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})

	m, cmd := step(m, views.PaletteChoiceMsg{ID: "search"})
	if m.searchOverlay == nil {
		t.Fatal("choosing search from the palette did not open the overlay")
	}
	if cmd != nil {
		t.Error("opening the overlay produced a Cmd; want none")
	}
}

// TestBackgroundColorSwapReachesOpenSearchOverlay extends the withStyles
// propagation to the one overlay that can hold styles across a swap: an
// open search overlay must render through the incoming flavour, not the one
// it opened with.
func TestBackgroundColorSwapReachesOpenSearchOverlay(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{})
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	m = drive(t, m, key("/"))
	if m.searchOverlay == nil {
		t.Fatal("setup: / did not open the search overlay")
	}

	dark, _ := step(m, tea.BackgroundColorMsg{Color: color.Black})
	if dark.searchOverlay == nil {
		t.Fatal("the background swap dropped the open overlay")
	}
	darkView := dark.searchOverlay.View(80, 24)

	light, _ := step(dark, tea.BackgroundColorMsg{Color: color.White})
	lightView := light.searchOverlay.View(80, 24)
	if darkView == lightView {
		t.Error("dark and light renders of the open overlay are byte-identical; the theme swap did not reach its styles")
	}
}

// TestSlashInertWhileQuitPromptOpen: the quit prompt owns the keyboard, so a
// `/` there neither opens the overlay nor decides the prompt — the same
// dead-key discipline every other non-answer key already gets.
func TestSlashInertWhileQuitPromptOpen(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{})
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	m, _ = step(m, key("esc"))
	if !m.quitPrompt {
		t.Fatal("setup: esc did not open the quit prompt")
	}

	m, cmd := step(m, key("/"))
	if m.searchOverlay != nil {
		t.Error("/ while the quit prompt owns the keyboard opened the overlay")
	}
	if cmd != nil {
		t.Error("/ at the quit prompt produced a Cmd; want none")
	}
	if !m.quitPrompt {
		t.Error("/ dismissed the quit prompt; only y/n/esc may decide it")
	}
}

// TestSearchOverlayOwnsGlobalKeys: while the overlay is open, q types into
// the query instead of quitting and ? does not open help — the overlay owns
// the whole keyboard, the same contract as the palette.
func TestSearchOverlayOwnsGlobalKeys(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{})
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	m = drive(t, m, key("/"))
	if m.searchOverlay == nil {
		t.Fatal("setup: / did not open the search overlay")
	}

	m, _ = step(m, key("q"))
	if m.quitting {
		t.Fatal("q inside the overlay quit the program instead of typing")
	}
	if m.searchOverlay == nil {
		t.Fatal("q inside the overlay closed it")
	}

	m, _ = step(m, key("?"))
	if m.helpOpen {
		t.Error("? inside the overlay opened help instead of typing")
	}
	if m.searchOverlay == nil {
		t.Error("? inside the overlay closed it")
	}
}

// TestSearchFindsProjectTrackedWhileOverlayOpen pins the freshness half of
// the overlay's Collect seam: the fleet can change while the overlay is
// open (a background projects refresh lands, an enrollment completes), and
// the NEXT query must see it. The overlay's deps were bound over the root's
// value-semantics Model at open time — a frozen snapshot — so this only
// holds because forwardToSearchOverlay re-binds Collect from the live model
// on every forwarded message; a query still bound to open-time state would
// be searching the old fleet and miss the new folder entirely.
func TestSearchFindsProjectTrackedWhileOverlayOpen(t *testing.T) {
	t.Parallel()
	registry := browserRegistry(t)
	acmeDir, zenithDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(acmeDir, "apple.md"), []byte("alpha line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(zenithDir, "zebra.md"), []byte("stripes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := New(Config{Data: &fakeData{}, Registry: registry})
	m, _ = step(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m.projects.SetUnits([]api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: acmeDir}})

	m = drive(t, m, key("/"))
	if m.searchOverlay == nil {
		t.Fatal("setup: / did not open the search overlay")
	}

	// zenith joins the fleet only AFTER the overlay is already open — the
	// same mutation a projectsMsg refresh performs on the live model.
	m.projects.SetUnits([]api.UnitInfo{
		{Provider: "claude", Folder: "acme", LocalDir: acmeDir},
		{Provider: "claude", Folder: "zenith", LocalDir: zenithDir},
	})

	m = drive(t, m, key("z")) // real debounce; only zebra (zenith) matches "z"

	view := plain(m.View().Content)
	for _, want := range []string{"zenith", "zebra"} {
		if !strings.Contains(view, want) {
			t.Errorf("a query after the fleet grew misses %q — the overlay searched a stale fleet snapshot; view:\n%s", want, view)
		}
	}
}

// TestSearchChoiceLinkIndexFailureToastsAndStillOpens pins the degraded
// half of the choice→Reading push: when enumerating the chosen memory's
// folder for the link index fails, the reading still opens — the chosen
// memory itself is in hand and readable, so refusing the open over link
// decoration would be worse — but the failure is SAID, as a toast, never
// swallowed: the user who later hits a dangling [[link]] deserves to know
// links were never going to resolve.
func TestSearchChoiceLinkIndexFailureToastsAndStillOpens(t *testing.T) {
	t.Parallel()
	registry := browserRegistry(t)
	folderDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(folderDir, "alpha.md"), []byte("see [[beta]]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := New(Config{Data: &fakeData{}, Registry: registry})
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	// The chosen memory's folder holds a unit whose provider the registry
	// does not know — memoryfs.List's registry-miss failure, at choice time.
	m.projects.SetUnits([]api.UnitInfo{{Provider: "ghost", Folder: "acme", LocalDir: folderDir}})

	chosen := memoryfs.Memory{
		Provider: "claude",
		Folder:   "acme",
		LocalDir: folderDir,
		RelPath:  "alpha.md",
		RepoPath: "claude/alpha.md",
		Name:     "alpha",
	}
	m = drive(t, m, views.SearchChoiceMsg{Memory: chosen})

	if len(m.stack) != 1 {
		t.Fatalf("stack depth = %d, want 1 — a link-index failure must not block opening the memory itself", len(m.stack))
	}
	view := plain(m.View().Content)
	if !strings.Contains(view, "link index unavailable") {
		t.Errorf("view carries no toast for the failed link-index build; got:\n%s", view)
	}
}

// TestSlashWhileProjectsModalOpenTypesIntoModalInput pins the loser's side
// of the `/` priority race INSIDE the root's own chrome (the stacked-Screen
// side has its own pin above): a Projects modal — here the add flow's
// path-confirm input — owns the keyboard before the global dispatch loop
// runs, so `/` is a literal path character, exactly what typing a Unix path
// needs, and the search overlay stays closed.
func TestSlashWhileProjectsModalOpenTypesIntoModalInput(t *testing.T) {
	t.Parallel()
	candidates := []views.TrackCandidate{{
		Provider:  "claude",
		Label:     "claude  ~/dev/acme",
		PathGuess: "/home/u/dev/acme",
		Roots:     []views.TrackRoot{{LocalDir: "/home/u/dev/acme/.claude"}},
	}}
	m := New(addConfig(&fakeData{}, candidates))
	m.active = tabProjects

	m = drive(t, m, key("a"))     // discover → picker
	m = drive(t, m, key("space")) // select the per-project candidate
	m = drive(t, m, key("enter")) // confirm the set → confirm-path input
	if m.projects.Adding != views.AddConfirmPath {
		t.Fatalf("setup: Adding = %v, want the confirm-path stage", m.projects.Adding)
	}

	m = drive(t, m, key("/"))

	if m.searchOverlay != nil {
		t.Fatal("/ inside a Projects modal opened the search overlay; the modal owns the key")
	}
	if m.projects.Adding != views.AddConfirmPath {
		t.Fatalf("Adding = %v after /, want the confirm-path input still open", m.projects.Adding)
	}
	if got := plain(m.projects.View("")); !strings.Contains(got, "/home/u/dev/acme/") {
		t.Errorf("the modal's path input did not receive the literal /; view:\n%s", got)
	}
}

// TestPushHistoryIssuesInitCmdAndForwards pins the stacked-screen async
// plumbing Task 14 introduces: pushing a History screen issues its InitCmd
// (the version fetch), and the fetch result — a HistoryVersionsMsg the root
// forwards to the stack top — is adopted by the screen, moving it off its
// loading notice. The two together are the round trip every stacked async
// fetch now relies on.
func TestPushHistoryIssuesInitCmdAndForwards(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{})
	history := views.NewHistory(views.HistoryDeps{Folder: "acme", RepoPath: "claude/note.md", Data: m.data})

	m, cmd := step(m, views.PushScreenMsg{Screen: history})
	if cmd == nil {
		t.Fatal("pushing a History screen issued no InitCmd")
	}
	msgs := drain(cmd)
	if !containsMsg[views.HistoryVersionsMsg](msgs) {
		t.Fatalf("the History InitCmd did not fetch versions; drained %#v", msgs)
	}

	for _, msg := range msgs {
		m, _ = step(m, msg)
	}
	top, ok := m.stackTop()
	if !ok {
		t.Fatal("History screen left the stack")
	}
	if got := plain(top.View(120, 30)); strings.Contains(got, "loading history") {
		t.Errorf("History screen still loading after its InitCmd result was forwarded back; got:\n%s", got)
	}
}

// --- Task 18: update banner + one-key self-update + re-exec (spec §11) ---

// newUpdateModel builds a sized root model wired with the injected update
// closures, its daemon already reporting ready — the common setup for the
// update tests below.
func newUpdateModel(check func(context.Context) (string, error), apply func(context.Context, string) error) Model {
	m := New(Config{
		Data:         &fakeData{status: readyStatus()},
		StartService: func() error { return nil },
		CheckUpdate:  check,
		ApplyUpdate:  apply,
	})
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	m.status = readyStatus()
	return m
}

// offeredUpdateModel is a root model in the offered phase — the state in which
// update-agent-brain is dispatchable — for the palette/availability pins.
func offeredUpdateModel() Model {
	m := newTestModel(&fakeData{})
	m.updatePhase = updateOffered
	m.updateTag = "v2.1.0"
	return m
}

// TestCheckUpdateFiresOnceAfterFirstStatusSuccess pins spec §11's cadence: the
// release check is scheduled at most once per hub session, and only after the
// FIRST successful statusMsg — a status error (daemon down) never triggers it,
// and a second success does not re-fire it (the "two ticks, one call" shape,
// driven here with the statusMsg each tick resolves to).
func TestCheckUpdateFiresOnceAfterFirstStatusSuccess(t *testing.T) {
	t.Parallel()

	t.Run("a status error does not fire the check", func(t *testing.T) {
		t.Parallel()
		var checkCalls int
		check := func(context.Context) (string, error) { checkCalls++; return "v9.9.9", nil }
		m := newUpdateModel(check, nil)

		_, cmd := step(m, statusMsg{err: api.ErrDaemonNotRunning})
		drain(cmd)
		if checkCalls != 0 {
			t.Errorf("CheckUpdate fired on a status error; want it gated on a status SUCCESS (calls=%d)", checkCalls)
		}
	})

	t.Run("two successes fire the check exactly once", func(t *testing.T) {
		t.Parallel()
		var checkCalls int
		check := func(context.Context) (string, error) { checkCalls++; return "v9.9.9", nil }
		m := newUpdateModel(check, nil)

		m, cmd1 := step(m, statusMsg{resp: readyStatus()})
		drain(cmd1)
		_, cmd2 := step(m, statusMsg{resp: readyStatus()})
		drain(cmd2)
		if checkCalls != 1 {
			t.Errorf("CheckUpdate fired %d times across two status successes, want exactly 1 (spec §11: at most once per session)", checkCalls)
		}
	})

	t.Run("a nil CheckUpdate simply never checks", func(t *testing.T) {
		t.Parallel()
		m := newUpdateModel(nil, nil)
		_, cmd := step(m, statusMsg{resp: readyStatus()})
		// No panic, no banner — the best-effort posture with the closure absent.
		if containsMsg[updateCheckedMsg](drain(cmd)) {
			t.Error("a nil CheckUpdate produced an updateCheckedMsg")
		}
	})
}

// TestUpdateBannerRendersOfferedTag pins the banner surface (spec §11): once
// CheckUpdate reports a newer release, the status header names it with the U
// affordance, and the model records the offered phase.
func TestUpdateBannerRendersOfferedTag(t *testing.T) {
	t.Parallel()
	m := newUpdateModel(nil, nil)
	m, _ = step(m, updateCheckedMsg{tag: "v2.1.0"})

	if m.updatePhase != updateOffered {
		t.Fatalf("updatePhase = %v after a tag arrived, want updateOffered", m.updatePhase)
	}
	if m.updateTag != "v2.1.0" {
		t.Errorf("updateTag = %q, want v2.1.0", m.updateTag)
	}
	if body := plain(m.View().Content); !strings.Contains(body, "v2.1.0 available — U to update") {
		t.Errorf("update banner missing from the status header; got:\n%s", body)
	}
}

// TestCheckUpdateSilentWhenCurrentOrErrored pins the best-effort posture: a
// check that errors, and a check that reports "already current" (empty tag),
// both leave the banner absent and push NO toast — the banner is never noise
// (Toast-tier addendum: CheckUpdate errors stay silent).
func TestCheckUpdateSilentWhenCurrentOrErrored(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		msg  updateCheckedMsg
	}{
		{name: "errored check", msg: updateCheckedMsg{err: errors.New("gh: not authenticated")}},
		{name: "already current", msg: updateCheckedMsg{tag: ""}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			m := newUpdateModel(nil, nil)
			m, cmd := step(m, testCase.msg)
			if cmd != nil {
				t.Error("a silent check produced a Cmd")
			}
			if m.updatePhase != updateIdle {
				t.Errorf("updatePhase = %v, want updateIdle (no banner)", m.updatePhase)
			}
			if m.updateTag != "" {
				t.Errorf("updateTag = %q, want empty", m.updateTag)
			}
			if m.toast != nil || m.stickyToast != nil {
				t.Errorf("a best-effort check toasted: info=%+v sticky=%+v", m.toast, m.stickyToast)
			}
			if body := plain(m.View().Content); strings.Contains(body, "U to update") {
				t.Errorf("banner shown after a silent check; got:\n%s", body)
			}
		})
	}
}

// TestUpdateConfirmAppliesOnYes pins the U → confirm → y path: U opens a footer
// confirm naming the tag, and y hands ApplyUpdate the exact offered tag and
// enters the applying phase.
func TestUpdateConfirmAppliesOnYes(t *testing.T) {
	t.Parallel()
	var applyCalls int
	var appliedTag string
	apply := func(_ context.Context, tag string) error { applyCalls++; appliedTag = tag; return nil }
	m := newUpdateModel(nil, apply)
	m.updateTag = "v2.1.0"
	m.updatePhase = updateOffered

	m, _ = step(m, key("U"))
	if m.updatePhase != updateConfirm {
		t.Fatalf("U did not open the confirm; updatePhase = %v", m.updatePhase)
	}
	if foot := plain(m.footer()); !strings.Contains(foot, "update to v2.1.0?") {
		t.Errorf("confirm footer missing the prompt; got %q", foot)
	}

	m, cmd := step(m, key("y"))
	if m.updatePhase != updateApplying {
		t.Fatalf("y did not enter the applying phase; updatePhase = %v", m.updatePhase)
	}
	msgs := drain(cmd)
	if applyCalls != 1 || appliedTag != "v2.1.0" {
		t.Errorf("ApplyUpdate calls=%d tag=%q, want exactly one call with v2.1.0", applyCalls, appliedTag)
	}
	if !containsMsg[updateAppliedMsg](msgs) {
		t.Errorf("the apply Cmd did not resolve to updateAppliedMsg; drained %#v", msgs)
	}
	if applyingFoot := plain(m.footer()); !strings.Contains(applyingFoot, "installing") {
		t.Errorf("applying footer does not name the in-flight install; got %q", applyingFoot)
	}
}

// TestUpdateConfirmDismissed pins that n and esc both back the confirm out to
// the offer without applying anything.
func TestUpdateConfirmDismissed(t *testing.T) {
	t.Parallel()
	for _, dismiss := range []string{"n", "esc"} {
		t.Run(dismiss, func(t *testing.T) {
			t.Parallel()
			var applyCalls int
			apply := func(context.Context, string) error { applyCalls++; return nil }
			m := newUpdateModel(nil, apply)
			m.updateTag = "v2.1.0"
			m.updatePhase = updateConfirm

			m, cmd := step(m, key(dismiss))
			if m.updatePhase != updateOffered {
				t.Errorf("%s did not return the confirm to the offer; updatePhase = %v", dismiss, m.updatePhase)
			}
			drain(cmd)
			if applyCalls != 0 {
				t.Errorf("%s applied the update; ApplyUpdate must only run on y", dismiss)
			}
		})
	}
}

// TestUpdateApplyErrorTogglesStickyAndOffers pins the apply-failure path
// (Toast-tier addendum): the verbatim error lands in the STICKY slot (an
// unresolved failure the user must act on, not a 5s info toast), and the banner
// returns to offering a retry.
func TestUpdateApplyErrorTogglesStickyAndOffers(t *testing.T) {
	t.Parallel()
	m := newUpdateModel(nil, nil)
	m.updateTag = "v2.1.0"
	m.updatePhase = updateApplying

	m, _ = step(m, updateAppliedMsg{err: errors.New("update: download failed: 503 Service Unavailable")})
	if m.updatePhase != updateOffered {
		t.Errorf("a failed apply did not return to the offer; updatePhase = %v", m.updatePhase)
	}
	if m.stickyToast == nil || !strings.Contains(m.stickyToast.text, "download failed: 503 Service Unavailable") {
		t.Errorf("apply error not surfaced verbatim in the sticky slot; got %+v", m.stickyToast)
	}
	if m.toast != nil {
		t.Errorf("apply error went to the info slot; a failure requiring action must be sticky (got %+v)", m.toast)
	}
}

// TestUpdateApplySuccessOffersRestart pins the clean-apply path: the banner
// swaps to the installed state with the R restart affordance.
func TestUpdateApplySuccessOffersRestart(t *testing.T) {
	t.Parallel()
	m := newUpdateModel(nil, nil)
	m.updateTag = "v2.1.0"
	m.updatePhase = updateApplying

	m, _ = step(m, updateAppliedMsg{err: nil})
	if m.updatePhase != updateInstalled {
		t.Fatalf("a clean apply did not reach the installed phase; updatePhase = %v", m.updatePhase)
	}
	if m.stickyToast != nil {
		t.Errorf("a clean apply pushed a sticky toast; want none (got %+v)", m.stickyToast)
	}
	if body := plain(m.View().Content); !strings.Contains(body, "installed v2.1.0 — R to restart the hub on it (or restart manually)") {
		t.Errorf("installed banner / R offer missing; got:\n%s", body)
	}
}

// TestRestartKeyLatchesReExecAndQuits pins R (spec §11): from the installed
// state R latches the re-exec and quits, so launchHub restarts the process onto
// the freshly installed binary.
func TestRestartKeyLatchesReExecAndQuits(t *testing.T) {
	t.Parallel()
	m := newUpdateModel(nil, nil)
	m.updateTag = "v2.1.0"
	m.updatePhase = updateInstalled

	m, cmd := step(m, key("R"))
	if !m.reExec || !m.ReExecRequested() {
		t.Error("R did not latch the re-exec request")
	}
	if !m.quitting {
		t.Error("R did not set the model quitting")
	}
	if cmd == nil {
		t.Fatal("R produced no Cmd; want tea.Quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("R's Cmd did not produce a QuitMsg")
	}
}

// TestRestartKeyInertUntilInstalled pins that R is only the restart key in the
// installed state — before an install lands it is not latched.
func TestRestartKeyInertUntilInstalled(t *testing.T) {
	t.Parallel()
	for _, phase := range []updatePhase{updateIdle, updateOffered} {
		m := newUpdateModel(nil, nil)
		m.updateTag = "v2.1.0"
		m.updatePhase = phase
		m, _ = step(m, key("R"))
		if m.reExec {
			t.Errorf("R latched a re-exec in phase %v; it must only restart from the installed state", phase)
		}
	}
}

// TestApplyingFreezesInputExceptCtrlC pins the applying-state precedence: every
// key but ctrl+c is ignored while the binary swap is in flight (spec §11), so a
// mutating tab key cannot reach the daemon and a tab switch cannot navigate away
// mid-swap.
func TestApplyingFreezesInputExceptCtrlC(t *testing.T) {
	t.Parallel()
	fake := &fakeData{status: readyStatus(), syncResp: api.SyncResponse{Status: "completed"}}
	m := New(Config{Data: fake, StartService: func() error { return nil }})
	m, _ = step(m, tea.WindowSizeMsg{Width: 110, Height: 40})
	m.status = readyStatus()
	m.updateTag = "v2.1.0"
	m.updatePhase = updateApplying
	m.active = tabProjects
	m.projects.SetUnits([]api.UnitInfo{{Provider: "claude", Folder: "x", LocalDir: "/l/x", WatchState: "watching"}})

	if m2, cmd := step(m, key("s")); cmd != nil || m2.updatePhase != updateApplying {
		t.Errorf("s during applying produced a Cmd or changed phase; want frozen (cmd=%v phase=%v)", cmd, m2.updatePhase)
	}
	if len(fake.syncCalls) != 0 {
		t.Errorf("s during applying reached the daemon: %v", fake.syncCalls)
	}
	if m3, _ := step(m, key("2")); m3.active != tabProjects {
		t.Error("a tab switch worked during applying; want frozen")
	}

	m4, cmd := step(m, key("ctrl+c"))
	if !m4.quitting {
		t.Error("ctrl+c did not quit during applying")
	}
	if cmd == nil {
		t.Fatal("ctrl+c produced no Cmd during applying")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("ctrl+c Cmd did not produce a QuitMsg during applying")
	}
}

// TestApplyingHoldsThroughTransientDaemonDown pins the interaction between the
// self-managed restart and daemon-down detection: while applying, the daemon we
// restart ourselves is transiently unreachable, which is EXPECTED — it must not
// flip the alarming daemon-down screen. The installing banner holds instead.
func TestApplyingHoldsThroughTransientDaemonDown(t *testing.T) {
	t.Parallel()
	m := newUpdateModel(nil, nil)
	m.updateTag = "v2.1.0"
	m.updatePhase = updateApplying

	m, _ = step(m, statusMsg{err: api.ErrDaemonNotRunning})
	if m.daemonDown {
		t.Error("a transient daemon-down during our own restart flipped the daemon-down screen")
	}
	body := plain(m.View().Content)
	if !strings.Contains(body, "installing v2.1.0") {
		t.Errorf("installing banner not held through the restart; got:\n%s", body)
	}
	if strings.Contains(body, "daemon is not running") {
		t.Errorf("daemon-down screen shown mid-apply; got:\n%s", body)
	}
}

// TestDaemonDownSuppressionEndsWhenApplyResolves is the release counterpart to
// TestApplyingHoldsThroughTransientDaemonDown: the daemon-down suppression is
// scoped to the applying window ONLY. The moment the apply resolves — installed
// on success, back to the offer on failure — the next daemon-down status must
// flip the daemon-down screen again, so a genuinely dead daemon after an update
// is never masked behind a normal-looking tab with a stale header. Pins the
// suppression conjunct's BOUND (m.updatePhase != updateApplying), which a later
// refactor could widen (e.g. to keep suppressing "while the daemon settles")
// with no other red test.
func TestDaemonDownSuppressionEndsWhenApplyResolves(t *testing.T) {
	t.Parallel()

	t.Run("installed: a daemon-down status flips the screen", func(t *testing.T) {
		t.Parallel()
		m := newUpdateModel(nil, nil)
		m.updateTag = "v2.1.0"
		m.updatePhase = updateInstalled

		m, _ = step(m, statusMsg{err: api.ErrDaemonNotRunning})
		if !m.daemonDown {
			t.Error("a daemon-down status at updateInstalled did not flip the daemon-down screen; suppression must end on the success side")
		}
	})

	t.Run("offered after a failed apply: a daemon-down status flips the screen", func(t *testing.T) {
		t.Parallel()
		m := newUpdateModel(nil, nil)
		m.updateTag = "v2.1.0"
		m.updatePhase = updateApplying
		// A failed apply returns the machine to the offer (the transition
		// TestUpdateApplyErrorTogglesStickyAndOffers pins).
		m, _ = step(m, updateAppliedMsg{err: errors.New("update: download failed")})
		if m.updatePhase != updateOffered {
			t.Fatalf("setup: a failed apply did not return to the offer; phase = %v", m.updatePhase)
		}

		m, _ = step(m, statusMsg{err: api.ErrDaemonNotRunning})
		if !m.daemonDown {
			t.Error("a daemon-down status at updateOffered (after a failed apply) did not flip the daemon-down screen; suppression must end on the error side too")
		}
	})
}

// TestForeignKeysInertWhileUpdateConfirmOpen mirrors
// TestSlashInertWhileQuitPromptOpen for the update confirm (spec §11): the
// confirm prompt owns the keyboard, so a foreign key — a chrome opener like `/`,
// or the very `U` that opened the confirm — neither opens an overlay, decides
// the prompt, nor produces a Cmd. Only y/Y/n/N/esc answer it. The `/` case is
// the load-bearing probe of the confirm block's default return: search-open is
// not phase-gated, so that catch-all is its only guard. The `U` case documents
// the finding's literal concern — U is inert here via the catch-all AND the
// offered-only availability gate (defense in depth), so it stands as regression
// cover even though either guard alone would hold it.
func TestForeignKeysInertWhileUpdateConfirmOpen(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"/", "U"} {
		t.Run("key "+name, func(t *testing.T) {
			t.Parallel()
			m := newUpdateModel(nil, nil)
			m.updateTag = "v2.1.0"
			m.updatePhase = updateConfirm

			m2, cmd := step(m, key(name))
			if m2.searchOverlay != nil {
				t.Errorf("%q opened the search overlay while the update confirm owns the keyboard", name)
			}
			if cmd != nil {
				t.Errorf("%q at the update confirm produced a Cmd; want none", name)
			}
			if m2.updatePhase != updateConfirm {
				t.Errorf("%q disturbed the update confirm phase (got %v); only y/n/esc may decide it", name, m2.updatePhase)
			}
		})
	}
}

// TestFlowStartRefusedDuringUpdateFlow pins that a queued flow-request message
// (the bubbletea no-ordering-guarantee race the chrome gates already close) is
// refused while an update confirm or apply owns the interaction — no flow modal
// opens underneath it, and the update phase is untouched.
func TestFlowStartRefusedDuringUpdateFlow(t *testing.T) {
	t.Parallel()
	for _, phase := range []updatePhase{updateConfirm, updateApplying} {
		m := newUpdateModel(nil, nil)
		m.updateTag = "v2.1.0"
		m.updatePhase = phase

		m2, cmd := step(m, views.EditRequestMsg{Memory: memoryfs.Memory{}})
		if m2.flowModal != nil {
			t.Errorf("phase %v: a flow modal opened under the update flow", phase)
		}
		if cmd != nil {
			t.Errorf("phase %v: the refused flow request produced a Cmd", phase)
		}
		if m2.toast == nil {
			t.Errorf("phase %v: the refused flow start did not toast", phase)
		}
		if m2.updatePhase != phase {
			t.Errorf("phase %v: the update phase changed on a refused flow request (got %v)", phase, m2.updatePhase)
		}
	}
}

// TestUpdateAgentBrainAvailability pins the U row's live gate: it is available
// (footer + palette) exactly in the offered phase — never idle (nothing to
// update), never installed (already applied; U must not re-open the confirm).
func TestUpdateAgentBrainAvailability(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{})

	if m.available("update-agent-brain") || m.paletteAvailable("update-agent-brain") {
		t.Error("update-agent-brain available while idle")
	}

	m.updatePhase = updateOffered
	m.updateTag = "v2.1.0"
	if !m.available("update-agent-brain") || !m.paletteAvailable("update-agent-brain") {
		t.Error("update-agent-brain not available while an update is offered")
	}

	m.updatePhase = updateInstalled
	if m.available("update-agent-brain") || m.paletteAvailable("update-agent-brain") {
		t.Error("update-agent-brain available while installed — U must not re-enter the confirm")
	}
}

// TestStackFrameExactFillWithBannerPresent extends the pushed-screen exact-fill
// pin to the banner-present case: the update banner is an inline segment on the
// status-header LINE (spec §2), so it adds no header row and the frame line
// count is IDENTICAL to the banner-absent pin at every toast occupancy. A width
// wide enough that the longest banner never wraps isolates the reservation
// arithmetic from wrapping. Asserting == both directions makes a banner that
// silently grew the header (pushing the footer off the bottom) fail here.
func TestStackFrameExactFillWithBannerPresent(t *testing.T) {
	t.Parallel()
	const height = 40
	const width = 160
	tests := []struct {
		name      string
		phase     updatePhase
		sticky    string
		info      string
		wantLines int
	}{
		{name: "offered, zero toasts", phase: updateOffered, wantLines: height - 4},
		{name: "offered, both toasts", phase: updateOffered, sticky: "save failed — kept at /scratch/x.md", info: "path: /home/u/x.md", wantLines: height},
		{name: "installed, zero toasts", phase: updateInstalled, wantLines: height - 4},
		{name: "installed, both toasts", phase: updateInstalled, sticky: "save failed — kept at /scratch/x.md", info: "path: /home/u/x.md", wantLines: height},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			m := newTestModel(&fakeData{status: readyStatus()})
			m, _ = step(m, tea.WindowSizeMsg{Width: width, Height: height})
			m.status = readyStatus()
			m.updateTag = "v2.1.0"
			m.updatePhase = testCase.phase
			m = m.pushScreen(fixedHeightScreen{title: "stub"})
			if testCase.sticky != "" {
				m.pushStickyToast(testCase.sticky)
			}
			if testCase.info != "" {
				m.pushToast(testCase.info)
			}

			body := plain(m.View().Content)
			if gotLines := strings.Count(body, "\n") + 1; gotLines != testCase.wantLines {
				t.Errorf("banner-present stack frame is %d lines, want exactly %d — the inline banner added a header row", gotLines, testCase.wantLines)
			}
			wantBanner := "v2.1.0 available — U to update"
			if testCase.phase == updateInstalled {
				wantBanner = "installed v2.1.0 — R to restart"
			}
			if !strings.Contains(body, wantBanner) {
				t.Errorf("banner %q missing from the frame:\n%s", wantBanner, body)
			}
		})
	}
}

// TestProjectsTabFrameExactFillWithBannerPresent extends the Projects TAB
// exact-fill pin to the banner-present case: the status header the tab budget
// reserves one line for stays one line with the banner, so the frame still
// fills the terminal exactly at full chrome and runs short with fewer toasts.
func TestProjectsTabFrameExactFillWithBannerPresent(t *testing.T) {
	t.Parallel()
	const height = 40
	const width = 160
	tests := []struct {
		name      string
		sticky    string
		info      string
		wantLines int
	}{
		{name: "zero toasts", wantLines: height - 4},
		{name: "both slots", sticky: "save failed — kept at /scratch/x.md", info: "path: /home/u/x.md", wantLines: height},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			m := newTestModel(&fakeData{status: readyStatus(), syncResp: api.SyncResponse{Status: "completed"}})
			m.version = "vtest"
			m, _ = step(m, tea.WindowSizeMsg{Width: width, Height: height})
			m.status = readyStatus()
			m.now = time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC)
			m.updateTag = "v2.1.0"
			m.updatePhase = updateOffered
			m.projects.SetUnits([]api.UnitInfo{
				{Provider: "claude", Folder: "agent-brain", LocalDir: "/l/a", WatchState: "watching"},
				{Provider: "codex", Folder: "_global", LocalDir: "/l/c", WatchState: "failed: x"},
			})
			m, _ = step(m, key("s")) // action notice — the reservation's last chrome line
			if testCase.sticky != "" {
				m.pushStickyToast(testCase.sticky)
			}
			if testCase.info != "" {
				m.pushToast(testCase.info)
			}

			body := plain(m.View().Content)
			if gotLines := strings.Count(body, "\n") + 1; gotLines != testCase.wantLines {
				t.Errorf("banner-present Projects frame is %d lines, want exactly %d — the inline banner added a header row", gotLines, testCase.wantLines)
			}
			if !strings.Contains(body, "v2.1.0 available — U to update") {
				t.Errorf("banner missing from the Projects frame:\n%s", body)
			}
		})
	}
}

// doctorReport builds a one-check battery for the doctor-action tests below.
func doctorReport(status doctor.Status, fix string) doctor.Report {
	return doctor.Report{Results: []doctor.CheckResult{
		{Name: "filters", Status: status, Detail: "filter wiring", Fix: fix},
	}}
}

// TestDoctorActionRerunRefetches pins that r on the Doctor tab re-runs the
// read-only battery on demand (spec §11) — the existing doctorCmd, now keyed.
func TestDoctorActionRerunRefetches(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{report: doctorReport(doctor.StatusOK, "")})
	m.active = tabDoctor

	_, cmd := step(m, key("r"))
	if cmd == nil {
		t.Fatal("r on the Doctor tab produced no Cmd; want a battery refetch")
	}
	if msgs := drain(cmd); !containsMsg[doctorMsg](msgs) {
		t.Fatalf("r did not refetch the doctor battery; msgs = %#v", msgs)
	}
}

// TestDoctorActionFixInvokesClosureRendersReportAndInfoToast pins the full f
// flow (spec §11): the fixing state latches, the injected RunDoctorFix runs
// once, the re-checked report re-renders, and an INFO toast confirms it.
func TestDoctorActionFixInvokesClosureRendersReportAndInfoToast(t *testing.T) {
	t.Parallel()
	fixed := doctor.Report{Results: []doctor.CheckResult{
		{Name: "filters", Status: doctor.StatusOK, Detail: "filter wiring installed", Fixed: true},
	}}
	var fixCalls int
	m := New(Config{
		Data: &fakeData{},
		RunDoctorFix: func(context.Context) (doctor.Report, error) {
			fixCalls++
			return fixed, nil
		},
	})
	m, _ = step(m, tea.WindowSizeMsg{Width: 100, Height: 40})
	m.active = tabDoctor
	m, _ = step(m, doctorMsg{report: doctorReport(doctor.StatusFail, "run `agent-brain doctor --fix`")})

	m2, cmd := step(m, key("f"))
	if cmd == nil {
		t.Fatal("f on a fixable report produced no Cmd")
	}
	if body := plain(m2.doctor.View()); !strings.Contains(body, "fixing…") {
		t.Errorf("f did not enter the fixing state; got:\n%s", body)
	}

	msgs := drain(cmd)
	if fixCalls != 1 {
		t.Fatalf("RunDoctorFix invoked %d times, want 1", fixCalls)
	}
	m3 := m2
	for _, msg := range msgs {
		m3, _ = step(m3, msg)
	}
	if body := plain(m3.doctor.View()); !strings.Contains(body, "filter wiring installed") {
		t.Errorf("re-checked report not rendered after fix; got:\n%s", body)
	}
	if m3.toast == nil || !strings.Contains(m3.toast.text, "fix applied — re-checked") {
		t.Errorf("info toast missing after fix; toast = %+v", m3.toast)
	}
	if m3.stickyToast != nil {
		t.Errorf("a clean fix must not push a sticky toast; got %+v", m3.stickyToast)
	}
}

// TestDoctorActionFixGatedOnFailingReport pins that f is inert on a passing
// report: no Cmd, no closure call, and the footer never advertises it.
func TestDoctorActionFixGatedOnFailingReport(t *testing.T) {
	t.Parallel()
	var fixCalls int
	m := New(Config{
		Data:         &fakeData{},
		RunDoctorFix: func(context.Context) (doctor.Report, error) { fixCalls++; return doctor.Report{}, nil },
	})
	m, _ = step(m, tea.WindowSizeMsg{Width: 100, Height: 40})
	m.active = tabDoctor
	m, _ = step(m, doctorMsg{report: doctorReport(doctor.StatusOK, "")})

	_, cmd := step(m, key("f"))
	if cmd != nil {
		t.Fatal("f on a passing report produced a Cmd; fix must be gated")
	}
	if fixCalls != 0 {
		t.Fatalf("RunDoctorFix invoked %d times on a passing report, want 0", fixCalls)
	}
	if got := plain(m.footer()); strings.Contains(got, "f fix") {
		t.Errorf("Doctor footer advertises fix on a passing report: %q", got)
	}
}

// TestDoctorActionScanRendersFindings pins the full s flow (spec §12): the
// scanning state latches, Scan runs over every unit (folder ""), and the
// findings render under the checks.
func TestDoctorActionScanRendersFindings(t *testing.T) {
	t.Parallel()
	var scanFolders []string
	m := New(Config{
		Data: &fakeData{},
		Scan: func(_ context.Context, folder string) ([]views.ScanFinding, error) {
			scanFolders = append(scanFolders, folder)
			return []views.ScanFinding{{Folder: "work", File: "notes.md", Rule: "generic-api-key", Line: 7}}, nil
		},
	})
	m, _ = step(m, tea.WindowSizeMsg{Width: 100, Height: 40})
	m.active = tabDoctor
	m, _ = step(m, doctorMsg{report: doctorReport(doctor.StatusOK, "")})

	m2, cmd := step(m, key("s"))
	if cmd == nil {
		t.Fatal("s on the Doctor tab produced no Cmd")
	}
	if body := plain(m2.doctor.View()); !strings.Contains(body, "scanning…") {
		t.Errorf("s did not enter the scanning state; got:\n%s", body)
	}

	msgs := drain(cmd)
	if diff := cmp.Diff([]string{""}, scanFolders); diff != "" {
		t.Fatalf("Scan folder args (-want +got):\n%s", diff)
	}
	m3 := m2
	for _, msg := range msgs {
		m3, _ = step(m3, msg)
	}
	body := plain(m3.doctor.View())
	for _, want := range []string{"1 finding in 1 file", "work/notes.md:7", "generic-api-key"} {
		if !strings.Contains(body, want) {
			t.Errorf("scan findings not rendered; missing %q; got:\n%s", want, body)
		}
	}
}

// TestDoctorPaletteDispatchReachesFixAndScan is the spec §14 parity pin: a
// palette choice runs the identical runner a direct key does, for both doctor
// actions that carry one.
func TestDoctorPaletteDispatchReachesFixAndScan(t *testing.T) {
	t.Parallel()
	var fixCalls, scanCalls int
	newModel := func() Model {
		m := New(Config{
			Data: &fakeData{},
			RunDoctorFix: func(context.Context) (doctor.Report, error) {
				fixCalls++
				return doctorReport(doctor.StatusOK, ""), nil
			},
			Scan: func(context.Context, string) ([]views.ScanFinding, error) {
				scanCalls++
				return nil, nil
			},
		})
		m, _ = step(m, tea.WindowSizeMsg{Width: 100, Height: 40})
		m.active = tabDoctor
		m, _ = step(m, doctorMsg{report: doctorReport(doctor.StatusFail, "run `agent-brain doctor --fix`")})
		return m
	}

	drive(t, newModel(), views.PaletteChoiceMsg{ID: "scan"})
	if scanCalls != 1 {
		t.Fatalf("palette scan invoked the closure %d times, want 1", scanCalls)
	}
	drive(t, newModel(), views.PaletteChoiceMsg{ID: "doctor-fix"})
	if fixCalls != 1 {
		t.Fatalf("palette doctor-fix invoked the closure %d times, want 1", fixCalls)
	}
}

// TestDoctorFixRefusedWhileQuiesced pins the Mutates half of doctor-fix (spec
// §15): f holds the daemon quiescent, so pressing it while a quiesce is active
// must refuse LOCALLY — before the fix Cmd is ever scheduled — and toast why,
// exactly like sync/untrack on the Projects tab.
func TestDoctorFixRefusedWhileQuiesced(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	var fixCalls int
	m := New(Config{
		Data:         &fakeData{},
		RunDoctorFix: func(context.Context) (doctor.Report, error) { fixCalls++; return doctor.Report{}, nil },
	})
	m, _ = step(m, tea.WindowSizeMsg{Width: 100, Height: 40})
	m.now = now
	m.status = api.StatusResponse{State: "ready", QuiescedUntil: &future}
	m.active = tabDoctor
	m, _ = step(m, doctorMsg{report: doctorReport(doctor.StatusFail, "run `agent-brain doctor --fix`")})

	next, cmd := step(m, key("f"))
	if cmd != nil {
		t.Fatal("f produced a Cmd while quiesced; the fix must be refused before it is scheduled")
	}
	if fixCalls != 0 {
		t.Fatalf("doctor fix ran while quiesced: %d calls", fixCalls)
	}
	if next.toast == nil || !strings.Contains(next.toast.text, "quiesced") {
		t.Errorf("f while quiesced did not toast the refusal; toast = %+v", next.toast)
	}
	if strings.Contains(plain(next.doctor.View()), "fixing…") {
		t.Error("the fixing state latched despite the quiesce refusal")
	}
}

// TestDoctorScanNotRefusedWhileQuiesced is the false-positive guard for the
// same gate: scan is advisory and never joins SafetyGate (spec §12), so it is
// NOT Mutates and must run normally even while the daemon is quiesced — a
// quiesce must never gate a read-only hygiene sweep.
func TestDoctorScanNotRefusedWhileQuiesced(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	var scanCalls int
	m := New(Config{
		Data: &fakeData{},
		Scan: func(context.Context, string) ([]views.ScanFinding, error) { scanCalls++; return nil, nil },
	})
	m, _ = step(m, tea.WindowSizeMsg{Width: 100, Height: 40})
	m.now = now
	m.status = api.StatusResponse{State: "ready", QuiescedUntil: &future}
	m.active = tabDoctor
	m, _ = step(m, doctorMsg{report: doctorReport(doctor.StatusOK, "")})

	next, cmd := step(m, key("s"))
	if cmd == nil {
		t.Fatal("s produced no Cmd while quiesced; an advisory scan must not be quiesce-gated")
	}
	if next.toast != nil {
		t.Errorf("scan toasted a quiesce refusal; it is not a Mutates action: %+v", next.toast)
	}
	drain(cmd)
	if scanCalls != 1 {
		t.Fatalf("scan ran %d times while quiesced, want 1", scanCalls)
	}
}
