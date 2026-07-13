package lint_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/links"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/lint"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
	"github.com/Sawmonabo/agent-brain/internal/provider"
)

// writeMemoryFile creates a real file under dir. The frontmatter and
// index-drift rules read through memoryfs.Meta/memoryfs.ReadBody, which
// open an actual path — unlike the dangling-link and stale rules (pure
// Memory-field and in-memory-body checks), these two cannot be exercised
// through a synthetic readBody map alone.
func writeMemoryFile(t *testing.T, dir, relPath, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// noBody is a readBody stub for fixtures that never exercise index-drift.
func noBody(memoryfs.Memory) (string, error) { return "", nil }

// emptyIndex is a links.Index with nothing registered, for fixtures that
// only exercise rules other than dangling-link.
func emptyIndex() *links.Index {
	return links.BuildIndex(nil, noBody)
}

// issuesFor returns m's Issues from results, or nil if m has no Result —
// Check only returns memories WITH at least one issue, so "want nil" and
// "memory absent from Results" are the same assertion.
func issuesFor(results []lint.Result, m memoryfs.Memory) []lint.Issue {
	for _, r := range results {
		if r.Memory.RepoPath == m.RepoPath {
			return r.Issues
		}
	}
	return nil
}

// TestCheckFrontmatterRule pins the claude-only, fact-class-only
// frontmatter completeness rule and its three exact Detail strings.
func TestCheckFrontmatterRule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		content    string
		provider   string
		class      provider.Class
		wantIssues []lint.Issue
	}{
		{
			name:       "missing frontmatter entirely",
			content:    "just prose, no fences",
			provider:   "claude",
			class:      provider.ClassFact,
			wantIssues: []lint.Issue{{Rule: "frontmatter", Detail: "missing frontmatter"}},
		},
		{
			name:       "frontmatter present but name blank",
			content:    "---\nname:\ndescription: has a hook\n---\nbody",
			provider:   "claude",
			class:      provider.ClassFact,
			wantIssues: []lint.Issue{{Rule: "frontmatter", Detail: "frontmatter missing name"}},
		},
		{
			name:       "frontmatter present but description blank",
			content:    "---\nname: topic\ndescription:\n---\nbody",
			provider:   "claude",
			class:      provider.ClassFact,
			wantIssues: []lint.Issue{{Rule: "frontmatter", Detail: "frontmatter missing description"}},
		},
		{
			name:     "frontmatter present but both name and description blank",
			content:  "---\nname:\ndescription:\n---\nbody",
			provider: "claude",
			class:    provider.ClassFact,
			wantIssues: []lint.Issue{
				{Rule: "frontmatter", Detail: "frontmatter missing name"},
				{Rule: "frontmatter", Detail: "frontmatter missing description"},
			},
		},
		{
			name:       "complete frontmatter has no issues",
			content:    "---\nname: topic\ndescription: a hook\n---\nbody",
			provider:   "claude",
			class:      provider.ClassFact,
			wantIssues: nil,
		},
		{
			name:       "non-claude provider is exempt from the frontmatter rule",
			content:    "just prose, no fences",
			provider:   "codex",
			class:      provider.ClassFact,
			wantIssues: nil,
		},
		{
			name:       "claude derived-index class is exempt from the frontmatter rule",
			content:    "just prose, no fences",
			provider:   "claude",
			class:      provider.ClassDerivedIndex,
			wantIssues: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeMemoryFile(t, dir, "topic.md", tt.content)
			m := memoryfs.Memory{
				Provider: tt.provider, LocalDir: dir, RelPath: "topic.md", RepoPath: tt.provider + "/topic.md",
				Name: "topic", Class: tt.class, ModTime: time.Now(),
			}

			results := lint.Check([]memoryfs.Memory{m}, emptyIndex(), noBody, 0, time.Now())

			if diff := cmp.Diff(tt.wantIssues, issuesFor(results, m)); diff != "" {
				t.Errorf("frontmatter issues diff (-want +got):\n%s", diff)
			}
		})
	}
}

// TestCheckDanglingLinkRule pins the "one Issue per target" contract
// (including a target dangling twice yielding two Issues) and the exact
// Detail wording.
func TestCheckDanglingLinkRule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		body       string
		wantIssues []lint.Issue
	}{
		{
			name:       "single dangling link",
			body:       "See [[ghost]].",
			wantIssues: []lint.Issue{{Rule: "dangling-link", Detail: "[[ghost]] resolves to no memory in this project"}},
		},
		{
			name: "same dangling target occurring twice yields two issues",
			body: "See [[ghost]] and [[ghost]] again.",
			wantIssues: []lint.Issue{
				{Rule: "dangling-link", Detail: "[[ghost]] resolves to no memory in this project"},
				{Rule: "dangling-link", Detail: "[[ghost]] resolves to no memory in this project"},
			},
		},
		{
			name:       "no dangling links produces no issues",
			body:       "just prose, no links",
			wantIssues: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Provider "codex" keeps this fixture clear of the
			// claude-only frontmatter/index-drift rules so only
			// dangling-link is in play.
			memA := memoryfs.Memory{Provider: "codex", RepoPath: "codex/a.md", RelPath: "a.md", Name: "a", Class: provider.ClassFact}
			bodies := map[string]string{memA.RepoPath: tt.body}
			readBody := func(m memoryfs.Memory) (string, error) { return bodies[m.RepoPath], nil }
			index := links.BuildIndex([]memoryfs.Memory{memA}, readBody)

			results := lint.Check([]memoryfs.Memory{memA}, index, readBody, 0, time.Now())

			if diff := cmp.Diff(tt.wantIssues, issuesFor(results, memA)); diff != "" {
				t.Errorf("dangling-link issues diff (-want +got):\n%s", diff)
			}
		})
	}
}

// TestCheckStaleRule pins the strict-greater-than boundary (unmodified for
// exactly staleAfterDays is NOT yet stale) and that staleAfterDays == 0
// disables the rule regardless of age.
func TestCheckStaleRule(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name           string
		modTime        time.Time
		staleAfterDays int
		wantIssues     []lint.Issue
	}{
		{
			name:           "unmodified exactly at the threshold is not yet stale",
			modTime:        now.Add(-90 * 24 * time.Hour),
			staleAfterDays: 90,
			wantIssues:     nil,
		},
		{
			name:           "unmodified one day past the threshold is stale",
			modTime:        now.Add(-91 * 24 * time.Hour),
			staleAfterDays: 90,
			wantIssues:     []lint.Issue{{Rule: "stale", Detail: "unmodified for 91 days"}},
		},
		{
			name:           "staleAfterDays of zero disables the rule regardless of age",
			modTime:        now.Add(-10000 * 24 * time.Hour),
			staleAfterDays: 0,
			wantIssues:     nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			memA := memoryfs.Memory{Provider: "codex", RepoPath: "codex/a.md", RelPath: "a.md", Name: "a", Class: provider.ClassFact, ModTime: tt.modTime}

			results := lint.Check([]memoryfs.Memory{memA}, emptyIndex(), noBody, tt.staleAfterDays, now)

			if diff := cmp.Diff(tt.wantIssues, issuesFor(results, memA)); diff != "" {
				t.Errorf("stale issues diff (-want +got):\n%s", diff)
			}
		})
	}
}

// TestCheckIndexDriftRule pins both drift directions in one claude unit:
// a fact file absent from MEMORY.md, and a MEMORY.md line naming a file
// that no longer exists. The index body mirrors claude/reconcile.go's
// exact rendered link shape ("- [title](file.md)").
func TestCheckIndexDriftRule(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeMemoryFile(t, dir, "present.md", "---\nname: present\ndescription: a hook\n---\nbody")
	writeMemoryFile(t, dir, "orphan.md", "---\nname: orphan\ndescription: a hook\n---\nbody")
	indexBody := "# Memory index\n\n" +
		"- [present](present.md)\n" +
		"- [Ghost](ghost.md)\n"
	writeMemoryFile(t, dir, "MEMORY.md", indexBody)

	indexMemory := memoryfs.Memory{Provider: "claude", LocalDir: dir, RelPath: "MEMORY.md", RepoPath: "claude/MEMORY.md", Name: "MEMORY", Class: provider.ClassDerivedIndex}
	present := memoryfs.Memory{Provider: "claude", LocalDir: dir, RelPath: "present.md", RepoPath: "claude/present.md", Name: "present", Class: provider.ClassFact}
	orphan := memoryfs.Memory{Provider: "claude", LocalDir: dir, RelPath: "orphan.md", RepoPath: "claude/orphan.md", Name: "orphan", Class: provider.ClassFact}
	memories := []memoryfs.Memory{indexMemory, present, orphan}

	index := links.BuildIndex(memories, memoryfs.ReadBody)
	results := lint.Check(memories, index, memoryfs.ReadBody, 0, time.Now())

	if len(results) != 2 {
		t.Fatalf("Check() returned %d results, want 2 (orphan.md and MEMORY.md only): %+v", len(results), results)
	}
	if diff := cmp.Diff([]lint.Issue{{Rule: "index-drift", Detail: "absent from MEMORY.md"}}, issuesFor(results, orphan)); diff != "" {
		t.Errorf("orphan.md issues diff (-want +got):\n%s", diff)
	}
	if got := issuesFor(results, present); got != nil {
		t.Errorf("present.md issues = %+v, want nil (correctly indexed)", got)
	}
	if diff := cmp.Diff([]lint.Issue{{Rule: "index-drift", Detail: "MEMORY.md links missing file ghost.md"}}, issuesFor(results, indexMemory)); diff != "" {
		t.Errorf("MEMORY.md issues diff (-want +got):\n%s", diff)
	}
}

// TestCheckIndexDriftRuleIsClaudeOnly pins that index-drift never runs for
// a non-claude unit, even one whose own derived-index file happens to be
// named "MEMORY.md" too — the rule specifically mirrors
// claude/reconcile.go's rendered format, which no other provider shares.
func TestCheckIndexDriftRuleIsClaudeOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeMemoryFile(t, dir, "topic.md", "prose, no frontmatter needed: codex is exempt from that rule too")
	writeMemoryFile(t, dir, "MEMORY.md", "# Memory index\n\nstale hand-written notes, never parsed as an index for codex\n")

	topic := memoryfs.Memory{Provider: "codex", LocalDir: dir, RelPath: "topic.md", RepoPath: "codex/topic.md", Name: "topic", Class: provider.ClassFact}
	indexMemory := memoryfs.Memory{Provider: "codex", LocalDir: dir, RelPath: "MEMORY.md", RepoPath: "codex/MEMORY.md", Name: "MEMORY", Class: provider.ClassRegenerated}
	memories := []memoryfs.Memory{topic, indexMemory}

	results := lint.Check(memories, links.BuildIndex(memories, memoryfs.ReadBody), memoryfs.ReadBody, 0, time.Now())

	if len(results) != 0 {
		t.Fatalf("Check() = %+v, want no issues (codex is exempt from index-drift)", results)
	}
}

// TestCheckCleanFixtureYieldsNoIssues proves a fully healthy project —
// complete frontmatter, no dangling links, fresh mtimes, and a MEMORY.md
// that exactly matches its unit's fact files — produces an empty result.
func TestCheckCleanFixtureYieldsNoIssues(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeMemoryFile(t, dir, "alpha.md", "---\nname: alpha\ndescription: a hook\n---\nSee [[beta]].\n")
	writeMemoryFile(t, dir, "beta.md", "---\nname: beta\ndescription: another hook\n---\nbody\n")
	writeMemoryFile(t, dir, "MEMORY.md", "# Memory index\n\n"+
		"- [alpha](alpha.md) — a hook\n"+
		"- [beta](beta.md) — another hook\n")

	now := time.Now()
	alpha := memoryfs.Memory{Provider: "claude", LocalDir: dir, RelPath: "alpha.md", RepoPath: "claude/alpha.md", Name: "alpha", Class: provider.ClassFact, ModTime: now}
	beta := memoryfs.Memory{Provider: "claude", LocalDir: dir, RelPath: "beta.md", RepoPath: "claude/beta.md", Name: "beta", Class: provider.ClassFact, ModTime: now}
	indexMemory := memoryfs.Memory{Provider: "claude", LocalDir: dir, RelPath: "MEMORY.md", RepoPath: "claude/MEMORY.md", Name: "MEMORY", Class: provider.ClassDerivedIndex, ModTime: now}
	memories := []memoryfs.Memory{alpha, beta, indexMemory}

	index := links.BuildIndex(memories, memoryfs.ReadBody)
	results := lint.Check(memories, index, memoryfs.ReadBody, 90, now)

	if results != nil {
		t.Fatalf("Check() = %+v, want nil (nothing wrong with this fixture)", results)
	}
}

// TestCheckResultsAreSortedByRepoPath proves Check's output order is
// deterministic — sorted by RepoPath — regardless of input order.
func TestCheckResultsAreSortedByRepoPath(t *testing.T) {
	t.Parallel()
	now := time.Now()
	old := now.Add(-1000 * 24 * time.Hour)
	memZ := memoryfs.Memory{Provider: "codex", RepoPath: "codex/z.md", RelPath: "z.md", Class: provider.ClassFact, ModTime: old}
	memA := memoryfs.Memory{Provider: "codex", RepoPath: "codex/a.md", RelPath: "a.md", Class: provider.ClassFact, ModTime: old}

	results := lint.Check([]memoryfs.Memory{memZ, memA}, emptyIndex(), noBody, 90, now)

	if len(results) != 2 {
		t.Fatalf("Check() returned %d results, want 2", len(results))
	}
	if results[0].Memory.RepoPath != memA.RepoPath || results[1].Memory.RepoPath != memZ.RepoPath {
		t.Fatalf("Check() order = [%s, %s], want [%s, %s]",
			results[0].Memory.RepoPath, results[1].Memory.RepoPath, memA.RepoPath, memZ.RepoPath)
	}
}

// TestCheckAccumulatesMultipleRuleViolationsOnOneMemory proves a memory
// failing more than one rule collects every Issue, in the brief's rule
// enumeration order (frontmatter, dangling-link, stale), not just the
// first rule that happened to match.
func TestCheckAccumulatesMultipleRuleViolationsOnOneMemory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeMemoryFile(t, dir, "bad.md", "no frontmatter, and a [[ghost]] link")

	now := time.Now()
	m := memoryfs.Memory{
		Provider: "claude", LocalDir: dir, RelPath: "bad.md", RepoPath: "claude/bad.md",
		Name: "bad", Class: provider.ClassFact, ModTime: now.Add(-1000 * 24 * time.Hour),
	}
	memories := []memoryfs.Memory{m}
	index := links.BuildIndex(memories, memoryfs.ReadBody)

	results := lint.Check(memories, index, memoryfs.ReadBody, 90, now)

	want := []lint.Issue{
		{Rule: "frontmatter", Detail: "missing frontmatter"},
		{Rule: "dangling-link", Detail: "[[ghost]] resolves to no memory in this project"},
		{Rule: "stale", Detail: "unmodified for 1000 days"},
	}
	if diff := cmp.Diff(want, issuesFor(results, m)); diff != "" {
		t.Errorf("accumulated issues diff (-want +got):\n%s", diff)
	}
}
