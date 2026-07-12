# agent-brain

Invisible cross-machine sync for AI coding agents' per-project memory (Claude Code,
Codex), through an encrypted private GitHub repo. A single Go binary runs a resident
user daemon that watches providers' native memory directories and syncs continuously;
the `agent-brain` CLI is the management surface. Plain `claude` and `codex` keep
working with zero ceremony — no wrapper, no settings injection. A single-goroutine
sync engine is the only writer, git filters encrypt content transparently on the wire
(Tink AES-SIV), and overlapping edits are retained side by side, never overwritten.

> **Status: v2 rebuild on `develop`.** All v2 work happens on `develop`; `main` still
> holds the retired bash/chezmoi/age v1 system until v2 merges (ADR 11). The public
> `v2.0.0` release and Homebrew tap activate only after the ADR-13 history scrub.

## Table of contents

- [Install](#install)
- [Quickstart](#quickstart)
- [Commands](#commands)
  - [`agent-brain dashboard`](#agent-brain-dashboard)
- [Security model](#security-model)
- [Uninstall](#uninstall)
- [Documentation](#documentation)
- [License](#license)

## Install

**Homebrew (once the repo is public):**

```bash
brew install sawmonabo/tap/agent-brain
```

**While the repo is private** (the current posture, as of 2026-07-11 — Homebrew
fetches release assets anonymously, so `brew install` is not yet live):

```bash
# Authenticated download of a release archive (owner/collaborator gh auth):
gh release download <tag> -R Sawmonabo/agent-brain -p '*darwin_arm64*' -O - \
  | tar -xz -C ~/.local/bin agent-brain

# …or build from source (owner git access; Linux/WSL2 no-brew fallback):
go install github.com/Sawmonabo/agent-brain/cmd/agent-brain@latest
```

Once installed, `agent-brain update` keeps the binary current through the same
authenticated `gh` path — it works against the private repo, verifies the release
checksums, swaps atomically, and restarts the service (Homebrew installs use
`brew upgrade` instead). Naming a version pins it exactly — `agent-brain update
v2.0.0-rc.2` — including a deliberate, warned rollback; `--select` picks from a
list on a terminal, `--list` shows what is installable.

Per-OS runbooks (macOS, Linux, WSL2) live in [docs/onboarding.md](docs/onboarding.md).

## Quickstart

```bash
agent-brain init      # gh auth → create/clone agent-brain-memories → keyset → service → enroll
agent-brain track     # enroll the current project's memory root (or: agent-brain track --all)
```

That is the whole setup. The daemon now watches the enrolled memory directories and
syncs every change; idle machines stay fresh through a 5-minute pull ticker. Joining a
second machine is `init` again with `key import` from your password manager — see the
onboarding runbook.

## Commands

Bare `agent-brain` prints help. The full tree (`agent-brain <command> --help` for
flags):

| Command | What it does |
|---|---|
| `init` | First-run onboarding: gh, repo, keyset, wiring, config, service, enrollment |
| `track [path]` | Enroll a memory root for cross-machine sync (`--all` enrolls every discovered root) |
| `untrack <path\|folder>` | Remove an enrollment (`--purge` also drops the repo folder; `--yes` skips the confirm) |
| `sync` | Trigger a sync cycle now and report the outcome (`--project` limits it to one folder) |
| `status` | Show daemon state and the last sync cycle (`--json` for the raw payload) |
| `projects` | List enrolled projects and their health (`--json`) |
| `conflicts` | Inspect retain-both conflict blocks (`list`, `show <path>`) |
| `doctor` | Check (and with `--fix`, repair) this machine's wiring — filters, attributes, credential helper, maintenance posture (`--json`, `--offline`) |
| `scan` | Scan enrolled memory for pasted secrets via gitleaks — advisory (`--project`, `--json`, `--reveal-secrets`) |
| `dashboard` | Live TUI over the running daemon (projects, conflicts, activity, doctor) |
| `key export` / `key import [--force]` / `key rotate` | Manage the shared Tink keyset (back up, restore, fleet-rotate) |
| `service install\|uninstall\|start\|stop\|status\|logs` | Install or control the login-started daemon service |
| `update [version]` | Self-update to the newest release — or the named one, incl. deliberate rollback — and restart the service (`--check`, `--list [--json]`, `--select`, `--no-restart`) |
| `migrate` | One-time import of the bash-era `~/.agent-brain` memory tree (`--yes`, `--skip-preflight`; spec §10) |
| `daemon run` | Run the sync daemon in the foreground (the service manager invokes this) |
| `completion <shell>` | Generate the shell autocompletion script (bash, zsh, fish, powershell) |

Read commands offer `--json`; `NO_COLOR` and non-TTY output degrade to plain text.

### `agent-brain dashboard`

A live [bubbletea](https://charm.land) TUI over the running daemon, four tabs
refreshed on a 2-second poll (`s` syncs the selected unit, `t` untracks it behind a
`y/N` confirm, `a` adds (tracks) a discovered memory root, `tab`/`1`–`4` switch tabs, `q` quits). It needs an interactive
terminal — `status --json` and `projects --json` are the scriptable equivalents. The
Projects tab, rendered from the daemon's live telemetry:

```text
daemon: watching · last cycle: ok

[1 Projects]  2 Conflicts   3 Activity   4 Doctor

Projects

PROVIDER   FOLDER                HEALTH     WATCH      LAST CYCLE
claude     agent-brain           ok         watching   ok
codex      _global               degraded   failed     degraded

tab/1–4 switch · ↑/↓ select · s sync · t untrack · a add · q quit
```

`WATCH` reads `watching`/`failed`/`—`; `LAST CYCLE` reads `ok`/`degraded`/`error`/`—`
(the whole-cycle `error` a degraded flag alone cannot show). A `LOCAL DIR` column
appears on terminals ≥120 columns wide. While a modal owns the keyboard (the untrack
confirm or the add flow), the footer and the modal's inline hints advertise exactly
that modal's keys — both render from the same key bindings, so they cannot disagree.
**Activity** adds daemon uptime, any quiesce
deadline, the fleet watch-trigger count (the max over units, since triggers are
fleet-global), and the last cycle's mirror/push summary. **Doctor** renders the
read-only `--offline` battery with per-check `✓`/`⚠`/`✗`/`i` glyphs. When the daemon
is down the dashboard shows a full-screen notice offering `s` to start the login
service.

## Security model

- **Ciphertext on the wire.** All memory content is encrypted at rest on GitHub with
  Tink Deterministic AEAD (AES-SIV, RFC 5297) via git clean/smudge filters. Only
  `.agent-brain/` metadata (project registry, per-machine manifests) is plaintext.
- **Fail-closed.** The filter is wired `filter.agentbrain.required = true`: git
  refuses to commit plaintext when the filter is selected but missing or broken, and
  the daemon refuses to sync until `doctor` passes. A missing or stale keyset pauses
  that sync and degrades the unit — it never writes plaintext to a git object.
- **Single-writer integrity.** The daemon's engine is the checkout's only writer, and
  the checkout pins git's auto-maintenance to run inline, never detached (ADR 19) —
  installed at `init`, re-pinned every sync cycle, checked by `doctor` — so no
  background git process ever races the engine. If a machine goes offline, cycles
  keep capturing locally and queue the push; anything else that breaks a fetch
  (expired auth, a vanished remote) surfaces as a loud cycle error, never a silent
  "offline".
- **The keyset never enters any repo.** One shared Tink keyset lives at
  `~/.config/agent-brain/keyset.json`, mode 0600, gitignored, transferred between
  machines only by `key export` / `key import` over a channel you choose. `key rotate`
  re-encrypts the whole repo under a fresh primary; old keys are retained so history
  still decrypts.
- **`agent-brain scan`** runs gitleaks over enrolled plaintext memory to catch pasted
  secrets an agent may have written into a note (advisory — it never blocks a cycle).
- **Threat model** (full detail in [spec §5](docs/00-design-spec.md) and §11): this
  protects the repo at rest on GitHub. It does **not** protect local disk — worktree
  and provider directories are plaintext by design (agents must read them) — and it
  does not hide filenames, sizes, or timing, and accepts the deterministic
  equality leak (identical plaintext ⇒ identical ciphertext).

## Uninstall

```bash
agent-brain service uninstall     # remove the login service
```

Then delete the data directory (`~/Library/Application Support/agent-brain/` on macOS,
`~/.local/share/agent-brain/` on Linux), `~/.config/agent-brain/`, and — only when you
are certain you no longer need to decrypt history — your password-manager copy of the
keyset. The `agent-brain-memories` GitHub repo can be deleted separately; its history
retains everything until you do. The bash-era retirement checklist is in
[spec §10](docs/00-design-spec.md).

## Documentation

- Design spec: [docs/00-design-spec.md](docs/00-design-spec.md) — the canonical `what`;
  section refs (§4, §5…) are load-bearing.
- New-machine onboarding: [docs/onboarding.md](docs/onboarding.md)
- Decisions (ADRs): [docs/decisions/](docs/decisions/) — the `why`, alternatives, and
  research trail behind each choice.
- Plans: [docs/plans/](docs/plans/)

## License

[MIT](LICENSE)
