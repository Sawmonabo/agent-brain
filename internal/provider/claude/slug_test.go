package claude_test

import (
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/provider/claude"
)

func TestGuessPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		slug      string
		dirExists map[string]bool
		want      string
	}{
		{
			name: "naive reversal returned when it exists",
			slug: "-Users-u-dev-proj",
			dirExists: map[string]bool{
				"/Users/u/dev/proj": true,
			},
			want: "/Users/u/dev/proj",
		},
		{
			// The slug encoding is lossy: "-Users-u-dev-agent-brain" could
			// mean either /Users/u/dev/agent/brain or .../agent-brain. The
			// naive (all-slash) guess does not exist, so the walk falls
			// back segment by segment: /Users/u/dev/agent existing lets
			// the walk descend there, and "-brain" then extends that
			// committed component to the existing final directory. The
			// realistic variant of this fixture WITHOUT the
			// /Users/u/dev/agent decoy is covered by
			// TestGuessPathHyphenatedLeafWithoutDecoy.
			name: "dash-preserving reconstruction via the greedy filesystem-guided walk",
			slug: "-Users-u-dev-agent-brain",
			dirExists: map[string]bool{
				"/Users/u":                 true,
				"/Users/u/dev":             true,
				"/Users/u/dev/agent":       true,
				"/Users/u/dev/agent-brain": true,
			},
			want: "/Users/u/dev/agent-brain",
		},
		{
			name:      "naive reversal returned as last resort when nothing exists",
			slug:      "-Users-u-dev-agent-brain",
			dirExists: map[string]bool{},
			want:      "/Users/u/dev/agent/brain",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dirExists := func(path string) bool { return tt.dirExists[path] }
			if got := claude.GuessPath(tt.slug, dirExists); got != tt.want {
				t.Fatalf("GuessPath(%q) = %q, want %q", tt.slug, got, tt.want)
			}
		})
	}
}

// TestGuessPathHyphenatedLeafWithoutDecoy pins the realistic
// hyphenated-leaf case: only the true project directory and its real
// ancestors exist — e.g. this very repository checked out at
// ".../dev/agent-brain" with no sibling ".../dev/agent" directory. The
// greedy walk dead-ends after ".../dev" (no decoy to descend through),
// so GuessPath must recover the leaf by retrying the unresolved
// dash-run as ONE hyphenated component under the deepest verified
// boundary. GuessPath is reused verbatim by migrate (spec §10), so this
// reconstruction contract is shared.
func TestGuessPathHyphenatedLeafWithoutDecoy(t *testing.T) {
	t.Parallel()
	dirExists := map[string]bool{
		"/Users/u":                 true,
		"/Users/u/dev":             true,
		"/Users/u/dev/agent-brain": true,
		// deliberately absent: "/Users/u/dev/agent" — no decoy directory.
	}
	got := claude.GuessPath("-Users-u-dev-agent-brain", func(path string) bool { return dirExists[path] })
	if want := "/Users/u/dev/agent-brain"; got != want {
		t.Fatalf("GuessPath(%q) = %q, want %q", "-Users-u-dev-agent-brain", got, want)
	}
}
