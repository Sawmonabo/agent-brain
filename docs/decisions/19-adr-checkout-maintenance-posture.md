# ADR 19: Checkout maintenance posture — foreground auto-maintenance, engine-owned

- **Status:** Accepted
- **Date:** 2026-07-11
- **Deciders:** Sawmon (checkout-maintenance hardening approved in-session)
- **Related:** ADR 03 (daemon = single writer), ADR 15 (hermetic test
  isolation), spec §5 (the local `.git/config` wiring init/doctor install and
  the `prepareCheckout` scrub-contract preamble)

## Context

Git runs its automatic maintenance DETACHED by default. After a command that
writes to a repository, git may spawn `git maintenance run --auto` (older
gits: `git gc --auto`), which daemonizes and keeps running after the
foreground command returns. In the agent-brain memories checkout that
violates the single-writer model (ADR 03): the sync engine's one goroutine is
meant to be the ONLY process touching the checkout, and a detached
maintenance process races whatever runs next — a later engine cycle, an
init/doctor mutation performed inside a TTL-quiesce window, or (in tests)
`t.TempDir()` teardown.

This is not hypothetical. A detached `gc --auto` runs `update_server_info` as
its closing step, which writes `.git/info/refs`. In the e2e suite that write
landed while the test's `t.TempDir()` cleanup was deleting the checkout tree,
failing the removal with "directory not empty" on a slow filesystem. That was
closed at the TEST-harness seam (`test/e2e/harness_test.go`, which now
neutralizes maintenance globally for every git invocation the suite makes).
The PRODUCTION checkout has the same exposure — a detached maintenance
process outliving the engine child that spawned it — and nothing had closed
it there. This ADR does.

## Verified git semantics

From https://git-scm.com/docs/git-maintenance and
https://git-scm.com/docs/git-gc:

- `maintenance.auto` (default **true**): whether commands run
  `git maintenance run --auto` after their normal work.
- `maintenance.autoDetach`: whether that auto maintenance runs in the
  foreground or detaches. If unset, the value of `gc.autoDetach` is used as a
  fallback; it defaults to **true** (detaches) if both are unset.
- `gc.autoDetach` (default **true**): `git gc --auto` returns immediately and
  runs in the background.

## Decision

Pin auto maintenance to the **foreground** in the memories checkout's LOCAL
`.git/config`, by writing both keys explicitly:

- `gc.autoDetach = false`
- `maintenance.autoDetach = false`

Auto maintenance itself stays **enabled** — `gc.auto` / `maintenance.auto` are
left at their defaults, so loose objects still get packed when the thresholds
trip and the repo stays healthy. Only the DETACH is disabled: the maintenance
work now runs inline inside the engine's own git children, serialized by the
single writer, never a detached process racing anything.

Both keys are written even though setting only `maintenance.autoDetach` works
today through the `gc.autoDetach` fallback chain: that chain is an upstream
implementation detail, fragile to a future change, and the explicit pair is
self-documenting. The values are literals, so no shell quoting is involved
(unlike the filter/merge command values `InstallFilters` wires).

Enforcement is layered, because `.git/config` is not versioned and every
machine's checkout is wired independently:

| Site | When | Role |
|---|---|---|
| `init` (`stepWiring`) | checkout creation | pins the posture on every fresh clone and adopt-existing |
| engine (`prepareCheckout`) | top of every Sync + admin op | stateless re-pin — converges pre-ADR-19 machines on their first post-upgrade cycle and self-heals any later drift |
| `doctor` battery (`checkMaintenancePosture`) | on demand | advisory warn naming any drifted/missing key |
| `doctor --fix` | on demand | re-installs the posture |

The engine re-pin is deliberately STATELESS — two cheap `git config` writes at
the top of every cycle, not cached behind a once-flag. That buys immediate
convergence for every existing fleet member and self-healing of any later
drift, with no engine state to reason about. `checkMaintenancePosture` is
ADVISORY (warn) and deliberately NOT in `SafetyGate`: wrong posture is
operational hygiene, not a data-safety refusal, and `prepareCheckout` re-pins
it every cycle — gating a cycle on it would refuse the very sync that heals
it, the same reasoning that keeps `checkGitMeta` out of the gate.

### Production vs test posture — do not unify

The two configs are DIFFERENT on purpose and must stay so:

- **Production** wants maintenance ON but INLINE: `gc.auto` /
  `maintenance.auto` untouched (repo health preserved), `gc.autoDetach` /
  `maintenance.autoDetach` = false (no detached racer).
- **Tests** want ZERO maintenance for determinism: the e2e harness sets
  `gc.auto = 0` and `maintenance.auto = false` (plus `gc.autoDetach = false`)
  in a hermetic global config, so no git invocation in the suite ever forks a
  background process that could outlive the test that started it.

Setting `gc.auto = 0` / `maintenance.auto = false` in PRODUCTION would disable
maintenance entirely and let loose objects accumulate unbounded; setting only
the detach keys in TESTS would still allow an inline `gc --auto` to run
mid-test and perturb timing. This distinction is recorded here so nobody
"unifies" the two configs later.

## Consequences

- Every engine cycle spends two additional `git config --local` subprocess
  spawns re-pinning the posture — negligible against the fetch/merge/commit/
  push git traffic a cycle already runs, and the deliberate price of the
  stateless design: no once-flag and no engine state, so every pre-ADR-19
  fleet member converges on its first post-upgrade cycle and any later drift
  self-heals without a restart.
- The `doctor` battery grows from 19 to 20 checks. `maintenance-posture` is
  advisory (warn, never fail) and outside `SafetyGate`, so it never blocks a
  sync — a drifted posture is reported, not enforced, and the next cycle
  repairs it regardless.
- No new failure mode is introduced: `InstallMaintenancePosture` returns its
  error up `prepareCheckout`'s existing error path, exactly like the recover
  and scrub steps beside it, so a config-write failure aborts the cycle loudly
  rather than degrading silently — the same fail-closed posture the rest of
  the preamble already has.

## Alternatives rejected

- **(a) Disable auto maintenance in production and have the engine schedule an
  explicit `git maintenance run`.** Adds a scheduler and a new failure mode —
  a scheduling bug means maintenance never runs and the repo degrades
  silently — for no capability gain at this repo's scale. Inline foreground
  maintenance already runs the same packing, serialized by the writer, with
  zero extra moving parts.
- **(b) Set only `gc.autoDetach = false` and lean on the fallback chain.**
  Works today, but depends on an upstream implementation detail
  (`maintenance.autoDetach` inheriting `gc.autoDetach`); the explicit pair is
  robust to a change in that chain and self-documents intent.
- **(c) Fix it only at the test-harness seam.** That is where the race was
  first observed and closed, but it leaves the production checkout exposed to
  exactly the same detached-maintenance race. The production seam is the
  engine's own wiring, which this ADR closes.

## Sources

- https://git-scm.com/docs/git-maintenance (`maintenance.auto`,
  `maintenance.autoDetach`, and its `gc.autoDetach` fallback)
- https://git-scm.com/docs/git-gc (`gc.autoDetach` default-true background
  behavior)
