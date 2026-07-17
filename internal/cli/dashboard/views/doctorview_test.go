package views

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/doctor"
)

// paneTestWidth/paneTestHeight are the render dimensions the Doctor and Activity
// content tests pass their View: wide and tall enough that the bounded viewport
// never truncates a line or clips a row, so the substring/row-count assertions
// see the full body. The scroll tests below deliberately pass a SMALL height to
// force overflow instead.
const (
	paneTestWidth  = 300
	paneTestHeight = 4000
)

func TestDoctorViewGlyphsAndSummary(t *testing.T) {
	t.Parallel()
	report := doctor.Report{Results: []doctor.CheckResult{
		{Name: "settings", Status: doctor.StatusOK, Detail: "config.toml parsed"},
		{Name: "gh", Status: doctor.StatusWarn, Detail: "gh not authenticated"},
		{Name: "keyset", Status: doctor.StatusFail, Detail: "keyset.json not found", Fix: "run `agent-brain init`"},
		{Name: "keyset-decrypt", Status: doctor.StatusInfo, Detail: "no encrypted content in the checkout yet — nothing to probe"},
	}}

	var view DoctorView
	view.Set(report, nil)
	body := plain(view.View(paneTestWidth, paneTestHeight))

	wants := []string{
		"✓", "settings", "config.toml parsed",
		"⚠", "gh", "gh not authenticated",
		"✗", "FAIL", "keyset", "keyset.json not found",
		"fix: run `agent-brain init`",
		"i", "keyset-decrypt", "nothing to probe",
		"1 ok · 1 warn · 1 fail · 1 info",
	}
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("doctor view missing %q; got:\n%s", want, body)
		}
	}
}

func TestDoctorViewStates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(*DoctorView)
		want  string
	}{
		{name: "not loaded", setup: func(*DoctorView) {}, want: "running checks"},
		{name: "error", setup: func(v *DoctorView) { v.Set(doctor.Report{}, errors.New("no paths")) }, want: "doctor checks unavailable"},
		{name: "empty report", setup: func(v *DoctorView) { v.Set(doctor.Report{}, nil) }, want: "no checks reported"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var view DoctorView
			testCase.setup(&view)
			if got := plain(view.View(paneTestWidth, paneTestHeight)); !strings.Contains(got, testCase.want) {
				t.Errorf("doctor view missing %q; got:\n%s", testCase.want, got)
			}
		})
	}
}

// doctorOKReport is the minimal passing battery the action-state tests below
// render their fix/scan sections beneath.
func doctorOKReport() doctor.Report {
	return doctor.Report{Results: []doctor.CheckResult{
		{Name: "settings", Status: doctor.StatusOK, Detail: "config.toml parsed"},
	}}
}

// doctorFailingFixableReport is a battery with a hard failure that carries a
// Fix line — the state `f` is offered on (spec §11).
func doctorFailingFixableReport() doctor.Report {
	return doctor.Report{Results: []doctor.CheckResult{
		{Name: "filters", Status: doctor.StatusFail, Detail: "filter wiring missing", Fix: "run `agent-brain doctor --fix`"},
	}}
}

// TestDoctorViewCanFix pins the `f` gate (spec §11): the fix action is offered
// only when the battery has a hard failure AND some row carries a Fix line —
// never on a clean report, and never when nothing suggests a repair.
//
// The load-bearing case is "warn carrying a fix": the REAL battery routinely
// emits non-failed rows that carry a Fix line — a StatusWarn `service` row with
// "run `agent-brain service install`" (checks.go), the claude/codex prereq
// warns, gitleaks-missing — so the `Failed()` guard, not the fixable-row scan,
// is what keeps `f` off a green-but-warned machine. A gate that only checked for
// a fixable row would light up `f` on that stock dev posture and run real
// quiesce+git surgery from a passing report; this case is what fails if that
// guard is ever dropped.
func TestDoctorViewCanFix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		report doctor.Report
		want   bool
	}{
		{
			name:   "passing report offers no fix",
			report: doctorOKReport(),
			want:   false,
		},
		{
			name:   "failure with no fixable row offers no fix",
			report: doctor.Report{Results: []doctor.CheckResult{{Name: "keyset", Status: doctor.StatusFail, Detail: "keyset.json not found"}}},
			want:   false,
		},
		{
			name:   "warn carrying a fix still offers no fix — only a hard failure does",
			report: doctor.Report{Results: []doctor.CheckResult{{Name: "service", Status: doctor.StatusWarn, Detail: "login service not installed", Fix: "run `agent-brain service install`"}}},
			want:   false,
		},
		{
			name:   "failure with a fixable row offers fix",
			report: doctorFailingFixableReport(),
			want:   true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var view DoctorView
			view.Set(testCase.report, nil)
			if got := view.CanFix(); got != testCase.want {
				t.Errorf("CanFix() = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestDoctorViewFixingAndScanningStates pins the two transient in-flight
// indicators (spec §11/§12): the view announces the pending action inline
// while its Cmd is outstanding.
func TestDoctorViewFixingAndScanningStates(t *testing.T) {
	t.Parallel()
	t.Run("fixing", func(t *testing.T) {
		t.Parallel()
		var view DoctorView
		view.Set(doctorFailingFixableReport(), nil)
		view.SetFixing()
		if got := plain(view.View(paneTestWidth, paneTestHeight)); !strings.Contains(got, "fixing…") {
			t.Errorf("fixing state missing its indicator; got:\n%s", got)
		}
	})
	t.Run("scanning", func(t *testing.T) {
		t.Parallel()
		var view DoctorView
		view.Set(doctorOKReport(), nil)
		view.SetScanning()
		if got := plain(view.View(paneTestWidth, paneTestHeight)); !strings.Contains(got, "scanning…") {
			t.Errorf("scanning state missing its indicator; got:\n%s", got)
		}
	})
}

// TestDoctorViewFixErrorRendersInline pins that a failed fix renders inline
// (the established in-screen convention) and, crucially, leaves the existing
// battery on screen rather than replacing it with a bare error.
func TestDoctorViewFixErrorRendersInline(t *testing.T) {
	t.Parallel()
	var view DoctorView
	view.Set(doctorFailingFixableReport(), nil)
	view.SetFixResult(doctor.Report{}, errors.New("doctor fix filters: not a git repository"))
	body := plain(view.View(paneTestWidth, paneTestHeight))
	if !strings.Contains(body, "fix failed") || !strings.Contains(body, "not a git repository") {
		t.Errorf("fix error not rendered inline; got:\n%s", body)
	}
	if !strings.Contains(body, "filters") {
		t.Errorf("fix error clobbered the battery; the checks must stay on screen; got:\n%s", body)
	}
}

// TestDoctorViewFixResultReRendersReport pins the success path: a clean fix
// replaces the failing battery with the re-checked one.
func TestDoctorViewFixResultReRendersReport(t *testing.T) {
	t.Parallel()
	var view DoctorView
	view.Set(doctorFailingFixableReport(), nil)
	fixed := doctor.Report{Results: []doctor.CheckResult{
		{Name: "filters", Status: doctor.StatusOK, Detail: "filter wiring installed", Fixed: true},
	}}
	view.SetFixResult(fixed, nil)
	body := plain(view.View(paneTestWidth, paneTestHeight))
	if !strings.Contains(body, "filter wiring installed") {
		t.Errorf("re-checked report not rendered after a clean fix; got:\n%s", body)
	}
	if view.CanFix() {
		t.Error("CanFix() still true after the failure was repaired")
	}
}

// TestDoctorViewScanFindings pins the advisory scan section (spec §12): a
// header naming the finding and file counts with the advisory qualifier, then
// one row per finding locating it by folder/file:line and rule.
func TestDoctorViewScanFindings(t *testing.T) {
	t.Parallel()
	var view DoctorView
	view.Set(doctorOKReport(), nil)
	view.SetScanResult([]ScanFinding{
		{Folder: "work", File: "notes.md", Rule: "generic-api-key", Line: 12},
		{Folder: "work", File: "creds.md", Rule: "aws-access-token", Line: 3},
	}, nil)
	body := plain(view.View(paneTestWidth, paneTestHeight))
	wants := []string{
		"2 findings in 2 files — advisory, plaintext hygiene only",
		"work/notes.md:12", "generic-api-key",
		"work/creds.md:3", "aws-access-token",
	}
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("scan section missing %q; got:\n%s", want, body)
		}
	}
}

// TestDoctorViewScanZeroAndError pins the two non-table scan outcomes: a clean
// sweep and an inline error (never a toast — the view is the surface the user
// is looking at).
func TestDoctorViewScanZeroAndError(t *testing.T) {
	t.Parallel()
	t.Run("zero findings", func(t *testing.T) {
		t.Parallel()
		var view DoctorView
		view.Set(doctorOKReport(), nil)
		view.SetScanResult(nil, nil)
		if got := plain(view.View(paneTestWidth, paneTestHeight)); !strings.Contains(got, "no plaintext leaks found") {
			t.Errorf("clean scan missing its line; got:\n%s", got)
		}
	})
	t.Run("scan error", func(t *testing.T) {
		t.Parallel()
		var view DoctorView
		view.Set(doctorOKReport(), nil)
		view.SetScanResult(nil, errors.New("gitleaks not found on PATH"))
		body := plain(view.View(paneTestWidth, paneTestHeight))
		if !strings.Contains(body, "scan failed") || !strings.Contains(body, "gitleaks not found on PATH") {
			t.Errorf("scan error not rendered inline; got:\n%s", body)
		}
	})
}

// TestDoctorViewScanSingularPluralization pins that the count header reads
// naturally for a single finding in a single file.
func TestDoctorViewScanSingularPluralization(t *testing.T) {
	t.Parallel()
	var view DoctorView
	view.Set(doctorOKReport(), nil)
	view.SetScanResult([]ScanFinding{{Folder: "work", File: "notes.md", Rule: "generic-api-key", Line: 1}}, nil)
	if got := plain(view.View(paneTestWidth, paneTestHeight)); !strings.Contains(got, "1 finding in 1 file — advisory, plaintext hygiene only") {
		t.Errorf("singular scan header wrong; got:\n%s", got)
	}
}

// TestDoctorViewFindingsCap pins the no-scroll-surface guard (mirrors
// ConflictsView's maxConflictRows): a pathological sweep renders at most
// maxDoctorFindingRows rows, the header still counts every finding, and the
// remainder is disclosed — but the note must be exactly-true, absent when the
// count lands exactly on the cap.
func TestDoctorViewFindingsCap(t *testing.T) {
	t.Parallel()
	makeFindings := func(n int) []ScanFinding {
		findings := make([]ScanFinding, n)
		for i := range findings {
			findings[i] = ScanFinding{Folder: "work", File: fmt.Sprintf("f%d.md", i), Rule: "generic-api-key", Line: i + 1}
		}
		return findings
	}

	t.Run("over the cap truncates rows and discloses the remainder", func(t *testing.T) {
		t.Parallel()
		var view DoctorView
		view.Set(doctorOKReport(), nil)
		view.SetScanResult(makeFindings(maxDoctorFindingRows+5), nil)
		body := plain(view.View(paneTestWidth, paneTestHeight))
		// The header counts every finding, not just the rendered window.
		if want := fmt.Sprintf("%d findings", maxDoctorFindingRows+5); !strings.Contains(body, want) {
			t.Errorf("header must count all findings (%q); got:\n%s", want, body)
		}
		// Exactly maxDoctorFindingRows location rows render (each row carries ".md:").
		if got := strings.Count(body, ".md:"); got != maxDoctorFindingRows {
			t.Errorf("rendered %d finding rows, want %d (capped)", got, maxDoctorFindingRows)
		}
		if want := "… and 5 more findings — run `agent-brain scan` for the full report"; !strings.Contains(body, want) {
			t.Errorf("truncation note missing/wrong; want %q; got tail:\n%s", want, tail(body))
		}
	})

	t.Run("exactly at the cap renders every row and no note", func(t *testing.T) {
		t.Parallel()
		var view DoctorView
		view.Set(doctorOKReport(), nil)
		view.SetScanResult(makeFindings(maxDoctorFindingRows), nil)
		body := plain(view.View(paneTestWidth, paneTestHeight))
		if got := strings.Count(body, ".md:"); got != maxDoctorFindingRows {
			t.Errorf("rendered %d finding rows at the cap, want %d", got, maxDoctorFindingRows)
		}
		if strings.Contains(body, "more finding") {
			t.Errorf("a truncation note appeared at exactly the cap (must be exactly-true at len==cap); got tail:\n%s", tail(body))
		}
	})
}

// tail returns the last few lines of s for compact failure messages when the
// rendered body (a capped-but-still-long findings list) would otherwise swamp
// the output.
func tail(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	return strings.Join(lines, "\n")
}

// bigDoctorReport builds n OK checks with zero-padded, individually
// identifiable names, so a scroll test can assert exactly which rows sit in the
// window before and after a keypress.
func bigDoctorReport(n int) doctor.Report {
	results := make([]doctor.CheckResult, n)
	for i := range results {
		results[i] = doctor.CheckResult{Name: fmt.Sprintf("check-%03d", i), Status: doctor.StatusOK, Detail: "ok"}
	}
	return doctor.Report{Results: results}
}

// lineCount reports how many terminal rows s occupies. An SGR escape never
// carries a newline, so splitting on "\n" counts display rows regardless of the
// styling woven through them.
func lineCount(s string) int {
	return strings.Count(s, "\n") + 1
}

// TestDoctorViewScrollsWhenOverflowing pins the bounded-viewport contract at a
// height too small for the battery (spec §7): the tab body never exceeds the
// budget and spends its bottom line on the overflow hint; ctrl+d advances the
// window (a later row appears, the top row is gone); and G/g jump to the ends.
//
// The line-count assertion is the mutation sentinel for this task: dropping the
// height bound in scrollPane.fit (rendering the whole battery instead of the
// windowed slice) makes the body run to ~100 lines here and fails this test,
// while the root frame's own clamp would mask that overflow — which is why this
// asserts on the view's OWN output, not a root-composed frame.
func TestDoctorViewScrollsWhenOverflowing(t *testing.T) {
	t.Parallel()
	const height = 12 // 2 lines of fixed title chrome over a 10-line pane (9 content + 1 hint)
	view := NewDoctorView()
	view.Set(bigDoctorReport(100), nil)

	top := plain(view.View(paneTestWidth, height))
	if got := lineCount(top); got > height {
		t.Errorf("tab body is %d lines, over the %d-line budget — the viewport is not bounding it; got:\n%s", got, height, top)
	}
	if !strings.Contains(top, "pgup/pgdn scroll") {
		t.Errorf("overflowing body missing the scroll hint; got:\n%s", top)
	}
	if !strings.Contains(top, "check-000") || strings.Contains(top, "check-050") {
		t.Errorf("top window wrong — want the first row present and a far row absent; got:\n%s", top)
	}

	view.Scroll(ctrlKey('d'), paneTestWidth, height)
	advanced := plain(view.View(paneTestWidth, height))
	if strings.Contains(advanced, "check-000") {
		t.Errorf("ctrl+d did not advance the window — the top row is still visible; got:\n%s", advanced)
	}
	if !strings.Contains(advanced, "check-009") {
		t.Errorf("ctrl+d did not reveal a later row (check-009); got:\n%s", advanced)
	}

	view.Scroll(key("G"), paneTestWidth, height)
	bottom := plain(view.View(paneTestWidth, height))
	if !strings.Contains(bottom, "check-099") || !strings.Contains(bottom, "100 ok") {
		t.Errorf("G did not jump to the bottom — the last check and the summary must show; got:\n%s", bottom)
	}

	view.Scroll(key("g"), paneTestWidth, height)
	backToTop := plain(view.View(paneTestWidth, height))
	if !strings.Contains(backToTop, "check-000") || strings.Contains(backToTop, "100 ok") {
		t.Errorf("g did not jump back to the top; got:\n%s", backToTop)
	}
}

// TestDoctorViewFitsWithoutHintOrScroll pins the other side of the budget: a
// battery that fits shows no overflow hint and its scroll keys are harmless
// no-ops (there is nothing off-screen to reveal), so a short Doctor tab reads
// exactly as it did before the viewport, never wasting a line on an affordance
// for scrolling that cannot happen.
func TestDoctorViewFitsWithoutHintOrScroll(t *testing.T) {
	t.Parallel()
	view := NewDoctorView()
	view.Set(doctorOKReport(), nil)

	before := plain(view.View(paneTestWidth, paneTestHeight))
	if strings.Contains(before, "pgup/pgdn scroll") {
		t.Errorf("a fitting body must not show the scroll hint; got:\n%s", before)
	}

	view.Scroll(key("G"), paneTestWidth, paneTestHeight)
	after := plain(view.View(paneTestWidth, paneTestHeight))
	if before != after {
		t.Errorf("a scroll key changed a fitting body — it must be a no-op;\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestDoctorViewRefreshKeepsScrollPosition pins the anti-yank rule (spec §7): an
// identical re-run of the battery (the r key, or a 2s poll returning the same
// report) must leave the reader where they scrolled, while a genuinely changed
// report resets to the top. Without the identity guard a refresh would GotoTop
// every poll and make a long battery impossible to read past the first screen.
func TestDoctorViewRefreshKeepsScrollPosition(t *testing.T) {
	t.Parallel()
	const height = 12
	view := NewDoctorView()
	view.Set(bigDoctorReport(100), nil)
	view.Scroll(ctrlKey('d'), paneTestWidth, height)
	scrolled := plain(view.View(paneTestWidth, height))

	// An identical report arriving again (a poll) must not move the window.
	view.Set(bigDoctorReport(100), nil)
	if got := plain(view.View(paneTestWidth, height)); got != scrolled {
		t.Errorf("an identical re-run yanked the scroll position;\nwas:\n%s\nnow:\n%s", scrolled, got)
	}

	// A materially different report resets to the top.
	view.Set(bigDoctorReport(80), nil)
	if got := plain(view.View(paneTestWidth, height)); !strings.Contains(got, "check-000") {
		t.Errorf("a changed report did not reset to the top; got:\n%s", got)
	}
}
