// Package lint runs advisory checks over a project's memories (spec §8):
// frontmatter completeness, dangling wiki-links, staleness, and (for the
// claude provider) index-drift against MEMORY.md. Lint is advisory only —
// like scan (internal/cli/scan.go), it never joins SafetyGate and never
// blocks sync — so the dashboard hub's memory browser (Task 11+) can badge
// issues without any mutation path depending on this package. It imports
// only memoryfs + links + stdlib (package-boundary rule, spec §8): no
// bubbletea, no lipgloss, no engine/daemon/cli-root.
package lint

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/links"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
)

// factClassName is provider.ClassFact's String() value. This package stays
// outside the provider-import boundary (memoryfs + links + stdlib only),
// so a memory's class is compared against the stable String() interchange
// form (documented in internal/provider/provider.go as exactly this kind
// of external, decoupled comparison) rather than importing provider.Class
// directly.
const factClassName = "fact"

// Issue is one advisory finding attached to a memory.
type Issue struct {
	Rule   string // "frontmatter", "dangling-link", "stale", "index-drift"
	Detail string // human sentence naming the specifics
}

// Result pairs a memory with the issues found for it.
type Result struct {
	Memory memoryfs.Memory
	Issues []Issue
}

// Check runs every advisory rule over memories and returns one Result per
// memory that has at least one Issue — a memory with nothing wrong is
// simply absent from the returned slice, and the browser badges any memory
// present in it. Results are sorted by RepoPath, independent of the order
// memories arrives in.
//
// index answers the dangling-link rule's lookups (built by the caller via
// links.BuildIndex, typically once per browser refresh). readBody is the
// same seam BuildIndex takes — Check's own use is narrow: reading a
// claude unit's MEMORY.md body for the index-drift rule. now is injected
// so the stale rule is deterministic in tests, never wall-clock-dependent.
func Check(memories []memoryfs.Memory, index *links.Index, readBody func(memoryfs.Memory) (string, error), staleAfterDays int, now time.Time) []Result {
	acc := newAccumulator()
	for _, m := range memories {
		checkFrontmatter(acc, m)
		checkDanglingLinks(acc, index, m)
		checkStale(acc, m, staleAfterDays, now)
	}
	checkIndexDrift(acc, memories, readBody)
	return acc.results()
}

// isFactMarkdown reports whether m is a fact-class ".md" file — the
// baseline predicate both the frontmatter and index-drift rules narrow
// further (to claude-only, and to claude's top-level files, respectively).
func isFactMarkdown(m memoryfs.Memory) bool {
	return m.Class.String() == factClassName && strings.HasSuffix(m.RelPath, ".md")
}

// isClaudeFactMarkdown reports whether m is a claude-provider fact-class
// ".md" file — the frontmatter rule's exact scope (spec §6: only claude's
// memory convention specifies frontmatter at all).
func isClaudeFactMarkdown(m memoryfs.Memory) bool {
	return m.Provider == "claude" && isFactMarkdown(m)
}

// isTopLevelFactMarkdown reports whether m is a fact-class ".md" file
// directly under its unit's LocalDir — the same shape
// internal/provider/claude/reconcile.go itself walks (os.ReadDir,
// non-recursive, "*.md" except "MEMORY.md") — so index-drift only ever
// compares files ReconcileIndex would actually have indexed.
func isTopLevelFactMarkdown(m memoryfs.Memory) bool {
	return isFactMarkdown(m) && !strings.Contains(m.RelPath, "/")
}

// fullPath mirrors memoryfs's own unexported localPath: the join is a
// one-line composition, not logic worth crossing memoryfs's package
// boundary for (memoryfs.go's own classifyRel doc comment establishes the
// same precedent for a one-line duplication over a cross-boundary import).
func fullPath(m memoryfs.Memory) string {
	return filepath.Join(m.LocalDir, filepath.FromSlash(m.RelPath))
}

// checkFrontmatter applies the frontmatter rule: a claude-provider
// fact-class ".md" file must have a frontmatter block with non-empty name
// and description. name/description are re-read via memoryfs.Meta rather
// than trusted off the Memory struct, because List already backfills
// Memory.Name to the filename stem when frontmatter's name is blank — the
// very condition this rule needs to detect.
func checkFrontmatter(acc *accumulator, m memoryfs.Memory) {
	if !isClaudeFactMarkdown(m) {
		return
	}
	name, description, hasFrontmatter := memoryfs.Meta(fullPath(m))
	if !hasFrontmatter {
		acc.add(m, Issue{Rule: "frontmatter", Detail: "missing frontmatter"})
		return
	}
	if name == "" {
		acc.add(m, Issue{Rule: "frontmatter", Detail: "frontmatter missing name"})
	}
	if description == "" {
		acc.add(m, Issue{Rule: "frontmatter", Detail: "frontmatter missing description"})
	}
}

// checkDanglingLinks applies the dangling-link rule: one Issue per
// unresolved [[target]] found in m's own body, in Parse's order (a target
// dangling twice yields two Issues).
func checkDanglingLinks(acc *accumulator, index *links.Index, m memoryfs.Memory) {
	for _, link := range index.Dangling(m) {
		acc.add(m, Issue{Rule: "dangling-link", Detail: fmt.Sprintf("[[%s]] resolves to no memory in this project", link.Target)})
	}
}

// checkStale applies the staleness rule: staleAfterDays <= 0 disables it
// entirely; otherwise a memory unmodified for strictly more than that many
// days (an exact match at the boundary is not yet stale) is flagged with
// the actual elapsed day count, not the configured threshold.
func checkStale(acc *accumulator, m memoryfs.Memory, staleAfterDays int, now time.Time) {
	if staleAfterDays <= 0 {
		return
	}
	const day = 24 * time.Hour
	threshold := time.Duration(staleAfterDays) * day
	elapsed := now.Sub(m.ModTime)
	if elapsed <= threshold {
		return
	}
	elapsedDays := int(elapsed / day)
	acc.add(m, Issue{Rule: "stale", Detail: fmt.Sprintf("unmodified for %d days", elapsedDays)})
}

// checkIndexDrift applies the index-drift rule, claude units only: it
// groups memories by (Provider, LocalDir) unit, and for every unit that
// currently has a MEMORY.md memory, parses that body's rendered links and
// compares them against the unit's own top-level fact ".md" files in both
// directions. A unit with no MEMORY.md memory in this snapshot (not yet
// synced, or genuinely empty) is skipped — reconcile.go's own doc comment
// establishes that "no topic files, no existing index" is the ordinary,
// not-yet-drifted state, and lint extends that same tolerance to "index
// not present in this snapshot yet" rather than manufacturing a drift
// finding out of a timing gap. A readBody error for one unit's MEMORY.md
// skips only that unit, matching links.BuildIndex's own fail-soft posture.
func checkIndexDrift(acc *accumulator, memories []memoryfs.Memory, readBody func(memoryfs.Memory) (string, error)) {
	type unitKey struct{ provider, localDir string }
	units := make(map[unitKey][]memoryfs.Memory)
	for _, m := range memories {
		if m.Provider != "claude" {
			continue
		}
		key := unitKey{m.Provider, m.LocalDir}
		units[key] = append(units[key], m)
	}

	for _, unitMemories := range units {
		var indexMemory memoryfs.Memory
		haveIndex := false
		for _, m := range unitMemories {
			if m.RelPath == "MEMORY.md" {
				indexMemory, haveIndex = m, true
				break
			}
		}
		if !haveIndex {
			continue
		}
		body, err := readBody(indexMemory)
		if err != nil {
			continue
		}
		linkedFiles := parseIndexLinks(body)
		linkedSet := make(map[string]struct{}, len(linkedFiles))
		for _, file := range linkedFiles {
			linkedSet[file] = struct{}{}
		}

		factRelPaths := make(map[string]struct{})
		for _, m := range unitMemories {
			if !isTopLevelFactMarkdown(m) {
				continue
			}
			factRelPaths[m.RelPath] = struct{}{}
			if _, ok := linkedSet[m.RelPath]; !ok {
				acc.add(m, Issue{Rule: "index-drift", Detail: "absent from MEMORY.md"})
			}
		}
		for _, file := range linkedFiles {
			if _, ok := factRelPaths[file]; !ok {
				acc.add(indexMemory, Issue{Rule: "index-drift", Detail: fmt.Sprintf("MEMORY.md links missing file %s", file)})
			}
		}
	}
}

// parseIndexLinks extracts every markdown-link target from body, mirroring
// claude/reconcile.go's exact rendered shape: each index line reads
// "- [title](file.md)" or "- [title](file.md) — hook", so the linked
// filename is the substring between the first "](" on a line and the next
// ")". Lines with neither (the "# Memory index" header, blank lines) are
// skipped naturally: strings.Cut reports no match, not a bogus zero-length
// target.
func parseIndexLinks(body string) []string {
	var files []string
	for line := range strings.SplitSeq(body, "\n") {
		_, rest, ok := strings.Cut(line, "](")
		if !ok {
			continue
		}
		file, _, ok := strings.Cut(rest, ")")
		if !ok {
			continue
		}
		files = append(files, file)
	}
	return files
}

// accumulator collects Issues per memory, keyed by RepoPath — the same
// identity key links.Index uses — so index-drift's second pass can attach
// an Issue to MEMORY.md's own Result without disturbing whatever the
// per-memory pass already recorded for it.
type accumulator struct {
	order  []string
	byPath map[string]*Result
}

func newAccumulator() *accumulator {
	return &accumulator{byPath: make(map[string]*Result)}
}

func (a *accumulator) add(m memoryfs.Memory, issue Issue) {
	result, ok := a.byPath[m.RepoPath]
	if !ok {
		result = &Result{Memory: m}
		a.byPath[m.RepoPath] = result
		a.order = append(a.order, m.RepoPath)
	}
	result.Issues = append(result.Issues, issue)
}

// results returns every memory with at least one issue, sorted by
// RepoPath — deterministic and independent of map iteration order or the
// caller's input order.
func (a *accumulator) results() []Result {
	if len(a.order) == 0 {
		return nil
	}
	sortedPaths := slices.Clone(a.order)
	slices.Sort(sortedPaths)
	out := make([]Result, 0, len(sortedPaths))
	for _, repoPath := range sortedPaths {
		out = append(out, *a.byPath[repoPath])
	}
	return out
}
