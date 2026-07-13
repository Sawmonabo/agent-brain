package memoryfs_test

import (
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
	"github.com/Sawmonabo/agent-brain/internal/provider"
)

// TestClassifyRepoPath pins the bare-path classification seam restore's
// derived-class gate uses for a DELETED memory: the same verdict List would
// give the live file, derived from the repo path alone. It shares
// testRegistry with the enumeration tests (claude: MEMORY.md → derived-index;
// codex: memories/MEMORY.md → regenerated), so a drift between how a live
// file and a bare path classify would surface here.
func TestClassifyRepoPath(t *testing.T) {
	t.Parallel()
	registry := testRegistry(t)

	tests := []struct {
		name      string
		repoPath  string
		wantClass provider.Class
		wantOK    bool
	}{
		{"plain fact memory", "claude/notes.md", provider.ClassFact, true},
		{"claude derived index", "claude/MEMORY.md", provider.ClassDerivedIndex, true},
		{"codex regenerated under repo subdir", "codex/memories/MEMORY.md", provider.ClassRegenerated, true},
		{"codex fact under repo subdir", "codex/memories/topic.md", provider.ClassFact, true},
		{"nested fact defaults to fact", "claude/notes/deep.md", provider.ClassFact, true},
		{"unregistered provider refuses", "gemini/notes.md", 0, false},
		{"no provider segment refuses", "loose.md", 0, false},
		{"empty path refuses", "", 0, false},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			gotClass, gotOK := memoryfs.ClassifyRepoPath(registry, testCase.repoPath)
			if gotOK != testCase.wantOK {
				t.Fatalf("ClassifyRepoPath(%q) ok = %v, want %v", testCase.repoPath, gotOK, testCase.wantOK)
			}
			if gotOK && gotClass != testCase.wantClass {
				t.Errorf("ClassifyRepoPath(%q) class = %v, want %v", testCase.repoPath, gotClass, testCase.wantClass)
			}
		})
	}
}

// TestClassifyRepoPathNilRegistry pins the defensive nil-registry guard: a
// model built without a provider registry (only ever a test, but the seam
// must not panic) refuses rather than dereferencing nil.
func TestClassifyRepoPathNilRegistry(t *testing.T) {
	t.Parallel()
	if _, ok := memoryfs.ClassifyRepoPath(nil, "claude/notes.md"); ok {
		t.Error("ClassifyRepoPath(nil, …) reported ok; a nil registry must refuse")
	}
}
