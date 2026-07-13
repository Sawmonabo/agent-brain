package views

import (
	"fmt"
	"strings"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
)

// DoctorView renders the doctor battery (spec §7) with a per-check status
// glyph. The battery runs read-only with --offline semantics (no ls-remote);
// the root model fetches it through DataSource.Doctor, whose production
// implementation assembles the full doctor.Deps in package cli (the registry/
// gh composition lives outside this package's import allowlist).
type DoctorView struct {
	styles theme.Styles
	report doctor.Report
	err    error
	loaded bool
}

// SetStyles installs the palette-derived style set this view renders
// through. Root calls it once on construction and again on every
// tea.BackgroundColorMsg — never per render.
func (v *DoctorView) SetStyles(styles theme.Styles) {
	v.styles = styles
}

// Set installs a freshly-run doctor report.
func (v *DoctorView) Set(report doctor.Report, err error) {
	v.report = report
	v.err = err
	v.loaded = true
}

// View renders the Doctor tab from the last Set snapshot.
func (v DoctorView) View() string {
	var b strings.Builder
	b.WriteString(sectionTitle(v.styles, "Doctor"))
	b.WriteString("\n\n")

	switch {
	case v.err != nil:
		fmt.Fprintf(&b, "doctor checks unavailable: %v", v.err)
		return b.String()
	case !v.loaded:
		b.WriteString(v.styles.Dim.Render("running checks…"))
		return b.String()
	case len(v.report.Results) == 0:
		b.WriteString(v.styles.Dim.Render("no checks reported"))
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
		fmt.Fprintf(&b, "%s %-4s %-20s %s\n", statusGlyph(v.styles, result.Status), label, result.Name, result.Detail)
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
func statusGlyph(styles theme.Styles, status doctor.Status) string {
	switch status {
	case doctor.StatusOK:
		return styles.OK.Render("✓")
	case doctor.StatusWarn:
		return styles.Warn.Render("⚠")
	case doctor.StatusFail:
		return styles.Fail.Render("✗")
	case doctor.StatusInfo:
		return styles.Info.Render("i")
	default:
		return "?"
	}
}
