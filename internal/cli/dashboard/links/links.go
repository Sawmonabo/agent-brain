// Package links is spec §4's [[link]] navigation substrate: it scans a
// memory body for [[target]] spans and, given a project's full memory set,
// resolves each target to the memory it names and answers backlink and
// dangling-link queries. It imports only memoryfs + stdlib — no bubbletea,
// no lipgloss (package-boundary rule, spec §8) — so the browser and reading
// view (later tasks) can both build on it without pulling in the TUI.
package links

import (
	"cmp"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
)

// Link is one [[target]] occurrence; offsets are byte positions in the body
// (the reading view highlights and cycles them in order).
type Link struct {
	Target     string // inner text, trimmed
	Start, End int    // byte span INCLUDING the brackets
}

// Parse scans body for non-nested [[target]] spans: a single left-to-right
// pass that finds the next "[[", then the next "]]" (or newline) after it.
//
// Rules (each pinned by a links_test.go row):
//   - no newline inside: a "\n" before the closing "]]" invalidates the
//     span; scanning resumes one byte past the failed opener, so a later
//     legitimate "[[" is still found.
//   - empty target ignored: "[[]]" (or whitespace-only content) is not a
//     Link, though its brackets are still consumed — scanning resumes past
//     the closing "]]".
//   - unterminated "[[x" ignored: if no "]]" exists anywhere after an
//     opener, nothing later in body can close it either, so the scan ends.
//   - "[[a|b]]" uses a as target: the content is cut at the first "|"; the
//     alias half is discarded here (shown by the caller, which can re-slice
//     body[Start:End] itself if it needs it).
//
// Matched spans never overlap or nest: once a span is found (emitted or
// not), the scan resumes strictly after it, so a "[[" appearing inside an
// already-matched span is treated as literal target text, never as the
// start of a second, nested Link.
//
// Byte-level scanning for the single-byte ASCII delimiters "[", "]", "|",
// and "\n" is UTF-8-safe: every continuation and lead byte of a multi-byte
// rune is >= 0x80, so it can never be misread as one of these delimiters,
// and slicing at a delimiter's byte offset never splits a multi-byte rune.
func Parse(body string) []Link {
	var found []Link
	i := 0
	for {
		relOpen := strings.Index(body[i:], "[[")
		if relOpen == -1 {
			return found
		}
		open := i + relOpen
		rest := body[open+2:]

		relClose := strings.Index(rest, "]]")
		if relClose == -1 {
			return found // unterminated: no close anywhere further in body
		}
		if relNewline := strings.IndexByte(rest, '\n'); relNewline != -1 && relNewline < relClose {
			i = open + 1 // newline before the close: invalid, retry just past it
			continue
		}

		end := open + 2 + relClose + 2
		target, _, _ := strings.Cut(rest[:relClose], "|")
		target = strings.TrimSpace(target)
		if target != "" {
			found = append(found, Link{Target: target, Start: open, End: end})
		}
		i = end
	}
}

// Index resolves [[target]] wiki-links against a project's memories and
// answers backlinks. Resolution tries an exact filename-stem match first,
// then a frontmatter-Name match, both case-insensitive — the two normally
// coincide (memory naming convention: stem == frontmatter name) and only
// diverge when a memory's frontmatter name has drifted from its filename.
type Index struct {
	memories  map[string]memoryfs.Memory     // RepoPath -> Memory (identity key)
	byStem    map[string]memoryfs.Memory     // lowercased filename stem -> Memory
	byName    map[string]memoryfs.Memory     // lowercased frontmatter/display Name -> Memory
	outbound  map[string][]Link              // RepoPath -> links parsed from that memory's own body
	backlinks map[string]map[string]struct{} // target RepoPath -> set of linking RepoPaths
}

// BuildIndex parses every memory's body once via the readBody seam — tests
// need no real files, and the browser can reuse already-cached bodies.
//
// Registration (the stem/name lookup maps) happens in a first pass over
// every memory, before any body is parsed in a second pass, so a link's
// resolution never depends on the order memories happen to be processed
// in. Memories are also processed in a stable RepoPath order regardless of
// the order the caller supplies: when two memories collide on the same
// stem or Name (case-insensitively) — e.g. two providers each enrolling
// their own "foo" memory — the lexicographically earliest RepoPath always
// wins the registration, deterministically and independent of input order.
//
// A readBody error for one memory drops only that memory's own outbound
// links; its registration (so other memories can still resolve and link to
// it) and every other memory's own links are unaffected.
func BuildIndex(memories []memoryfs.Memory, readBody func(memoryfs.Memory) (string, error)) *Index {
	ordered := make([]memoryfs.Memory, len(memories))
	copy(ordered, memories)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].RepoPath < ordered[j].RepoPath })

	ix := &Index{
		memories:  make(map[string]memoryfs.Memory, len(ordered)),
		byStem:    make(map[string]memoryfs.Memory, len(ordered)),
		byName:    make(map[string]memoryfs.Memory, len(ordered)),
		outbound:  make(map[string][]Link, len(ordered)),
		backlinks: make(map[string]map[string]struct{}),
	}
	for _, m := range ordered {
		ix.memories[m.RepoPath] = m
		stemKey := strings.ToLower(stem(m))
		if _, taken := ix.byStem[stemKey]; !taken {
			ix.byStem[stemKey] = m
		}
		nameKey := strings.ToLower(m.Name)
		if _, taken := ix.byName[nameKey]; !taken {
			ix.byName[nameKey] = m
		}
	}

	for _, m := range ordered {
		body, err := readBody(m)
		if err != nil {
			continue
		}
		parsed := Parse(body)
		ix.outbound[m.RepoPath] = parsed
		for _, l := range parsed {
			target, ok := ix.Resolve(l.Target)
			if !ok {
				continue
			}
			set, exists := ix.backlinks[target.RepoPath]
			if !exists {
				set = make(map[string]struct{})
				ix.backlinks[target.RepoPath] = set
			}
			set[m.RepoPath] = struct{}{}
		}
	}
	return ix
}

// stem returns m's filename stem — its base name without extension,
// independent of any frontmatter Name — the priority key Resolve tries
// first. Mirrors memoryfs's own stem-fallback computation (listUnit).
func stem(m memoryfs.Memory) string {
	base := path.Base(m.RelPath)
	return strings.TrimSuffix(base, path.Ext(base))
}

// Resolve looks up target (case-insensitive, surrounding whitespace
// tolerated) first against filename stems, then against frontmatter/
// display Names.
func (ix *Index) Resolve(target string) (memoryfs.Memory, bool) {
	key := strings.ToLower(strings.TrimSpace(target))
	if m, ok := ix.byStem[key]; ok {
		return m, true
	}
	if m, ok := ix.byName[key]; ok {
		return m, true
	}
	return memoryfs.Memory{}, false
}

// Backlinks returns every memory with at least one link resolving to m,
// sorted by Name and, to break ties between memories that share a Name
// (e.g. two projects each with their own memory frontmatter-named "notes"),
// by RepoPath. RepoPath is unique per memory, so this is a total order:
// the result never depends on the backlink set's map iteration order, which
// varies from call to call. A memory linking to m more than once still
// appears only once. Returns nil if nothing links to m or m is unknown to
// the Index.
func (ix *Index) Backlinks(m memoryfs.Memory) []memoryfs.Memory {
	set := ix.backlinks[m.RepoPath]
	if len(set) == 0 {
		return nil
	}
	out := make([]memoryfs.Memory, 0, len(set))
	for repoPath := range set {
		out = append(out, ix.memories[repoPath])
	}
	slices.SortFunc(out, func(a, b memoryfs.Memory) int {
		if c := cmp.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return cmp.Compare(a.RepoPath, b.RepoPath)
	})
	return out
}

// Dangling returns the links found in m's own body that do not resolve to
// any memory, in the order Parse found them. Returns nil if m has no
// unresolved links, m's body could not be read during BuildIndex, or m is
// unknown to the Index.
func (ix *Index) Dangling(m memoryfs.Memory) []Link {
	var out []Link
	for _, l := range ix.outbound[m.RepoPath] {
		if _, ok := ix.Resolve(l.Target); !ok {
			out = append(out, l)
		}
	}
	return out
}
