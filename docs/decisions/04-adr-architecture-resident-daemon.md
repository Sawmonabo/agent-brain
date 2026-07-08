# ADR 04: Architecture — resident user daemon with WSL2 on-demand fallback

- **Status:** Accepted
- **Date:** 2026-07-07
- **Deciders:** Sawmon (selected Approach A from three researched architectures)
- **Related:** ADR 01 (dashboard needs a live daemon), ADR 03 (single serialized writer),
  ADR 07 (watching), ADR 09 (IPC)

## Context

Plain `claude`/`codex` must just work — agent-brain starts, syncs, watches, and tracks
automatically with no wrapper command. Memory data only mutates while an agent session
runs, but cross-machine freshness requires pulls even on idle machines. Research on how
production tools ship this: Syncthing uses OS-native user-level supervision
(launchd LaunchAgent on macOS, `systemd --user` on Linux); atuin uses CLI-spawned
on-demand daemons with idle exit and ships no unit files; tailscaled is system-level
(needs privileges — not our model). On WSL2, `systemd --user` sessions are
documented-fragile (login-session teardown of `/run/user/$UID`; VM idle-exit unless
`vmIdleTimeout` is tuned), so OS supervision cannot be relied on there.

## Options considered

1. **Resident user daemon (chosen):** OS-supervised user-level service on macOS/Linux,
   atuin-style on-demand spawn + idle-exit on WSL2, periodic pull timer for idle
   machines.
2. **On-demand daemon everywhere:** no OS service; hook/CLI-spawned with idle exit.
   Zero service management, but idle machines never pull and Codex-only sessions may
   lack a spawn trigger.
3. **Hook-fired sync, no daemon:** pull at session start, push at end. Fails the
   explicit "automatically start watching" requirement; batches conflicts into large
   end-of-session merges; loses mid-session changes on crash.

## Decision

Option 1. A single `agent-brain` binary serves both roles (CLI + `agent-brain daemon`).
Service install/uninstall/start/stop via `github.com/kardianos/service` (LaunchAgent on
macOS, `systemd --user` on Linux), exposed as `agent-brain service …` and run by the
first-run wizard. WSL2 gets the on-demand path as a first-class branch, not an
afterthought. A periodic timer (systemd `.timer` / launchd `StartCalendarInterval`,
templated by us) triggers pulls on idle machines.

## Consequences

- One-time `agent-brain service install` per machine (wizard-driven).
- The WSL2 branch must be tested as its own platform (spawn, idle-exit, `/run/user`
  recreation on start).
- kardianos/service installs units but does not emit rich per-unit extras — timer
  units and any hardening directives are templated by agent-brain itself.
- Continuous watching keeps commits small and frequent, which is what keeps merges
  trivial (ADR 03).

## Buy vs build

**Buy:** `kardianos/service` v1.3.0 — the de-facto cross-platform service abstraction,
back under active maintenance (v1.3.0 published 2026-07-06 after 2022–2024 dormancy);
supports user-scope services. Known limits accepted (`Dependencies` unimplemented on
Linux/launchd). **Rejected:** hand-written unit files + installer (own three platforms'
templates for no gain at our scale); pure on-demand everywhere (staleness on idle
machines).

## Sources

Research delegated to a parallel research team (~45 searches/fetches, all accessed
2026-07-07); links below are the sources cited in its Topic A report.

- https://docs.syncthing.net/users/autostart.html
- https://docs.atuin.sh/cli/reference/daemon/
- https://blog.atuin.sh/atuin-v18-3-0/
- https://tailscale.com/kb/1278/tailscaled
- https://github.com/tailscale/tailscale/wiki/Tailscaled-on-macOS
- https://pkg.go.dev/github.com/kardianos/service?tab=versions
- WSL2 lifecycle/user-session issues: https://github.com/microsoft/WSL/issues/9968,
  https://github.com/microsoft/WSL/issues/10138,
  https://github.com/microsoft/WSL/issues/10205,
  https://github.com/microsoft/WSL/issues/13053,
  https://github.com/microsoft/WSL/issues/13826
- UNVERIFIED (flagged by researcher): the claim that Teleport uses kardianos/service.
