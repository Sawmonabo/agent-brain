# ADR 07: File watching — fsnotify + thin dynamic-watch manager + debounce + polling backstop

- **Status:** Accepted
- **Date:** 2026-07-07
- **Deciders:** Sawmon (accepted within Approach A / buy-vs-build table)
- **Related:** ADR 04 (daemon), ADR 02 (provider roots to watch)

## Context

The daemon watches provider memory roots — `~/.claude/projects/*/memory/` (new project
dirs appear at any time) and `~/.codex/memories/` (+ Chronicle dir) — and must coalesce
write bursts into single sync operations. Platform realities (verified 2026-07-07):
fsnotify v1.10.1 (active, Go 1.23+) has **no recursive watching** (issue #18 open since
2014), **no polling backend**, and uses **kqueue on macOS** (FSEvents not implemented;
one fd per watched dir — default `ulimit -n` 256). WSL2: inotify works on the ext4 VHD
where `~/.claude`/`~/.codex` live, but is **dead on `/mnt/c`** for Windows-side writes
(microsoft/WSL#4739, still open) — only polling covers that.

## Options considered

1. **fsnotify + thin hand-rolled manager (chosen):** watch parent roots
   non-recursively; on directory-`Create`, add a watch **and immediately
   `filepath.WalkDir` the new dir** to close the created-with-contents race; trailing
   debounce timer (reset per event, fire after ~1–5s quiet); `NewBufferedWatcher` to
   absorb bursts; periodic mtime/size scan (~15–60s) as backstop.
2. **rjeczalik/notify (or the syncthing/notify fork):** native FSEvents recursion on
   macOS — but last tagged release v0.9.3 (2023-01-13); the maintained fork is
   commit-pinned with no tags; and on Linux/WSL2 its "recursion" is user-space anyway
   (inotify has no kernel recursion), i.e. exactly what we'd hand-roll.
3. **Watchman:** solves recursion/new-subdir/coalescing natively and is aggressively
   maintained — but it's a separate C++ service the user must install; against the
   grain of a self-contained single binary.

## Decision

Option 1. At our scale (a handful of memory dirs plus their parents), fsnotify's
longevity and maintenance beat native-recursion convenience. Raise `RLIMIT_NOFILE` at
startup for macOS kqueue headroom. The polling backstop is the only thing that covers
WSL2 `/mnt/c` and flaky mounts, and doubles as self-healing if a watch is silently
dropped.

## Consequences

- We own a small watch-manager component (dynamic subdir adds, debounce, backstop) —
  it must be unit-tested against the created-with-contents race.
- Debounce interval is a tunable; too short spams commits, too long batches conflicts.

## Buy vs build

**Buy** fsnotify v1.10.1 (the maintained cross-platform standard). **Build** the thin
recursive/new-dir manager — no maintained library provides it (rjeczalik stale,
watchman heavyweight), and it is small, testable code.

## Sources

Research delegated to a parallel research team (accessed 2026-07-07); links below are
the sources cited in its Topic D report.

- https://pkg.go.dev/github.com/fsnotify/fsnotify
- https://github.com/fsnotify/fsnotify/issues/18 (no recursion since 2014)
- https://github.com/microsoft/WSL/issues/4739 (inotify dead on /mnt/c)
- https://github.com/rjeczalik/notify, https://github.com/syncthing/notify
- https://github.com/facebook/watchman
