package claude_test

import (
	"strings"
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

func TestSlugFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "simple path", path: "/Users/u/dev/proj", want: "-Users-u-dev-proj"},
		{name: "hyphenated leaf", path: "/Users/u/dev/agent-brain", want: "-Users-u-dev-agent-brain"},
		{name: "root", path: "/", want: "-"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := claude.SlugFor(tt.path); got != tt.want {
				t.Fatalf("SlugFor(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

// TestSlugForAndGuessPathRoundTripWhenDirectoryExists pins the relationship
// track's path-argument resolution depends on: SlugFor is the exact
// forward encoding Claude Code itself applies, so feeding it back through
// GuessPath — with every real ancestor of the project directory reported
// as existing, exactly what os.Stat sees on an actual filesystem — must
// recover the original path, even for a hyphenated leaf like
// "agent-brain" where the naive all-slash branch alone would not.
func TestSlugForAndGuessPathRoundTripWhenDirectoryExists(t *testing.T) {
	t.Parallel()
	path := "/Users/u/dev/agent-brain"
	slug := claude.SlugFor(path)
	dirExists := func(p string) bool { return p == path || strings.HasPrefix(path, p+"/") }
	if got := claude.GuessPath(slug, dirExists); got != path {
		t.Fatalf("GuessPath(SlugFor(%q)) = %q, want %q", path, got, path)
	}
}
