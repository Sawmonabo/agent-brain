package search_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/search"
)

// mem builds a Memory identified by repoPath (unique per fixture, asserted
// on in results) with the given Name and Description.
func mem(repoPath, name, description string) memoryfs.Memory {
	return memoryfs.Memory{RepoPath: repoPath, Name: name, Description: description}
}

// fakeReadBody returns a readBody stand-in keyed by a Memory's RepoPath:
// bodies supplies each memory's content (absent keys read as ""), errs
// forces a specific RepoPath's read to fail — the same shape a real
// memoryfs.ReadBody failure (e.g. ErrTooLarge) takes.
func fakeReadBody(bodies map[string]string, errs map[string]error) func(memoryfs.Memory) (string, error) {
	return func(m memoryfs.Memory) (string, error) {
		if err, ok := errs[m.RepoPath]; ok {
			return "", err
		}
		return bodies[m.RepoPath], nil
	}
}

// repoPaths projects Hits down to their Memory.RepoPath, in result order —
// the shape every ranking assertion below compares against.
func repoPaths(hits []search.Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Memory.RepoPath
	}
	return out
}

// TestQueryRanking pins Query's full sort contract: Tier ascending (name
// beats description beats body), then within a tier the name-match quality
// (prefix beats substring beats bare subsequence) — deliberately naming the
// body-only hit alphabetically first and the name hit alphabetically last,
// so a test that passed on accidental alphabetical order would fail here.
func TestQueryRanking(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		memories  []memoryfs.Memory
		bodies    map[string]string
		query     string
		wantOrder []string
	}{
		{
			name: "name quality: prefix beats substring beats bare subsequence",
			memories: []memoryfs.Memory{
				mem("subsequence", "axbxc", ""),
				mem("prefix", "abcdef", ""),
				mem("substring", "xabcdef", ""),
			},
			query:     "abc",
			wantOrder: []string{"prefix", "substring", "subsequence"},
		},
		{
			name: "tier: name beats description beats body regardless of alphabetical name",
			memories: []memoryfs.Memory{
				mem("aaa-body-only", "aaa-body-only", ""),
				mem("mmm-desc-hit", "mmm-desc-hit", "a needle in the description"),
				mem("zzz-name-hit", "needle-in-name", ""),
			},
			bodies: map[string]string{
				"aaa-body-only": "line one\nneedle appears here\nline three",
			},
			query:     "needle",
			wantOrder: []string{"zzz-name-hit", "mmm-desc-hit", "aaa-body-only"},
		},
		{
			name: "same tier, same quality: falls back to Name ascending",
			memories: []memoryfs.Memory{
				mem("b", "needle-b", ""),
				mem("a", "needle-a", ""),
			},
			query:     "needle",
			wantOrder: []string{"a", "b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hits := search.Query(tc.memories, fakeReadBody(tc.bodies, nil), tc.query, 0)
			if diff := cmp.Diff(tc.wantOrder, repoPaths(hits)); diff != "" {
				t.Errorf("Query() order mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestQueryStableOnFullTie proves ties (identical Tier, quality, and Name)
// preserve input order rather than reordering arbitrarily — Query's
// documented determinism guarantee when nothing else distinguishes hits.
func TestQueryStableOnFullTie(t *testing.T) {
	t.Parallel()
	memories := []memoryfs.Memory{
		mem("second", "duplicate", ""),
		mem("first", "duplicate", ""),
	}
	hits := search.Query(memories, fakeReadBody(nil, nil), "duplicate", 0)
	want := []string{"second", "first"}
	if diff := cmp.Diff(want, repoPaths(hits)); diff != "" {
		t.Errorf("Query() stable-tie order mismatch (-want +got):\n%s", diff)
	}
}

// TestQueryOneHitPerMemory pins that a memory matching at multiple tiers
// (here: both Name and body contain the query) contributes exactly one
// Hit, at its best tier — the body match never surfaces as a second Hit.
func TestQueryOneHitPerMemory(t *testing.T) {
	t.Parallel()
	memories := []memoryfs.Memory{mem("m1", "needle-name", "")}
	bodies := map[string]string{"m1": "needle also appears in the body"}
	hits := search.Query(memories, fakeReadBody(bodies, nil), "needle", 0)
	if len(hits) != 1 {
		t.Fatalf("len(hits) = %d, want 1", len(hits))
	}
	if hits[0].Tier != search.TierName {
		t.Errorf("Tier = %v, want TierName (the name hit must suppress the body hit)", hits[0].Tier)
	}
}

// TestQueryDoesNotReadBodyWhenNameOrDescriptionMatch pins Query's laziness:
// readBody is only ever invoked once the cheaper name and description
// tiers have both already failed to match. The fake fails the test outright
// if called, so any regression that reads bodies eagerly is caught here
// rather than merely producing a slower query.
func TestQueryDoesNotReadBodyWhenNameOrDescriptionMatch(t *testing.T) {
	t.Parallel()
	memories := []memoryfs.Memory{
		mem("name-hit", "needle-name", ""),
		mem("desc-hit", "unrelated", "needle description"),
	}
	poison := func(memoryfs.Memory) (string, error) {
		t.Fatal("readBody must not be called once the name or description tier already matched")
		return "", nil
	}
	hits := search.Query(memories, poison, "needle", 0)
	if len(hits) != 2 {
		t.Fatalf("len(hits) = %d, want 2", len(hits))
	}
}

// TestQueryReadBodyErrorSkipsMemory pins that a readBody failure (the
// documented ErrTooLarge shape, or any other error) drops that memory from
// the Body tier only — a memory whose Name still matches the query
// participates exactly as if its body were readable (title-only, not
// skipped outright), while a memory with no other way to match and an
// unreadable body is excluded entirely. Search is best-effort by design.
func TestQueryReadBodyErrorSkipsMemory(t *testing.T) {
	t.Parallel()
	memories := []memoryfs.Memory{
		mem("ok", "unrelated-ok", ""),
		mem("broken-body-only", "unrelated-broken", ""),
		mem("broken-but-name-matches", "needle-title-only", ""),
	}
	bodies := map[string]string{"ok": "needle appears here"}
	errs := map[string]error{
		"broken-body-only":        errors.New("boom"),
		"broken-but-name-matches": errors.New("boom"),
	}
	hits := search.Query(memories, fakeReadBody(bodies, errs), "needle", 0)
	want := []string{"broken-but-name-matches", "ok"}
	if diff := cmp.Diff(want, repoPaths(hits)); diff != "" {
		t.Errorf("Query() mismatch with a failing readBody (-want +got):\n%s", diff)
	}
}

// TestQueryEmptyOrWhitespaceQuery pins that a blank query (empty, or
// whitespace-only after trimming) returns nil rather than every memory
// unfiltered.
func TestQueryEmptyOrWhitespaceQuery(t *testing.T) {
	t.Parallel()
	memories := []memoryfs.Memory{mem("m1", "anything", "")}
	for _, query := range []string{"", "   ", "\t\n", "  \t "} {
		if hits := search.Query(memories, fakeReadBody(nil, nil), query, 0); hits != nil {
			t.Errorf("Query(%q) = %v, want nil", query, hits)
		}
	}
}

// TestQueryLimit pins the explicit-limit and default-limit (0, and any
// out-of-domain negative value substitute 50) contracts. All 51 memories
// tie at TierBody/no-name-quality, so the deterministic Name-ascending
// tie-break makes "unrelated-50" — alphabetically last — the one dropped
// by the default cap.
func TestQueryLimit(t *testing.T) {
	t.Parallel()
	const total = 51
	memories := make([]memoryfs.Memory, total)
	bodies := make(map[string]string, total)
	for i := range total {
		repoPath := fmt.Sprintf("unrelated-%02d", i)
		memories[i] = mem(repoPath, repoPath, "")
		bodies[repoPath] = "needle appears in every body"
	}

	t.Run("explicit limit honored", func(t *testing.T) {
		t.Parallel()
		hits := search.Query(memories, fakeReadBody(bodies, nil), "needle", 3)
		if len(hits) != 3 {
			t.Fatalf("len(hits) = %d, want 3", len(hits))
		}
	})

	for _, limit := range []int{0, -1, -100} {
		t.Run(fmt.Sprintf("limit %d defaults to 50", limit), func(t *testing.T) {
			t.Parallel()
			hits := search.Query(memories, fakeReadBody(bodies, nil), "needle", limit)
			if len(hits) != 50 {
				t.Fatalf("len(hits) = %d, want 50", len(hits))
			}
			for _, repoPath := range repoPaths(hits) {
				if repoPath == "unrelated-50" {
					t.Errorf("expected the alphabetically-last memory to be dropped by the default cap, but it was present")
				}
			}
		})
	}
}

// TestQueryAcrossFolders pins the ≥2-projects fixture shape the search
// overlay's own acceptance seed (Task 15) reuses: memories from different
// folders are matched independently of folder identity.
func TestQueryAcrossFolders(t *testing.T) {
	t.Parallel()
	memories := []memoryfs.Memory{
		{Folder: "acme", RepoPath: "acme/needle-one", Name: "needle-one"},
		{Folder: "beta", RepoPath: "beta/needle-two", Name: "needle-two"},
	}
	hits := search.Query(memories, fakeReadBody(nil, nil), "needle", 0)
	gotFolders := make(map[string]bool, len(hits))
	for _, h := range hits {
		gotFolders[h.Memory.Folder] = true
	}
	if len(hits) != 2 || !gotFolders["acme"] || !gotFolders["beta"] {
		t.Errorf("Query() hits = %v, want exactly one hit per folder (acme and beta)", hits)
	}
}

// TestQueryUnicodeCaseFolding pins that case folding is not ASCII-only: a
// non-ASCII letter (Ü) must fold the same as its lowercase counterpart.
func TestQueryUnicodeCaseFolding(t *testing.T) {
	t.Parallel()
	memories := []memoryfs.Memory{mem("m1", "MÜNCHEN", "")}
	hits := search.Query(memories, fakeReadBody(nil, nil), "münchen", 0)
	if len(hits) != 1 {
		t.Fatalf("len(hits) = %d, want 1 (case folding must cover non-ASCII letters)", len(hits))
	}
}

// TestQueryFragment covers Fragment extraction across all three tiers:
// verbatim for a short Name/Description, trimmed-but-needle-preserved for
// a long body line, stripped of a trailing CRLF carriage return, and
// left uncentered (truncated from the match's start rather than around it)
// when the query itself is longer than the 120-rune cap.
func TestQueryFragment(t *testing.T) {
	t.Parallel()

	t.Run("name fragment is the Name verbatim when short", func(t *testing.T) {
		t.Parallel()
		memories := []memoryfs.Memory{mem("m1", "needle-name", "")}
		hits := search.Query(memories, fakeReadBody(nil, nil), "needle", 0)
		if len(hits) != 1 || hits[0].Fragment != "needle-name" {
			t.Errorf("Query() = %+v, want a single hit with Fragment == Name", hits)
		}
		if hits[0].Line != 0 {
			t.Errorf("Line = %d, want 0 for a TierName hit", hits[0].Line)
		}
	})

	t.Run("description fragment is the Description verbatim when short", func(t *testing.T) {
		t.Parallel()
		memories := []memoryfs.Memory{mem("m1", "unrelated", "a needle in the description")}
		hits := search.Query(memories, fakeReadBody(nil, nil), "needle", 0)
		if len(hits) != 1 || hits[0].Fragment != "a needle in the description" {
			t.Errorf("Query() = %+v, want a single hit with Fragment == Description", hits)
		}
		if hits[0].Line != 0 {
			t.Errorf("Line = %d, want 0 for a TierDescription hit", hits[0].Line)
		}
	})

	t.Run("short body line returned unchanged with its 1-based line number", func(t *testing.T) {
		t.Parallel()
		memories := []memoryfs.Memory{mem("m1", "unrelated-name", "")}
		bodies := map[string]string{"m1": "line one\nneedle is here\nline three"}
		hits := search.Query(memories, fakeReadBody(bodies, nil), "needle", 0)
		if len(hits) != 1 {
			t.Fatalf("len(hits) = %d, want 1", len(hits))
		}
		if hits[0].Fragment != "needle is here" {
			t.Errorf("Fragment = %q, want %q", hits[0].Fragment, "needle is here")
		}
		if hits[0].Line != 2 {
			t.Errorf("Line = %d, want 2", hits[0].Line)
		}
	})

	t.Run("long line trimmed to 120 runes around the needle, needle preserved", func(t *testing.T) {
		t.Parallel()
		line := strings.Repeat("x", 200) + "NEEDLE" + strings.Repeat("y", 200)
		memories := []memoryfs.Memory{mem("m1", "unrelated-name", "")}
		bodies := map[string]string{"m1": line}
		hits := search.Query(memories, fakeReadBody(bodies, nil), "needle", 0)
		if len(hits) != 1 {
			t.Fatalf("len(hits) = %d, want 1", len(hits))
		}
		fragment := hits[0].Fragment
		if runeCount := len([]rune(fragment)); runeCount > 120 {
			t.Errorf("Fragment rune length = %d, want <= 120", runeCount)
		}
		if !strings.Contains(strings.ToLower(fragment), "needle") {
			t.Errorf("Fragment = %q, want it to still contain the matched needle", fragment)
		}
	})

	t.Run("query over 120 runes is not centered: fragment is the match's first 120 runes", func(t *testing.T) {
		t.Parallel()
		query := strings.Repeat("abcdefghij", 13) // 130 runes, over fragmentRuneCap's 120
		memories := []memoryfs.Memory{mem("m1", "unrelated-name", "")}
		bodies := map[string]string{"m1": query}
		hits := search.Query(memories, fakeReadBody(bodies, nil), query, 0)
		if len(hits) != 1 {
			t.Fatalf("len(hits) = %d, want 1", len(hits))
		}
		want := strings.Repeat("abcdefghij", 12) // first 120 runes of the 130-rune match
		if hits[0].Fragment != want {
			t.Errorf("Fragment = %q, want %q (the match's first 120 runes, uncentered)", hits[0].Fragment, want)
		}
	})

	t.Run("CRLF line ending is stripped from the fragment", func(t *testing.T) {
		t.Parallel()
		memories := []memoryfs.Memory{mem("m1", "unrelated-name", "")}
		bodies := map[string]string{"m1": "line one\r\nneedle here\r\nline three\r\n"}
		hits := search.Query(memories, fakeReadBody(bodies, nil), "needle", 0)
		if len(hits) != 1 {
			t.Fatalf("len(hits) = %d, want 1", len(hits))
		}
		if strings.ContainsRune(hits[0].Fragment, '\r') {
			t.Errorf("Fragment = %q, want no trailing carriage return", hits[0].Fragment)
		}
		if hits[0].Line != 2 {
			t.Errorf("Line = %d, want 2", hits[0].Line)
		}
	})
}
