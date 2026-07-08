# ADR 09: Daemon IPC + single instance — HTTP/JSON over Unix socket, flock guard

- **Status:** Accepted
- **Date:** 2026-07-07
- **Deciders:** Sawmon (accepted within Approach A / buy-vs-build table)
- **Related:** ADR 04 (daemon), ADR 05 (dashboard is a client), ADR 03 (single writer
  needs single instance)

## Context

The CLI/dashboard needs a control plane to the daemon (status, discovered projects,
enroll/untrack, force sync, streamed logs), and exactly one daemon may run per user
(ADR 03's serialization guarantee). Reference designs: tailscaled's LocalAPI is
HTTP/1.1 + JSON over a Unix domain socket (CLI as thin client; curl-debuggable);
Syncthing uses loopback TCP + API key only because a *browser* GUI must reach it.

## Options considered

1. **HTTP/1.1 + JSON over a Unix domain socket (chosen):** stdlib-only, versioned
   routes (`/v0/status`), middleware, chunked streaming for `logs -f`, debuggable with
   `curl --unix-socket`. No open TCP port; auth by filesystem permission.
2. **gRPC over UDS** — typed RPC, but adds a proto toolchain and heavy deps, loses
   curl debuggability; disproportionate for this surface.
3. **Loopback TCP + token (Syncthing model)** — only needed if a browser client must
   connect; costs an open local port and token management.

## Decision

Option 1. Socket in `${XDG_RUNTIME_DIR:-/run/user/$UID}/agent-brain/` on Linux/WSL2 and
`${TMPDIR}/agent-brain/` on macOS (never bare `/tmp`), directory created `0700` on
**every** daemon start (WSL2 tears down `/run/user/$UID` across restarts), socket
`chmod 0600`. Short paths are load-bearing: `sun_path` is 104 bytes (macOS) / 108
(Linux), ruling out deep `~/Library/Application Support` paths. Optional
defense-in-depth: verify peer UID via `SO_PEERCRED` (Linux) / `LOCAL_PEERCRED` (macOS).

Single instance: advisory lock via `github.com/gofrs/flock` on a sidecar lockfile —
acquire `LOCK_EX|LOCK_NB` first; holding it, remove any stale socket and bind
(race-free because the lock is held); hold for process lifetime. The kernel releases
the lock on crash — no staleness, unlike pidfiles (stale-file + PID-reuse races).
Linux abstract sockets rejected (Linux-only). Go stdlib still has no public file-lock
API (golang/go#33974), so the third-party lib is required.

## Consequences

- The dashboard and every CLI subcommand are HTTP clients with a custom `DialContext`;
  the daemon is inspectable with plain curl.
- The flock + socket-rebind composite replaces the bash system's mkdir lock — and its
  orphaned-lock failure mode (locks held by crashed processes) disappears entirely,
  since the kernel drops flocks on process death.

## Buy vs build

**Buy:** net/http (stdlib), gofrs/flock (de-facto standard, ~1,500 importers, active).
**Build:** none beyond routing glue.

## Sources

Research delegated to a parallel research team (accessed 2026-07-07); links below are
the sources cited in its Topic F report.

- https://github.com/tailscale/tailscale/blob/v1.78.3/client/tailscale/localclient.go
- https://docs.syncthing.net/dev/rest.html
- https://github.com/gofrs/flock
- https://github.com/golang/go/issues/33974 (no stdlib file lock)
