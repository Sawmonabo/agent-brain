package dashboard

import (
	"fmt"
	"strings"

	"github.com/Sawmonabo/agent-brain/internal/doctor"
)

// doctorView renders the doctor battery (spec §7) with a per-check status
// glyph. The battery runs read-only with --offline semantics (no ls-remote);
// the root model fetches it through dashboardData.Doctor, whose production
// implementation assembles the full doctor.Deps in package cli (the registry/
// gh composition lives outside this package's import allowlist).
type doctorView struct {
	report doctor.Report
	err    error
	loaded bool
}

func (v *doctorView) set(report doctor.Report, err error) {
	v.report = report
	v.err = err
	v.loaded = true
}

func (v doctorView) view() string {
	var b strings.Builder
	b.WriteString(sectionTitle("Doctor"))
	b.WriteString("\n\n")

	switch {
	case v.err != nil:
		fmt.Fprintf(&b, "doctor checks unavailable: %v", v.err)
		return b.String()
	case !v.loaded:
		b.WriteString(dimStyle.Render("running checks…"))
		return b.String()
	case len(v.report.Results) == 0:
		b.WriteString(dimStyle.Render("no checks reported"))
		return b.String()
	}

	var ok, warn, fail, info int
	for _, result := range v.report.Results {
		switch result.Status {
		case doctor.StatusOK:
			ok++
		case doctor.StatusWarn:
			warn++
		case doctor.StatusFail:
			fail++
		case doctor.StatusInfo:
			info++
		}
		label := result.Status.String()
		if result.Status == doctor.StatusFail {
			label = strings.ToUpper(label)
		}
		fmt.Fprintf(&b, "%s %-4s %-20s %s\n", statusGlyph(result.Status), label, result.Name, result.Detail)
		if result.Status != doctor.StatusOK && result.Fix != "" {
			fmt.Fprintf(&b, "         %-20s fix: %s\n", "", result.Fix)
		}
		if result.Fixed {
			fmt.Fprintf(&b, "         %-20s fixed\n", "")
		}
	}

	b.WriteString("\n")
	fmt.Fprintf(&b, "%d ok · %d warn · %d fail · %d info", ok, warn, fail, info)
	return b.String()
}

// statusGlyph maps a check status to a testable glyph (the glyph, not the
// colour, is the signal — tests strip styling to plain). ✓/⚠/✗ read the same
// under NO_COLOR as they do in colour.
func statusGlyph(status doctor.Status) string {
	switch status {
	case doctor.StatusOK:
		return okStyle.Render("✓")
	case doctor.StatusWarn:
		return warnStyle.Render("⚠")
	case doctor.StatusFail:
		return failStyle.Render("✗")
	case doctor.StatusInfo:
		return infoStyle.Render("i")
	default:
		return "?"
	}
}
