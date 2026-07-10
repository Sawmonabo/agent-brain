package dashboard

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
)

func TestActivityView(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	quiesceUntil := now.Add(5 * time.Minute)

	tests := []struct {
		name       string
		status     api.StatusResponse
		statusErr  error
		wantSubstr []string
		notSubstr  []string
	}{
		{
			name: "ready with last sync",
			status: api.StatusResponse{
				State: "ready", Version: "dev", PID: 4242,
				StartedAt: now.Add(-2 * time.Hour),
				LastSync: &api.SyncSummary{
					At: now.Add(-time.Hour), Commits: []string{"seed agent-brain"}, Pushed: true,
				},
			},
			wantSubstr: []string{"daemon: ready", "up 2h0m0s", "last sync:", "commit: seed agent-brain", "pushed: true"},
		},
		{
			name:       "never synced",
			status:     api.StatusResponse{State: "ready", Version: "dev", StartedAt: now.Add(-time.Minute)},
			wantSubstr: []string{"last sync: never"},
		},
		{
			name:       "quiesced shows remaining",
			status:     api.StatusResponse{State: "ready", Version: "dev", QuiescedUntil: &quiesceUntil},
			wantSubstr: []string{"quiesced until", "5m0s remaining"},
		},
		{
			name:       "state detail surfaced",
			status:     api.StatusResponse{State: "uninitialized", Version: "dev", StateDetail: "doctor: keyset: missing"},
			wantSubstr: []string{"daemon: uninitialized", "detail: doctor: keyset: missing"},
		},
		{
			name:       "read error",
			statusErr:  errors.New("decode failed"),
			wantSubstr: []string{"status unavailable", "decode failed"},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			body := plain(activityView{}.view(testCase.status, testCase.statusErr, nil, now))
			for _, want := range testCase.wantSubstr {
				if !strings.Contains(body, want) {
					t.Errorf("activity view missing %q; got:\n%s", want, body)
				}
			}
			for _, notWant := range testCase.notSubstr {
				if strings.Contains(body, notWant) {
					t.Errorf("activity view unexpectedly contains %q; got:\n%s", notWant, body)
				}
			}
		})
	}
}

func TestActivityDropsStaleQuiesce(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute)
	// A deadline already in the past must not render as an active hold.
	body := plain(activityView{}.view(api.StatusResponse{State: "ready", QuiescedUntil: &past}, nil, nil, now))
	if strings.Contains(body, "quiesced until") {
		t.Errorf("stale quiesce deadline rendered as active; got:\n%s", body)
	}
}

// TestActivityShowsFleetTriggerMax proves Activity reports the fleet trigger
// count (spec §7's "watch trigger counts") as the MAX of the per-unit
// WatchTriggers, not the sum: triggers are fleet-global (ADR 07), so the
// longest-watched root's count IS the raw trigger count while a sum would
// amplify it by fleet size. The 12/30 fixture discriminates the two (max 30, not
// sum 42). No trigger line is shown for an empty fleet.
func TestActivityShowsFleetTriggerMax(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	status := api.StatusResponse{State: "ready", Version: "dev", StartedAt: now.Add(-time.Hour)}
	units := []api.UnitInfo{
		{Provider: "claude", Folder: "a", LocalDir: "/l/a", WatchTriggers: 12},
		{Provider: "codex", Folder: "b", LocalDir: "/l/b", WatchTriggers: 30},
	}
	body := plain(activityView{}.view(status, nil, units, now))
	if !strings.Contains(body, "watch triggers: 30") {
		t.Errorf("activity view missing max fleet trigger count 30; got:\n%s", body)
	}
	if strings.Contains(body, "watch triggers: 42") {
		t.Errorf("activity view summed instead of maxing the fleet triggers; got:\n%s", body)
	}

	empty := plain(activityView{}.view(status, nil, nil, now))
	if strings.Contains(empty, "watch triggers") {
		t.Errorf("activity view showed a trigger line for an empty fleet; got:\n%s", empty)
	}
}
