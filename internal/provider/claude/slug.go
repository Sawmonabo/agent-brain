// Package claude adapts Claude Code's per-project memory
// (~/.claude/projects/<slug>/memory/, zero-config since v2.1.59) to the
// agent-brain provider contract (spec §6).
//
// A note on what reconcile costs: regeneration replaces any hand-curation
// Claude's own sessions did to MEMORY.md wording — the frontmatter
// description IS the hook text by construction, so curated descriptions
// survive in the topic files themselves; this is the spec §3
// derived-index trade, decided (files are the source of truth, the index
// is a view).
package claude

import (
	"path/filepath"
	"strings"
)

// slugReadings lists, in preference order, the characters a slug dash
// can stand for. '/' first keeps the search biased toward descending
// into real directories (the bias the previous greedy walk had), and
// '-' stays ahead of the rarer punctuation so a hyphenated leaf like
// "agent-brain" keeps winning any tie against a dotted sibling. The
// set mirrors what real project paths contain; characters outside it
// (unicode, quotes, …) also encode to '-' but are unrecoverable by
// construction — Discover sidesteps that entirely by preferring the
// session files' recorded cwd (see sessionCWD).
var slugReadings = [...]byte{'/', '-', '.', '_', ' '}

// guessPathBudget caps the directory probes one GuessPath call may
// spend. Filesystem pruning keeps real slugs cheap (each committed '/'
// must name an existing directory), but a long dash-run inside one
// component has no boundary to prune at; the cap turns that worst case
// into "fall back to the naive reversal" instead of a stall.
const guessPathBudget = 8192

// GuessPath reverses Claude's project slug into a best-guess project
// directory. The encoding is lossy — every non-alphanumeric character
// becomes '-' (see SlugFor) — so a dash may be a path separator, a
// literal '-', or punctuation. The reconstruction is a filesystem-guided
// backtracking search: at each dash it tries the slugReadings in
// preference order, prunes any branch whose committed directory prefix
// does not exist, and returns the first complete reading that names a
// real directory. When no reading resolves (or the probe budget runs
// out), the naive all-slash reversal is returned as the documented last
// resort. dirExists is injected for testability (production passes a
// stat closure).
//
// Exported because migrate (spec §10) maps identical slugs under
// ~/.agent-brain/.
func GuessPath(slug string, dirExists func(string) bool) string {
	trimmed := strings.TrimPrefix(slug, "-")
	budget := guessPathBudget
	if found, ok := resolveSlug(trimmed, "/", &budget, dirExists); ok {
		return found
	}
	return "/" + strings.Join(strings.Split(trimmed, "-"), "/")
}

// resolveSlug is GuessPath's search: rest is the unread slug tail, built
// the path committed so far (always beginning "/", possibly ending
// mid-component). Each dash branches over slugReadings; choosing '/'
// commits the current component, which must be non-empty and — pruning —
// an existing directory. Budget counts dirExists probes; exhaustion
// abandons the search (false), never a partial answer.
func resolveSlug(rest, built string, budget *int, dirExists func(string) bool) (string, bool) {
	head, tail, hasDash := strings.Cut(rest, "-")
	if !hasDash {
		full := built + rest
		if *budget <= 0 {
			return "", false
		}
		*budget--
		if dirExists(full) {
			return full, true
		}
		return "", false
	}
	for _, reading := range slugReadings {
		if reading == '/' {
			candidate := built + head
			// "//" is no path: a '/' reading needs a non-empty component.
			if strings.HasSuffix(candidate, "/") {
				continue
			}
			if *budget <= 0 {
				return "", false
			}
			*budget--
			if !dirExists(candidate) {
				continue
			}
			if found, ok := resolveSlug(tail, candidate+"/", budget, dirExists); ok {
				return found, true
			}
			continue
		}
		if found, ok := resolveSlug(tail, built+head+string(reading), budget, dirExists); ok {
			return found, true
		}
		if *budget <= 0 {
			return "", false
		}
	}
	return "", false
}

// SlugFor encodes path the way Claude Code itself does when it creates
// ~/.claude/projects/<slug>: every UTF-16 code unit outside [a-zA-Z0-9]
// becomes one '-'. Verified against real Claude Code v2.1.205
// (2026-07-09) by running one-shot sessions in probe directories and
// reading back the slugs it created: the observed rule is JavaScript's
// replace(/[^a-zA-Z0-9]/g, "-") over the absolute path, and JS regexes
// operate on UTF-16 — a BMP rune ('.', '_', ' ', 'ö') is one unit → one
// dash, an astral rune (🚀, a surrogate pair) is two units → TWO dashes.
// Invalid UTF-8 decodes rune-by-rune to U+FFFD (BMP) → one dash each.
//
// This is the exact forward direction — no directory probing — and the
// daemon's watch path derives from it (MemoryDirFor), so fidelity here
// decides whether tracking a project syncs anything at all. track's
// path-argument resolution uses it to compute the slug a given project
// path would have produced, rather than reversing every discovered slug
// looking for a match.
func SlugFor(path string) string {
	var slug strings.Builder
	slug.Grow(len(path))
	for _, r := range path {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9':
			slug.WriteRune(r)
		case r > 0xFFFF:
			slug.WriteString("--")
		default:
			slug.WriteByte('-')
		}
	}
	return slug.String()
}

// MemoryDirFor returns the memory directory Claude Code uses for path
// under home. Shared by Discover (which lists every slug already on
// disk) and track/migrate's path-argument resolution (which probes one
// specific path directly via SlugFor, without listing anything).
func MemoryDirFor(home, path string) string {
	return filepath.Join(home, ".claude", "projects", SlugFor(path), "memory")
}
