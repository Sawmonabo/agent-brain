package doctor

import (
	"context"
	"fmt"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/provider"
)

// SafetyGate is the sync-blocking subset of the check battery (spec §5:
// "the daemon refuses to sync until doctor passes"): the checkout is a
// git repo, the keyset loads, filter/merge wiring is present with
// required=true and points at binaryPath, and the checkout root
// .gitattributes is byte-canonical. It reuses the exact same check
// functions the full battery does (checks.go), just in this narrower,
// checkout-first order, and is cheap enough to run before every daemon
// cycle — a few config reads plus one keyset parse. It stops at, and
// names, the first failing axis rather than running the whole battery.
//
// Membership rule: a check belongs here ONLY if a cycle cannot safely run
// while it fails AND running a cycle cannot repair it. checkGitMeta is the
// counter-example and is deliberately excluded — the engine's own
// prepareCheckout scrub is what removes resident git-meta, so gating the
// cycle on its absence would refuse the sync that performs the heal. Every
// axis gated here is one only a HUMAN (or `doctor --fix`) can repair.
func SafetyGate(ctx context.Context, paths config.Paths, registry *provider.Registry, binaryPath string) error {
	deps := Deps{Paths: paths, Registry: registry, BinaryPath: binaryPath}
	for _, check := range []checkFunc{
		checkCheckout,
		checkKeyset,
		checkFilters,
		checkAttributes,
	} {
		if res, _ := check(ctx, deps); res.Status == StatusFail {
			return fmt.Errorf("doctor: %s: %s", res.Name, res.Detail)
		}
	}
	return nil
}
