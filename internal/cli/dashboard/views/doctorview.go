package views

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
)

// ScanFinding is one advisory gitleaks hit the Doctor screen renders under
// its checks (spec §12). It is the hub projection of cli's scanFinding: File/
// Rule/Line locate the leak so the operator can rotate it, while the raw
// Secret/Match text is deliberately absent — the scan runs gitleaks --redact,
// so plaintext secrets never enter hub memory even transiently. A finding is
// advisory only: it never gates a sync (never joins SafetyGate, spec §12).
type ScanFinding struct {
	Folder string // enrolled folder whose unit produced the finding
	File   string // path within the unit, relative (hubScanRunner trims gitleaks' scanned-root prefix)
	Rule   string // gitleaks rule id
	Line   int    // 1-based line within File
}

// maxDoctorFindingRows bounds how many advisory findings the Doctor tab renders
// at once — the same guard ConflictsView applies (maxConflictRows): there is no
// scroll component here, so a pathological sweep (a pasted credential dump,
// thousands of hits) must not rebuild a giant frame each tick and push the
// footer off-screen. The header still counts every finding; only the row list is
// capped, with the remainder disclosed and the full report available through
// `agent-brain scan`.
const maxDoctorFindingRows = 200

// DoctorView renders the doctor battery (spec §7) with a per-check status
// glyph, plus the two on-demand actions layered beneath it (spec §11/§12): a
// one-key `doctor --fix` and an advisory gitleaks scan. The battery runs
// read-only with --offline semantics (no ls-remote); the root model fetches it
// through DataSource.Doctor, drives fix/scan through the injected Config
// closures, and feeds every result back through this view's Set* methods —
// the view itself performs no I/O, so its whole surface is table-testable.
type DoctorView struct {
	styles theme.Styles
	report doctor.Report
	err    error
	loaded bool

	// Fix action state (spec §11). fixing is true while the RunDoctorFix Cmd is
	// outstanding; fixErr holds a failed fix's error, rendered inline beneath
	// the checks (the established in-screen convention — the view is the
	// surface the user is looking at, so an error beats any toast) while the
	// prior battery stays on screen. A successful fix clears both and installs
	// the re-checked report through the ordinary report field.
	fixing bool
	fixErr error

	// Scan action state (spec §12). scanning is true while the Scan Cmd is
	// outstanding; scanned latches once a scan has completed at least once, so
	// an empty findings slice renders "no plaintext leaks found" rather than
	// nothing. findings are the advisory hits; scanErr renders inline like
	// fixErr. Advisory throughout — nothing here ever gates a sync.
	scanning bool
	scanned  bool
	findings []ScanFinding
	scanErr  error

	// pane bounds the battery to the tab's height budget and scrolls it in place
	// (spec §7): the full battery — checks, summary, and the fix/scan surfaces —
	// can run past a short terminal, so it renders through a viewport rather than
	// growing the frame and letting the root clamp silently cut the tail. Its
	// content carries no now-relative text, so the rendered body doubles as the
	// pane's change key (syncPane).
	pane scrollPane
}

// NewDoctorView builds a Doctor tab with its scroll pane initialized. The
// battery/fix/scan state all start zero (loaded false → "running checks…"); the
// model constructs it here rather than zero-valuing it because the pane's
// viewport needs its scroll keymap before ctrl+d/G ever reach it.
func NewDoctorView() DoctorView {
	return DoctorView{pane: newScrollPane()}
}

// SetStyles installs the palette-derived style set this view renders
// through. Root calls it once on construction and again on every
// tea.BackgroundColorMsg — never per render.
func (v *DoctorView) SetStyles(styles theme.Styles) {
	v.styles = styles
}

// Set installs a freshly-run doctor report (the r re-run and the tab poll).
// It leaves the fix/scan overlays untouched — a background poll landing mid-fix
// must not clear the "fixing…" indicator; only the fix's own result does.
func (v *DoctorView) Set(report doctor.Report, err error) {
	v.report = report
	v.err = err
	v.loaded = true
}

// CanFix reports whether the last battery is in the state `f` is offered on
// (spec §11): a hard failure AND some row carrying a Fix line to act on. A
// clean report, or one whose failures suggest no repair at all, offers
// nothing to fix. The root consults this for available("doctor-fix"), so the
// footer, palette, and the f key all gate on the identical predicate.
func (v DoctorView) CanFix() bool {
	if !v.report.Failed() {
		return false
	}
	for _, result := range v.report.Results {
		if result.Fix != "" {
			return true
		}
	}
	return false
}

// SetFixing marks a fix Cmd in flight (spec §11), clearing any prior fix error
// so a retry starts clean.
func (v *DoctorView) SetFixing() {
	v.fixing = true
	v.fixErr = nil
}

// SetFixResult resolves an outstanding fix. A failure keeps the existing
// battery on screen and records the error for an inline note; a success
// installs the re-checked report and clears the error. Either way the fixing
// indicator clears.
func (v *DoctorView) SetFixResult(report doctor.Report, err error) {
	v.fixing = false
	if err != nil {
		v.fixErr = err
		return
	}
	v.fixErr = nil
	v.report = report
	v.err = nil
	v.loaded = true
}

// SetScanning marks a scan Cmd in flight (spec §12), clearing any prior scan
// error so a re-scan starts clean.
func (v *DoctorView) SetScanning() {
	v.scanning = true
	v.scanErr = nil
}

// SetScanResult resolves an outstanding scan: it latches scanned (so a clean
// sweep renders its own line rather than nothing) and records the findings or
// the error for the findings section.
func (v *DoctorView) SetScanResult(findings []ScanFinding, err error) {
	v.scanning = false
	v.scanned = true
	v.findings = findings
	v.scanErr = err
}

// View renders the Doctor tab from the last Set snapshot: a fixed section title
// over a height-bounded, scrollable body (spec §7). width/height come fresh from
// the root every call, so a resize is handled by construction; height is the
// whole tab-body budget, from which the fixed title chrome is subtracted for the
// pane. Value receiver — the viewport's content/size mutations are local to this
// render, and the scroll OFFSET is advanced only by Scroll on the root's
// addressable copy of the view.
func (v DoctorView) View(width, height int) string {
	v.syncPane()
	return sectionTitle(v.styles, "Doctor") + "\n\n" +
		v.pane.render(v.styles, width, max(height-sectionChromeLines, 1))
}

// Scroll routes a scroll key to the battery pane, sizing it to the same budget
// View uses so the page math matches what is drawn. It reports whether the key
// was a scroll key it consumed, so the root's handleDoctorKey still owns r/f/s
// on a miss.
func (v *DoctorView) Scroll(msg tea.KeyPressMsg, width, height int) bool {
	v.syncPane()
	return v.pane.scroll(msg, width, max(height-sectionChromeLines, 1))
}

// syncPane installs the current battery body in the scroll pane, resetting the
// scroll to the top only when the rendered body changed — a re-run producing an
// identical report, or an unchanged 2s poll, leaves the reader where they are; a
// genuinely different battery starts at the top. The body carries no
// now-relative text, so it IS its own change key.
func (v *DoctorView) syncPane() {
	body := v.body()
	v.pane.refresh(body, body)
}

// body renders everything BELOW the section title — the battery, its summary,
// and the fix/scan surfaces — or the error/loading/empty placeholder. It is the
// scroll pane's content (the title is fixed chrome above it); being free of any
// now-relative text, it doubles as the pane's change key (syncPane).
func (v DoctorView) body() string {
	switch {
	case v.err != nil:
		return fmt.Sprintf("doctor checks unavailable: %v", v.err)
	case !v.loaded:
		return v.styles.Dim.Render("running checks…")
	case len(v.report.Results) == 0:
		return v.styles.Dim.Render("no checks reported")
	}

	var b strings.Builder
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

	v.writeFixState(&b)
	v.writeScanState(&b)
	// The findings rows leave a trailing newline; trim it so the root's
	// header/body/footer join spaces the frame consistently (as ConflictsView
	// does), whether or not a scan has run.
	return strings.TrimRight(b.String(), "\n")
}

// writeFixState appends the fix action's transient surface beneath the battery
// summary: "fixing…" while the Cmd is outstanding, or a failed fix's error
// inline (spec §11). A clean fix leaves nothing here — its result is the
// re-rendered battery above plus the root's info toast.
func (v DoctorView) writeFixState(b *strings.Builder) {
	switch {
	case v.fixing:
		fmt.Fprintf(b, "\n\n%s", v.styles.Dim.Render("fixing…"))
	case v.fixErr != nil:
		fmt.Fprintf(b, "\n\nfix failed: %v", v.fixErr)
	}
}

// writeScanState appends the advisory scan surface beneath the battery (spec
// §12): "scanning…" while outstanding, an inline error, or — once a scan has
// completed — the findings section or a clean-sweep line.
func (v DoctorView) writeScanState(b *strings.Builder) {
	switch {
	case v.scanning:
		fmt.Fprintf(b, "\n\n%s", v.styles.Dim.Render("scanning…"))
	case v.scanErr != nil:
		fmt.Fprintf(b, "\n\nscan failed: %v", v.scanErr)
	case v.scanned:
		v.writeFindings(b)
	}
}

// writeFindings renders the advisory finding rows (spec §12). Zero findings is
// its own reassuring line; otherwise a header counts findings and distinct
// files with the advisory qualifier, then one row per finding locates it by
// folder/file:line and names the gitleaks rule. No secret text is ever printed
// — a finding carries only its location (the scan ran --redact upstream).
func (v DoctorView) writeFindings(b *strings.Builder) {
	if len(v.findings) == 0 {
		fmt.Fprintf(b, "\n\n%s", v.styles.Dim.Render("no plaintext leaks found"))
		return
	}
	files := make(map[string]struct{}, len(v.findings))
	for _, finding := range v.findings {
		files[finding.Folder+"/"+finding.File] = struct{}{}
	}
	fmt.Fprintf(b, "\n\n%s in %s — advisory, plaintext hygiene only\n",
		quantity(len(v.findings), "finding", "findings"),
		quantity(len(files), "file", "files"))
	rows := v.findings
	truncated := 0
	if len(rows) > maxDoctorFindingRows {
		truncated = len(rows) - maxDoctorFindingRows
		rows = rows[:maxDoctorFindingRows]
	}
	for _, finding := range rows {
		fmt.Fprintf(b, "  %s/%s:%d  %s\n", finding.Folder, finding.File, finding.Line, finding.Rule)
	}
	if truncated > 0 {
		fmt.Fprintf(b, "%s\n", v.styles.Dim.Render(fmt.Sprintf(
			"… and %s — run `agent-brain scan` for the full report",
			quantity(truncated, "more finding", "more findings"),
		)))
	}
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
