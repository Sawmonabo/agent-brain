package memoryfs_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
)

// TestMeta covers the fence-detection and key-extraction rules: presence,
// absence, an unclosed block, quoted values, and CRLF line endings — the
// same tolerance internal/provider/claude/reconcile.go's own frontmatter
// reader applies, mirrored here rather than imported (it is unexported and
// adapter-internal).
func TestMeta(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		content         string
		wantName        string
		wantDescription string
		wantHasFront    bool
	}{
		{
			name:            "fences present with name and description",
			content:         "---\nname: Topic\ndescription: A hook\n---\nbody\n",
			wantName:        "Topic",
			wantDescription: "A hook",
			wantHasFront:    true,
		},
		{
			name:         "fences absent",
			content:      "# Just a heading\n\nbody\n",
			wantHasFront: false,
		},
		{
			name:         "empty file",
			content:      "",
			wantHasFront: false,
		},
		{
			name:         "unclosed fence renders as no-frontmatter",
			content:      "---\nname: Topic\ndescription: A hook\n",
			wantHasFront: false,
		},
		{
			name:            "quoted values are trimmed",
			content:         "---\nname: \"Quoted Name\"\ndescription: \"Quoted description\"\n---\nbody\n",
			wantName:        "Quoted Name",
			wantDescription: "Quoted description",
			wantHasFront:    true,
		},
		{
			name:            "crlf line endings tolerated",
			content:         "---\r\nname: CRLF Name\r\ndescription: CRLF hook\r\n---\r\nbody\r\n",
			wantName:        "CRLF Name",
			wantDescription: "CRLF hook",
			wantHasFront:    true,
		},
		{
			name:            "unrecognized keys ignored, nested metadata skipped",
			content:         "---\nname: Topic\nmetadata:\n  type: fact\ndescription: A hook\n---\nbody\n",
			wantName:        "Topic",
			wantDescription: "A hook",
			wantHasFront:    true,
		},
		{
			name:         "empty frontmatter block",
			content:      "---\n---\nbody\n",
			wantHasFront: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "memory.md")
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			name, description, hasFrontmatter := memoryfs.Meta(path)
			if name != tt.wantName || description != tt.wantDescription || hasFrontmatter != tt.wantHasFront {
				t.Errorf("Meta() = (%q, %q, %v), want (%q, %q, %v)",
					name, description, hasFrontmatter, tt.wantName, tt.wantDescription, tt.wantHasFront)
			}
		})
	}
}

// TestMetaMissingFileIsGraceful pins the "never an error" contract: Meta has
// no error return, so an unreadable path (never happens for List's own
// walk-derived callers, but a caller could still hand it a stale path)
// degrades to no-frontmatter rather than panicking.
func TestMetaMissingFileIsGraceful(t *testing.T) {
	t.Parallel()
	name, description, hasFrontmatter := memoryfs.Meta(filepath.Join(t.TempDir(), "missing.md"))
	if name != "" || description != "" || hasFrontmatter {
		t.Fatalf("Meta(missing) = (%q, %q, %v), want (\"\", \"\", false)", name, description, hasFrontmatter)
	}
}

// TestMetaRespectsFourKiBReadCap constructs content straddling the
// documented 4 KiB read cap: a closing fence comfortably inside the cap
// parses normally, while one placed well beyond it reads as absent — Meta
// must never read past its documented window to find a fence, which makes
// a merely-truncated read indistinguishable from a genuinely unclosed one
// (both degrade the same way, per TestMeta's "unclosed" row).
func TestMetaRespectsFourKiBReadCap(t *testing.T) {
	t.Parallel()
	const readCap = 4096
	header := "---\nname: Boundary\n"
	closer := "---\nbody\n"
	build := func(fillerLen int) string {
		return header + strings.Repeat("x", fillerLen) + "\n" + closer
	}

	t.Run("closing fence within cap parses", func(t *testing.T) {
		t.Parallel()
		content := build(readCap - len(header) - len(closer) - 200)
		path := filepath.Join(t.TempDir(), "memory.md")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		name, _, hasFrontmatter := memoryfs.Meta(path)
		if !hasFrontmatter || name != "Boundary" {
			t.Fatalf("Meta() = (%q, hasFrontmatter=%v), want (%q, true)", name, hasFrontmatter, "Boundary")
		}
	})

	t.Run("closing fence beyond cap reads as absent", func(t *testing.T) {
		t.Parallel()
		content := build(readCap + 1000)
		path := filepath.Join(t.TempDir(), "memory.md")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		name, description, hasFrontmatter := memoryfs.Meta(path)
		if hasFrontmatter || name != "" || description != "" {
			t.Fatalf("Meta() = (%q, %q, hasFrontmatter=%v), want (\"\", \"\", false)", name, description, hasFrontmatter)
		}
	})
}
