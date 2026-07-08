package provider

import (
	"fmt"
	"path"
	"strings"
)

// Match reports whether rel (slash-separated, no leading slash) matches
// glob. Semantics — the documented contract shared with attribute
// generation (repo package):
//
//   - Segments are split on '/'.
//   - A segment of exactly "**" followed by more glob segments matches
//     zero or more whole segments (git's "middle **" rule — e.g.
//     "skills/**/SKILL.md" matches "skills/SKILL.md").
//   - A trailing "**" (the last glob segment) matches one or more
//     remaining segments — git's "trailing /**" rule, "everything
//     inside" — so "dir/**" matches "dir/x" but not the bare "dir".
//   - Any other segment matches one segment via path.Match ('*', '?',
//     '[...]' within the segment; '*' never crosses '/').
//   - Malformed per-segment patterns match nothing; ValidateGlob rejects
//     them up front so registries fail fast instead.
func Match(glob, rel string) bool {
	return matchSegments(strings.Split(glob, "/"), strings.Split(rel, "/"))
}

func matchSegments(globParts, relParts []string) bool {
	if len(globParts) == 0 {
		return len(relParts) == 0
	}
	head, rest := globParts[0], globParts[1:]
	if head == "**" {
		if len(rest) == 0 {
			// Trailing '**' is "everything inside": it needs at least
			// one remaining segment, unlike a middle '**' below.
			return len(relParts) > 0
		}
		// Middle '**': zero segments…
		if matchSegments(rest, relParts) {
			return true
		}
		// …or consume one and stay on '**'.
		if len(relParts) > 0 {
			return matchSegments(globParts, relParts[1:])
		}
		return false
	}
	if len(relParts) == 0 {
		return false
	}
	ok, err := path.Match(head, relParts[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(rest, relParts[1:])
}

// ValidateGlob rejects globs whose segments path.Match cannot parse, and
// characters that would corrupt the .gitattributes lines the same glob
// becomes in repo.GenerateAttributes: whitespace splits an attributes
// line into pattern+attrs, '#' comments it out, leading '!' inverts it.
// Registries call this at construction so a bad pattern is a loud
// startup error — never a silently-never-matching or file-corrupting rule.
func ValidateGlob(glob string) error {
	if glob == "" {
		return fmt.Errorf("empty glob")
	}
	if strings.HasPrefix(glob, "!") {
		return fmt.Errorf("glob %q: leading '!' would negate a .gitattributes pattern", glob)
	}
	if strings.ContainsAny(glob, " \t\n\r\"#") {
		return fmt.Errorf("glob %q: whitespace, quotes, and '#' are unrepresentable in .gitattributes lines", glob)
	}
	for _, seg := range strings.Split(glob, "/") {
		if seg == "" {
			return fmt.Errorf("glob %q: empty segment (leading/trailing '/' or '//') would corrupt a .gitattributes line", glob)
		}
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, "probe"); err != nil {
			return fmt.Errorf("glob %q: segment %q: %w", glob, seg, err)
		}
	}
	return nil
}
