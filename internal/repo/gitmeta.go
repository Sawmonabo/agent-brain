package repo

import (
	"slices"
	"strings"
)

// gitMetaNames are the path segments that carry git's own semantics and so
// must never originate from provider content or survive in the checkout
// below its root: `.gitattributes`, `.gitignore`, `.git`. Unexported: the
// list is IsGitMetaPath's implementation detail, and exporting a mutable
// package-level slice would let any importer edit a security predicate.
var gitMetaNames = []string{".gitattributes", ".gitignore", ".git"}

// IsGitMetaPath reports whether any slash-separated segment of the
// slash-separated rel is a git metadata name, compared case-insensitively.
//
// SECURITY (spec §5, absolute invariant): a `.gitattributes` below the
// checkout root overrides the root attributes for its subtree by git's
// deepest-file-wins precedence. A single `* -filter` line UNSELECTS the
// encryption clean filter — so no filter runs, `filter.agentbrain.required`
// never fires (required only errors when a SELECTED filter fails), and
// sibling memory files commit as PLAINTEXT and push to the remote in the
// clear. A `.gitignore` sibling silently stops files syncing; a `.git`
// segment embeds a gitlink or nested repo.
//
// This predicate lives in repo, not engine, because two packages must agree
// on it exactly: the engine ENFORCES it (mirror-in refusal, checkout scrub,
// mirror-out refusal, admin-op preamble) and doctor OBSERVES it (the
// advisory git-meta check). Two copies of a security predicate are two
// predicates, and the one that drifts is the hole.
//
// EqualFold because case-insensitive filesystems (macOS) resolve
// `.GITATTRIBUTES` when git opens `.gitattributes`. Callers pass paths
// relative to the scope they are guarding, so a caller working
// unit-relative never sees the legitimate root `.gitattributes`; callers
// walking the checkout root (the scrub, doctor) exempt that one path
// explicitly.
//
// Whole-segment matching is deliberate: `.gitmodules` and `.github` are
// non-matches (git reads .gitmodules only at the repository root, which a
// unit-relative path can never name), and over-matching would silently stop
// legitimate files syncing — its own spec violation.
func IsGitMetaPath(rel string) bool {
	for segment := range strings.SplitSeq(rel, "/") {
		if slices.ContainsFunc(gitMetaNames, func(meta string) bool {
			return strings.EqualFold(segment, meta)
		}) {
			return true
		}
	}
	return false
}
