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
			body := plain(activityView{}.view(testCase.status, testCase.statusErr, now))
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
	body := plain(activityView{}.view(api.StatusResponse{State: "ready", QuiescedUntil: &past}, nil, now))
	if strings.Contains(body, "quiesced until") {
		t.Errorf("stale quiesce deadline rendered as active; got:\n%s", body)
	}
}
