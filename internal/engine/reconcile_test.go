package engine

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func TestReconcileCallsEachUnitAndSkipsDegraded(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	alpha, beta := unit(t, "alpha"), unit(t, "beta")

	err := engine.reconcile(context.Background(), []repo.Unit{alpha, beta}, map[string]bool{"alpha": true})
	if err != nil {
		t.Fatal(err)
	}
	fakeProv, _ := engine.registry.Get("claude")
	fake := fakeProv.(interface{ ReconcileCalls() []string })
	want := []string{engine.layout.UnitDir("beta", "claude")}
	if diff := cmp.Diff(want, fake.ReconcileCalls()); diff != "" {
		t.Fatalf("reconcile calls (-want +got):\n%s", diff)
	}
}

// TestReconcileDedupesUnitsSharingFolderAndProvider pins the codex
// two-root shape (spec §3): two units mapped into the SAME provider dir
// via different RepoSubdirs must reconcile that dir exactly ONCE per
// cycle — ReconcileIndex spans the whole provider dir (both subdirs),
// not a single unit's subtree, so calling it twice would be redundant
// (and, for a non-idempotent reconciler, potentially harmful).
func TestReconcileDedupesUnitsSharingFolderAndProvider(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	memories := repo.Unit{Provider: "claude", ProjectID: "a", Folder: "shared", LocalDir: t.TempDir(), RepoSubdir: "memories"}
	chronicle := repo.Unit{Provider: "claude", ProjectID: "b", Folder: "shared", LocalDir: t.TempDir(), RepoSubdir: "chronicle"}

	err := engine.reconcile(context.Background(), []repo.Unit{memories, chronicle}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fakeProv, _ := engine.registry.Get("claude")
	fake := fakeProv.(interface{ ReconcileCalls() []string })
	want := []string{engine.layout.UnitDir("shared", "claude")}
	if diff := cmp.Diff(want, fake.ReconcileCalls()); diff != "" {
		t.Fatalf("reconcile calls (-want +got):\n%s", diff)
	}
}
