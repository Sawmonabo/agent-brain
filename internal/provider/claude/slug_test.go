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
			// back segment by segment. It resolves to the hyphenated
			// directory here specifically because /Users/u/dev/agent ALSO
			// exists (dirExists is checked one slug segment at a time, so
			// the walk commits to "/agent" as its own directory before
			// ever considering "-brain"); with only the ancestors and the
			// final combined directory present (no /Users/u/dev/agent),
			// this same algorithm instead falls through to the naive
			// guess — see TestGuessPathRealisticHyphenatedLeafHasNoDecoy.
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

// TestGuessPathRealisticHyphenatedLeafHasNoDecoy pins a known limitation:
// the documented "greedy filesystem-guided walk" only recovers a
// hyphenated leaf directory when a shorter, non-hyphenated directory of
// the same prefix ALSO happens to exist (a decoy — see the case above).
// The realistic case — only the true, hyphenated project directory and
// its real ancestors exist, e.g. this very repository checked out at
// ".../dev/agent-brain" with no sibling ".../dev/agent" directory — does
// NOT reconstruct correctly: the walk commits to the wrong intermediate
// boundary and falls back to the (incorrect) naive guess. This is
// implemented exactly as specified; flagged in the task report rather
// than silently patched, since GuessPath is reused verbatim by migrate
// (spec §10) and the fix changes shared reconstruction semantics.
func TestGuessPathRealisticHyphenatedLeafHasNoDecoy(t *testing.T) {
	t.Parallel()
	dirExists := map[string]bool{
		"/Users/u":                 true,
		"/Users/u/dev":             true,
		"/Users/u/dev/agent-brain": true,
		// deliberately absent: "/Users/u/dev/agent" — no decoy directory.
	}
	got := claude.GuessPath("-Users-u-dev-agent-brain", func(path string) bool { return dirExists[path] })
	want := "/Users/u/dev/agent/brain" // known-wrong naive fallback, not .../agent-brain
	if got != want {
		t.Fatalf("GuessPath(%q) = %q, want %q (documenting the current limitation; "+
			"update this test if GuessPath's reconstruction is strengthened)", "-Users-u-dev-agent-brain", got, want)
	}
}
