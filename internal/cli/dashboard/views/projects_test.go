package views

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/google/go-cmp/cmp"

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
// unconditionally at Render and has no plain-render mode, so stripping the
// escapes here keeps this package's import set free of test-only
// dependencies.
func plain(s string) string {
	return csiPattern.ReplaceAllString(s, "")
}

// fakeData is an injectable DataSource: canned reads, and recorded mutating
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

// History and Blob satisfy the grown DataSource surface (Task 14). The
// ProjectsView tests this fake serves never drill into version history, so
// they answer empty — the History screen's own suite injects a dedicated
// HistoryDataSource fake instead.
func (f *fakeData) History(context.Context, string, string, int) (api.HistoryResponse, error) {
	return api.HistoryResponse{}, nil
}

func (f *fakeData) Blob(context.Context, string, string, string) (api.BlobResponse, error) {
	return api.BlobResponse{}, nil
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
	case "space":
		// The multi-select picker's toggle: Code 0x20 stringifies to "space",
		// which is what DashboardKeys.Toggle matches — key("space") via the
		// rune-default arm would wrongly build an 's' press, so it is explicit here.
		return tea.KeyPressMsg{Code: ' ', Text: " "}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	default:
		return tea.KeyPressMsg{Code: []rune(name)[0], Text: name}
	}
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

func readyStatus() api.StatusResponse {
	return api.StatusResponse{
		State:     "ready",
		Version:   "dev",
		PID:       4242,
		StartedAt: time.Date(2026, 7, 9, 9, 0, 0, 0, time.UTC),
		LastSync:  &api.SyncSummary{At: time.Date(2026, 7, 9, 11, 0, 0, 0, time.UTC), Pushed: true},
	}
}

func twoUnits() api.ProjectsResponse {
	finished := time.Date(2026, 7, 9, 11, 0, 0, 0, time.UTC)
	return api.ProjectsResponse{Units: []api.UnitInfo{
		{
			Provider: "claude", Folder: "agent-brain", LocalDir: "/home/u/.claude/projects/agent-brain/memory", Degraded: false,
			WatchState: "watching", WatchTriggers: 12, LastCycle: &api.UnitCycleResult{Outcome: "ok", FinishedAt: finished},
		},
		{
			Provider: "codex", Folder: "_global", LocalDir: "/home/u/.codex/memories", Degraded: true,
			WatchState:    "failed: watch /home/u/.codex/memories: too many open files; ticker/poll backstop still covers it",
			WatchTriggers: 0, LastCycle: &api.UnitCycleResult{Outcome: "degraded", FinishedAt: finished},
		},
	}}
}

// loadedProjectsView builds a ProjectsView sized for a roomy-enough terminal
// with the given status + units already delivered — the views-level
// equivalent of driving a full root Model through New/step now that the
// view is tested standalone (root's own test suite covers the root+view
// wiring: tab routing, the persistent status header, and the post-track
// fleet-sync orchestration).
func loadedProjectsView(data *fakeData) ProjectsView {
	view := NewProjectsView()
	view.SetSize(110, 40)
	view.SetUnits(data.projects.Units)
	return view
}

// trackActionsFor builds a TrackActions whose Discover/Identify closures
// return the given candidates/identity without any real provider
// composition.
func trackActionsFor(candidates []TrackCandidate, identity provider.Identity, identifyErr error) TrackActions {
	return TrackActions{
		Discover: func(context.Context) ([]TrackCandidate, error) {
			return candidates, nil
		},
		Identify: func(_ context.Context, _ string, _ TrackRoot, _ string) (provider.Identity, error) {
			return identity, identifyErr
		},
	}
}

// drive feeds one message into the view and executes any returned Cmd
// synchronously — flattening tea.Batch the way a running program would —
// dispatching every produced message to the method the root would have
// routed it to, until the view goes quiet. It lets a test walk the full
// a → discover → pick → confirm → identify → track chain without a running
// program or the root model that ordinarily does this routing.
func drive(t *testing.T, view *ProjectsView, data DataSource, actions TrackActions, msg tea.Msg) {
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
		var cmd tea.Cmd
		switch msg := next.(type) {
		case tea.KeyPressMsg:
			cmd = view.Update(msg, data, actions, MigrateActions{})
		case DiscoverMsg:
			view.OnDiscover(msg)
		case IdentifyMsg:
			cmd = view.OnIdentify(msg, data)
		case TrackResultMsg:
			// OnTrackResult returns the next queued candidate's Cmd (multi-select),
			// so the driver must execute it to walk the rest of the queue.
			cmd = view.OnTrackResult(msg, data)
		}
		if cmd != nil {
			queue = append(queue, cmd())
		}
	}
}

// driveMigrate is drive's twin for the spec §10 migrate flow: it walks the
// m → preflight → discover → pick → confirm → identify → migrate chain,
// dispatching each produced message to the On* method the root would have
// routed it to. The root, not the view, owns the terminal toasts, so this
// helper stands in for only the routing a view test needs: a failed preflight
// resets the flow here the way the root's handler does, and the success toast /
// fleet sync it fires are asserted in the root suite instead.
func driveMigrate(t *testing.T, view *ProjectsView, data DataSource, migrate MigrateActions, msg tea.Msg) {
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
		var cmd tea.Cmd
		switch msg := next.(type) {
		case tea.KeyPressMsg:
			cmd = view.Update(msg, data, TrackActions{}, migrate)
		case MigratePreflightMsg:
			if msg.Err != nil {
				view.ResetMigrate()
			} else {
				cmd = view.OnMigratePreflightOK(migrate)
			}
		case MigrateDiscoverMsg:
			view.OnMigrateDiscover(msg)
		case MigrateIdentifyMsg:
			cmd = view.OnMigrateIdentify(msg, data)
		case MigrateResultMsg:
			view.OnMigrateResult(msg)
		}
		if cmd != nil {
			queue = append(queue, cmd())
		}
	}
}

// migrateActionsFor builds a MigrateActions whose closures return the given
// canned values without any real chezmoi/provider composition. Preflight always
// passes here — the once-per-session gate and its failure path build their own
// actions (TestMigratePreflightRunsOncePerSession and the root's sticky-abort
// test). candidates feed the picker; identity/identifyErr resolve the confirmed
// path; liveDir is what LiveDirFor returns when the user CORRECTS the path (an
// unchanged path uses the candidate's own precomputed LiveDir, never this).
func migrateActionsFor(candidates []MigrateCandidate, identity provider.Identity, identifyErr error, liveDir string) MigrateActions {
	return MigrateActions{
		Preflight: func(context.Context) error { return nil },
		Discover: func(context.Context) ([]MigrateCandidate, error) {
			return candidates, nil
		},
		Identify: func(_ context.Context, _ string, _ TrackRoot, _ string) (provider.Identity, error) {
			return identity, identifyErr
		},
		LiveDirFor: func(_ /*providerName*/, _ /*projectPath*/ string) (string, error) {
			return liveDir, nil
		},
	}
}

func TestProjectsTableRenders(t *testing.T) {
	t.Parallel()
	data := &fakeData{status: readyStatus(), projects: twoUnits()}
	view := loadedProjectsView(data)

	// The table carries per-unit columns, now including the genuine per-unit
	// watch state and last-cycle the API serves (Task 6.5): claude/agent-brain is
	// watching + ok, codex/_global is a failed watch + degraded.
	table := plain(view.View(""))
	for _, want := range []string{
		"PROVIDER", "FOLDER", "HEALTH", "WATCH", "LAST CYCLE",
		"claude", "agent-brain", "codex", "_global", "degraded",
		"watching", "failed", "ok",
	} {
		if !strings.Contains(table, want) {
			t.Errorf("projects table missing %q; got:\n%s", want, table)
		}
	}
}

func TestWatchCell(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct{ in, want string }{
		{"watching", "watching"},
		{"failed: watch /l/x: too many open files; ticker/poll backstop still covers it", "failed"},
		{"", "—"},
	} {
		if got := watchCell(testCase.in); got != testCase.want {
			t.Errorf("watchCell(%q) = %q, want %q", testCase.in, got, testCase.want)
		}
	}
}

func TestLastCycleCell(t *testing.T) {
	t.Parallel()
	finished := time.Date(2026, 7, 9, 11, 0, 0, 0, time.UTC)
	if got := lastCycleCell(nil); got != "—" {
		t.Errorf("lastCycleCell(nil) = %q, want — (no cycle yet)", got)
	}
	for _, outcome := range []string{"ok", "degraded", "error"} {
		if got := lastCycleCell(&api.UnitCycleResult{Outcome: outcome, FinishedAt: finished}); got != outcome {
			t.Errorf("lastCycleCell(%q) = %q, want %q", outcome, got, outcome)
		}
	}
}

// TestProjectsTelemetryColumnsRenderEmptyAndError covers the two per-unit states
// the shared twoUnits fixture omits: a unit with no telemetry yet (dashes) and a
// unit whose last cycle errored (a state HEALTH's degraded-bool cannot show).
func TestProjectsTelemetryColumnsRenderEmptyAndError(t *testing.T) {
	t.Parallel()
	finished := time.Date(2026, 7, 9, 11, 0, 0, 0, time.UTC)
	units := api.ProjectsResponse{Units: []api.UnitInfo{
		{Provider: "claude", Folder: "erroring", LocalDir: "/l/e", WatchState: "watching", LastCycle: &api.UnitCycleResult{Outcome: "error", FinishedAt: finished}},
		{Provider: "claude", Folder: "fresh", LocalDir: "/l/f"}, // never watched, never cycled
	}}
	data := &fakeData{status: readyStatus(), projects: units}
	view := loadedProjectsView(data)
	table := plain(view.View(""))
	for _, want := range []string{"error", "—"} {
		if !strings.Contains(table, want) {
			t.Errorf("projects table missing %q; got:\n%s", want, table)
		}
	}
}

func TestProjectsWideTableShowsLocalDir(t *testing.T) {
	t.Parallel()
	data := &fakeData{status: readyStatus(), projects: twoUnits()}
	view := loadedProjectsView(data) // shared 110-col size: the five essentials only

	// LOCAL DIR is the optional roomy-terminal column; it stays hidden until the
	// terminal is wide enough to carry the full path beside the essentials.
	narrow := plain(view.View(""))
	if strings.Contains(narrow, "LOCAL DIR") {
		t.Errorf("LOCAL DIR shown at the narrow 110-col size; got:\n%s", narrow)
	}

	view.SetSize(130, 40)
	wide := plain(view.View(""))
	for _, want := range []string{"LOCAL DIR", "/home/u/.claude/projects/agent-brain/memory"} {
		if !strings.Contains(wide, want) {
			t.Errorf("wide projects table missing %q; got:\n%s", want, wide)
		}
	}
}

// TestProjectsResizeAcrossWideBoundary pins the resize-crash fix: a width
// change that crosses the LOCAL DIR boundary must swap the table's rows
// and columns atomically. bubbles renders inside SetColumns (SetColumns ->
// UpdateViewport -> renderRow indexes m.cols by each ROW's cell count), so
// installing the narrow five-column set while the table still holds the wide
// six-cell rows indexes cols[5] out of range and panics. The fixture drives a
// full narrow-seed -> wide -> narrow -> wide round trip so BOTH boundary
// directions are exercised (wide -> narrow is the one that panicked; narrow ->
// wide survived only by accident, the shorter row bounding the loop), and it
// moves the selection off row 0 so the resize's selection-preservation is
// observable rather than masked by a default-zero cursor.
func TestProjectsResizeAcrossWideBoundary(t *testing.T) {
	t.Parallel()
	units := []api.UnitInfo{
		{Provider: "claude", Folder: "agent-brain", LocalDir: "/home/u/.claude/projects/agent-brain/memory"},
		{Provider: "codex", Folder: "_global", LocalDir: "/home/u/.codex/memories"},
	}
	const localDir = "/home/u/.claude/projects/agent-brain/memory"

	view := NewProjectsView() // constructed narrow: five columns, no LOCAL DIR
	view.SetUnits(units)      // five-cell rows seeded, cursor seated on row 0

	// Fresh narrow -> wide: installs the sixth column over five-cell rows. Safe
	// even unfixed (the shorter row bounds renderRow's loop), so it must stay
	// safe and actually surface LOCAL DIR.
	view.SetSize(130, 40)
	wide := plain(view.View(""))
	for _, want := range []string{"LOCAL DIR", localDir} {
		if !strings.Contains(wide, want) {
			t.Fatalf("wide table missing %q before the shrink; got:\n%s", want, wide)
		}
	}

	// Move the selection to row 1 so preservation across the swap is a real
	// assertion, not a 0 == 0 tautology.
	view.Update(key("down"), &fakeData{}, TrackActions{}, MigrateActions{})
	if sel, ok := view.SelectedUnit(); !ok || sel.Folder != units[1].Folder {
		t.Fatalf("cursor did not move to row 1: selected %q (ok=%v), want %q", sel.Folder, ok, units[1].Folder)
	}

	// Wide -> narrow crosses the boundary the other way. Unfixed this panics
	// inside SetColumns (cols[5] on a six-cell row); fixed it is safe, drops the
	// LOCAL DIR column, and keeps the selection on the row-1 unit.
	view.SetSize(118, 40)
	narrow := plain(view.View(""))
	if strings.Contains(narrow, "LOCAL DIR") {
		t.Errorf("LOCAL DIR still shown after shrinking below the wide boundary; got:\n%s", narrow)
	}
	if strings.Contains(narrow, localDir) {
		t.Errorf("narrow table still renders the LOCAL DIR path; got:\n%s", narrow)
	}
	if sel, ok := view.SelectedUnit(); !ok || sel.Folder != units[1].Folder {
		t.Errorf("selection after wide->narrow = %q (ok=%v), want the row-1 unit %q preserved across the resize", sel.Folder, ok, units[1].Folder)
	}

	// Narrow -> wide brings the column back and re-seeds the paths into it.
	view.SetSize(130, 40)
	reWide := plain(view.View(""))
	for _, want := range []string{"LOCAL DIR", localDir} {
		if !strings.Contains(reWide, want) {
			t.Errorf("wide table after the round trip missing %q; got:\n%s", want, reWide)
		}
	}
	if sel, ok := view.SelectedUnit(); !ok || sel.Folder != units[1].Folder {
		t.Errorf("selection after the round trip = %q (ok=%v), want %q still preserved", sel.Folder, ok, units[1].Folder)
	}
}

// TestProjectsViewTableFillsBodyBudget pins SetSize's reservation directly: the
// table sizes itself to the tab body budget it is handed minus the fixed chrome
// the Projects view draws around it — the section title, its trailing blank
// line, the fleet header one row above the table, and the action-notice row one
// row below — plus the table's own column-header row. A taller budget therefore
// shows strictly more rows, and the rendered body never exceeds the budget, so
// the root's fitAndFillHeight never has to clip a real row. This is the per-view
// half of the reflow; the root wires the budget to the live toast occupancy
// (dashboard_test.go's reflow pins).
func TestProjectsViewTableFillsBodyBudget(t *testing.T) {
	t.Parallel()
	// A fleet taller than any budget below, so the visible window — never the row
	// count — is the binding constraint and the count reads back the reservation.
	units := make([]api.UnitInfo, 40)
	for i := range units {
		units[i] = api.UnitInfo{Provider: "claude", Folder: fmt.Sprintf("proj-%02d", i), LocalDir: fmt.Sprintf("/l/%02d", i), WatchState: "watching"}
	}
	view := NewProjectsView()
	view.SetUnits(units)
	// A populated action notice: the notice row renders only when non-empty (View's
	// notice case), so without it the fixture's body reads one line short and the
	// exact-fill check below cannot see downward tableChromeLines drift. The text
	// must avoid the "proj-" row token so it does not inflate the row count.
	view.notice = "action pending"

	for _, budget := range []int{18, 24, 30} {
		view.SetSize(110, budget)
		body := plain(view.View("40 units"))
		// tableChromeLines (4) of chrome plus the table's own header row leave
		// budget-5 data rows visible; a static, budget-blind reservation would show
		// the same small count at every budget.
		if got, want := strings.Count(body, "proj-"), budget-5; got != want {
			t.Errorf("budget %d: %d table rows visible, want %d", budget, got, want)
		}
		// The rendered body fills the budget EXACTLY: the four chrome lines
		// (tableChromeLines — title, blank, fleet header, action notice) plus the
		// table sized to budget-tableChromeLines. A body one short would let the
		// root pad a blank band above the footer; one long would force
		// fitAndFillHeight to clip a real row. Either way tableChromeLines no longer
		// matches the chrome the view draws — which a `> budget` ceiling misses.
		if got := strings.Count(body, "\n") + 1; got != budget {
			t.Errorf("budget %d: body is %d lines, want exactly %d", budget, got, budget)
		}
	}
}

func TestProjectsEmptyState(t *testing.T) {
	t.Parallel()
	data := &fakeData{status: readyStatus(), projects: api.ProjectsResponse{}}
	view := loadedProjectsView(data)
	body := plain(view.View(""))
	if !strings.Contains(body, "no projects enrolled") {
		t.Errorf("empty projects view missing guidance; got:\n%s", body)
	}
}

func TestProjectsSyncKey(t *testing.T) {
	t.Parallel()
	data := &fakeData{status: readyStatus(), projects: twoUnits(), syncResp: api.SyncResponse{Status: "completed"}}
	view := loadedProjectsView(data)

	cmd := view.Update(key("s"), data, TrackActions{}, MigrateActions{})
	msgs := drain(cmd)

	if diff := cmp.Diff([]string{"agent-brain"}, data.syncCalls); diff != "" {
		t.Errorf("Sync calls mismatch (-want +got):\n%s", diff)
	}
	if !containsMsg[SyncResultMsg](msgs) {
		t.Error("s did not produce a SyncResultMsg")
	}
	// Feeding the result back surfaces a notice on the view.
	for _, m := range msgs {
		if result, ok := m.(SyncResultMsg); ok {
			view.OnSyncResult(result)
		}
	}
	if !strings.Contains(plain(view.View("")), "synced agent-brain") {
		t.Errorf("sync notice not shown; got:\n%s", plain(view.View("")))
	}
}

func TestProjectsUntrackToggleConfirmsThenCalls(t *testing.T) {
	t.Parallel()
	data := &fakeData{status: readyStatus(), projects: twoUnits(), untrackResp: api.UntrackResponse{Removed: true}}
	view := loadedProjectsView(data)

	// u opens the confirm; no call yet.
	cmd := view.Update(key("u"), data, TrackActions{}, MigrateActions{})
	if cmd != nil {
		t.Error("u should not act before confirmation")
	}
	if !strings.Contains(plain(view.View("")), "untrack agent-brain? (y/N)") {
		t.Errorf("confirm prompt not shown; got:\n%s", plain(view.View("")))
	}
	if len(data.untrackCalls) != 0 {
		t.Fatalf("Untrack called before confirmation: %+v", data.untrackCalls)
	}

	// y confirms and fires exactly the enrolled unit, never a purge.
	cmd = view.Update(key("y"), data, TrackActions{}, MigrateActions{})
	msgs := drain(cmd)
	want := []api.UntrackRequest{{Provider: "claude", LocalDir: "/home/u/.claude/projects/agent-brain/memory", Purge: false}}
	if diff := cmp.Diff(want, data.untrackCalls); diff != "" {
		t.Errorf("Untrack request mismatch (-want +got):\n%s", diff)
	}
	if !containsMsg[UntrackResultMsg](msgs) {
		t.Error("y did not produce an UntrackResultMsg")
	}
}

func TestProjectsUntrackToggleCancels(t *testing.T) {
	t.Parallel()
	data := &fakeData{status: readyStatus(), projects: twoUnits()}
	view := loadedProjectsView(data)

	view.Update(key("u"), data, TrackActions{}, MigrateActions{})
	cmd := view.Update(key("n"), data, TrackActions{}, MigrateActions{})
	if cmd != nil {
		t.Error("cancelling the confirm should issue no Cmd")
	}
	if len(data.untrackCalls) != 0 {
		t.Errorf("Untrack called despite cancel: %+v", data.untrackCalls)
	}
	if view.Confirming {
		t.Error("confirm state not cleared after cancel")
	}
	if !strings.Contains(plain(view.View("")), "untrack cancelled") {
		t.Errorf("cancel notice not shown; got:\n%s", plain(view.View("")))
	}
}

// TestProjectsUntrackUsesCapturedUnitNotMovingCursor pins I-1: `y` must untrack
// the unit named in the prompt, never whatever the cursor happens to point at
// when the key is pressed. The shared 2s poll rebuilds rows while the confirm
// sits open, and rebuild only clamps out-of-range cursors — so an in-range
// index can come to point at a different unit mid-confirm. Capturing the unit at
// `t` and untracking that identity at `y` (never re-resolving through the moved
// cursor) is the fix.
func TestProjectsUntrackUsesCapturedUnitNotMovingCursor(t *testing.T) {
	t.Parallel()
	data := &fakeData{status: readyStatus(), projects: twoUnits(), untrackResp: api.UntrackResponse{Removed: true}}
	view := loadedProjectsView(data) // cursor seats on row 0 = claude/agent-brain (unit X)

	// Open the confirm on the highlighted unit X.
	view.Update(key("u"), data, TrackActions{}, MigrateActions{})
	if !strings.Contains(plain(view.View("")), "untrack agent-brain? (y/N)") {
		t.Fatalf("confirm did not open on agent-brain; got:\n%s", plain(view.View("")))
	}

	// A poll lands mid-confirm and reorders the fleet, so cursor index 0 now
	// points at a DIFFERENT unit Y (codex/_global).
	reordered := []api.UnitInfo{
		twoUnits().Units[1], // codex/_global now at index 0
		twoUnits().Units[0], // claude/agent-brain now at index 1
	}
	view.SetUnits(reordered)

	// y must untrack X (the named unit), never Y (the unit under the moved cursor).
	cmd := view.Update(key("y"), data, TrackActions{}, MigrateActions{})
	drain(cmd)

	wantX := []api.UntrackRequest{{Provider: "claude", LocalDir: "/home/u/.claude/projects/agent-brain/memory", Purge: false}}
	if diff := cmp.Diff(wantX, data.untrackCalls); diff != "" {
		t.Errorf("untrack hit the wrong unit after a mid-confirm fleet reorder (-want +got):\n%s", diff)
	}
}

// TestUntrackRebindToU pins the spec §13 rebind: u opens the untrack confirm
// and t is now a plain dead key on the Projects tab — not just "u also
// works," but "t specifically no longer does," since a stale binding still
// working would silently resurrect the old key the palette/footer no longer
// advertise.
func TestUntrackRebindToU(t *testing.T) {
	t.Parallel()
	data := &fakeData{status: readyStatus(), projects: twoUnits()}
	view := loadedProjectsView(data)

	cmd := view.Update(key("t"), data, TrackActions{}, MigrateActions{})
	if cmd != nil {
		t.Error("t produced a Cmd; want a dead key now that untrack is rebound to u")
	}
	if view.Confirming {
		t.Fatal("t opened the untrack confirm; want it dead post-rebind")
	}

	cmd = view.Update(key("u"), data, TrackActions{}, MigrateActions{})
	if cmd != nil {
		t.Error("u should not act before confirmation")
	}
	if !view.Confirming {
		t.Fatal("u did not open the untrack confirm")
	}
	if !strings.Contains(plain(view.View("")), "untrack agent-brain? (y/N)") {
		t.Errorf("confirm prompt not shown; got:\n%s", plain(view.View("")))
	}
}

// TestProjectsLoadError renders the Projects error path, matching the error
// coverage the Conflicts, Activity, and Doctor views already carry (N-2).
func TestProjectsLoadError(t *testing.T) {
	t.Parallel()
	view := NewProjectsView()
	view.SetLoadErr(errors.New("dial unix: connection refused"))

	body := plain(view.View(""))
	for _, want := range []string{"projects unavailable", "connection refused"} {
		if !strings.Contains(body, want) {
			t.Errorf("projects load-error view missing %q; got:\n%s", want, body)
		}
	}
}

// TestProjectsLoadingBeforeFirstLoad pins N-1: until the first unit delivery
// arrives the view shows a neutral loading line, so the genuinely-empty
// "no projects enrolled" guidance cannot flash on open before data loads.
func TestProjectsLoadingBeforeFirstLoad(t *testing.T) {
	t.Parallel()
	view := NewProjectsView()
	view.SetSize(110, 40)

	body := plain(view.View(""))
	if strings.Contains(body, "no projects enrolled") {
		t.Errorf("empty-state guidance flashed before the first load; got:\n%s", body)
	}
	if !strings.Contains(body, "loading") {
		t.Errorf("pre-load view missing a loading indicator; got:\n%s", body)
	}
}

func TestProjectsAddDiscoverEmpty(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	actions := trackActionsFor(nil, provider.Identity{}, nil)
	view := NewProjectsView()

	drive(t, &view, fake, actions, key("a"))
	if got := plain(view.View("")); !strings.Contains(got, "no new memory roots") {
		t.Fatalf("empty discovery view = %q, want a 'no new memory roots' notice", got)
	}
	if len(fake.trackCalls) != 0 {
		t.Fatalf("empty discovery must not track: %v", fake.trackCalls)
	}
}

func TestProjectsAddRemoteProjectFlow(t *testing.T) {
	t.Parallel()
	fake := &fakeData{trackResp: api.TrackResponse{Folder: "myrepo"}}
	candidates := []TrackCandidate{{
		Provider:  "claude",
		Label:     "claude  myrepo  → /g/myrepo",
		PathGuess: "/g/myrepo",
		Roots:     []TrackRoot{{LocalDir: "/home/u/.claude/projects/-g-myrepo/memory"}},
	}}
	identity := provider.Identity{ProjectID: "github.com/owner/myrepo", PreferredFolder: "myrepo"}
	actions := trackActionsFor(candidates, identity, nil)
	view := NewProjectsView()

	drive(t, &view, fake, actions, key("a"))     // discover → picker
	drive(t, &view, fake, actions, key("space")) // select the highlighted candidate
	drive(t, &view, fake, actions, key("enter")) // confirm the set → path-confirm input, prefilled with PathGuess
	if got := plain(view.View("")); !strings.Contains(got, "/g/myrepo") {
		t.Fatalf("path-confirm view = %q, want the PathGuess prefill visible", got)
	}
	drive(t, &view, fake, actions, key("enter")) // accept path → identify → track → fleet sync

	if len(fake.trackCalls) != 1 {
		t.Fatalf("trackCalls = %v, want exactly one", fake.trackCalls)
	}
	call := fake.trackCalls[0]
	if call.ProjectID != "github.com/owner/myrepo" || call.PreferredFolder != "myrepo" ||
		call.LocalDir != "/home/u/.claude/projects/-g-myrepo/memory" {
		t.Fatalf("track request = %+v, want the identified project", call)
	}
}

func TestProjectsAddRemotelessNamesFolder(t *testing.T) {
	t.Parallel()
	fake := &fakeData{trackResp: api.TrackResponse{Folder: "scratch"}}
	candidates := []TrackCandidate{{
		Provider:  "claude",
		Label:     "claude  scratch  → /g/scratch",
		PathGuess: "/g/scratch",
		Roots:     []TrackRoot{{LocalDir: "/home/u/.claude/projects/-g-scratch/memory"}},
	}}
	// Identify resolves no remote: empty ProjectID, PreferredFolder as hint.
	identity := provider.Identity{PreferredFolder: "scratch"}
	actions := trackActionsFor(candidates, identity, nil)
	view := NewProjectsView()

	drive(t, &view, fake, actions, key("a"))
	drive(t, &view, fake, actions, key("space")) // select the candidate
	drive(t, &view, fake, actions, key("enter")) // confirm the set → path confirm
	drive(t, &view, fake, actions, key("enter")) // accept path → identify → remoteless → naming input

	// An invalid name must be refused locally (repo.ValidateFolderName),
	// before any wire call.
	view.addInput.SetValue("bad/name")
	drive(t, &view, fake, actions, key("enter"))
	if len(fake.trackCalls) != 0 {
		t.Fatalf("invalid folder name reached the daemon: %v", fake.trackCalls)
	}

	view.addInput.SetValue("scratch")
	drive(t, &view, fake, actions, key("enter"))
	if len(fake.trackCalls) != 1 {
		t.Fatalf("trackCalls = %v, want exactly one after a valid name", fake.trackCalls)
	}
	if got := fake.trackCalls[0].ProjectID; got != "named/scratch" {
		t.Fatalf("ProjectID = %q, want %q (provider.NamedIdentity contract)", got, "named/scratch")
	}
}

// TestProjectsAddIdentifyFailureAborts covers the identity-resolution error
// path: when Identify fails (e.g. an unreadable git remote), the flow resets
// to no add, surfaces the reason, and never reaches the daemon with a
// half-resolved enrollment.
func TestProjectsAddIdentifyFailureAborts(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	candidates := []TrackCandidate{{
		Provider:  "claude",
		Label:     "claude  myrepo  → /g/myrepo",
		PathGuess: "/g/myrepo",
		Roots:     []TrackRoot{{LocalDir: "/home/u/.claude/projects/-g-myrepo/memory"}},
	}}
	actions := trackActionsFor(candidates, provider.Identity{}, errors.New("read git remote: permission denied"))
	view := NewProjectsView()

	drive(t, &view, fake, actions, key("a"))     // discover → picker
	drive(t, &view, fake, actions, key("space")) // select the candidate
	drive(t, &view, fake, actions, key("enter")) // confirm the set → path confirm
	drive(t, &view, fake, actions, key("enter")) // accept path → identify (fails)

	if view.Adding != AddNone {
		t.Fatalf("Adding = %v, want AddNone after an identify failure", view.Adding)
	}
	if len(fake.trackCalls) != 0 {
		t.Fatalf("identify failure still reached the daemon: %v", fake.trackCalls)
	}
	if got := plain(view.View("")); !strings.Contains(got, "identify failed") {
		t.Fatalf("view = %q, want an 'identify failed' notice", got)
	}
}

// TestProjectsAddDiscoverFailureAborts covers the discovery error branch: when
// the injected discover closure fails (e.g. a provider scan cannot read a
// memory root), pressing a resets the flow to no add, surfaces the reason, and
// never reaches the daemon. Wires Identify too, since add availability gates
// on BOTH closures (a build with only discover keeps the a key dead, covered
// by TestFooterAndDispatchGateAddOnBothClosures at the root).
func TestProjectsAddDiscoverFailureAborts(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	actions := TrackActions{
		Discover: func(context.Context) ([]TrackCandidate, error) {
			return nil, errors.New("scan providers: permission denied")
		},
		Identify: func(context.Context, string, TrackRoot, string) (provider.Identity, error) {
			return provider.Identity{}, nil
		},
	}
	view := NewProjectsView()

	drive(t, &view, fake, actions, key("a")) // discover fails

	if view.Adding != AddNone {
		t.Fatalf("Adding = %v, want AddNone after a discover failure", view.Adding)
	}
	if got := plain(view.View("")); !strings.Contains(got, "discover failed") ||
		!strings.Contains(got, "permission denied") {
		t.Fatalf("view = %q, want a 'discover failed' notice carrying the reason", got)
	}
	if len(fake.trackCalls) != 0 || len(fake.syncCalls) != 0 {
		t.Fatalf("discover failure still reached the daemon: track=%v sync=%v", fake.trackCalls, fake.syncCalls)
	}
}

// TestProjectsStaleTrackResultPreservesPickerCursorAndCandidates is the
// views-level counterpart to root's TestProjectsStaleTrackResultKeepsNewFlow:
// it drives the identical stale-result interleaving directly against
// ProjectsView so it can assert on addCursor and addCandidates, fields
// unexported from this package and therefore invisible to a test built from
// the root package.
func TestProjectsStaleTrackResultPreservesPickerCursorAndCandidates(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	candidates := []TrackCandidate{
		{Provider: "claude", Label: "claude  one  → /g/one", PathGuess: "/g/one", Roots: []TrackRoot{{LocalDir: "/x/one"}}},
		{Provider: "claude", Label: "claude  two  → /g/two", PathGuess: "/g/two", Roots: []TrackRoot{{LocalDir: "/x/two"}}},
	}
	actions := trackActionsFor(candidates, provider.Identity{}, nil)
	view := NewProjectsView()

	// Reach a real AddPicking state through key events, then move the cursor off
	// row 0 so "cursor untouched" is a meaningful claim.
	drive(t, &view, fake, actions, key("a"))
	drive(t, &view, fake, actions, key("j"))
	if view.Adding != AddPicking {
		t.Fatalf("setup: Adding = %v, want AddPicking", view.Adding)
	}
	if view.addCursor != 1 {
		t.Fatalf("setup: addCursor = %d, want 1", view.addCursor)
	}

	// The prior enrollment's result lands now. It is no longer AddTracking, so
	// it must not stomp the new picker — but it is still a real outcome.
	view.OnTrackResult(TrackResultMsg{Folders: []string{"agent-brain"}}, fake)

	if view.Adding != AddPicking {
		t.Fatalf("Adding = %v, want AddPicking (a stale result must not reset the new flow)", view.Adding)
	}
	if view.addCursor != 1 {
		t.Fatalf("addCursor = %d, want 1 (a stale result moved the picker cursor)", view.addCursor)
	}
	if diff := cmp.Diff(candidates, view.addCandidates); diff != "" {
		t.Fatalf("a stale result mutated the picker candidates (-want +got):\n%s", diff)
	}
}

// TestProjectsAddEscCancelsEachStage pins the updateAdd Cancel branch across
// all three interactive add stages: esc must reset Adding to AddNone and
// never reach the daemon, whether it fires from the picker, the path
// confirm, or the folder naming input.
func TestProjectsAddEscCancelsEachStage(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	candidates := []TrackCandidate{{
		Provider:  "claude",
		Label:     "claude  myrepo  → /g/myrepo",
		PathGuess: "/g/myrepo",
		Roots:     []TrackRoot{{LocalDir: "/x/memory"}},
	}}
	identity := provider.Identity{PreferredFolder: "myrepo"}
	actions := trackActionsFor(candidates, identity, nil)
	view := NewProjectsView()

	// Stage 1: cancel from the picker.
	drive(t, &view, fake, actions, key("a"))
	drive(t, &view, fake, actions, key("esc"))
	if view.Adding != AddNone {
		t.Fatal("esc in the picker did not reset the add flow")
	}

	// Stage 2: cancel from the path confirm.
	drive(t, &view, fake, actions, key("a"))
	drive(t, &view, fake, actions, key("space"))
	drive(t, &view, fake, actions, key("enter"))
	drive(t, &view, fake, actions, key("esc"))
	if view.Adding != AddNone {
		t.Fatal("esc in the path confirm did not reset the add flow")
	}

	// Stage 3: cancel from the folder naming input.
	drive(t, &view, fake, actions, key("a"))
	drive(t, &view, fake, actions, key("space"))
	drive(t, &view, fake, actions, key("enter"))
	drive(t, &view, fake, actions, key("enter"))
	drive(t, &view, fake, actions, key("esc"))
	if view.Adding != AddNone {
		t.Fatal("esc in the naming input did not reset the add flow")
	}
	if len(fake.trackCalls) != 0 {
		t.Fatalf("cancelled flows must never track: %v", fake.trackCalls)
	}
}

// TestAddViewHintsRenderFromModalBindings pins the add flow's inline hints to
// the same ForModal bindings the root's global footer renders (dashboard.go's
// footer()), so the two surfaces cannot hand-drift the way they already had:
// the inline hint used to read "↑/↓ move · enter select · esc cancel" while
// the footer read "↑/↓ select · enter confirm · esc cancel" for the identical
// AddPicking stage. The assertion is expressed through HelpLine + ForModal,
// not a re-hardcoded string, so a future wording change cannot split the
// surfaces again.
func TestAddViewHintsRenderFromModalBindings(t *testing.T) {
	t.Parallel()
	candidates := []TrackCandidate{{
		Provider:  "claude",
		Label:     "claude  myrepo  → /g/myrepo",
		PathGuess: "/g/myrepo",
		Roots:     []TrackRoot{{LocalDir: "/x/memory"}},
	}}
	identity := provider.Identity{PreferredFolder: "myrepo"} // empty ProjectID reaches AddNamingFolder

	tests := []struct {
		name  string
		stage AddStage
		keys  []string
	}{
		{name: "add picking", stage: AddPicking, keys: []string{"a"}},
		{name: "add confirm path", stage: AddConfirmPath, keys: []string{"a", "space", "enter"}},
		{name: "add naming folder", stage: AddNamingFolder, keys: []string{"a", "space", "enter", "enter"}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeData{}
			actions := trackActionsFor(candidates, identity, nil)
			view := NewProjectsView()
			for _, k := range testCase.keys {
				drive(t, &view, fake, actions, key(k))
			}
			if view.Adding != testCase.stage {
				t.Fatalf("setup: Adding = %v, want %v", view.Adding, testCase.stage)
			}

			want := HelpLine(DashboardKeys.ForModal(false, testCase.stage))
			if got := plain(view.View("")); !strings.Contains(got, want) {
				t.Errorf("addView = %q, want it to contain the shared hint %q", got, want)
			}
		})
	}
}

// TestProjectsAddMultiSelectEnrollsSelectedInOrder pins the spec §13 multi-
// select: space toggles rows into the set, enter confirms the SET, and the
// confirmed candidates enroll one at a time in CANDIDATE order (not selection
// order), each through the existing per-candidate confirm→identify→track
// stages, with the progress line naming the one in flight.
func TestProjectsAddMultiSelectEnrollsSelectedInOrder(t *testing.T) {
	t.Parallel()
	fake := &fakeData{trackResp: api.TrackResponse{Folder: "enrolled"}}
	candidates := []TrackCandidate{
		{Provider: "claude", Label: "zero", PathGuess: "/g/zero", Roots: []TrackRoot{{LocalDir: "/x/zero"}}},
		{Provider: "claude", Label: "one", PathGuess: "/g/one", Roots: []TrackRoot{{LocalDir: "/x/one"}}},
		{Provider: "claude", Label: "two", PathGuess: "/g/two", Roots: []TrackRoot{{LocalDir: "/x/two"}}},
	}
	identity := provider.Identity{ProjectID: "github.com/o/r", PreferredFolder: "r"}
	actions := trackActionsFor(candidates, identity, nil)
	view := NewProjectsView()

	// Select rows 0 and 2 (skip 1): space toggles, j moves the cursor.
	drive(t, &view, fake, actions, key("a"))
	drive(t, &view, fake, actions, key("space")) // row 0 in
	drive(t, &view, fake, actions, key("j"))
	drive(t, &view, fake, actions, key("j"))
	drive(t, &view, fake, actions, key("space")) // row 2 in
	drive(t, &view, fake, actions, key("enter")) // confirm the set of two → first candidate's path confirm

	// A two-deep queue shows which candidate is in flight, in candidate order.
	if got := plain(view.View("")); !strings.Contains(got, "enrolling 1 of 2: zero") {
		t.Fatalf("first-candidate view = %q, want the 'enrolling 1 of 2: zero' progress line", got)
	}
	drive(t, &view, fake, actions, key("enter")) // accept row 0's path → track → advance to row 2
	if got := plain(view.View("")); !strings.Contains(got, "enrolling 2 of 2: two") {
		t.Fatalf("second-candidate view = %q, want the 'enrolling 2 of 2: two' progress line", got)
	}
	drive(t, &view, fake, actions, key("enter")) // accept row 2's path → track → queue drains

	if len(fake.trackCalls) != 2 {
		t.Fatalf("trackCalls = %v, want exactly two (rows 0 and 2, never row 1)", fake.trackCalls)
	}
	gotDirs := []string{fake.trackCalls[0].LocalDir, fake.trackCalls[1].LocalDir}
	if diff := cmp.Diff([]string{"/x/zero", "/x/two"}, gotDirs); diff != "" {
		t.Errorf("enrolled out of order — the queue must follow candidate order (-want +got):\n%s", diff)
	}
	if view.Adding != AddNone {
		t.Errorf("Adding = %v, want AddNone once the queue drained", view.Adding)
	}
}

// TestProjectsAddEmptySelectionStaysInPicker pins the hub's deliberate
// divergence from the cli picker (spec §13, addendum): an enter with nothing
// toggled is NOT an enroll-nothing outcome — it keeps the picker open and
// surfaces the correction, never reaching the daemon.
func TestProjectsAddEmptySelectionStaysInPicker(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	candidates := []TrackCandidate{
		{Provider: "claude", Label: "zero", PathGuess: "/g/zero", Roots: []TrackRoot{{LocalDir: "/x/zero"}}},
	}
	actions := trackActionsFor(candidates, provider.Identity{ProjectID: "x", PreferredFolder: "x"}, nil)
	view := NewProjectsView()

	drive(t, &view, fake, actions, key("a"))     // → picker
	drive(t, &view, fake, actions, key("enter")) // enter with nothing selected

	if view.Adding != AddPicking {
		t.Fatalf("Adding = %v, want AddPicking (an empty-set enter must keep the picker open)", view.Adding)
	}
	if got := plain(view.View("")); !strings.Contains(got, "select at least one with space") {
		t.Fatalf("view = %q, want the empty-selection guidance", got)
	}
	if len(fake.trackCalls) != 0 {
		t.Fatalf("empty-set enter still tracked: %v", fake.trackCalls)
	}
}

// TestProjectsAddDiscoverErrorShowsNoticeAndResets pins OnDiscover's error
// branch (the add twin of the migrate discover-error pin): a failed root
// discovery resets the flow and surfaces the reason verbatim as a notice, never
// reaching the picker. Only the empty-discovery branch was pinned before.
func TestProjectsAddDiscoverErrorShowsNoticeAndResets(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	actions := TrackActions{
		Discover: func(context.Context) ([]TrackCandidate, error) {
			return nil, errors.New("providers unreachable")
		},
		Identify: func(context.Context, string, TrackRoot, string) (provider.Identity, error) {
			return provider.Identity{}, nil
		},
	}
	view := NewProjectsView()

	drive(t, &view, fake, actions, key("a")) // a → discover → error branch

	if view.Adding != AddNone {
		t.Fatalf("Adding = %v, want AddNone after a discover error", view.Adding)
	}
	if got := plain(view.View("")); !strings.Contains(got, "discover failed: providers unreachable") {
		t.Fatalf("view = %q, want the verbatim discover-failure notice", got)
	}
	if len(fake.trackCalls) != 0 {
		t.Fatalf("a discover error still tracked: %v", fake.trackCalls)
	}
}

// TestProjectsAddMultiSelectRendersCheckboxes pins the picker's checkbox
// grammar: every row starts "[ ]", a space-toggle flips exactly the highlighted
// row to "[x]", and its neighbours stay unchecked.
func TestProjectsAddMultiSelectRendersCheckboxes(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	candidates := []TrackCandidate{
		{Provider: "claude", Label: "alpha", PathGuess: "/g/alpha", Roots: []TrackRoot{{LocalDir: "/x/alpha"}}},
		{Provider: "claude", Label: "beta", PathGuess: "/g/beta", Roots: []TrackRoot{{LocalDir: "/x/beta"}}},
	}
	actions := trackActionsFor(candidates, provider.Identity{}, nil)
	view := NewProjectsView()

	drive(t, &view, fake, actions, key("a")) // → picker, both unchecked
	if got := plain(view.View("")); !strings.Contains(got, "[ ] alpha") || !strings.Contains(got, "[ ] beta") {
		t.Fatalf("fresh picker = %q, want both rows rendered '[ ]'", got)
	}
	drive(t, &view, fake, actions, key("space")) // toggle the highlighted row 0
	got := plain(view.View(""))
	if !strings.Contains(got, "[x] alpha") {
		t.Errorf("toggled row not checked; view = %q, want '[x] alpha'", got)
	}
	if !strings.Contains(got, "[ ] beta") {
		t.Errorf("neighbour wrongly checked; view = %q, want '[ ] beta'", got)
	}
}

// TestProjectsAddEscMidQueueNamesEnrolled pins the truthful partial outcome
// (same rule as OnTrackResult's partial wording): esc while a multi-select
// queue is mid-flight abandons the REST but names exactly what already enrolled,
// and never tracks the abandoned candidate.
func TestProjectsAddEscMidQueueNamesEnrolled(t *testing.T) {
	t.Parallel()
	fake := &fakeData{trackResp: api.TrackResponse{Folder: "first"}}
	candidates := []TrackCandidate{
		{Provider: "claude", Label: "zero", PathGuess: "/g/zero", Roots: []TrackRoot{{LocalDir: "/x/zero"}}},
		{Provider: "claude", Label: "one", PathGuess: "/g/one", Roots: []TrackRoot{{LocalDir: "/x/one"}}},
	}
	identity := provider.Identity{ProjectID: "github.com/o/r", PreferredFolder: "r"}
	actions := trackActionsFor(candidates, identity, nil)
	view := NewProjectsView()

	// Select both, enroll the first, then esc while the second waits at its path
	// confirm.
	drive(t, &view, fake, actions, key("a"))
	drive(t, &view, fake, actions, key("space")) // row 0 in
	drive(t, &view, fake, actions, key("j"))
	drive(t, &view, fake, actions, key("space")) // row 1 in
	drive(t, &view, fake, actions, key("enter")) // confirm set → row 0 path confirm
	drive(t, &view, fake, actions, key("enter")) // track row 0 → advance to row 1 path confirm

	if view.Adding != AddConfirmPath {
		t.Fatalf("Adding = %v, want AddConfirmPath (row 1 awaiting its path)", view.Adding)
	}
	if len(fake.trackCalls) != 1 {
		t.Fatalf("trackCalls = %v, want exactly one (row 0 only) before the abandon", fake.trackCalls)
	}

	drive(t, &view, fake, actions, key("esc")) // abandon the rest of the queue

	if view.Adding != AddNone {
		t.Fatalf("Adding = %v, want AddNone after esc", view.Adding)
	}
	if len(fake.trackCalls) != 1 {
		t.Fatalf("esc mid-queue enrolled more: %v", fake.trackCalls)
	}
	if got := plain(view.View("")); !strings.Contains(got, "enrolled first — cancelled the rest") {
		t.Fatalf("view = %q, want a truthful partial notice naming what already enrolled", got)
	}
}
