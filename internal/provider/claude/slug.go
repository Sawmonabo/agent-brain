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
// falling back to extending the current component with '-'. dirExists is
// injected for testability (production passes a stat closure).
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
	for _, segment := range segments[1:] {
		asChild := current + "/" + segment
		if dirExists(asChild) {
			current = asChild
			continue
		}
		current += "-" + segment
	}
	if dirExists(current) {
		return current
	}
	return filepath.ToSlash(naive)
}
