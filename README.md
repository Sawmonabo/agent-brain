# agent-brain

Invisible cross-machine sync for AI coding agents' per-project memory (Claude Code,
Codex), through an encrypted private GitHub repo. A single Go binary runs a resident
user daemon that watches providers' native memory directories and syncs continuously;
the `agent-brain` CLI is the management surface. Plain `claude` and `codex` keep
working with zero ceremony — no wrapper, no settings injection. A single-goroutine
sync engine is the only writer, git filters encrypt content transparently on the wire
(Tink AES-SIV), and overlapping edits are retained side by side, never overwritten.

> **Status: public since 2026-07-17.** The ADR-13 history scrub is executed, `main`
> tracks `develop`, and releases (first public: `v1.0.0`) install from the Homebrew
> tap below.

## Table of contents

- [Install](#install)
- [Quickstart](#quickstart)
- [Commands](#commands)
  - [The dashboard hub](#the-dashboard-hub)
- [Security model](#security-model)
- [Uninstall](#uninstall)
- [Documentation](#documentation)
- [License](#license)

## Install

**Homebrew (macOS/Linux):**

```bash
brew install sawmonabo/tap/agent-brain
```

**Without Homebrew** — download a release archive, or build from source:

```bash
# Release archive (latest); swap the pattern per platform (darwin/linux × arm64/amd64):
gh release download -R Sawmonabo/agent-brain -p '*darwin_arm64*' -O - \
  | tar -xz -C ~/.local/bin agent-brain

# …or build from source (Linux/WSL2 no-brew fallback):
go install github.com/Sawmonabo/agent-brain/cmd/agent-brain@latest
```

Once installed, `agent-brain update` keeps the binary current through `gh` — it
verifies the release checksums, swaps atomically, and restarts the service
(Homebrew installs use `brew upgrade` instead). Naming a version pins it
exactly — `agent-brain update v1.0.1` — including a deliberate, warned rollback;
`--select` picks from a list on a terminal, `--list` shows what is installable.

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

Bare `agent-brain` opens the [dashboard hub](#the-dashboard-hub) on an interactive
terminal; `agent-brain --help` prints the full tree (`agent-brain <command> --help`
for per-command flags):

| Command | What it does |
|---|---|
| `init` | First-run onboarding: gh, repo, keyset, wiring, config, service, enrollment |
| `track [path]` | Enroll a memory root for cross-machine sync (`--all` enrolls every discovered root) |
| `untrack <path\|folder>` | Remove an enrollment (`--purge` also drops the repo folder; `--yes` skips the confirm) |
| `sync` | Trigger a sync cycle now and report the outcome (`--project` limits it to one folder) |
| `status` | Show daemon state and the last sync cycle (`--json` for the raw payload) |
| `projects` | List enrolled projects and their health (`--json`) |
| `conflicts` | Inspect retain-both conflict blocks (`list`, `show <path>`) |
| `doctor` | Check (and with `--fix`, repair, then re-check) this machine's wiring — filters, attributes, credential helper, maintenance posture (`--json`; `--offline` skips the network probe and is honored under `--fix`) |
| `scan` | Scan enrolled memory for pasted secrets via gitleaks — advisory (`--project`, `--json`, `--reveal-secrets`) |
| `dashboard` | Alias for the bare command — opens the [dashboard hub](#the-dashboard-hub) (the interactive default when `agent-brain` runs with no arguments) |
| `key export` / `key import [--force]` / `key rotate` | Manage the shared Tink keyset (back up, restore, fleet-rotate) |
| `service install\|uninstall\|start\|stop\|status\|logs` | Install or control the login-started daemon service |
| `update [version]` | Self-update to the newest release — or the named one, incl. deliberate rollback — and restart the service (`--check`, `--list [--json]`, `--select`, `--no-restart`) |
| `migrate` | One-time import of the bash-era `~/.agent-brain` memory tree (`--yes`, `--skip-preflight`; spec §10) |
| `daemon run` | Run the sync daemon in the foreground (the service manager invokes this) |
| `completion <shell>` | Generate the shell autocompletion script (bash, zsh, fish, powershell) |

Read commands offer `--json`; `NO_COLOR` and non-TTY output degrade to plain text.

### The dashboard hub

Bare `agent-brain` — or the `agent-brain dashboard` alias — opens the hub: a live
[bubbletea](https://charm.land) TUI over the running daemon that browses, reads,
edits, and time-travels every enrolled project's memories without leaving the
terminal. It needs an interactive terminal — for scripting, `status --json` and
`projects --json` are the equivalents. On a machine that is not set up yet, a human
terminal is walked through guided `init` first and then lands in the hub; a detected
coding-agent environment or a non-interactive shell gets an `agent-brain init`
pointer instead. If the daemon is down the hub still opens, with a degraded banner
and a full-screen notice offering `s` to start the login service.

Four tabs sit at the root, refreshed on a 2-second poll and switched with
`tab`/`shift+tab` or `1`–`4`:

- **Projects** — the per-unit table (provider · folder · health · watch state ·
  last-cycle result; a `LOCAL DIR` column appears on terminals ≥120 columns wide).
  `WATCH` shows `watching`, `failed`, or `—` before the daemon's first watcher
  build; `LAST CYCLE` shows `ok`, `degraded`, `error`, or `—` until that folder's
  first cycle. `s` syncs the selected unit, `u` untracks it behind a `y/N`
  confirm, `a` runs the init-style enrollment picker over newly discovered
  memory roots, `m` runs the bash-era migrate flow, and `enter` opens that
  project's memory browser.
- **Conflicts** — retained retain-both records; `enter` opens a detail view where
  `enter` jumps to the memory, `e` edits the merged file, and `h` opens its history.
- **Activity** — the sync/capture feed: daemon uptime, any quiesce deadline, the
  fleet watch-trigger count, and the last cycle's mirror/push summary.
- **Doctor** — the read-only `--offline` check battery with per-check
  `✓`/`⚠`/`✗`/`i` glyphs; `r` re-runs it, `f` runs the quiesce-aware `doctor --fix`
  (always re-checking offline) on a fixable failure, and `s` runs the advisory
  gitleaks scan.

Inside a project's **memory browser**, memories are grouped by provider with lint
badges: `enter` reads one, `e` edits, `n` creates, `r` renames, `d` deletes, `h`
opens version history, `x` toggles the deleted-memory recovery view, `i` opens
project insights, `o` reorders, and `/` filters the list. The **reading view**
renders with theme-aware glamour and follows `[[wiki-links]]` (`tab` cycles link
targets, `enter` jumps, `b` toggles backlinks, dangling targets are marked); `y`
copies the memory's provider-file path, `e` edits, and `h` opens history.
**History** lists each version (short rev, time, machine, and which one is live):
`enter` renders a version, `d`/`D` diffs it against the live content or the adjacent
version, and `R` restores it — restore writes the chosen content back as a new
capture, so history only ever grows, and it works for deleted memories too.

`/` from any root view opens a **global search** overlay across every tracked
project (matched by name, then description, then body text); `enter` opens the
matched memory.

**Editing never embeds an editor.** `e` suspends the hub and hands your configured
editor (`editor.command` if set, else `$VISUAL`, else `$EDITOR`) a disposable scratch
copy — never the live file. A byte-identical save makes zero commits and toasts
that the edit was cancelled; a real change is written back with one atomic
rename, then captured and pushed by the normal engine cycle (exactly one capture
per changed save). If no editor is configured the binding is visibly disabled
rather than falling back to a default.

When a newer release is available the status bar shows `vX.Y.Z available — U to
update`; `U` confirms and runs the checksum-verified self-update (ADR 18), and on
success the hub offers `R` to re-exec onto the new binary. A `ctrl+k` command
palette and `?` help overlay reach every action (including a fleet-wide sync) —
both, with each view's footer, render from one action registry, so a key can
never mean two things — and while the daemon is quiesced (during `init` or
`doctor --fix`) every mutating action greys out.

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
