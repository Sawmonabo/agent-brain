package dashboard

import (
	"errors"
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

	var view doctorView
	view.set(report, nil)
	body := plain(view.view())

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
		setup func(*doctorView)
		want  string
	}{
		{name: "not loaded", setup: func(*doctorView) {}, want: "running checks"},
		{name: "error", setup: func(v *doctorView) { v.set(doctor.Report{}, errors.New("no paths")) }, want: "doctor checks unavailable"},
		{name: "empty report", setup: func(v *doctorView) { v.set(doctor.Report{}, nil) }, want: "no checks reported"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var view doctorView
			testCase.setup(&view)
			if got := plain(view.view()); !strings.Contains(got, testCase.want) {
				t.Errorf("doctor view missing %q; got:\n%s", testCase.want, got)
			}
		})
	}
}
