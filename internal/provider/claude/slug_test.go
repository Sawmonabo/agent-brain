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
			// search tries the '/' reading first and descends through the
			// /Users/u/dev/agent decoy, but the full reading fails its
			// final probe, so it backtracks to the '-' reading and finds
			// the real directory. The realistic variant WITHOUT the decoy
			// is covered by TestGuessPathHyphenatedLeafWithoutDecoy.
			// Fixture maps model what os.Stat reports, so every real
			// ancestor is present — the search prunes on missing ones.
			name: "hyphenated leaf recovered past a decoy sibling",
			slug: "-Users-u-dev-agent-brain",
			dirExists: map[string]bool{
				"/Users":                   true,
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
		{
			// A dot-prefixed component: the "--" run encodes "/." and the
			// search must recover it when the directory really exists.
			name: "dot-prefixed component recovered",
			slug: "-Users-u-dev-ai-sidekicks--worktrees-audit",
			dirExists: map[string]bool{
				"/Users":                               true,
				"/Users/u":                             true,
				"/Users/u/dev":                         true,
				"/Users/u/dev/ai-sidekicks":            true,
				"/Users/u/dev/ai-sidekicks/.worktrees": true,
				"/Users/u/dev/ai-sidekicks/.worktrees/audit": true,
			},
			want: "/Users/u/dev/ai-sidekicks/.worktrees/audit",
		},
		{
			// Dot AND underscore inside one leaf component — the shape the
			// 2026-07-09 real-machine probe used. Only the true directory
			// (and its real ancestors) exist.
			name: "dotted underscored leaf recovered",
			slug: "-Users-u-dev-smoke-test-proj",
			dirExists: map[string]bool{
				"/Users":                       true,
				"/Users/u":                     true,
				"/Users/u/dev":                 true,
				"/Users/u/dev/smoke.test_proj": true,
			},
			want: "/Users/u/dev/smoke.test_proj",
		},
		{
			// Preference order: '/' beats '-' beats '.' — when the
			// hyphenated leaf genuinely exists it wins over a dotted
			// reading that would also exist.
			name: "hyphen preferred over dot on ties",
			slug: "-srv-a-b",
			dirExists: map[string]bool{
				"/srv":     true,
				"/srv/a-b": true,
				"/srv/a.b": true,
			},
			want: "/srv/a-b",
		},
		{
			// A space in a component. (Unicode dashes are unrecoverable by
			// construction — no reading maps '-' back to 'ö' — so the
			// fixture stays ASCII; SlugFor's table covers the unicode
			// forward direction.)
			name: "space in component recovered",
			slug: "-tmp-my-proj",
			dirExists: map[string]bool{
				"/tmp":         true,
				"/tmp/my proj": true,
			},
			want: "/tmp/my proj",
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
// '/' reading of the final dash prunes immediately (no decoy to descend
// through), so the search recovers the leaf via the '-' reading under
// the deepest verified boundary. GuessPath is reused verbatim by
// migrate (spec §10), so this reconstruction contract is shared.
func TestGuessPathHyphenatedLeafWithoutDecoy(t *testing.T) {
	t.Parallel()
	dirExists := map[string]bool{
		"/Users":                   true,
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

// TestSlugFor pins the encoding against ground truth captured from real
// Claude Code v2.1.205 (2026-07-09): one-shot sessions were run in probe
// directories and the slug directories it created were read back. The
// observed rule is the JavaScript replace /[^a-zA-Z0-9]/g over the
// absolute path — and JS regexes see UTF-16 code units, so a BMP rune
// ('.', '_', ' ', 'ö') becomes ONE dash while an astral rune (🚀, a
// surrogate pair) becomes TWO.
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
		{
			// Machine-verified 2026-07-09: real Claude Code created exactly
			// this slug for this path (dot AND underscore both → '-').
			name: "dot and underscore ground truth",
			path: "/Users/sawmonabo/dev/smoke.test_proj",
			want: "-Users-sawmonabo-dev-smoke-test-proj",
		},
		{
			// Machine-verified 2026-07-09: 'ö' (BMP) → one dash; ' ' → one
			// dash; '🚀' (astral, surrogate pair in UTF-16) → TWO dashes;
			// '.' → one dash.
			name: "space, BMP unicode, astral emoji ground truth",
			path: "/Users/sawmonabo/dev/pröbe 🚀x.zz",
			want: "-Users-sawmonabo-dev-pr-be---x-zz",
		},
		{
			// Machine-verified 2026-07-09 (pre-existing slug on the probe
			// machine): a dot-prefixed component doubles the dash — one for
			// the '/' and one for the '.'.
			name: "dot-prefixed component (.worktrees)",
			path: "/Users/u/dev/ai-sidekicks/.worktrees/plan-execution-audit",
			want: "-Users-u-dev-ai-sidekicks--worktrees-plan-execution-audit",
		},
		{name: "BMP accent is one dash", path: "/tmp/café", want: "-tmp-caf-"},
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
// recover the original path. The roundtrip holds exactly when every
// non-alphanumeric character in the path is one of GuessPath's five
// readings ('/', '-', '.', '_', ' '); unicode folds to dashes that no
// reverse reading can recover, which is why Discover prefers the session
// files' recorded cwd over any reconstruction.
func TestSlugForAndGuessPathRoundTripWhenDirectoryExists(t *testing.T) {
	t.Parallel()
	paths := []string{
		"/Users/u/dev/agent-brain",
		"/Users/u/dev/smoke.test_proj",
		"/Users/u/dev/ai-sidekicks/.worktrees/plan-execution-audit",
		"/tmp/my proj",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			slug := claude.SlugFor(path)
			dirExists := func(p string) bool { return p == path || strings.HasPrefix(path, p+"/") }
			if got := claude.GuessPath(slug, dirExists); got != path {
				t.Fatalf("GuessPath(SlugFor(%q)) = %q, want %q", path, got, path)
			}
		})
	}
}
