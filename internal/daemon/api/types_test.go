package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestUnitInfoTelemetryRoundTrips proves the Task-6.5 per-unit telemetry fields
// (WatchState, WatchTriggers, LastCycle) survive a JSON round-trip intact — the
// wire contract old and new clients both decode.
func TestUnitInfoTelemetryRoundTrips(t *testing.T) {
	t.Parallel()
	finished := time.Date(2026, 7, 10, 9, 30, 0, 0, time.UTC)
	want := ProjectsResponse{Units: []UnitInfo{
		{
			Provider:      "claude",
			Folder:        "alpha",
			LocalDir:      "/home/u/.claude/projects/alpha/memory",
			Degraded:      false,
			WatchState:    "watching",
			WatchTriggers: 7,
			LastCycle:     &UnitCycleResult{Outcome: "ok", FinishedAt: finished},
		},
		{
			Provider:      "codex",
			Folder:        "_global",
			LocalDir:      "/home/u/.codex/memories",
			Degraded:      true,
			WatchState:    "failed: watch /home/u/.codex/memories: too many open files; ticker/poll backstop still covers it",
			WatchTriggers: 0,
			LastCycle:     &UnitCycleResult{Outcome: "degraded", FinishedAt: finished},
		},
	}}

	blob, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got ProjectsResponse
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("UnitInfo telemetry round-trip (-want +got):\n%s", diff)
	}
}

// TestUnitInfoTelemetryOmitsEmpty pins the strictly-additive contract: a unit
// with no telemetry yet (zero WatchState/WatchTriggers, nil LastCycle) must
// serialize WITHOUT the new keys, so an old client's decode is byte-identical to
// before this task — the fields are inert until the daemon populates them.
func TestUnitInfoTelemetryOmitsEmpty(t *testing.T) {
	t.Parallel()
	blob, err := json.Marshal(UnitInfo{Provider: "claude", Folder: "alpha", LocalDir: "/l/a"})
	if err != nil {
		t.Fatal(err)
	}
	got := string(blob)
	for _, absent := range []string{"watch_state", "watch_triggers", "last_cycle"} {
		if strings.Contains(got, absent) {
			t.Errorf("empty UnitInfo serialized %q; additive fields must omitempty:\n%s", absent, got)
		}
	}
	// The pre-existing fields stay present (Degraded has no omitempty).
	for _, present := range []string{"provider", "folder", "local_dir", "degraded"} {
		if !strings.Contains(got, present) {
			t.Errorf("empty UnitInfo dropped pre-existing key %q:\n%s", present, got)
		}
	}
}

// TestUnitCycleResultOutcomes round-trips each cycle outcome the daemon emits.
func TestUnitCycleResultOutcomes(t *testing.T) {
	t.Parallel()
	finished := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	for _, outcome := range []string{"ok", "degraded", "error"} {
		want := UnitCycleResult{Outcome: outcome, FinishedAt: finished}
		blob, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		var got UnitCycleResult
		if err := json.Unmarshal(blob, &got); err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("UnitCycleResult %q round-trip (-want +got):\n%s", outcome, diff)
		}
	}
}
