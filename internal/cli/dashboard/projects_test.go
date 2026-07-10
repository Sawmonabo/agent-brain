package dashboard

import (
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

	body := plain(model.View().Content)
	for _, want := range []string{"claude", "agent-brain", "codex", "_global", "watching", "degraded"} {
		if !strings.Contains(body, want) {
			t.Errorf("projects view missing %q; got:\n%s", want, body)
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

func TestWatchState(t *testing.T) {
	t.Parallel()
	future := time.Now().Add(time.Hour)
	tests := []struct {
		name   string
		status api.StatusResponse
		want   string
	}{
		{name: "ready", status: api.StatusResponse{State: "ready"}, want: "watching"},
		{name: "held when quiesced", status: api.StatusResponse{State: "ready", QuiescedUntil: &future}, want: "held"},
		{name: "uninitialized verbatim", status: api.StatusResponse{State: "uninitialized"}, want: "uninitialized"},
		{name: "empty state dash", status: api.StatusResponse{}, want: "—"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := watchState(testCase.status); got != testCase.want {
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
