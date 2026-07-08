package crypto

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestRewriteRetainBoth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		input         string
		wantConflicts bool
		want          string
	}{
		{
			name:          "clean merge untouched",
			input:         "line 1\nline 2\n",
			wantConflicts: false,
			want:          "line 1\nline 2\n",
		},
		{
			name: "single hunk becomes retain-both block",
			input: "intro\n" +
				"<<<<<<< version A\n" +
				"fact from machine A\n" +
				"=======\n" +
				"fact from machine B\n" +
				">>>>>>> version B\n" +
				"outro\n",
			wantConflicts: true,
			want: "intro\n" +
				"<!-- agent-brain conflict 2026-07-07T00:00:00Z: both versions retained — keep what is right, then delete these comment lines (spec §4) -->\n" +
				"<!-- agent-brain version: version A -->\n" +
				"fact from machine A\n" +
				"<!-- agent-brain version: version B -->\n" +
				"fact from machine B\n" +
				"<!-- agent-brain conflict end -->\n" +
				"outro\n",
		},
		{
			name: "two hunks both rewritten",
			input: "<<<<<<< version A\na1\n=======\nb1\n>>>>>>> version B\n" +
				"mid\n" +
				"<<<<<<< version A\na2\n=======\nb2\n>>>>>>> version B\n",
			wantConflicts: true,
			want: "<!-- agent-brain conflict 2026-07-07T00:00:00Z: both versions retained — keep what is right, then delete these comment lines (spec §4) -->\n" +
				"<!-- agent-brain version: version A -->\na1\n" +
				"<!-- agent-brain version: version B -->\nb1\n" +
				"<!-- agent-brain conflict end -->\n" +
				"mid\n" +
				"<!-- agent-brain conflict 2026-07-07T00:00:00Z: both versions retained — keep what is right, then delete these comment lines (spec §4) -->\n" +
				"<!-- agent-brain version: version A -->\na2\n" +
				"<!-- agent-brain version: version B -->\nb2\n" +
				"<!-- agent-brain conflict end -->\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, hadConflicts := RewriteRetainBoth([]byte(test.input), "version A", "version B", "2026-07-07T00:00:00Z")
			if hadConflicts != test.wantConflicts {
				t.Fatalf("hadConflicts = %v, want %v", hadConflicts, test.wantConflicts)
			}
			if diff := cmp.Diff(test.want, string(got)); diff != "" {
				t.Fatalf("output mismatch (-want +got):\n%s", diff)
			}
			if strings.Contains(string(got), "<<<<<<<") {
				t.Fatal("git conflict markers leaked into output")
			}
		})
	}
}
