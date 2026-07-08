package provider_test

import (
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/provider"
)

func TestMatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		glob, rel string
		want      bool
	}{
		// Single-segment wildcards stay within a segment.
		{"*.md", "MEMORY.md", true},
		{"*.md", "notes/deep.md", false},
		{"rollout_summaries/*", "rollout_summaries/a.md", true},
		{"rollout_summaries/*", "rollout_summaries/sub/a.md", false},
		// '**' as a full segment spans zero or more segments (git semantics).
		{"skills/**/SKILL.md", "skills/git/SKILL.md", true},
		{"skills/**/SKILL.md", "skills/a/b/SKILL.md", true},
		{"skills/**/SKILL.md", "skills/SKILL.md", true},
		{"**", "anything/at/all.md", true},
		{".lock/**", ".lock/pid", true},
		{".lock/**", ".lock", false}, // '**' needs the prefix segment to exist as a dir path
		// Literals.
		{"MEMORY.md", "MEMORY.md", true},
		{"MEMORY.md", "memory.md", false},
	}
	for _, tt := range tests {
		if got := provider.Match(tt.glob, tt.rel); got != tt.want {
			t.Fatalf("Match(%q, %q) = %v, want %v", tt.glob, tt.rel, got, tt.want)
		}
	}
}

func TestValidateGlob(t *testing.T) {
	t.Parallel()
	valid := []string{"skills/**/SKILL.md", "*.md", "rollout_summaries/*", "MEMORY.md"}
	for _, glob := range valid {
		if err := provider.ValidateGlob(glob); err != nil {
			t.Fatalf("valid glob %q rejected: %v", glob, err)
		}
	}
	// Rejections fail fast at construction, not silently at match time.
	// Whitespace / '#' / leading '!' / empty would corrupt or invert the
	// generated .gitattributes lines this glob eventually becomes
	// (Task 2's repo.GenerateAttributes) — the same table drives both.
	// Empty segments (leading/trailing/doubled '/') would do the same.
	invalid := []string{"bad[range.md", "has space.md", "has\ttab.md", "#comment.md", "!negated.md", "", "a/", "/a", "a//b"}
	for _, glob := range invalid {
		if err := provider.ValidateGlob(glob); err == nil {
			t.Fatalf("ValidateGlob(%q) = nil, want error", glob)
		}
	}
}
