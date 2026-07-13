// Package search implements spec §7's tiered, cross-project memory search:
// Query ranks every candidate Memory by where it matched — its Name (fuzzy
// subsequence) beats its Description (substring) beats its body content
// (substring, read lazily via the caller-supplied readBody) — so one engine
// serves every root view's `/` binding without caring which folder a memory
// belongs to. It holds no state of its own; the global search overlay
// (Task 15) re-collects memories fresh and calls Query on each keystroke.
package search

import (
	"cmp"
	"slices"
	"strings"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
)

// Tier ranks how a Hit matched, ascending from strongest to weakest.
type Tier int

// TierName, TierDescription, and TierBody are Query's three match tiers, in
// ascending (strongest-first) order.
const (
	TierName        Tier = iota // fuzzy (subsequence) on Memory.Name
	TierDescription             // case-insensitive substring on Description
	TierBody                    // case-insensitive substring in the body
)

// Hit is one Memory matching a query, at the single strongest tier it
// matched — see Query for why a memory can never contribute more than one
// Hit.
type Hit struct {
	Memory memoryfs.Memory
	Tier   Tier
	// Fragment is the matched text, trimmed to at most 120 runes around the
	// match: the Name verbatim for TierName, the Description verbatim for
	// TierDescription, or the matching body line for TierBody.
	Fragment string
	// Line is the 1-based body line Fragment came from; 0 for TierName and
	// TierDescription, which have no line of their own.
	Line int
}

// defaultLimit is Query's result cap when limit is 0 (or any other
// out-of-domain, non-positive value).
const defaultLimit = 50

// fragmentRuneCap bounds Fragment's length so a long body line, name, or
// description never blows out the overlay's row width.
const fragmentRuneCap = 120

// quality ranks how strongly a Memory's Name matches the query, independent
// of Tier — Query's tie-break within a tier. Every TierName Hit's Name
// matches at some quality above qualityNone by construction (Query tries
// the name tier first); a TierDescription or TierBody Hit's Name never
// matches the query at all, since a Name match would already have won
// TierName for that memory instead — so those always share qualityNone,
// and the tie falls through to Name.
type quality int

const (
	qualityPrefix quality = iota
	qualitySubstring
	qualitySubsequence
	qualityNone
)

// scoredHit pairs a Hit with the nameQuality tie-break Query sorts by.
// matchOne computes it once, alongside the Hit itself, from the same
// folded name it already used to test the Name tier — never recomputed
// later during ranking or inside the sort comparator.
type scoredHit struct {
	hit     Hit
	quality quality
}

// Query searches memories for query and returns matching Hits ranked by
// Tier ascending, then by how strongly the Memory's Name matches query (an
// exact prefix beats a non-prefix substring beats a bare subsequence), then
// by Name — a total order, so a full tie (identical Tier, quality, and
// Name) falls back to memories' input order.
//
// Each memory is tried at each tier in priority order — name, then
// description, then body — and contributes at most one Hit, at the first
// tier that matches: a Name hit suppresses that memory's Description and
// Body tiers entirely, so readBody is only ever invoked once both cheaper
// tiers have already failed to match. A readBody error (including
// memoryfs.ErrTooLarge for a file over its 1 MiB read cap) is treated
// identically to "the body doesn't match" and silently drops that memory
// from the Body tier only — search is best-effort over live files that can
// vanish or exceed memoryfs' read cap mid-query, never a hard failure. An
// oversize or otherwise-unreadable file still participates by Name and
// Description exactly like any other memory, since neither tier ever calls
// readBody: it is title-only, not skipped outright.
//
// query is trimmed of leading/trailing whitespace before matching; a query
// that trims to "" returns nil rather than every memory unfiltered. limit
// bounds the number of Hits returned; limit <= 0 (covering the documented 0
// case and any out-of-domain negative value) substitutes defaultLimit.
func Query(memories []memoryfs.Memory, readBody func(memoryfs.Memory) (string, error), query string, limit int) []Hit {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	if limit <= 0 {
		limit = defaultLimit
	}
	foldedQuery := foldRunes(query)

	scoredHits := make([]scoredHit, 0, len(memories))
	for _, memory := range memories {
		scored, ok := matchOne(memory, readBody, foldedQuery)
		if !ok {
			continue
		}
		scoredHits = append(scoredHits, scored)
	}

	slices.SortStableFunc(scoredHits, func(a, b scoredHit) int {
		if c := cmp.Compare(a.hit.Tier, b.hit.Tier); c != 0 {
			return c
		}
		if c := cmp.Compare(a.quality, b.quality); c != 0 {
			return c
		}
		return cmp.Compare(a.hit.Memory.Name, b.hit.Memory.Name)
	})

	if len(scoredHits) > limit {
		scoredHits = scoredHits[:limit]
	}
	hits := make([]Hit, len(scoredHits))
	for i, s := range scoredHits {
		hits[i] = s.hit
	}
	return hits
}

// matchOne tries memory's three tiers in priority order — name, then
// description, then body — and returns the first that matches as a
// scoredHit, pairing the Hit with the nameQuality tie-break Query sorts by.
// A Description or Body match is assigned qualityNone directly, without
// calling nameQuality at all: Tier already proves the Name didn't even
// subsequence-match (see the quality doc comment), so there is nothing left
// to classify. readBody is only called once the cheaper name and
// description checks have both already failed.
func matchOne(memory memoryfs.Memory, readBody func(memoryfs.Memory) (string, error), foldedQuery []rune) (scoredHit, bool) {
	if fragment, matchQuality, ok := matchName(memory.Name, foldedQuery); ok {
		return scoredHit{hit: Hit{Memory: memory, Tier: TierName, Fragment: fragment}, quality: matchQuality}, true
	}
	if fragment, ok := matchDescription(memory.Description, foldedQuery); ok {
		return scoredHit{hit: Hit{Memory: memory, Tier: TierDescription, Fragment: fragment}, quality: qualityNone}, true
	}
	body, err := readBody(memory)
	if err != nil {
		return scoredHit{}, false
	}
	if fragment, line, ok := matchBody(body, foldedQuery); ok {
		return scoredHit{hit: Hit{Memory: memory, Tier: TierBody, Fragment: fragment, Line: line}, quality: qualityNone}, true
	}
	return scoredHit{}, false
}

// matchName reports whether foldedQuery occurs in name as a case-insensitive
// subsequence, returning name trimmed to a display Fragment around the
// matched span, plus its nameQuality classification — computed here from
// the same folded name used for the subsequence test, rather than folding
// name again later to recompute it.
func matchName(name string, foldedQuery []rune) (fragment string, matchQuality quality, ok bool) {
	foldedName := foldRunes(name)
	start, end, ok := subsequenceSpan(foldedName, foldedQuery)
	if !ok {
		return "", qualityNone, false
	}
	return trimFragment([]rune(name), start, end), nameQuality(foldedName, foldedQuery), true
}

// matchDescription reports whether foldedQuery occurs in description as a
// case-insensitive substring, returning description trimmed to a display
// Fragment around the matched span.
func matchDescription(description string, foldedQuery []rune) (fragment string, ok bool) {
	start, end, ok := substringSpan(foldRunes(description), foldedQuery)
	if !ok {
		return "", false
	}
	return trimFragment([]rune(description), start, end), true
}

// matchBody scans body line by line for the first case-insensitive
// substring match, returning that line's Fragment and its 1-based line
// number. Scanning stops at the first match — later lines are never
// inspected, so a memory can contribute at most one body Hit regardless of
// how many lines actually contain the query. A trailing '\r' (CRLF line
// endings) is stripped from each candidate line before matching, so it
// never leaks into Fragment.
func matchBody(body string, foldedQuery []rune) (fragment string, line int, ok bool) {
	lineNumber := 0
	for candidate := range strings.SplitSeq(body, "\n") {
		lineNumber++
		candidate = strings.TrimRight(candidate, "\r")
		start, end, found := substringSpan(foldRunes(candidate), foldedQuery)
		if !found {
			continue
		}
		return trimFragment([]rune(candidate), start, end), lineNumber, true
	}
	return "", 0, false
}

// nameQuality classifies how strongly foldedName matches foldedQuery: an
// exact case-insensitive prefix ranks above a non-prefix substring, which
// ranks above a bare (non-contiguous) subsequence. Both arguments must
// already be folded by the caller. matchName calls this only once
// subsequenceSpan has already confirmed at least a subsequence match, so
// unlike matchName's own check, this never needs to re-test for one: the
// subsequence case is the fallback once prefix and substring have both
// missed.
func nameQuality(foldedName, foldedQuery []rune) quality {
	if len(foldedQuery) <= len(foldedName) && runesEqual(foldedName[:len(foldedQuery)], foldedQuery) {
		return qualityPrefix
	}
	if _, _, ok := substringSpan(foldedName, foldedQuery); ok {
		return qualitySubstring
	}
	return qualitySubsequence
}

// foldRunes case-folds s to a rune slice for matching: rune-slice indexing
// keeps the [start, end) spans substringSpan and subsequenceSpan report
// aligned with what a cell-oriented terminal UI highlights. strings.ToLower
// is rune-count-preserving in Go — its non-ASCII path is
// Map(unicode.ToLower, s), and unicode.ToLower has signature func(rune)
// rune, so it can only ever replace one rune with one rune. (Go deliberately
// doesn't implement ICU-style full case folding, which can expand a single
// rune — e.g. Turkish İ — into several; ordinary simple case folding never
// shifts offsets the way full folding could.)
func foldRunes(s string) []rune {
	return []rune(strings.ToLower(s))
}

// runesEqual reports whether a and b hold the same runes in the same order.
func runesEqual(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// subsequenceSpan reports whether every rune of needle occurs in haystack in
// order (not necessarily contiguous) — e.g. "cnf" matches "config" via
// c-o-n-f-ig — and if so, the [start, end) rune span from the first to the
// last matched rune, greedily matching each needle rune at the earliest
// possible haystack position. Both slices must already be folded to the
// same case by the caller.
func subsequenceSpan(haystack, needle []rune) (start, end int, ok bool) {
	if len(needle) == 0 {
		return 0, 0, true
	}
	needleIndex := 0
	start = -1
	for haystackIndex, r := range haystack {
		if r != needle[needleIndex] {
			continue
		}
		if start < 0 {
			start = haystackIndex
		}
		needleIndex++
		if needleIndex == len(needle) {
			return start, haystackIndex + 1, true
		}
	}
	return 0, 0, false
}

// substringSpan reports the [start, end) rune span of needle's first
// occurrence in haystack, or ok=false if absent. Both slices must already
// be folded to the same case by the caller.
func substringSpan(haystack, needle []rune) (start, end int, ok bool) {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if runesEqual(haystack[i:i+len(needle)], needle) {
			return i, i + len(needle), true
		}
	}
	return 0, 0, false
}

// trimFragment returns text trimmed to at most fragmentRuneCap runes,
// centered on the [matchStart, matchEnd) rune span so the text that matched
// stays inside the result; text no longer than the cap is returned
// unchanged. A match itself wider than the cap (a query longer than
// fragmentRuneCap) is not centered — the window simply starts at
// matchStart, which may cut off the match's tail rather than its head.
func trimFragment(text []rune, matchStart, matchEnd int) string {
	if len(text) <= fragmentRuneCap {
		return string(text)
	}
	slack := max(fragmentRuneCap-(matchEnd-matchStart), 0)
	start := max(matchStart-slack/2, 0)
	end := min(start+fragmentRuneCap, len(text))
	start = max(end-fragmentRuneCap, 0)
	return string(text[start:end])
}
