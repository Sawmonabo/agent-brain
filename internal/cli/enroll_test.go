package cli

import (
	"errors"
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
