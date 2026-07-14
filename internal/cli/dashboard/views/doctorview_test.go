package views

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/doctor"
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
	body := plain(view.View())

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
			if got := plain(view.View()); !strings.Contains(got, testCase.want) {
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
		if got := plain(view.View()); !strings.Contains(got, "fixing…") {
			t.Errorf("fixing state missing its indicator; got:\n%s", got)
		}
	})
	t.Run("scanning", func(t *testing.T) {
		t.Parallel()
		var view DoctorView
		view.Set(doctorOKReport(), nil)
		view.SetScanning()
		if got := plain(view.View()); !strings.Contains(got, "scanning…") {
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
	body := plain(view.View())
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
	body := plain(view.View())
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
	body := plain(view.View())
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
		if got := plain(view.View()); !strings.Contains(got, "no plaintext leaks found") {
			t.Errorf("clean scan missing its line; got:\n%s", got)
		}
	})
	t.Run("scan error", func(t *testing.T) {
		t.Parallel()
		var view DoctorView
		view.Set(doctorOKReport(), nil)
		view.SetScanResult(nil, errors.New("gitleaks not found on PATH"))
		body := plain(view.View())
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
	if got := plain(view.View()); !strings.Contains(got, "1 finding in 1 file — advisory, plaintext hygiene only") {
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
		body := plain(view.View())
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
		body := plain(view.View())
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
