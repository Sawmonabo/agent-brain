package cli

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	keybinding "charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/google/go-cmp/cmp"
)

// TestCancellableKeyMapAdvertisesEscAndCtrlC guards the exact shape upstream
// drift could silently break: esc must stay first (it's the key the help
// text names), ctrl+c must remain bound alongside it, and the help pair
// must read "esc"/"cancel" — the literal text titleWithCancelHint promises
// callers is real.
func TestCancellableKeyMapAdvertisesEscAndCtrlC(t *testing.T) {
	t.Parallel()
	keyMap := cancellableKeyMap()

	wantKeys := []string{"esc", "ctrl+c"}
	if diff := cmp.Diff(wantKeys, keyMap.Quit.Keys()); diff != "" {
		t.Errorf("Quit.Keys() (-want +got):\n%s", diff)
	}

	wantHelp := keybinding.Help{Key: "esc", Desc: "cancel"}
	if diff := cmp.Diff(wantHelp, keyMap.Quit.Help()); diff != "" {
		t.Errorf("Quit.Help() (-want +got):\n%s", diff)
	}
}

// TestTitleWithCancelHint pins the exact contract every call site relies
// on: the hint is appended only when accessible is false. See
// titleWithCancelHint's own doc comment for why accessible must suppress
// it outright — huh's accessible mode has no notion of "keys" at all, so
// advertising one there is a false promise, not just a redundant one.
func TestTitleWithCancelHint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		accessible bool
		want       string
	}{
		{name: "TTY appends the hint on its own line", accessible: false, want: "Pick something\n· esc cancel"},
		{name: "accessible returns the bare title", accessible: true, want: "Pick something"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := titleWithCancelHint("Pick something", testCase.accessible); got != testCase.want {
				t.Errorf("titleWithCancelHint(%q, %v) = %q, want %q", "Pick something", testCase.accessible, got, testCase.want)
			}
		})
	}
}

// TestFormCancelled pins formCancelled against every shape a form's Run
// error can take: unwrapped, wrapped, absent, and unrelated.
func TestFormCancelled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "ErrUserAborted directly", err: huh.ErrUserAborted, want: true},
		{name: "ErrUserAborted wrapped", err: fmt.Errorf("form: %w", huh.ErrUserAborted), want: true},
		{name: "unrelated error", err: errors.New("boom"), want: false},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := formCancelled(testCase.err); got != testCase.want {
				t.Errorf("formCancelled(%v) = %v, want %v", testCase.err, got, testCase.want)
			}
		})
	}
}

// formTestANSIPattern matches the CSI escape sequences lipgloss emits — ESC
// '[', numeric parameters, a letter terminator — so rendered-form
// assertions can match the visible text a user would actually read.
var formTestANSIPattern = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripANSI(s string) string {
	return formTestANSIPattern.ReplaceAllString(s, "")
}

// renderForm drives form the way bubbletea's own runtime would before its
// first paint: Init(), then the WindowSizeMsg its own RequestWindowSize
// command would eventually deliver (a form's group/field widths are zero,
// and so unrendered, until a width arrives). The returned View has its
// ANSI styling stripped so assertions see only the text a user would read.
func renderForm(form *huh.Form) string {
	form.Init()
	model, _ := form.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return stripANSI(model.View())
}

// TestFormHelpAdvertisesEscCancel is the mechanism-verification this
// package's cancel support depends on: huh renders a group's help footer
// from the FOCUSED FIELD's own KeyBinds(), never from the form-level
// keymap (confirmed by reading every field_*.go KeyBinds() implementation
// — Quit lives only on *Form, and no field can return it), so
// cancellableKeyMap's Quit binding never surfaces in the rendered help
// line on its own, no matter its help text. This test proves both halves
// of that finding, across every field type this package builds forms
// from: the native help line stays silent about esc without the title
// hint, and titleWithCancelHint actually makes "esc cancel" appear.
func TestFormHelpAdvertisesEscCancel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		build func(title string) *huh.Form
	}{
		{
			name: "Input",
			build: func(title string) *huh.Form {
				var value string
				return huh.NewForm(huh.NewGroup(
					huh.NewInput().Title(title).Value(&value),
				))
			},
		},
		{
			name: "Select",
			build: func(title string) *huh.Form {
				var value string
				return huh.NewForm(huh.NewGroup(
					huh.NewSelect[string]().Title(title).Options(huh.NewOption("a", "a"), huh.NewOption("b", "b")).Value(&value),
				))
			},
		},
		{
			name: "MultiSelect",
			build: func(title string) *huh.Form {
				var value []string
				return huh.NewForm(huh.NewGroup(
					huh.NewMultiSelect[string]().Title(title).Filterable(false).
						Options(huh.NewOption("a", "a"), huh.NewOption("b", "b")).Value(&value),
				))
			},
		},
		{
			name: "Confirm",
			build: func(title string) *huh.Form {
				var value bool
				return huh.NewForm(huh.NewGroup(
					huh.NewConfirm().Title(title).Value(&value),
				))
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			bareRendered := renderForm(testCase.build("Pick something").WithKeyMap(cancellableKeyMap()))
			if strings.Contains(bareRendered, "esc cancel") {
				t.Fatalf("%s: native huh help unexpectedly shows esc cancel without the title hint — "+
					"the title-suffix mechanism would no longer be necessary:\n%s", testCase.name, bareRendered)
			}

			hintedRendered := renderForm(testCase.build(titleWithCancelHint("Pick something", false)).WithKeyMap(cancellableKeyMap()))
			if !strings.Contains(hintedRendered, "esc cancel") {
				t.Fatalf("%s: rendered form does not visibly advertise esc cancel:\n%s", testCase.name, hintedRendered)
			}
		})
	}
}

// TestProductionFormsPinTheCancelHint drives every real production form
// constructor this package builds — the exact function shipped code calls,
// not a hand-built replica of its shape — and asserts the rendered form
// visibly advertises "esc cancel" in TTY mode (accessible: false, the mode
// every one of these forms actually runs in outside a screen reader).
// TestFormHelpAdvertisesEscCancel above proves the MECHANISM works in the
// abstract; this proves each of the nine call sites actually wires it in,
// so a future edit dropping WithKeyMap(cancellableKeyMap()) or a
// titleWithCancelHint call at any one site fails a test instead of just
// silently shipping a form with no visible way out.
func TestProductionFormsPinTheCancelHint(t *testing.T) {
	t.Parallel()
	candidates := []enrollCandidate{{label: "example/project"}}
	choices := []releaseChoice{{tag: "v1.0.0", label: "v1.0.0"}}

	tests := []struct {
		name  string
		build func() *huh.Form
	}{
		{name: "enroll.go pickEnrollUnitsInteractive (MultiSelect)", build: func() *huh.Form {
			var chosen []int
			return buildEnrollPickerForm(candidates, false, &chosen)
		}},
		{name: "enroll.go confirmProjectPathInteractive (Input)", build: func() *huh.Form {
			var path string
			return buildConfirmProjectPathForm(false, &path)
		}},
		{name: "enroll.go nameRemotelessFolderInteractive (Input)", build: func() *huh.Form {
			var name string
			return buildNameRemotelessFolderForm(false, &name)
		}},
		{name: "init.go resolveKeysetDecision source (Select)", build: func() *huh.Form {
			var choice string
			return buildKeysetSourceForm(false, &choice)
		}},
		{name: "init.go resolveKeysetDecision import (Input)", build: func() *huh.Form {
			var armored string
			return buildKeysetImportForm(false, &armored)
		}},
		{name: "init.go confirmKeysetStored (Confirm)", build: func() *huh.Form {
			var confirmed bool
			return buildKeysetStoredConfirmForm(false, &confirmed)
		}},
		{name: "track.go confirmPurgeInteractive (Input)", build: func() *huh.Form {
			var typed string
			return buildPurgeConfirmForm("example-folder", false, &typed)
		}},
		{name: "key.go confirmRotateInteractive (Confirm)", build: func() *huh.Form {
			var confirmed bool
			return buildRotateConfirmForm(false, &confirmed)
		}},
		{name: "update.go pickReleaseInteractive (Select)", build: func() *huh.Form {
			var tag string
			return buildReleasePickerForm(choices, &tag)
		}},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			rendered := renderForm(testCase.build())
			if !strings.Contains(rendered, "esc cancel") {
				t.Fatalf("%s: rendered form does not visibly advertise esc cancel:\n%s", testCase.name, rendered)
			}
		})
	}
}
