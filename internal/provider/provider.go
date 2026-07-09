// Package provider defines the contract every memory-provider adapter
// implements, plus the class/pattern model driving merge policy and
// .gitattributes generation (spec §6; ADRs 02/03). Phase 2 shipped the
// contract and a test fake; the claude/codex adapters are Phase 3.
// Discover/Identify (spec §6) join the interface in Phase 3 alongside
// enrollment, their first consumer.
package provider

import (
	"context"
	"fmt"
)

// Class is a file's merge-policy class (spec §3 "File classes").
type Class int

const (
	// ClassFact merges 3-way with retain-both on overlap — the default
	// for anything unrecognized: data is never dropped, never newest-wins.
	ClassFact Class = iota
	// ClassDerivedIndex rides the fact driver at merge time; the engine's
	// reconcile step regenerates it afterward (spec §4 step 4).
	ClassDerivedIndex
	// ClassRegenerated is provider-rebuildable output — newest-wins
	// (merge=agentbrain-lww in generated attributes).
	ClassRegenerated
	// ClassIgnore is never mirrored into the repo (locks, scratch).
	ClassIgnore
)

func (c Class) String() string {
	switch c {
	case ClassFact:
		return "fact"
	case ClassDerivedIndex:
		return "derived-index"
	case ClassRegenerated:
		return "regenerated"
	case ClassIgnore:
		return "ignore"
	default:
		return "unknown"
	}
}

// ClassFromString parses s — one of the exact String() values ("fact",
// "derived-index", "regenerated", "ignore") — back into a Class. It is
// the inverse of String(), used to validate and load config-overridable
// classification tables (spec §6): an adapter's classify overrides
// travel through config.toml as strings, never as the unexported Class
// int, so loading them must reject anything String() would not itself
// produce.
func ClassFromString(s string) (Class, error) {
	switch s {
	case ClassFact.String():
		return ClassFact, nil
	case ClassDerivedIndex.String():
		return ClassDerivedIndex, nil
	case ClassRegenerated.String():
		return ClassRegenerated, nil
	case ClassIgnore.String():
		return ClassIgnore, nil
	default:
		return 0, fmt.Errorf("class %q: not one of %q, %q, %q, %q",
			s, ClassFact, ClassDerivedIndex, ClassRegenerated, ClassIgnore)
	}
}

// Scope is where a provider keeps memory (ADR 02).
type Scope int

const (
	// ScopePerProject providers key memory by project (Claude Code).
	ScopePerProject Scope = iota
	// ScopeGlobal providers keep one user-global pool (Codex → _global/).
	ScopeGlobal
)

func (s Scope) String() string {
	switch s {
	case ScopePerProject:
		return "per-project"
	case ScopeGlobal:
		return "global"
	default:
		return "unknown"
	}
}

// Pattern binds a slash-separated glob (relative to a memory root) to a
// Class. Tables are ordered; the first matching pattern wins.
type Pattern struct {
	Glob  string
	Class Class
}

// Discovered is one enrollable memory root an adapter found on this
// machine. For per-project providers each project yields one entry; a
// global provider may yield several (one per RepoSubdir root).
type Discovered struct {
	// LocalDir is the absolute local memory root to mirror/watch.
	LocalDir string
	// RepoSubdir is the slash-separated subdir under <folder>/<provider>/
	// this root maps to ("" = the provider dir itself). Mirrors
	// repo.Unit.RepoSubdir.
	RepoSubdir string
	// Label is what the enrollment picker shows (a slug, "codex memories", …).
	Label string
	// PathGuess is the adapter's best guess at the PROJECT path the memory
	// belongs to (per-project scope; "" for global). The picker shows it
	// for confirmation — it is a GUESS (slug reversal is lossy).
	PathGuess string
}

// Identity is the cross-machine binding for one discovered root
// (spec §3 "Project identity"). Global-scope providers return the zero
// Identity — their folder is repo.GlobalFolder by construction.
type Identity struct {
	// ProjectID is the canonical machine-independent id
	// (host/owner/repo from the normalized git remote), or "" when the
	// project has no remote — the caller must then ask the user to name
	// the folder and uses named/<folder> as the id.
	ProjectID string
	// PreferredFolder is the repo folder to propose (repo basename).
	PreferredFolder string
}

// Provider is the Phase-2 adapter contract. Implementations must be
// safe for concurrent use by multiple goroutines (the daemon reads them
// from its API server while the engine syncs).
type Provider interface {
	// Name is the stable identifier used in repo paths (<project>/<name>/)
	// and registries. Lowercase, path-safe, never empty.
	Name() string
	// Scope reports whether memory is per-project or user-global.
	Scope() Scope
	// Patterns returns the ordered classification table. One table, two
	// consumers: Classify (mirror decisions) and repo attribute
	// generation (merge-driver wiring).
	Patterns() []Pattern
	// ReconcileIndex deterministically rebuilds the provider's derived
	// index inside dir — a <project>/<provider>/ dir in the checkout —
	// after integrate (spec §4 step 4). No-op when nothing applies.
	ReconcileIndex(ctx context.Context, dir string) error
	// Discover enumerates this machine's enrollable memory roots. Roots
	// already enrolled are included — the caller filters against its
	// registry (the adapter is stateless).
	Discover(ctx context.Context) ([]Discovered, error)
	// Identify resolves a Discovered root to its cross-machine identity,
	// confirming/deriving the project path (reads the git remote for
	// per-project scope). projectPath is the user-confirmed project
	// directory (equal to d.PathGuess unless the user corrected it).
	Identify(ctx context.Context, d Discovered, projectPath string) (Identity, error)
}

// Classify resolves rel (slash-separated, relative to the memory root)
// against p's table. Unmatched paths are ClassFact — the safest default
// for data (spec §6).
func Classify(p Provider, rel string) Class {
	for _, pat := range p.Patterns() {
		if Match(pat.Glob, rel) {
			return pat.Class
		}
	}
	return ClassFact
}
