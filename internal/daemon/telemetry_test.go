package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// telemetryDaemon wires a Daemon to a fresh registry file holding units, with
// the per-unit telemetry maps initialized — enough to drive the bookkeeping and
// the Projects() projection without standing up a real daemon (no socket, no
// checkout, no doctor battery).
func telemetryDaemon(t *testing.T, units ...repo.Unit) *Daemon {
	t.Helper()
	paths := config.Paths{ConfigDir: t.TempDir(), DataDir: t.TempDir()}
	registry := repo.NewLocalRegistry()
	for _, u := range units {
		if err := registry.Enroll(u); err != nil {
			t.Fatal(err)
		}
	}
	if err := registry.Save(paths.LocalRegistryFile()); err != nil {
		t.Fatal(err)
	}
	return &Daemon{
		cfg:           Config{Paths: paths},
		state:         "ready",
		degraded:      map[string]bool{},
		watchState:    map[string]string{},
		watchTriggers: map[string]uint64{},
		lastCycle:     map[string]*api.UnitCycleResult{},
	}
}

func unitsByFolder(units []api.UnitInfo) map[string]api.UnitInfo {
	byFolder := make(map[string]api.UnitInfo, len(units))
	for _, u := range units {
		byFolder[u.Folder] = u
	}
	return byFolder
}

// TestProjectsProjectsPerUnitTelemetry proves Projects() joins the daemon's
// per-root/per-folder telemetry onto each enrolled unit: watch state by LocalDir,
// trigger count by LocalDir, last-cycle by folder.
func TestProjectsProjectsPerUnitTelemetry(t *testing.T) {
	t.Parallel()
	unitA := repo.Unit{Provider: "claude", Folder: "alpha", LocalDir: "/l/alpha", ProjectID: "id-a"}
	unitB := repo.Unit{Provider: "claude", Folder: "beta", LocalDir: "/l/beta", ProjectID: "id-b"}
	d := telemetryDaemon(t, unitA, unitB)

	finished := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	d.setWatchStates(map[string]string{
		"/l/alpha": "watching",
		"/l/beta":  "failed: watch /l/beta: too many open files; ticker/poll backstop still covers it",
	})
	d.recordWatchTrigger("fs", []string{"/l/alpha", "/l/beta"})
	d.recordWatchTrigger("fs", []string{"/l/alpha"})
	d.recordOutcome(&api.SyncSummary{At: finished}, []repo.Unit{unitA})

	got := unitsByFolder(d.Projects().Units)

	alpha := got["alpha"]
	if alpha.WatchState != "watching" || alpha.WatchTriggers != 2 {
		t.Errorf("alpha watch telemetry = {%q, %d}, want {watching, 2}", alpha.WatchState, alpha.WatchTriggers)
	}
	if alpha.LastCycle == nil || alpha.LastCycle.Outcome != "ok" || !alpha.LastCycle.FinishedAt.Equal(finished) {
		t.Errorf("alpha last-cycle = %+v, want ok@%v", alpha.LastCycle, finished)
	}

	beta := got["beta"]
	if !strings.HasPrefix(beta.WatchState, "failed:") || !strings.Contains(beta.WatchState, "backstop") {
		t.Errorf("beta watch state = %q, want a failed:… conveying the backstop", beta.WatchState)
	}
	if beta.WatchTriggers != 1 {
		t.Errorf("beta watch triggers = %d, want 1", beta.WatchTriggers)
	}
	if beta.LastCycle != nil {
		t.Errorf("beta last-cycle = %+v, want nil (its folder never cycled)", beta.LastCycle)
	}
}

// TestRecordWatchTriggerCountsFilesystemNotPoll pins that WatchTriggers counts
// filesystem-driven triggers (fs, overflow) per watched root, but excludes the
// watch manager's timer backstop (poll) — the count is a watch-EVENT signal, not
// an uptime proxy.
func TestRecordWatchTriggerCountsFilesystemNotPoll(t *testing.T) {
	t.Parallel()
	d := &Daemon{watchTriggers: map[string]uint64{}}
	roots := []string{"/l/a", "/l/b"}
	d.recordWatchTrigger("fs", roots)
	d.recordWatchTrigger("overflow", roots)
	d.recordWatchTrigger("poll", roots) // backstop timer — must NOT count
	if d.watchTriggers["/l/a"] != 2 || d.watchTriggers["/l/b"] != 2 {
		t.Errorf("watchTriggers = %v, want each root 2 (fs+overflow; poll excluded)", d.watchTriggers)
	}
}

// TestRecordOutcomeTransitions pins the last-cycle bookkeeping: nil until a
// unit's folder cycles, then ok/degraded/error per outcome, and a filtered cycle
// updates only the folders it synced.
func TestRecordOutcomeTransitions(t *testing.T) {
	t.Parallel()
	unitA := repo.Unit{Provider: "claude", Folder: "alpha", LocalDir: "/l/alpha"}
	unitB := repo.Unit{Provider: "claude", Folder: "beta", LocalDir: "/l/beta"}
	d := telemetryDaemon(t, unitA, unitB)

	// Before any cycle: no unit has a last-cycle.
	for _, u := range d.Projects().Units {
		if u.LastCycle != nil {
			t.Fatalf("%s carried a last-cycle before any cycle ran: %+v", u.Folder, u.LastCycle)
		}
	}

	t0 := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	// Cycle 1 — whole-fleet, beta degraded.
	d.recordOutcome(&api.SyncSummary{At: t0, Degraded: []string{"beta"}}, []repo.Unit{unitA, unitB})
	got := unitsByFolder(d.Projects().Units)
	if c := got["alpha"].LastCycle; c == nil || c.Outcome != "ok" {
		t.Errorf("alpha after cycle 1 = %+v, want ok", got["alpha"].LastCycle)
	}
	if c := got["beta"].LastCycle; c == nil || c.Outcome != "degraded" {
		t.Errorf("beta after cycle 1 = %+v, want degraded", got["beta"].LastCycle)
	}

	t1 := t0.Add(time.Minute)
	// Cycle 2 — whole-fleet error: every synced unit records "error".
	d.recordOutcome(&api.SyncSummary{At: t1, Error: "integrate: fetch: boom"}, []repo.Unit{unitA, unitB})
	got = unitsByFolder(d.Projects().Units)
	for _, folder := range []string{"alpha", "beta"} {
		if c := got[folder].LastCycle; c == nil || c.Outcome != "error" || !c.FinishedAt.Equal(t1) {
			t.Errorf("%s after cycle 2 = %+v, want error@%v", folder, got[folder].LastCycle, t1)
		}
	}

	t2 := t1.Add(time.Minute)
	// Cycle 3 — filtered to alpha only: beta's history must be untouched.
	d.recordOutcome(&api.SyncSummary{At: t2}, []repo.Unit{unitA})
	got = unitsByFolder(d.Projects().Units)
	if c := got["alpha"].LastCycle; c.Outcome != "ok" || !c.FinishedAt.Equal(t2) {
		t.Errorf("alpha after filtered cycle 3 = %+v, want ok@%v", got["alpha"].LastCycle, t2)
	}
	if c := got["beta"].LastCycle; c.Outcome != "error" || !c.FinishedAt.Equal(t1) {
		t.Errorf("beta changed on a cycle that did not sync it = %+v, want error@%v (unchanged)", got["beta"].LastCycle, t1)
	}
}

// TestSetWatchStatesReplacesAndPrunes proves a rebuild replaces the watch-state
// snapshot wholesale and prunes trigger counts for roots no longer watched
// (untracked), while still-watched roots keep their counts across the rebuild.
func TestSetWatchStatesReplacesAndPrunes(t *testing.T) {
	t.Parallel()
	d := &Daemon{watchState: map[string]string{}, watchTriggers: map[string]uint64{}}
	d.setWatchStates(map[string]string{"/l/a": "watching", "/l/b": "watching"})
	d.recordWatchTrigger("fs", []string{"/l/a", "/l/b"})

	// /l/b is untracked; the next rebuild reports only /l/a.
	d.setWatchStates(map[string]string{"/l/a": "watching"})

	if _, ok := d.watchState["/l/b"]; ok {
		t.Error("stale watch state for a removed root survived the rebuild")
	}
	if _, ok := d.watchTriggers["/l/b"]; ok {
		t.Error("stale trigger count for a removed root survived the rebuild")
	}
	if d.watchTriggers["/l/a"] != 1 {
		t.Errorf("still-watched root lost its trigger count across the rebuild: %v", d.watchTriggers)
	}
}
