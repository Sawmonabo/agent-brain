package dashboard

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestParseConflictLog(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		want    []ConflictRecord
		wantErr bool
	}{
		{
			name:  "empty input yields no records",
			input: "",
			want:  nil,
		},
		{
			name: "records return newest-first with blanks skipped",
			input: `{"time":"t1","path":"a.md","mode":"retain-both"}

{"time":"t2","path":"b.md","mode":"retain-both"}
`,
			want: []ConflictRecord{
				{Time: "t2", Path: "b.md", Mode: "retain-both"},
				{Time: "t1", Path: "a.md", Mode: "retain-both"},
			},
		},
		{
			name:    "malformed line errors",
			input:   `{"time":"t1"` + "\n" + `not json`,
			wantErr: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseConflictLog([]byte(testCase.input))
			if testCase.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got records %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("records mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
