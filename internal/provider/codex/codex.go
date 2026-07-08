// Package codex adapts OpenAI Codex CLI's user-global memory to the
// provider contract (spec §6). Ships experimental (ADR 02): the layout
// is partly third-party-documented, so the classification table is
// config-overridable and the adapter mirrors ONLY the two memory roots —
// never $CODEX_HOME itself, which holds credentials (auth.json).
package codex

import (
	"context"
	"os"
	"path/filepath"

	"github.com/Sawmonabo/agent-brain/internal/provider"
)

// Adapter implements provider.Provider for Codex CLI's user-global
// memory (spec §6, ADR 02).
type Adapter struct {
	codexHome string
	patterns  []provider.Pattern
}

// New constructs a codex Adapter. codexHome is $CODEX_HOME (the
// composition root defaults it to <home>/.codex when the env var is
// unset — see cli.buildRegistry). overrides, when non-empty, REPLACES
// the built-in classification table wholesale rather than patching it:
// overriding means owning the whole table (spec §6 — Codex's on-disk
// layout is partly third-party-documented, so upstream format drift
// must be absorbable without a release). nil or empty overrides keep
// the built-in table.
func New(codexHome string, overrides []provider.Pattern) *Adapter {
	patterns := builtinPatterns()
	if len(overrides) > 0 {
		patterns = overrides
	}
	return &Adapter{codexHome: codexHome, patterns: patterns}
}

// Name returns the stable adapter identifier used in repo paths
// (_global/codex/) and registries.
func (a *Adapter) Name() string { return "codex" }

// Scope reports that Codex memory is user-global, not per-project
// (ADR 02) — it mirrors into repo.GlobalFolder as one pseudo-project.
func (a *Adapter) Scope() provider.Scope { return provider.ScopeGlobal }

// Patterns returns the active classification table: the built-in table,
// or the caller's override wholesale (see New).
func (a *Adapter) Patterns() []provider.Pattern { return a.patterns }

// builtinPatterns is Codex's default classification table (spec §6).
// A non-empty overrides argument to New replaces this wholesale.
func builtinPatterns() []provider.Pattern {
	return []provider.Pattern{
		{Glob: "memories/raw_memories.md", Class: provider.ClassFact}, // append-mostly log
		{Glob: "memories/memory_summary.md", Class: provider.ClassRegenerated},
		{Glob: "memories/MEMORY.md", Class: provider.ClassRegenerated},
		{Glob: "memories/rollout_summaries/**", Class: provider.ClassRegenerated},
		{Glob: "memories/skills/**/SKILL.md", Class: provider.ClassFact},
		{Glob: "chronicle/**", Class: provider.ClassFact},
		{Glob: ".DS_Store", Class: provider.ClassIgnore},
		{Glob: "**/.DS_Store", Class: provider.ClassIgnore},
		// Unmatched → ClassFact via Classify's default: correct here
		// because units scope mirroring to the two roots above.
	}
}

// Discover reports whichever of Codex's two rescue-worthy roots exist on
// this machine — memories/ and memories_extensions/chronicle/ — as
// separate RepoSubdir units (mapping to _global/codex/memories and
// _global/codex/chronicle, spec §3). $CODEX_HOME itself is never a
// root: auth.json lives there, and this adapter must not go near it
// (spec §5 secret-adjacency). A missing root — either or both — is not
// an error: Codex may not be installed, or the memories/chronicle
// features may never have been enabled on this machine.
func (a *Adapter) Discover(_ context.Context) ([]provider.Discovered, error) {
	roots := []struct{ local, sub, label string }{
		{filepath.Join(a.codexHome, "memories"), "memories", "codex memories"},
		{filepath.Join(a.codexHome, "memories_extensions", "chronicle"), "chronicle", "codex chronicle"},
	}
	var found []provider.Discovered
	for _, r := range roots {
		if info, err := os.Stat(r.local); err == nil && info.IsDir() {
			found = append(found, provider.Discovered{LocalDir: r.local, RepoSubdir: r.sub, Label: r.label})
		}
	}
	return found, nil
}

// Identify always returns the zero Identity: Codex memory is
// user-global, so there is no per-project identity to resolve — the
// enrolled unit's folder is repo.GlobalFolder by construction at
// enrollment, not something Identify derives.
func (a *Adapter) Identify(_ context.Context, _ provider.Discovered, _ string) (provider.Identity, error) {
	return provider.Identity{}, nil
}

// ReconcileIndex is a no-op. Codex's own background consolidator owns
// memory_summary.md, MEMORY.md, and rollout_summaries/* — they are
// class-Regenerated precisely because Codex, not this adapter,
// regenerates them.
func (a *Adapter) ReconcileIndex(_ context.Context, _ string) error {
	return nil
}
