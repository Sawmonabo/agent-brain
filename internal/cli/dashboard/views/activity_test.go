package views

import (
	"errors"
	"fmt"
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
			body := plain(ActivityView{}.View(testCase.status, testCase.statusErr, nil, now, paneTestWidth, paneTestHeight))
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
	body := plain(ActivityView{}.View(api.StatusResponse{State: "ready", QuiescedUntil: &past}, nil, nil, now, paneTestWidth, paneTestHeight))
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
	body := plain(ActivityView{}.View(status, nil, units, now, paneTestWidth, paneTestHeight))
	if !strings.Contains(body, "watch triggers: 30") {
		t.Errorf("activity view missing max fleet trigger count 30; got:\n%s", body)
	}
	if strings.Contains(body, "watch triggers: 42") {
		t.Errorf("activity view summed instead of maxing the fleet triggers; got:\n%s", body)
	}

	empty := plain(ActivityView{}.View(status, nil, nil, now, paneTestWidth, paneTestHeight))
	if strings.Contains(empty, "watch triggers") {
		t.Errorf("activity view showed a trigger line for an empty fleet; got:\n%s", empty)
	}
}

func TestActivityShowsOfflineCycle(t *testing.T) {
	t.Parallel()
	status := api.StatusResponse{
		State: "ready", Version: "test", PID: 1,
		LastSync: &api.SyncSummary{Offline: true, PushQueued: true},
	}
	got := plain(ActivityView{}.View(status, nil, nil, time.Now(), paneTestWidth, paneTestHeight))
	if !strings.Contains(got, "offline: remote unreachable") {
		t.Fatalf("Activity view %q missing the offline line", got)
	}
}

// manyCommitStatus builds a status whose last-sync summary lists n
// individually-identifiable commit subjects, so the Activity snapshot overflows
// a short height and a scroll test can pin which rows sit in the window.
func manyCommitStatus(n int) api.StatusResponse {
	commits := make([]string, n)
	for i := range commits {
		commits[i] = fmt.Sprintf("commit-%03d", i)
	}
	return api.StatusResponse{
		State: "ready", Version: "dev", PID: 1,
		StartedAt: time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC),
		LastSync:  &api.SyncSummary{At: time.Date(2026, 7, 9, 11, 0, 0, 0, time.UTC), Commits: commits, Pushed: true},
	}
}

// TestActivityViewScrollsWhenOverflowing pins the Activity tab's bounded
// viewport at a height too small for a long sync summary: the body stays within
// the budget with the overflow hint on its bottom line, ctrl+d advances the
// window past the header, and G/g jump to the ends.
func TestActivityViewScrollsWhenOverflowing(t *testing.T) {
	t.Parallel()
	const height = 12
	now := time.Date(2026, 7, 9, 11, 0, 0, 0, time.UTC)
	status := manyCommitStatus(100)

	view := NewActivityView()
	view.OnData(status, nil, nil, now)

	top := plain(view.View(status, nil, nil, now, paneTestWidth, height))
	if got := lineCount(top); got > height {
		t.Errorf("tab body is %d lines, over the %d-line budget; got:\n%s", got, height, top)
	}
	if !strings.Contains(top, "pgup/pgdn scroll") {
		t.Errorf("overflowing snapshot missing the scroll hint; got:\n%s", top)
	}
	if !strings.Contains(top, "daemon: ready") || strings.Contains(top, "commit-050") {
		t.Errorf("top window wrong — want the daemon line present and a far commit absent; got:\n%s", top)
	}

	view.Scroll(ctrlKey('d'), status, nil, nil, now, paneTestWidth, height)
	advanced := plain(view.View(status, nil, nil, now, paneTestWidth, height))
	if strings.Contains(advanced, "daemon: ready") {
		t.Errorf("ctrl+d did not advance — the header line is still visible; got:\n%s", advanced)
	}

	view.Scroll(key("G"), status, nil, nil, now, paneTestWidth, height)
	bottom := plain(view.View(status, nil, nil, now, paneTestWidth, height))
	if !strings.Contains(bottom, "pushed: true") {
		t.Errorf("G did not jump to the bottom — the last summary line must show; got:\n%s", bottom)
	}

	view.Scroll(key("g"), status, nil, nil, now, paneTestWidth, height)
	if backToTop := plain(view.View(status, nil, nil, now, paneTestWidth, height)); !strings.Contains(backToTop, "daemon: ready") {
		t.Errorf("g did not jump back to the top; got:\n%s", backToTop)
	}
}

// TestActivityViewUptimeTickKeepsScroll pins the load-bearing subtlety of a tab
// whose body carries LIVE durations: the uptime advances every second, but that
// tick must not read as a new document and yank a scrolled reader back to the
// top — only a materially changed status does. It exercises the zero-time change
// key directly: re-rendering at a later clock (uptime ticked) holds the scroll,
// while a changed status resets it.
func TestActivityViewUptimeTickKeepsScroll(t *testing.T) {
	t.Parallel()
	const height = 12
	status := manyCommitStatus(100)
	early := time.Date(2026, 7, 9, 11, 0, 0, 0, time.UTC)
	later := early.Add(time.Hour) // uptime advances 1h → the daemon line's text changes

	view := NewActivityView()
	view.OnData(status, nil, nil, early)
	view.Scroll(ctrlKey('d'), status, nil, nil, early, paneTestWidth, height)
	if scrolled := plain(view.View(status, nil, nil, early, paneTestWidth, height)); strings.Contains(scrolled, "daemon: ready") {
		t.Fatalf("setup: ctrl+d should have scrolled the header off; got:\n%s", scrolled)
	}

	// A later poll of the SAME status (only the clock moved) must hold the scroll.
	view.OnData(status, nil, nil, later)
	if held := plain(view.View(status, nil, nil, later, paneTestWidth, height)); strings.Contains(held, "daemon: ready") {
		t.Errorf("the uptime tick yanked the scroll back to the top; got:\n%s", held)
	}

	// A materially changed status resets to the top.
	changed := manyCommitStatus(80)
	view.OnData(changed, nil, nil, later)
	if reset := plain(view.View(changed, nil, nil, later, paneTestWidth, height)); !strings.Contains(reset, "daemon: ready") {
		t.Errorf("a changed status did not reset the scroll to the top; got:\n%s", reset)
	}
}
