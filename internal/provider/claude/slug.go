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

// GuessPath reverses Claude's project slug (absolute path with '/'
// replaced by '-') into a best-guess project directory. The encoding is
// lossy — a '-' in a real path component is indistinguishable from a
// separator — so the reconstruction is filesystem-guided: walk the slug
// segments, preferring '/' when the resulting directory exists and
// falling back to extending the current component with '-'; a dash-run
// the walk cannot resolve is then retried as a single hyphenated leaf
// component under the deepest verified boundary (exactly how a project
// directory like "agent-brain" encodes) before the naive all-slash
// reversal is returned as the last resort. dirExists is injected for
// testability (production passes a stat closure).
//
// Exported because migrate (spec §10) maps identical slugs under
// ~/.agent-brain/.
func GuessPath(slug string, dirExists func(string) bool) string {
	segments := strings.Split(strings.TrimPrefix(slug, "-"), "-")
	if len(segments) == 0 {
		return ""
	}
	naive := "/" + strings.Join(segments, "/")
	if dirExists(naive) {
		return naive
	}
	current := "/" + segments[0]
	// confirmed is the deepest walk prefix verified to be a real directory
	// ("" until one is); pending is the dash-run of segments accumulated
	// past it. Together they let the fallback below retry an unresolved
	// run against the last trustworthy boundary.
	confirmed := ""
	var pending []string
	if dirExists(current) {
		confirmed = current
	} else {
		pending = append(pending, segments[0])
	}
	for _, segment := range segments[1:] {
		asChild := current + "/" + segment
		if dirExists(asChild) {
			current = asChild
			confirmed = asChild
			pending = nil
			continue
		}
		current += "-" + segment
		pending = append(pending, segment)
	}
	if dirExists(current) {
		return current
	}
	// The greedy walk merges an unresolvable dash-run into the last
	// component it committed to (".../dev" + agent,brain →
	// ".../dev-agent-brain"). The other lossy reading — the run is one NEW
	// hyphenated component under the last verified boundary
	// (".../dev/agent-brain") — is exactly how a hyphenated project leaf
	// directory encodes, and the walk above can only reach it when a
	// shorter decoy sibling (".../dev/agent") happens to exist. Try the
	// leaf reading before surrendering to the naive guess.
	if confirmed != "" && len(pending) > 0 {
		if leaf := confirmed + "/" + strings.Join(pending, "-"); dirExists(leaf) {
			return leaf
		}
	}
	return filepath.ToSlash(naive)
}

// SlugFor encodes path the way Claude Code itself does when it creates
// ~/.claude/projects/<slug>: every '/' becomes '-'. This is the exact
// forward direction — unlike GuessPath's lossy reverse, no directory
// probing is needed because the encoding has no ambiguity in this
// direction. track's path-argument resolution uses it to compute the
// slug a given project path would have produced, rather than reversing
// every discovered slug looking for a match.
func SlugFor(path string) string {
	return strings.ReplaceAll(filepath.ToSlash(path), "/", "-")
}

// MemoryDirFor returns the memory directory Claude Code uses for path
// under home. Shared by Discover (which lists every slug already on
// disk) and track/migrate's path-argument resolution (which probes one
// specific path directly via SlugFor, without listing anything).
func MemoryDirFor(home, path string) string {
	return filepath.Join(home, ".claude", "projects", SlugFor(path), "memory")
}
