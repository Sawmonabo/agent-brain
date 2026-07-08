package provider_test

import (
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/provider"
)

// FuzzMatch pins two properties: Match never panics on arbitrary inputs,
// and it is deterministic (same inputs ⇒ same answer).
func FuzzMatch(f *testing.F) {
	f.Add("skills/**/SKILL.md", "skills/a/SKILL.md")
	f.Add("*.md", "MEMORY.md")
	f.Add("**", "")
	f.Add("a[", "a")
	f.Fuzz(func(t *testing.T, glob, rel string) {
		first := provider.Match(glob, rel)
		if second := provider.Match(glob, rel); first != second {
			t.Fatalf("Match(%q, %q) nondeterministic: %v then %v", glob, rel, first, second)
		}
	})
}
