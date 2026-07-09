package repo

import "testing"

// TestIsGitMetaPath pins the guard's exact-segment semantics. The negatives
// matter as much as the positives: over-matching would silently stop
// legitimate files syncing, which is its own spec violation. .gitmodules and
// .github are deliberate non-matches — git reads .gitmodules only at the
// repository root, which unit-relative paths can never name, and .github pins
// that matching is whole-segment, not prefix.
func TestIsGitMetaPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		rel  string
		want bool
	}{
		{".gitattributes", true},
		{".gitignore", true},
		{".git", true},
		{".GITATTRIBUTES", true},
		{".GitIgnore", true},
		{"sub/.gitattributes", true},
		{"caps/.GITIGNORE", true},
		{".git/config", true},
		{"a/b/.git", true},
		{"", false},
		{"gitattributes", false},
		{"gitignore", false},
		{".gitattributes.bak", false},
		{"foo.gitignore", false},
		{".gitmodules", false},
		{".github", false},
		{".github/notes.md", false},
		{"memories/real.md", false},
	}
	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			t.Parallel()
			if got := IsGitMetaPath(tt.rel); got != tt.want {
				t.Fatalf("IsGitMetaPath(%q) = %v, want %v", tt.rel, got, tt.want)
			}
		})
	}
}
