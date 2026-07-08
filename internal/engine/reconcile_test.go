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
