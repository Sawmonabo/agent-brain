package theme

import (
	"fmt"
	"regexp"
	"testing"

	"charm.land/lipgloss/v2"
)

// csiPattern matches the CSI escape sequences lipgloss emits — ESC '[',
// numeric parameters, a letter terminator (SGR colour/attributes end in
// 'm'). Duplicated from the dashboard/views test suites (rather than
// exported from production code) because it is test-only scaffolding, and
// this package cannot import a package that imports theme (dashboard and
// views both do) without an import cycle in the test binary.
var csiPattern = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// plain strips styling so assertions match the visible text.
func plain(s string) string {
	return csiPattern.ReplaceAllString(s, "")
}

// TestDefaultStylesRenderPlainText pins the contract every dashboard view
// test depends on: every style Default returns carries color/attribute (SGR)
// escapes only, never a literal glyph, so stripping the escapes always
// recovers the exact input text — for BOTH the dark (Mocha) and light
// (Latte) palettes, so a background-color swap can never start wrapping
// rendered text in visible characters.
func TestDefaultStylesRenderPlainText(t *testing.T) {
	t.Parallel()
	const sample = "sample text"

	for _, isDark := range []bool{true, false} {
		styles := Default(isDark)
		tests := []struct {
			name  string
			style lipgloss.Style
		}{
			{"Title", styles.Title},
			{"Header", styles.Header},
			{"Dim", styles.Dim},
			{"OK", styles.OK},
			{"Warn", styles.Warn},
			{"Fail", styles.Fail},
			{"Info", styles.Info},
			{"ActiveTab", styles.ActiveTab},
			{"InactiveTab", styles.InactiveTab},
			{"Badge", styles.Badge},
			{"Toast", styles.Toast},
			{"Selected", styles.Selected},
		}
		for _, testCase := range tests {
			t.Run(fmt.Sprintf("isDark=%v/%s", isDark, testCase.name), func(t *testing.T) {
				t.Parallel()
				if got := plain(testCase.style.Render(sample)); got != sample {
					t.Errorf("%s.Render(%q) stripped to %q, want %q verbatim", testCase.name, sample, got, sample)
				}
			})
		}
	}
}

// TestDefaultDiffersByBackground proves Default(isDark) actually branches on
// its argument. TestDefaultStylesRenderPlainText's CSI round-trip holds even
// for a Default that ignored isDark and always returned one hardcoded
// flavour, so it alone cannot catch that regression — this test compares the
// resolved color itself. GetForeground reads the lipgloss.Style's stored
// color.Color value directly, not a rendered string: a rendered string's
// ANSI escapes depend on the terminal's color profile, which is not fixed
// when tests run without a TTY, so asserting on rendered output here would be
// flaky rather than a genuine flavour check.
func TestDefaultDiffersByBackground(t *testing.T) {
	t.Parallel()
	dark := Default(true)
	light := Default(false)

	tests := []struct {
		name  string
		dark  lipgloss.Style
		light lipgloss.Style
	}{
		{"Title", dark.Title, light.Title},
		{"Header", dark.Header, light.Header},
		{"Dim", dark.Dim, light.Dim},
		{"OK", dark.OK, light.OK},
		{"Warn", dark.Warn, light.Warn},
		{"Fail", dark.Fail, light.Fail},
		{"Info", dark.Info, light.Info},
		{"ActiveTab", dark.ActiveTab, light.ActiveTab},
		{"InactiveTab", dark.InactiveTab, light.InactiveTab},
		{"Badge", dark.Badge, light.Badge},
		{"Toast", dark.Toast, light.Toast},
		// Selected carries no palette-derived color (Reverse(true) only), so
		// it is identical by construction in both flavours — deliberately
		// excluded, since including it would assert a false invariant.
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := testCase.dark.GetForeground(); got == testCase.light.GetForeground() {
				t.Errorf("%s foreground identical between dark and light flavours: %v", testCase.name, got)
			}
		})
	}
}
