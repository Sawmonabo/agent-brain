package engine

import (
	"context"
	"fmt"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// reconcile runs each provider's derived-index reconciliation (spec §4
// step 4) on every non-degraded unit. Reconcilers are deterministic; a
// failure is a bug or corrupted state and fails the cycle loudly.
// ReconcileIndex always sees the whole PROVIDER dir (e.layout.UnitDir),
// never a unit's RepoSubdir slice — the index spans every root a global
// provider maps into that dir (codex: memories + chronicle, spec §3) —
// so a (folder, provider) pair sharing several RepoSubdir units
// reconciles exactly once per cycle, not once per unit.
func (e *Engine) reconcile(ctx context.Context, units []repo.Unit, skip map[string]bool) error {
	seen := map[string]bool{}
	for _, u := range units {
		key := u.Folder + "/" + u.Provider
		if skip[u.Folder] || seen[key] {
			continue
		}
		seen[key] = true
		prov, ok := e.registry.Get(u.Provider)
		if !ok {
			return fmt.Errorf("reconcile %s: provider %q not registered", u.Folder, u.Provider)
		}
		if err := prov.ReconcileIndex(ctx, e.layout.UnitDir(u.Folder, u.Provider)); err != nil {
			return fmt.Errorf("reconcile %s/%s: %w", u.Folder, u.Provider, err)
		}
	}
	return nil
}
