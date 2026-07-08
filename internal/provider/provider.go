// Package provider defines the contract every memory-provider adapter
// implements, plus the class/pattern model driving merge policy and
// .gitattributes generation (spec §6; ADRs 02/03). Phase 2 ships the
// contract and a test fake; the claude/codex adapters are Phase 3.
// DiscoverProjects/ResolveIdentity (spec §6) join the interface in
// Phase 3 alongside enrollment, their first consumer.
package provider

import "context"

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
