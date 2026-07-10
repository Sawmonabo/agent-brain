package dashboard

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
)

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
	return api.ProjectsResponse{Units: []api.UnitInfo{
		{Provider: "claude", Folder: "agent-brain", LocalDir: "/home/u/.claude/projects/agent-brain/memory", Degraded: false},
		{Provider: "codex", Folder: "_global", LocalDir: "/home/u/.codex/memories", Degraded: true},
	}}
}

// loadedProjects builds a root model sitting on the Projects tab with a sized
// window and the given status + units already delivered.
func loadedProjects(data *fakeData) Model {
	model := newTestModel(data)
	model, _ = step(model, tea.WindowSizeMsg{Width: 110, Height: 40})
	model, _ = step(model, statusMsg{resp: data.status})
	model, _ = step(model, projectsMsg{resp: data.projects})
	return model
}

func TestProjectsTableRenders(t *testing.T) {
	t.Parallel()
	data := &fakeData{status: readyStatus(), projects: twoUnits()}
	model := loadedProjects(data)

	// The table carries only genuinely per-unit columns.
	table := plain(model.projects.view())
	for _, want := range []string{"PROVIDER", "FOLDER", "HEALTH", "claude", "agent-brain", "codex", "_global", "degraded"} {
		if !strings.Contains(table, want) {
			t.Errorf("projects table missing %q; got:\n%s", want, table)
		}
	}
	// Fleet-wide posture must NOT be fabricated as a per-row column.
	if strings.Contains(table, "watching") {
		t.Errorf("projects table fabricated fleet watch-state as a column; got:\n%s", table)
	}

	// It lives once in the persistent header instead.
	header := plain(model.statusHeader())
	for _, want := range []string{"daemon", "watching", "last cycle"} {
		if !strings.Contains(header, want) {
			t.Errorf("status header missing %q; got:\n%s", want, header)
		}
	}
}

func TestProjectsWideTableShowsLocalDir(t *testing.T) {
	t.Parallel()
	data := &fakeData{status: readyStatus(), projects: twoUnits()}
	model := loadedProjects(data) // sized 110 wide → roomy

	table := plain(model.projects.view())
	for _, want := range []string{"LOCAL DIR", "/home/u/.claude/projects/agent-brain/memory"} {
		if !strings.Contains(table, want) {
			t.Errorf("wide projects table missing %q; got:\n%s", want, table)
		}
	}
}

func TestProjectsEmptyState(t *testing.T) {
	t.Parallel()
	data := &fakeData{status: readyStatus(), projects: api.ProjectsResponse{}}
	model := loadedProjects(data)
	body := plain(model.View().Content)
	if !strings.Contains(body, "no projects enrolled") {
		t.Errorf("empty projects view missing guidance; got:\n%s", body)
	}
}

func TestProjectsSyncKey(t *testing.T) {
	t.Parallel()
	data := &fakeData{status: readyStatus(), projects: twoUnits(), syncResp: api.SyncResponse{Status: "completed"}}
	model := loadedProjects(data)

	model, cmd := step(model, key("s"))
	msgs := drain(cmd)

	if diff := cmp.Diff([]string{"agent-brain"}, data.syncCalls); diff != "" {
		t.Errorf("Sync calls mismatch (-want +got):\n%s", diff)
	}
	if !containsMsg[syncResultMsg](msgs) {
		t.Error("s did not produce a syncResultMsg")
	}
	// Feeding the result back surfaces a notice on the view.
	for _, m := range msgs {
		if result, ok := m.(syncResultMsg); ok {
			model, _ = step(model, result)
		}
	}
	if !strings.Contains(plain(model.View().Content), "synced agent-brain") {
		t.Errorf("sync notice not shown; got:\n%s", plain(model.View().Content))
	}
}

func TestProjectsUntrackToggleConfirmsThenCalls(t *testing.T) {
	t.Parallel()
	data := &fakeData{status: readyStatus(), projects: twoUnits(), untrackResp: api.UntrackResponse{Removed: true}}
	model := loadedProjects(data)

	// t opens the confirm; no call yet.
	model, cmd := step(model, key("t"))
	if cmd != nil {
		t.Error("t should not act before confirmation")
	}
	if !strings.Contains(plain(model.View().Content), "untrack agent-brain? (y/N)") {
		t.Errorf("confirm prompt not shown; got:\n%s", plain(model.View().Content))
	}
	if len(data.untrackCalls) != 0 {
		t.Fatalf("Untrack called before confirmation: %+v", data.untrackCalls)
	}

	// y confirms and fires exactly the enrolled unit, never a purge.
	_, cmd = step(model, key("y"))
	msgs := drain(cmd)
	want := []api.UntrackRequest{{Provider: "claude", LocalDir: "/home/u/.claude/projects/agent-brain/memory", Purge: false}}
	if diff := cmp.Diff(want, data.untrackCalls); diff != "" {
		t.Errorf("Untrack request mismatch (-want +got):\n%s", diff)
	}
	if !containsMsg[untrackResultMsg](msgs) {
		t.Error("y did not produce an untrackResultMsg")
	}
}

func TestProjectsUntrackToggleCancels(t *testing.T) {
	t.Parallel()
	data := &fakeData{status: readyStatus(), projects: twoUnits()}
	model := loadedProjects(data)

	model, _ = step(model, key("t"))
	model, cmd := step(model, key("n"))
	if cmd != nil {
		t.Error("cancelling the confirm should issue no Cmd")
	}
	if len(data.untrackCalls) != 0 {
		t.Errorf("Untrack called despite cancel: %+v", data.untrackCalls)
	}
	if model.projects.confirming {
		t.Error("confirm state not cleared after cancel")
	}
	if !strings.Contains(plain(model.View().Content), "untrack cancelled") {
		t.Errorf("cancel notice not shown; got:\n%s", plain(model.View().Content))
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
	model := loadedProjects(data) // cursor seats on row 0 = claude/agent-brain (unit X)

	// Open the confirm on the highlighted unit X.
	model, _ = step(model, key("t"))
	if !strings.Contains(plain(model.View().Content), "untrack agent-brain? (y/N)") {
		t.Fatalf("confirm did not open on agent-brain; got:\n%s", plain(model.View().Content))
	}

	// A poll lands mid-confirm and reorders the fleet, so cursor index 0 now
	// points at a DIFFERENT unit Y (codex/_global).
	reordered := api.ProjectsResponse{Units: []api.UnitInfo{
		twoUnits().Units[1], // codex/_global now at index 0
		twoUnits().Units[0], // claude/agent-brain now at index 1
	}}
	model, _ = step(model, projectsMsg{resp: reordered})

	// y must untrack X (the named unit), never Y (the unit under the moved cursor).
	_, cmd := step(model, key("y"))
	drain(cmd)

	wantX := []api.UntrackRequest{{Provider: "claude", LocalDir: "/home/u/.claude/projects/agent-brain/memory", Purge: false}}
	if diff := cmp.Diff(wantX, data.untrackCalls); diff != "" {
		t.Errorf("untrack hit the wrong unit after a mid-confirm fleet reorder (-want +got):\n%s", diff)
	}
}

// TestProjectsLoadError renders the Projects error path, matching the error
// coverage the Conflicts, Activity, and Doctor views already carry (N-2).
func TestProjectsLoadError(t *testing.T) {
	t.Parallel()
	view := newProjectsView()
	view.setLoadErr(errors.New("dial unix: connection refused"))

	body := plain(view.view())
	for _, want := range []string{"projects unavailable", "connection refused"} {
		if !strings.Contains(body, want) {
			t.Errorf("projects load-error view missing %q; got:\n%s", want, body)
		}
	}
}

// TestProjectsLoadingBeforeFirstLoad pins N-1: until the first projectsMsg
// arrives the view shows a neutral loading line, so the genuinely-empty
// "no projects enrolled" guidance cannot flash on open before data loads.
func TestProjectsLoadingBeforeFirstLoad(t *testing.T) {
	t.Parallel()
	model := newTestModel(&fakeData{})
	model, _ = step(model, tea.WindowSizeMsg{Width: 110, Height: 40})

	body := plain(model.projects.view())
	if strings.Contains(body, "no projects enrolled") {
		t.Errorf("empty-state guidance flashed before the first load; got:\n%s", body)
	}
	if !strings.Contains(body, "loading") {
		t.Errorf("pre-load view missing a loading indicator; got:\n%s", body)
	}
}
