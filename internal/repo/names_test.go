package repo_test

import (
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func TestValidateFolderName(t *testing.T) {
	t.Parallel()
	valid := []string{"agent-brain", "ai-sidekicks", "Repo.Name", "a", "x_y", "proj-2"}
	for _, name := range valid {
		if err := repo.ValidateFolderName(name); err != nil {
			t.Fatalf("valid folder %q rejected: %v", name, err)
		}
	}
	invalid := []string{
		"",                       // empty
		".",                      // path special
		"..",                     // traversal
		".hidden",                // leading dot collides with meta/VCS space
		"_global",                // reserved for global-scope pools
		"_anything",              // whole '_' prefix reserved
		".agent-brain",           // reserved meta dir
		".git",                   // VCS
		"a/b",                    // separator
		`a\b`,                    // separator (windows-style)
		"has space",              // breaks .gitattributes and CLI ergonomics
		"a\x00b",                 // control byte
		strings.Repeat("x", 101), // length cap
	}
	for _, name := range invalid {
		if err := repo.ValidateFolderName(name); err == nil {
			t.Fatalf("ValidateFolderName(%q) = nil, want error", name)
		}
	}
}

func TestSanitizeHostname(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"Sawmons-MacBook-Pro", "Sawmons-MacBook-Pro"},
		{"host.local", "host.local"},
		{"weird host\n", "weird-host-"},
		{"a/b:c", "a-b-c"},
		{"", "unknown-host"},
		{strings.Repeat("h", 200), strings.Repeat("h", 100)},
	}
	for _, tt := range tests {
		if got := repo.SanitizeHostname(tt.in); got != tt.want {
			t.Fatalf("SanitizeHostname(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
