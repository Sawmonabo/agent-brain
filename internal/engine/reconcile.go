package engine

import (
	"context"
	"fmt"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// reconcile runs each provider's derived-index reconciliation (spec §4
// step 4) on every non-degraded unit. Reconcilers are deterministic; a
// failure is a bug or corrupted state and fails the cycle loudly.
func (e *Engine) reconcile(ctx context.Context, units []repo.Unit, skip map[string]bool) error {
	for _, u := range units {
		if skip[u.Folder] {
			continue
		}
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
