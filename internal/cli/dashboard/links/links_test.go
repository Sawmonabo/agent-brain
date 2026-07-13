package links_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/links"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
)

// TestParse exercises every rule governing a [[target]] span from the
// package doc (no newline inside; empty target ignored; unterminated
// "[[x" ignored; pipe alias), plus multiple/adjacent links, unicode
// targets, and literal (non-nested) brackets inside a target. Every case
// also checks that each returned span, sliced out of body, still begins
// and ends with the bracket pair — the "byte span INCLUDING the brackets"
// contract Link documents.
func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want []links.Link
	}{
		{
			name: "no links in plain text",
			body: "just some prose, no brackets here",
			want: nil,
		},
		{
			name: "single link",
			body: "see [[Target Memory]] for more",
			want: []links.Link{
				{Target: "Target Memory", Start: len("see "), End: len("see [[Target Memory]]")},
			},
		},
		{
			name: "surrounding whitespace inside brackets is trimmed",
			body: "[[  spaced  ]]",
			want: []links.Link{{Target: "spaced", Start: 0, End: len("[[  spaced  ]]")}},
		},
		{
			name: "multiple links in one body",
			body: "See [[a]] and [[b]].",
			want: []links.Link{
				{Target: "a", Start: len("See "), End: len("See [[a]]")},
				{Target: "b", Start: len("See [[a]] and "), End: len("See [[a]] and [[b]]")},
			},
		},
		{
			name: "adjacent links with no separator",
			body: "[[a]][[b]]",
			want: []links.Link{
				{Target: "a", Start: 0, End: len("[[a]]")},
				{Target: "b", Start: len("[[a]]"), End: len("[[a]][[b]]")},
			},
		},
		{
			name: "pipe alias uses text before the pipe as target",
			body: "[[real-target|Display Alias]]",
			want: []links.Link{{Target: "real-target", Start: 0, End: len("[[real-target|Display Alias]]")}},
		},
		{
			name: "pipe alias trims whitespace around the target",
			body: "[[ real | Display ]]",
			want: []links.Link{{Target: "real", Start: 0, End: len("[[ real | Display ]]")}},
		},
		{
			name: "empty target is ignored",
			body: "before [[]] after",
			want: nil,
		},
		{
			name: "whitespace-only target is ignored",
			body: "before [[   ]] after",
			want: nil,
		},
		{
			name: "empty target before a pipe is ignored",
			body: "[[|alias]]",
			want: nil,
		},
		{
			name: "newline inside brackets invalidates the span",
			body: "[[a\nb]]",
			want: nil,
		},
		{
			name: "newline-invalidated span does not hide a later valid link",
			body: "[[a\nb]] then [[real]]",
			want: []links.Link{
				{Target: "real", Start: len("[[a\nb]] then "), End: len("[[a\nb]] then [[real]]")},
			},
		},
		{
			name: "unterminated opener is ignored",
			body: "text [[unterminated with no close",
			want: nil,
		},
		{
			name: "bare opener at end of body is ignored",
			body: "[[",
			want: nil,
		},
		{
			name: "single closing bracket does not close a span",
			body: "[[a]",
			want: nil,
		},
		{
			name: "unicode target",
			body: "[[café]]",
			want: []links.Link{{Target: "café", Start: 0, End: len("[[café]]")}},
		},
		{
			name: "unicode target surrounded by prose",
			body: "prefix [[日本語ノート]] suffix",
			want: []links.Link{
				{Target: "日本語ノート", Start: len("prefix "), End: len("prefix [[日本語ノート]]")},
			},
		},
		{
			name: "brackets inside a target are literal, spans do not nest",
			body: "[[a[[b]]c]]",
			want: []links.Link{{Target: "a[[b", Start: 0, End: len("[[a[[b]]")}},
		},
		{
			name: "duplicate target occurring twice yields two spans",
			body: "[[dup]] middle [[dup]]",
			want: []links.Link{
				{Target: "dup", Start: 0, End: len("[[dup]]")},
				{Target: "dup", Start: len("[[dup]] middle "), End: len("[[dup]] middle [[dup]]")},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := links.Parse(tt.body)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Parse(%q) diff (-want +got):\n%s", tt.body, diff)
			}
			for _, l := range got {
				span := tt.body[l.Start:l.End]
				if !strings.HasPrefix(span, "[[") || !strings.HasSuffix(span, "]]") {
					t.Errorf("Parse(%q): span %q at [%d:%d) missing brackets", tt.body, span, l.Start, l.End)
				}
			}
		})
	}
}

// memory builds a minimal memoryfs.Memory for index fixtures: RepoPath is
// the identity fixtures assert against, RelPath supplies the filename stem
// Resolve tries first, and name carries the frontmatter (or stem-fallback)
// display Name Resolve falls back to.
func memory(repoPath, relPath, name string) memoryfs.Memory {
	return memoryfs.Memory{RepoPath: repoPath, RelPath: relPath, Name: name}
}

// TestIndexResolveBacklinksDangling is the brief's three-fixture scenario:
// A links B (by filename stem) and C (by frontmatter Name, which diverges
// from C's own stem), B links a nonexistent "ghost".
func TestIndexResolveBacklinksDangling(t *testing.T) {
	t.Parallel()

	memA := memory("claude/a.md", "a.md", "a")
	memB := memory("claude/b.md", "b.md", "b")
	// memC's frontmatter Name "C" diverges from its filename stem
	// "c-file" — this is what exercises the frontmatter-Name fallback
	// rather than the (usual) stem match.
	memC := memory("claude/c-file.md", "c-file.md", "C")

	bodies := map[string]string{
		memA.RepoPath: "Links to [[b]] and [[C]].",
		memB.RepoPath: "See [[ghost]].",
		memC.RepoPath: "no outbound links here",
	}
	readBody := func(m memoryfs.Memory) (string, error) { return bodies[m.RepoPath], nil }

	ix := links.BuildIndex([]memoryfs.Memory{memA, memB, memC}, readBody)

	t.Run("Resolve", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			target string
			want   memoryfs.Memory
			wantOK bool
		}{
			{target: "b", want: memB, wantOK: true},
			{target: "C", want: memC, wantOK: true},
			{target: "ghost", want: memoryfs.Memory{}, wantOK: false},
		}
		for _, tt := range tests {
			got, ok := ix.Resolve(tt.target)
			if ok != tt.wantOK {
				t.Errorf("Resolve(%q) ok = %v, want %v", tt.target, ok, tt.wantOK)
				continue
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Resolve(%q) diff (-want +got):\n%s", tt.target, diff)
			}
		}
	})

	t.Run("Backlinks", func(t *testing.T) {
		t.Parallel()
		if diff := cmp.Diff([]memoryfs.Memory{memA}, ix.Backlinks(memB)); diff != "" {
			t.Errorf("Backlinks(B) diff (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]memoryfs.Memory{memA}, ix.Backlinks(memC)); diff != "" {
			t.Errorf("Backlinks(C) diff (-want +got):\n%s", diff)
		}
		if got := ix.Backlinks(memA); got != nil {
			t.Errorf("Backlinks(A) = %+v, want nil (nobody links to A)", got)
		}
	})

	t.Run("Dangling", func(t *testing.T) {
		t.Parallel()
		want := []links.Link{
			{Target: "ghost", Start: len("See "), End: len("See [[ghost]]")},
		}
		if diff := cmp.Diff(want, ix.Dangling(memB)); diff != "" {
			t.Errorf("Dangling(B) diff (-want +got):\n%s", diff)
		}
		if got := ix.Dangling(memA); got != nil {
			t.Errorf("Dangling(A) = %+v, want nil (both of A's links resolve)", got)
		}
	})
}

// TestIndexResolutionIsCaseInsensitive pins that the filename-stem lookup
// folds case, per the brief's resolution rule.
func TestIndexResolutionIsCaseInsensitive(t *testing.T) {
	t.Parallel()
	memA := memory("claude/a.md", "MixedCase.md", "a")
	noBody := func(memoryfs.Memory) (string, error) { return "", nil }
	ix := links.BuildIndex([]memoryfs.Memory{memA}, noBody)

	for _, target := range []string{"mixedcase", "MIXEDCASE", "MixedCase", "mIxEdCaSe"} {
		got, ok := ix.Resolve(target)
		if !ok {
			t.Errorf("Resolve(%q) ok = false, want true", target)
			continue
		}
		if diff := cmp.Diff(memA, got); diff != "" {
			t.Errorf("Resolve(%q) diff (-want +got):\n%s", target, diff)
		}
	}
}

// TestIndexSelfLinkIsNotSpecialCased pins that a memory linking to itself
// resolves normally: it appears in its own Backlinks and is not Dangling.
// This is the simplest correct behavior for a self-reference — nothing in
// the brief calls for excluding it, so no special-casing is added.
func TestIndexSelfLinkIsNotSpecialCased(t *testing.T) {
	t.Parallel()
	memA := memory("claude/a.md", "a.md", "a")
	readBody := func(memoryfs.Memory) (string, error) { return "refers to [[a]] itself", nil }
	ix := links.BuildIndex([]memoryfs.Memory{memA}, readBody)

	if diff := cmp.Diff([]memoryfs.Memory{memA}, ix.Backlinks(memA)); diff != "" {
		t.Errorf("Backlinks(A) diff (-want +got):\n%s", diff)
	}
	if got := ix.Dangling(memA); got != nil {
		t.Errorf("Dangling(A) = %+v, want nil (self-link resolves)", got)
	}
}

// TestIndexDuplicateOccurrencesDedupeBacklinks pins that a memory linking
// to the same target twice contributes only ONE Backlinks entry — Parse
// legitimately reports both occurrences (the reading view cycles through
// each in turn), but a backlink list must not repeat the same referrer.
func TestIndexDuplicateOccurrencesDedupeBacklinks(t *testing.T) {
	t.Parallel()
	memA := memory("claude/a.md", "a.md", "a")
	memB := memory("claude/b.md", "b.md", "b")
	readBody := func(m memoryfs.Memory) (string, error) {
		if m.RepoPath == memA.RepoPath {
			return "[[b]] middle [[b]]", nil
		}
		return "", nil
	}
	ix := links.BuildIndex([]memoryfs.Memory{memA, memB}, readBody)

	if diff := cmp.Diff([]memoryfs.Memory{memA}, ix.Backlinks(memB)); diff != "" {
		t.Errorf("Backlinks(B) diff (-want +got):\n%s", diff)
	}
}

// TestIndexStemCollisionResolvesDeterministically pins the tie-break when
// two memories share a stem case-insensitively — e.g. two providers each
// enrolling their own "foo" memory. BuildIndex must resolve this the same
// way regardless of the slice order it is called with, so the test feeds
// both orderings and requires the same winner from each.
func TestIndexStemCollisionResolvesDeterministically(t *testing.T) {
	t.Parallel()
	claudeFoo := memory("claude/foo.md", "foo.md", "claude-foo")
	codexFoo := memory("codex/foo.md", "foo.md", "codex-foo")
	noBody := func(memoryfs.Memory) (string, error) { return "", nil }

	orderings := [][]memoryfs.Memory{
		{claudeFoo, codexFoo},
		{codexFoo, claudeFoo},
	}
	for _, order := range orderings {
		ix := links.BuildIndex(order, noBody)
		got, ok := ix.Resolve("FOO")
		if !ok {
			t.Fatalf("Resolve(FOO) ok = false, want true (order %v)", order)
		}
		if diff := cmp.Diff(claudeFoo, got); diff != "" {
			t.Errorf("Resolve(FOO) diff (-want +got) for order %v:\n%s", order, diff)
		}
	}
}

// TestIndexBacklinksSortedByName pins the documented ordering: multiple
// referrers come back sorted by their own Name, not by discovery order.
func TestIndexBacklinksSortedByName(t *testing.T) {
	t.Parallel()
	target := memory("claude/target.md", "target.md", "target")
	zebra := memory("claude/zebra.md", "zebra.md", "Zebra")
	alpha := memory("claude/alpha.md", "alpha.md", "Alpha")
	middle := memory("claude/middle.md", "middle.md", "Middle")

	readBody := func(m memoryfs.Memory) (string, error) {
		if m.RepoPath == target.RepoPath {
			return "", nil
		}
		return "[[target]]", nil
	}
	// Built deliberately out of name order to prove Backlinks sorts rather
	// than preserving BuildIndex's input or discovery order.
	ix := links.BuildIndex([]memoryfs.Memory{target, zebra, alpha, middle}, readBody)

	want := []memoryfs.Memory{alpha, middle, zebra}
	if diff := cmp.Diff(want, ix.Backlinks(target)); diff != "" {
		t.Errorf("Backlinks(target) diff (-want +got):\n%s", diff)
	}
}

// TestBuildIndexToleratesReadError pins the fault-isolation contract: a
// readBody error for one memory drops only that memory's own outbound
// links. Its registration — so other memories can still resolve and link
// to it — and every other memory's own links are unaffected, and
// BuildIndex never panics.
func TestBuildIndexToleratesReadError(t *testing.T) {
	t.Parallel()
	memX := memory("claude/x.md", "x.md", "x")
	memY := memory("claude/y.md", "y.md", "y")
	readErr := errors.New("simulated read failure")

	readBody := func(m memoryfs.Memory) (string, error) {
		switch m.RepoPath {
		case memX.RepoPath:
			return "[[y]]", nil
		case memY.RepoPath:
			return "", readErr // Y's own body is unreadable
		default:
			t.Fatalf("unexpected readBody call for %q", m.RepoPath)
			return "", nil
		}
	}

	ix := links.BuildIndex([]memoryfs.Memory{memX, memY}, readBody)

	// Y is still a valid resolution target, and X's link to it still
	// counts, even though Y's own body could not be read.
	if diff := cmp.Diff([]memoryfs.Memory{memX}, ix.Backlinks(memY)); diff != "" {
		t.Errorf("Backlinks(Y) diff (-want +got):\n%s", diff)
	}
	// Y's own outbound links are absent — never parsed — so Dangling
	// reports nothing rather than surfacing a link it never actually read.
	if got := ix.Dangling(memY); got != nil {
		t.Errorf("Dangling(Y) = %+v, want nil (Y's body was unreadable)", got)
	}
	if got := ix.Dangling(memX); got != nil {
		t.Errorf("Dangling(X) = %+v, want nil (X's link to Y resolves)", got)
	}
	if got := ix.Backlinks(memX); got != nil {
		t.Errorf("Backlinks(X) = %+v, want nil (nobody links to X)", got)
	}
}

// TestBuildIndexEmptyMemories pins that an Index built from zero memories
// (a brand-new enrollment with no files yet) behaves like any other empty
// Index rather than panicking on a nil internal map or similar.
func TestBuildIndexEmptyMemories(t *testing.T) {
	t.Parallel()
	calls := 0
	ix := links.BuildIndex(nil, func(memoryfs.Memory) (string, error) {
		calls++
		return "", nil
	})
	if calls != 0 {
		t.Errorf("readBody called %d times for zero memories, want 0", calls)
	}
	if _, ok := ix.Resolve("anything"); ok {
		t.Errorf("Resolve(anything) ok = true on an empty Index, want false")
	}
	unknown := memory("claude/unknown.md", "unknown.md", "unknown")
	if got := ix.Backlinks(unknown); got != nil {
		t.Errorf("Backlinks(unknown) = %+v, want nil", got)
	}
	if got := ix.Dangling(unknown); got != nil {
		t.Errorf("Dangling(unknown) = %+v, want nil", got)
	}
}

// TestIndexUnknownMemoryDegradesGracefully pins that Backlinks/Dangling
// never panic for a Memory the Index was never built from — a caller
// holding a stale reference gets empty results, not a crash.
func TestIndexUnknownMemoryDegradesGracefully(t *testing.T) {
	t.Parallel()
	known := memory("claude/known.md", "known.md", "known")
	unknown := memory("claude/unknown.md", "unknown.md", "unknown")
	noBody := func(memoryfs.Memory) (string, error) { return "", nil }
	ix := links.BuildIndex([]memoryfs.Memory{known}, noBody)

	if got := ix.Backlinks(unknown); got != nil {
		t.Errorf("Backlinks(unknown) = %+v, want nil", got)
	}
	if got := ix.Dangling(unknown); got != nil {
		t.Errorf("Dangling(unknown) = %+v, want nil", got)
	}
}
