package dashboard

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/provider"
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

	// The table carries per-unit columns, now including the genuine per-unit
	// watch state and last-cycle the API serves (Task 6.5): claude/agent-brain is
	// watching + ok, codex/_global is a failed watch + degraded.
	table := plain(model.projects.view())
	for _, want := range []string{
		"PROVIDER", "FOLDER", "HEALTH", "WATCH", "LAST CYCLE",
		"claude", "agent-brain", "codex", "_global", "degraded",
		"watching", "failed", "ok",
	} {
		if !strings.Contains(table, want) {
			t.Errorf("projects table missing %q; got:\n%s", want, table)
		}
	}

	// The persistent header still carries the fleet-level posture (it summarizes
	// daemon state, not any one unit) — the rows do not replace it.
	header := plain(model.statusHeader())
	for _, want := range []string{"daemon", "watching", "last cycle"} {
		if !strings.Contains(header, want) {
			t.Errorf("status header missing %q; got:\n%s", want, header)
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
	model := loadedProjects(data)
	table := plain(model.projects.view())
	for _, want := range []string{"error", "—"} {
		if !strings.Contains(table, want) {
			t.Errorf("projects table missing %q; got:\n%s", want, table)
		}
	}
}

func TestProjectsWideTableShowsLocalDir(t *testing.T) {
	t.Parallel()
	data := &fakeData{status: readyStatus(), projects: twoUnits()}
	model := loadedProjects(data) // shared 110-col size: the five essentials only

	// LOCAL DIR is the optional roomy-terminal column; it stays hidden until the
	// terminal is wide enough to carry the full path beside the essentials.
	narrow := plain(model.projects.view())
	if strings.Contains(narrow, "LOCAL DIR") {
		t.Errorf("LOCAL DIR shown at the narrow 110-col size; got:\n%s", narrow)
	}

	model, _ = step(model, tea.WindowSizeMsg{Width: 130, Height: 40})
	wide := plain(model.projects.view())
	for _, want := range []string{"LOCAL DIR", "/home/u/.claude/projects/agent-brain/memory"} {
		if !strings.Contains(wide, want) {
			t.Errorf("wide projects table missing %q; got:\n%s", want, wide)
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

// addConfig builds a Config whose discovery/identity closures return the
// given candidates/identity without any real provider composition.
func addConfig(fake *fakeData, candidates []TrackCandidate, identity provider.Identity, identifyErr error) Config {
	return Config{
		Data: fake,
		Discover: func(context.Context) ([]TrackCandidate, error) {
			return candidates, nil
		},
		Identify: func(_ context.Context, _ string, _ TrackRoot, _ string) (provider.Identity, error) {
			return identity, identifyErr
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

func TestProjectsAddDiscoverEmpty(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	m := New(addConfig(fake, nil, provider.Identity{}, nil))
	m.active = tabProjects

	m = drive(t, m, key("a"))
	if got := plain(m.projects.view()); !strings.Contains(got, "no new memory roots") {
		t.Fatalf("empty discovery view = %q, want a 'no new memory roots' notice", got)
	}
	if len(fake.trackCalls) != 0 {
		t.Fatalf("empty discovery must not track: %v", fake.trackCalls)
	}
}

func TestProjectsAddGlobalTracksAllRootsAndSyncs(t *testing.T) {
	t.Parallel()
	fake := &fakeData{trackResp: api.TrackResponse{Folder: "_global"}}
	candidates := []TrackCandidate{{
		Provider: "codex",
		Label:    "codex  global memories",
		Global:   true,
		Roots: []TrackRoot{
			{LocalDir: "/home/u/.codex/memories"},
			{LocalDir: "/home/u/.codex/notes", RepoSubdir: "notes"},
		},
	}}
	m := New(addConfig(fake, candidates, provider.Identity{}, nil))
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
	m := New(addConfig(fake, candidates, identity, nil))
	m.active = tabProjects

	m = drive(t, m, key("a"))     // discover → picker
	m = drive(t, m, key("enter")) // pick → path-confirm input, prefilled with PathGuess
	if got := plain(m.projects.view()); !strings.Contains(got, "/g/myrepo") {
		t.Fatalf("path-confirm view = %q, want the PathGuess prefill visible", got)
	}
	m = drive(t, m, key("enter")) // accept path → identify → track → fleet sync

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
	m := New(addConfig(fake, candidates, identity, nil))
	m.active = tabProjects

	m = drive(t, m, key("a"))
	m = drive(t, m, key("enter")) // pick
	m = drive(t, m, key("enter")) // accept path → identify → remoteless → naming input

	// An invalid name must be refused locally (repo.ValidateFolderName),
	// before any wire call.
	m.projects.addInput.SetValue("bad/name")
	m = drive(t, m, key("enter"))
	if len(fake.trackCalls) != 0 {
		t.Fatalf("invalid folder name reached the daemon: %v", fake.trackCalls)
	}

	m.projects.addInput.SetValue("scratch")
	m = drive(t, m, key("enter"))
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
	m := New(addConfig(fake, candidates, provider.Identity{}, errors.New("read git remote: permission denied")))
	m.active = tabProjects

	m = drive(t, m, key("a"))     // discover → picker
	m = drive(t, m, key("enter")) // pick → path confirm
	m = drive(t, m, key("enter")) // accept path → identify (fails)

	if m.projects.adding != addNone {
		t.Fatalf("adding = %v, want addNone after an identify failure", m.projects.adding)
	}
	if len(fake.trackCalls) != 0 {
		t.Fatalf("identify failure still reached the daemon: %v", fake.trackCalls)
	}
	if got := plain(m.projects.view()); !strings.Contains(got, "identify failed") {
		t.Fatalf("view = %q, want an 'identify failed' notice", got)
	}
}

// TestProjectsAddDiscoverFailureAborts covers the discovery error branch: when
// the injected discover closure fails (e.g. a provider scan cannot read a
// memory root), pressing a resets the flow to no add, surfaces the reason, and
// never reaches the daemon. addConfig hardwires a nil discover error, so this
// one builds the Config inline to inject the failure — and wires Identify too,
// since add availability now gates on BOTH closures (a build with only discover
// keeps the a key dead, covered by TestFooterAndDispatchGateAddOnBothClosures).
func TestProjectsAddDiscoverFailureAborts(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	m := New(Config{
		Data: fake,
		Discover: func(context.Context) ([]TrackCandidate, error) {
			return nil, errors.New("scan providers: permission denied")
		},
		Identify: func(context.Context, string, TrackRoot, string) (provider.Identity, error) {
			return provider.Identity{}, nil
		},
	})
	m.active = tabProjects

	m = drive(t, m, key("a")) // discover fails

	if m.projects.adding != addNone {
		t.Fatalf("adding = %v, want addNone after a discover failure", m.projects.adding)
	}
	if got := plain(m.projects.view()); !strings.Contains(got, "discover failed") ||
		!strings.Contains(got, "permission denied") {
		t.Fatalf("view = %q, want a 'discover failed' notice carrying the reason", got)
	}
	if len(fake.trackCalls) != 0 || len(fake.syncCalls) != 0 {
		t.Fatalf("discover failure still reached the daemon: track=%v sync=%v", fake.trackCalls, fake.syncCalls)
	}
}

// TestProjectsAddTrackFailureSurfacesAndSkipsSync covers the track error
// branch: when the daemon rejects the enrollment, the flow surfaces the reason
// and fires NO fleet sync — there is nothing new to mirror in, so syncing would
// be a lie. The recorded calls prove exactly one track attempt and zero syncs.
func TestProjectsAddTrackFailureSurfacesAndSkipsSync(t *testing.T) {
	t.Parallel()
	fake := &fakeData{trackErr: errors.New("daemon refused: quiesced")}
	candidates := []TrackCandidate{{
		Provider: "codex",
		Label:    "codex  global memories",
		Global:   true,
		Roots:    []TrackRoot{{LocalDir: "/home/u/.codex/memories"}},
	}}
	m := New(addConfig(fake, candidates, provider.Identity{}, nil))
	m.active = tabProjects

	m = drive(t, m, key("a"))     // discover → picker (one global row)
	m = drive(t, m, key("enter")) // global: track directly (fails)

	if len(fake.trackCalls) != 1 {
		t.Fatalf("trackCalls = %d, want 1 (the attempted enrollment)", len(fake.trackCalls))
	}
	if len(fake.syncCalls) != 0 {
		t.Fatalf("a failed track must not fire a fleet sync: %v", fake.syncCalls)
	}
	if got := plain(m.projects.view()); !strings.Contains(got, "track failed") ||
		!strings.Contains(got, "quiesced") {
		t.Fatalf("view = %q, want a 'track failed' notice carrying the reason", got)
	}
}

// TestProjectsStaleTrackResultKeepsNewFlow pins the onTrackResult reset guard:
// a prior enrollment's in-flight trackResultMsg can land after the user esc'd
// and reopened the add flow. drive is synchronous, so that interleaving is
// constructed directly — reach a real addPicking state through key events, then
// deliver the stale result through the model's Update. The reset must be
// skipped (the new picker survives untouched) while the notice and fleet sync
// still fire, because the stale enrollment genuinely happened.
func TestProjectsStaleTrackResultKeepsNewFlow(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	candidates := []TrackCandidate{
		{Provider: "claude", Label: "claude  one  → /g/one", PathGuess: "/g/one", Roots: []TrackRoot{{LocalDir: "/x/one"}}},
		{Provider: "claude", Label: "claude  two  → /g/two", PathGuess: "/g/two", Roots: []TrackRoot{{LocalDir: "/x/two"}}},
	}
	m := New(addConfig(fake, candidates, provider.Identity{}, nil))
	m.active = tabProjects

	// Reach a real addPicking state through key events, then move the cursor off
	// row 0 so "cursor untouched" is a meaningful claim.
	m = drive(t, m, key("a"))
	m = drive(t, m, key("j"))
	if m.projects.adding != addPicking {
		t.Fatalf("setup: adding = %v, want addPicking", m.projects.adding)
	}
	if m.projects.addCursor != 1 {
		t.Fatalf("setup: addCursor = %d, want 1", m.projects.addCursor)
	}

	// The prior enrollment's result lands now. It is no longer addTracking, so
	// it must not stomp the new picker — but it is still a real outcome.
	next, cmd := step(m, trackResultMsg{folders: []string{"agent-brain"}})

	if next.projects.adding != addPicking {
		t.Fatalf("adding = %v, want addPicking (a stale result must not reset the new flow)", next.projects.adding)
	}
	if next.projects.addCursor != 1 {
		t.Fatalf("addCursor = %d, want 1 (a stale result moved the picker cursor)", next.projects.addCursor)
	}
	if diff := cmp.Diff(candidates, next.projects.addCandidates); diff != "" {
		t.Fatalf("a stale result mutated the picker candidates (-want +got):\n%s", diff)
	}
	if got := plain(next.projects.view()); !strings.Contains(got, "tracked agent-brain") {
		t.Fatalf("view = %q, want the stale enrollment's notice still set", got)
	}
	drain(cmd)
	if diff := cmp.Diff([]string{""}, fake.syncCalls); diff != "" {
		t.Fatalf("a stale success did not fire the whole-fleet sync (-want +got):\n%s", diff)
	}
}

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

	// Stage 1: cancel from the picker.
	m := New(addConfig(fake, candidates, identity, nil))
	m.active = tabProjects
	m = drive(t, m, key("a"))
	m = drive(t, m, key("esc"))
	if m.projects.adding != addNone {
		t.Fatal("esc in the picker did not reset the add flow")
	}

	// Stage 2: cancel from the path confirm.
	m = drive(t, m, key("a"))
	m = drive(t, m, key("enter"))
	m = drive(t, m, key("esc"))
	if m.projects.adding != addNone {
		t.Fatal("esc in the path confirm did not reset the add flow")
	}

	// Stage 3: cancel from the folder naming input.
	m = drive(t, m, key("a"))
	m = drive(t, m, key("enter"))
	m = drive(t, m, key("enter"))
	m = drive(t, m, key("esc"))
	if m.projects.adding != addNone {
		t.Fatal("esc in the naming input did not reset the add flow")
	}
	if len(fake.trackCalls) != 0 {
		t.Fatalf("cancelled flows must never track: %v", fake.trackCalls)
	}
}

func TestFooterAdvertisesAddOnlyWhenWired(t *testing.T) {
	t.Parallel()
	wired := New(addConfig(&fakeData{}, nil, provider.Identity{}, nil))
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
		Discover: func(context.Context) ([]TrackCandidate, error) {
			return nil, nil
		},
		// Identify deliberately unwired: discovery alone must not enable add.
	})
	m.active = tabProjects

	if got := plain(m.footer()); strings.Contains(got, "a add") {
		t.Errorf("footer %q advertises add with identify unwired", got)
	}

	m = drive(t, m, key("a"))
	if m.projects.adding != addNone {
		t.Errorf("adding = %v, want addNone: a must be dead with identify unwired", m.projects.adding)
	}
	if got := plain(m.projects.view()); !strings.Contains(got, "add is unavailable") {
		t.Errorf("view = %q, want the 'add is unavailable' notice", got)
	}
	if len(fake.trackCalls) != 0 {
		t.Errorf("a with identify unwired still reached the daemon: %v", fake.trackCalls)
	}
}

// TestAddViewHintsRenderFromModalBindings pins the add flow's inline hints to
// the same forModal bindings the global footer renders (dashboard.go's
// footer()), so the two surfaces cannot hand-drift the way they already had:
// the inline hint used to read "↑/↓ move · enter select · esc cancel" while
// the footer read "↑/↓ select · enter confirm · esc cancel" for the identical
// addPicking stage. The assertion is expressed through helpLine + forModal,
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
	identity := provider.Identity{PreferredFolder: "myrepo"} // empty ProjectID reaches addNamingFolder

	tests := []struct {
		name  string
		stage addStage
		keys  []string
	}{
		{name: "add picking", stage: addPicking, keys: []string{"a"}},
		{name: "add confirm path", stage: addConfirmPath, keys: []string{"a", "enter"}},
		{name: "add naming folder", stage: addNamingFolder, keys: []string{"a", "enter", "enter"}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			m := New(addConfig(&fakeData{}, candidates, identity, nil))
			m.active = tabProjects
			for _, k := range testCase.keys {
				m = drive(t, m, key(k))
			}
			if m.projects.adding != testCase.stage {
				t.Fatalf("setup: adding = %v, want %v", m.projects.adding, testCase.stage)
			}

			want := helpLine(dashboardKeys.forModal(false, testCase.stage))
			if got := plain(m.projects.view()); !strings.Contains(got, want) {
				t.Errorf("addView = %q, want it to contain the shared hint %q", got, want)
			}
		})
	}
}
