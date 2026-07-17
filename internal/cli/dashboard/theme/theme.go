// Package theme resolves the dashboard's palette-derived style set: the
// catppuccin Mocha (dark) and Latte (light) flavours, collapsed into a fixed
// set of named lipgloss styles every dashboard view renders through. Views
// never touch a catppuccin color directly — each holds a Styles value (set
// once via SetStyles, never re-derived per render) and renders through its
// fields, so a background-color swap recolors every view uniformly from one
// root-level call.
//
// Every field renders color/attribute (SGR) escapes only — never a literal
// glyph, padding, or border — so the test suites' CSI-strip helper always
// recovers the exact input text regardless of flavour. TestDefaultStylesRenderPlainText
// pins that contract.
package theme

import (
	"image/color"

	catppuccin "github.com/catppuccin/go"

	"charm.land/lipgloss/v2"
)

// Styles is the dashboard's full named style set. Title/Header/Dim are
// structural (section headers, de-emphasized text); OK/Warn/Fail/Info are
// the doctor battery's and status glyphs' semantic colors; ActiveTab/
// InactiveTab render the tab bar. Toast renders the status-area info toast
// and ToastSticky its sticky error/action-required sibling (error-red, so it
// reads as a failure that needs attention rather than transient feedback).
// Badge/Selected are consumed by later screens in the dashboard-hub wave —
// they carry a palette-derived color today so that
// wave's screens inherit the swap for free instead of inventing their own
// colors later.
type Styles struct {
	Title       lipgloss.Style
	Header      lipgloss.Style
	Dim         lipgloss.Style
	OK          lipgloss.Style
	Warn        lipgloss.Style
	Fail        lipgloss.Style
	Info        lipgloss.Style
	ActiveTab   lipgloss.Style
	InactiveTab lipgloss.Style
	Badge       lipgloss.Style
	Toast       lipgloss.Style
	ToastSticky lipgloss.Style
	Selected    lipgloss.Style
}

// Default resolves the dashboard's style set for the terminal's reported
// background: Mocha when isDark, Latte otherwise (bubbletea's
// tea.BackgroundColorMsg.IsDark — requested at Init and re-derived on every
// change, spec §7). The root model defaults to Default(true) before the
// terminal answers, so the dashboard is never unstyled.
func Default(isDark bool) Styles {
	flavour := catppuccin.Latte
	if isDark {
		flavour = catppuccin.Mocha
	}
	return Styles{
		Title:       lipgloss.NewStyle().Bold(true).Foreground(hex(flavour.Text())),
		Header:      lipgloss.NewStyle().Bold(true).Foreground(hex(flavour.Subtext1())),
		Dim:         lipgloss.NewStyle().Faint(true).Foreground(hex(flavour.Overlay1())),
		OK:          lipgloss.NewStyle().Foreground(hex(flavour.Green())),
		Warn:        lipgloss.NewStyle().Foreground(hex(flavour.Yellow())),
		Fail:        lipgloss.NewStyle().Foreground(hex(flavour.Red())),
		Info:        lipgloss.NewStyle().Foreground(hex(flavour.Blue())),
		ActiveTab:   lipgloss.NewStyle().Bold(true).Foreground(hex(flavour.Mauve())),
		InactiveTab: lipgloss.NewStyle().Faint(true).Foreground(hex(flavour.Overlay0())),
		Badge:       lipgloss.NewStyle().Bold(true).Foreground(hex(flavour.Mauve())),
		Toast:       lipgloss.NewStyle().Italic(true).Foreground(hex(flavour.Teal())),
		ToastSticky: lipgloss.NewStyle().Italic(true).Foreground(hex(flavour.Red())),
		Selected:    lipgloss.NewStyle().Reverse(true),
	}
}

// hex adapts a catppuccin Color to a lipgloss foreground color via its hex
// string — the same representation lipgloss.Color documents for true-color
// terminals (bubbletea/lipgloss downgrade it for lower-color terminals at
// render time, not here).
func hex(c catppuccin.Color) color.Color {
	return lipgloss.Color(c.Hex)
}
