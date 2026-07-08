package claude_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/provider/claude"
)

// TestReconcileIndexRebuild exercises the three title/hook extraction
// tiers (frontmatter name/description, first heading, bare filename) and
// pins the exact rendered bytes.
//
// Entries are sorted by filename ascending ("bare.md" < "heading-only.md"
// < "with-frontmatter.md"), per the brief's explicit, twice-stated
// contract ("collect non-MEMORY.md *.md files sorted by name"; the
// determinism test below repeats "sorted by filename regardless of
// creation order"). That is the order asserted here.
func TestReconcileIndexRebuild(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "with-frontmatter.md", "---\nname: alpha-rule\ndescription: the hook text\n---\nbody")
	writeFile(t, dir, "heading-only.md", "# Heading Title\nbody")
	writeFile(t, dir, "bare.md", "just prose")

	if err := claude.New(t.TempDir()).ReconcileIndex(context.Background(), dir); err != nil {
		t.Fatalf("ReconcileIndex() error = %v", err)
	}

	want := "# Memory index\n\n" +
		"- [bare](bare.md)\n" +
		"- [Heading Title](heading-only.md)\n" +
		"- [alpha-rule](with-frontmatter.md) — the hook text\n"
	got := readMemoryIndex(t, dir)
	if got != want {
		t.Fatalf("MEMORY.md =\n%q\nwant\n%q", got, want)
	}
}

// TestReconcileIndexDeterministic proves file order depends only on
// filename, never on creation order (files are written zzz, aaa, mmm —
// deliberately not alphabetical), and that a second run is byte-identical
// AND skips the write entirely (mtime unchanged — "no mtime churn" per
// the ReconcileIndex doc comment, so mirror-out never sees a spurious
// change to re-sync).
func TestReconcileIndexDeterministic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "zzz-last.md", "prose")
	writeFile(t, dir, "aaa-first.md", "prose")
	writeFile(t, dir, "mmm-middle.md", "prose")

	adapter := claude.New(t.TempDir())
	ctx := context.Background()
	if err := adapter.ReconcileIndex(ctx, dir); err != nil {
		t.Fatalf("ReconcileIndex() run 1 error = %v", err)
	}
	first := readMemoryIndex(t, dir)
	want := "# Memory index\n\n" +
		"- [aaa-first](aaa-first.md)\n" +
		"- [mmm-middle](mmm-middle.md)\n" +
		"- [zzz-last](zzz-last.md)\n"
	if first != want {
		t.Fatalf("MEMORY.md (run 1) =\n%q\nwant\n%q", first, want)
	}

	infoBefore, err := os.Stat(filepath.Join(dir, "MEMORY.md"))
	if err != nil {
		t.Fatal(err)
	}
	// Guarantee any real rewrite would produce an observably later mtime,
	// regardless of filesystem timestamp granularity.
	time.Sleep(10 * time.Millisecond)

	if err := adapter.ReconcileIndex(ctx, dir); err != nil {
		t.Fatalf("ReconcileIndex() run 2 error = %v", err)
	}
	second := readMemoryIndex(t, dir)
	if second != first {
		t.Fatalf("MEMORY.md (run 2) =\n%q\nwant byte-identical to run 1:\n%q", second, first)
	}
	infoAfter, err := os.Stat(filepath.Join(dir, "MEMORY.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !infoAfter.ModTime().Equal(infoBefore.ModTime()) {
		t.Fatalf("MEMORY.md mtime changed on an identical second reconcile: before %v, after %v — "+
			"a rewrite happened when the brief's contract requires skipping it", infoBefore.ModTime(), infoAfter.ModTime())
	}
}

func TestReconcileIndexEdges(t *testing.T) {
	t.Parallel()

	t.Run("empty dir with no MEMORY.md is a no-op", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := claude.New(t.TempDir()).ReconcileIndex(context.Background(), dir); err != nil {
			t.Fatalf("ReconcileIndex() error = %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "MEMORY.md")); !os.IsNotExist(err) {
			t.Fatalf("MEMORY.md stat error = %v, want IsNotExist (never created)", err)
		}
	})

	t.Run("existing MEMORY.md with zero topic files is rewritten header-only", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFile(t, dir, "MEMORY.md", "# Memory index\n\nstale hand-written notes\n")

		if err := claude.New(t.TempDir()).ReconcileIndex(context.Background(), dir); err != nil {
			t.Fatalf("ReconcileIndex() error = %v", err)
		}
		want := "# Memory index\n\n"
		if got := readMemoryIndex(t, dir); got != want {
			t.Fatalf("MEMORY.md = %q, want %q (header only)", got, want)
		}
	})

	t.Run("MEMORY.md itself and non-.md files are never indexed", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFile(t, dir, "MEMORY.md", "stale")
		writeFile(t, dir, "topic1.md", "hello")
		writeFile(t, dir, "notes.txt", "ignore me")

		if err := claude.New(t.TempDir()).ReconcileIndex(context.Background(), dir); err != nil {
			t.Fatalf("ReconcileIndex() error = %v", err)
		}
		want := "# Memory index\n\n- [topic1](topic1.md)\n"
		if got := readMemoryIndex(t, dir); got != want {
			t.Fatalf("MEMORY.md = %q, want %q", got, want)
		}
	})
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readMemoryIndex(t *testing.T, dir string) string {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}
