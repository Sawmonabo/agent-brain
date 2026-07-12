package cli

import (
	"errors"

	keybinding "charm.land/bubbles/v2/key"
	"charm.land/huh/v2"
)

// cancellableKeyMap is the default huh keymap with the form-level Quit
// binding made discoverable: esc joins ctrl+c as an abort key. huh v2.0.3's
// Form.Update (form.go) matches every keypress against this exact binding
// before any group or field ever runs, so esc reliably cancels a form even
// mid-edit or mid-filter — no field-level key handling can shadow it. x is
// unavailable (MultiSelect binds it as the toggle key); esc is otherwise
// unclaimed by anything a human is meant to reach through the forms in this
// package.
func cancellableKeyMap() *huh.KeyMap {
	keyMap := huh.NewDefaultKeyMap()
	keyMap.Quit = keybinding.NewBinding(keybinding.WithKeys("esc", "ctrl+c"), keybinding.WithHelp("esc", "cancel"))
	return keyMap
}

// titleWithCancelHint appends a visible escape-key reminder to a field's
// title — the one place a cancel hint is guaranteed to render for every
// form in this package.
//
// It would be simpler if the rendered help line could just show it: huh
// renders a group's footer from the FOCUSED FIELD's own KeyBinds() (group.go
// Footer -> help.ShortHelpView), never from the form-level keymap, and
// Quit lives only on *Form — no Field.KeyBinds() implementation in
// field_*.go returns it or has any way to. Every field type this package
// builds forms from was checked for a spare slot in that returned list and
// found to have none: Input's and Confirm's Prev/Next are forced disabled
// by WithPosition for a single-field, single-group form (every site in this
// package builds exactly one), leaving only their already-essential
// Submit/Toggle/Accept/Reject bindings; MultiSelect's filter trio renders
// only while Filterable stays at its constructor default of true, and this
// package turns that off for the one MultiSelect it builds (see enroll.go)
// to keep esc from ambiguously colliding with "clear the in-progress
// filter"; Select's filter bindings are the sole slot that would survive,
// but reusing them would advertise the hint on selects only, not uniformly.
//
// accessible must be false to add the hint at all: huh's accessible mode
// (Form.runAccessible) never runs the bubbletea program the form-level Quit
// binding is matched inside — it calls each field's own RunAccessible(w, r)
// directly, a bare bufio.Scanner line read (internal/accessibility.PromptBool
// et al.) with no notion of "keys" at all, so esc reaches it as inert text,
// not a cancel. Every RunAccessible implementation renders title.val
// verbatim, so baking the hint into the title unconditionally would print a
// false promise on exactly the path a screen-reader user relies on being
// honest. Bare title, unchanged, is accessible mode's correct output.
//
// The hint starts its own line (a leading "\n", not just a joining space)
// so it is never at the mercy of where an unrelated, possibly long, title
// happens to wrap: appending it inline let a title's own line-wrap fall
// mid-hint at ordinary terminal widths, splitting "esc" onto one line and
// "cancel" onto the next (resolveKeysetDecision's first prompt is long
// enough to hit this at 80 columns). "· esc cancel" is short enough that,
// given its own line, it wraps on its own regardless of title length or
// terminal width.
func titleWithCancelHint(title string, accessible bool) string {
	if accessible {
		return title
	}
	return title + "\n· esc cancel"
}

// formCancelled reports whether err is the user backing out of an
// interactive form — a deliberate no-op, never a failure.
func formCancelled(err error) bool {
	return errors.Is(err, huh.ErrUserAborted)
}
