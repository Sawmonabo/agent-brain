package views

import (
	"errors"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/config"
)

// threeConflicts is a shared fixture: three records with distinct times so a
// marker assertion can name exactly which row the cursor sits on.
func threeConflicts() []config.ConflictRecord {
	return []config.ConflictRecord{
		{Time: "2026-07-09T11:00:00Z", Path: "acme/claude/first.md", Mode: "fact"},
		{Time: "2026-07-09T12:00:00Z", Path: "acme/claude/second.md", Mode: "fact"},
		{Time: "2026-07-09T13:00:00Z", Path: "acme/claude/third.md", Mode: "fact"},
	}
}

// markedRow reports which record time carries the "> " cursor marker in the
// plain-rendered view, or "" if none does.
func markedRow(t *testing.T, view ConflictsView, records []config.ConflictRecord) string {
	t.Helper()
	body := plain(view.View())
	marked := ""
	for _, record := range records {
		switch {
		case strings.Contains(body, "> "+record.Time):
			if marked != "" {
				t.Fatalf("two rows marked selected (%q and %q); got:\n%s", marked, record.Time, body)
			}
			marked = record.Time
		case !strings.Contains(body, "  "+record.Time):
			t.Fatalf("row %q rendered with neither the selected nor the unselected marker; got:\n%s", record.Time, body)
		}
	}
	return marked
}

// TestConflictsViewCursorMovesAndMarksSelection pins the flat list's new
// selection state: j/k (and ↓/↑) move the cursor within the list, the plain
// render marks exactly the selected row, and both ends clamp rather than wrap.
func TestConflictsViewCursorMovesAndMarksSelection(t *testing.T) {
	t.Parallel()
	records := threeConflicts()
	var view ConflictsView
	view.Set(records, nil)

	if got := markedRow(t, view, records); got != records[0].Time {
		t.Fatalf("initial selection = %q, want the first row %q", got, records[0].Time)
	}

	view.Update(key("j"))
	if got := markedRow(t, view, records); got != records[1].Time {
		t.Fatalf("after j, selection = %q, want %q", got, records[1].Time)
	}

	view.Update(key("down"))
	if got := markedRow(t, view, records); got != records[2].Time {
		t.Fatalf("after ↓, selection = %q, want %q", got, records[2].Time)
	}

	// j at the last row clamps (no wrap to the top).
	view.Update(key("j"))
	if got := markedRow(t, view, records); got != records[2].Time {
		t.Fatalf("j past the last row moved off it to %q, want it pinned at %q", got, records[2].Time)
	}

	view.Update(key("k"))
	view.Update(key("up"))
	if got := markedRow(t, view, records); got != records[0].Time {
		t.Fatalf("after k then ↑, selection = %q, want the first row %q", got, records[0].Time)
	}

	// k at the top clamps (no wrap to the bottom).
	view.Update(key("k"))
	if got := markedRow(t, view, records); got != records[0].Time {
		t.Fatalf("k above the first row moved off it to %q, want it pinned at %q", got, records[0].Time)
	}
}

// TestConflictsViewEnterEmitsSelectedRecord pins enter → OpenConflictMsg
// carrying the record the cursor sits on (the exact struct the root resolves
// into a detail screen), not the first record or a stale one.
func TestConflictsViewEnterEmitsSelectedRecord(t *testing.T) {
	t.Parallel()
	records := threeConflicts()
	var view ConflictsView
	view.Set(records, nil)
	view.Update(key("j")) // select the second record

	cmd := view.Update(key("enter"))
	msgs := drain(cmd)
	if len(msgs) != 1 {
		t.Fatalf("enter produced %d messages, want exactly 1 (OpenConflictMsg)", len(msgs))
	}
	open, ok := msgs[0].(OpenConflictMsg)
	if !ok {
		t.Fatalf("enter emitted %T, want OpenConflictMsg", msgs[0])
	}
	if open.Record != records[1] {
		t.Errorf("OpenConflictMsg.Record = %+v, want the selected %+v", open.Record, records[1])
	}
}

// TestConflictsViewEnterEmptyIsInert pins that enter on an empty log emits
// nothing — there is no record to open, so no detail push is requested.
func TestConflictsViewEnterEmptyIsInert(t *testing.T) {
	t.Parallel()
	var view ConflictsView
	view.Set(nil, nil)
	if cmd := view.Update(key("enter")); cmd != nil {
		t.Fatalf("enter on an empty log produced a Cmd %#v, want nil", drain(cmd))
	}
}

// TestConflictsViewSetReclampsCursor pins that a refreshed snapshot which
// shrank underneath the cursor re-seats it on the new last row rather than
// leaving it dangling past the end — the poll rebuilds this list every ~2s.
func TestConflictsViewSetReclampsCursor(t *testing.T) {
	t.Parallel()
	records := threeConflicts()
	var view ConflictsView
	view.Set(records, nil)
	view.Update(key("j"))
	view.Update(key("j")) // cursor on the third (last) row

	shrunk := records[:1]
	view.Set(shrunk, nil)
	if got := markedRow(t, view, shrunk); got != shrunk[0].Time {
		t.Fatalf("after the log shrank to one row, selection = %q, want it re-clamped to %q", got, shrunk[0].Time)
	}
}

func TestConflictsView(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		records    []config.ConflictRecord
		err        error
		loaded     bool
		wantSubstr []string
	}{
		{
			name:       "not loaded shows loading",
			loaded:     false,
			wantSubstr: []string{"Conflicts", "loading"},
		},
		{
			name:       "loaded empty shows reassurance",
			loaded:     true,
			wantSubstr: []string{"no retained conflicts"},
		},
		{
			name:   "records render time path mode",
			loaded: true,
			records: []config.ConflictRecord{
				{Time: "2026-07-09T11:00:00Z", Path: "MEMORY.md", Mode: "retain-both"},
			},
			wantSubstr: []string{"TIME", "PATH", "MODE", "2026-07-09T11:00:00Z", "MEMORY.md", "retain-both"},
		},
		{
			name:       "error surfaces plainly",
			loaded:     true,
			err:        errors.New("permission denied"),
			wantSubstr: []string{"conflict log unavailable", "permission denied"},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var view ConflictsView
			if testCase.loaded {
				view.Set(testCase.records, testCase.err)
			}
			body := plain(view.View())
			for _, want := range testCase.wantSubstr {
				if !strings.Contains(body, want) {
					t.Errorf("conflicts view missing %q; got:\n%s", want, body)
				}
			}
		})
	}
}
