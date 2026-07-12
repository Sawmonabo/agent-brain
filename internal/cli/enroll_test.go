package cli

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"charm.land/huh/v2"
	"github.com/google/go-cmp/cmp"
)

// TestEnrollPickerResultCancelRoutesToEmptySelection pins
// pickEnrollUnitsInteractive's cancel handling without driving a real huh
// form: a cancelled picker must enroll nothing, exactly the outcome an
// explicit empty selection already produces, so stepEnrollment and
// runTrackDiscover need no cancel-specific branch of their own. chosen
// carries stale, non-empty data on the cancel case deliberately — proving
// the cancel check wins over whatever MultiSelect's live selection state
// happened to be at the moment esc was pressed.
func TestEnrollPickerResultCancelRoutesToEmptySelection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		chosen     []int
		err        error
		wantChosen []int
		wantErr    error
	}{
		{
			name:       "cancelled with stale selections discards them",
			chosen:     []int{0, 2},
			err:        huh.ErrUserAborted,
			wantChosen: nil,
			wantErr:    nil,
		},
		{
			name:       "genuine selection passes through unchanged",
			chosen:     []int{1},
			err:        nil,
			wantChosen: []int{1},
			wantErr:    nil,
		},
		{
			name:       "non-cancel error propagates and discards any selection",
			chosen:     []int{0},
			err:        errors.New("boom"),
			wantChosen: nil,
			wantErr:    errors.New("boom"),
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			gotChosen, gotErr := enrollPickerResult(testCase.chosen, testCase.err)
			if diff := cmp.Diff(testCase.wantChosen, gotChosen); diff != "" {
				t.Errorf("chosen (-want +got):\n%s", diff)
			}
			if (gotErr == nil) != (testCase.wantErr == nil) || (gotErr != nil && gotErr.Error() != testCase.wantErr.Error()) {
				t.Errorf("err = %v, want %v", gotErr, testCase.wantErr)
			}
		})
	}
}

// TestEnrollPickerFormShowsAllCandidates pins buildEnrollPickerForm's
// explicit Height against huh v2.0.3's MultiSelect viewport bug (see the
// Height call's comment in enroll.go for the mechanism): every candidate's
// option row must survive rendering, not just however many happen to fit
// after the title eats its share of an unset, options-only-measured
// viewport. n=2 and n=3 are the exact sizes that collapsed to 0 and 1
// visible rows respectively with this field's two-line cancel-hint title
// before the fix; n=8 pins the general shape well past the collapse point.
// Every case also asserts the cancel hint still renders, so restoring the
// option rows cannot come at the cost of the advertised esc-cancel hint.
func TestEnrollPickerFormShowsAllCandidates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		n    int
	}{
		{name: "n=2 collapsed to 0 visible options before the fix", n: 2},
		{name: "n=3 collapsed to 1 visible option before the fix", n: 3},
		{name: "n=8 pins the general shape past the collapse point", n: 8},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			candidates := make([]enrollCandidate, testCase.n)
			for i := range candidates {
				candidates[i] = enrollCandidate{label: fmt.Sprintf("cand-%02d  -> ~/dev/cand-%02d", i, i)}
			}

			var chosen []int
			rendered := renderForm(buildEnrollPickerForm(candidates, false, &chosen))

			for _, candidate := range candidates {
				if !strings.Contains(rendered, candidate.label) {
					t.Errorf("rendered form missing candidate label %q:\n%s", candidate.label, rendered)
				}
			}
			if !strings.Contains(rendered, "· esc cancel") {
				t.Errorf("rendered form does not visibly advertise the cancel hint:\n%s", rendered)
			}
		})
	}
}
