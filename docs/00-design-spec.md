# agent-brain v2 — Design Specification

- **Status:** Approved design, pre-implementation (all 13 sections user-approved 2026-07-07)
- **Branch:** `develop` (main retains the bash-era system until v2 is proven; ADR 11)
- **Supersedes:** the bash/chezmoi/age system spec, archived at `docs/archive/00-design-spec-bash-era.md`
- **Decisions:** every load-bearing choice has an ADR under `docs/decisions/` — this spec states *what* the system is; the ADRs record *why*, the alternatives, buy-vs-build, and full research trails.

agent-brain v2 is a single Go binary that syncs AI coding agents' per-project memory
across machines through an encrypted private GitHub repo — invisibly. Plain `claude`
and `codex` work with zero ceremony: a resident user-level daemon discovers projects
as providers create memory for them, watches enrolled projects' memory directories,
and syncs continuously. A pretty interactive CLI is the management surface.

## Decision index

| # | Decision | ADR |
|---|---|---|
| 00 | Encryption model: transparent deterministic git-filter crypto | [00](decisions/00-adr-encryption-at-rest.md) |
| 01 | Tracking: automatic discovery, deliberate enrollment via interactive CLI | [01](decisions/01-adr-tracking-enrollment.md) |
| 02 | Providers v1: Claude Code + Codex (Codex user-global → `_global/`) | [02](decisions/02-adr-provider-scope-v1.md) |
| 03 | Conflicts: git 3-way + per-file-class policy + retain-both; daemon = single writer | [03](decisions/03-adr-conflict-resolution.md) |
| 04 | Architecture: resident user daemon; WSL2 on-demand fallback; pull ticker | [04](decisions/04-adr-architecture-resident-daemon.md) |
| 05 | CLI/TUI: cobra + fang, Charm v2 (bubbletea/lipgloss/bubbles/huh); Go 1.26 floor | [05](decisions/05-adr-cli-tui-stack.md) |
| 06 | Crypto: Tink Deterministic AEAD (AES-SIV, RFC 5297) filters + custom merge driver; system git | [06](decisions/06-adr-encryption-mechanism.md) |
| 07 | Watching: fsnotify + dynamic manager + 2s debounce + polling backstop | [07](decisions/07-adr-file-watching.md) |
| 08 | Provisioning/auth: v1 requires gh; device-flow/PAT deferred to v1.1 | [08](decisions/08-adr-github-provisioning-auth.md) |
| 09 | IPC: HTTP/JSON over 0600 UDS; gofrs/flock single instance | [09](decisions/09-adr-daemon-ipc-single-instance.md) |
| 10 | Prior art: build (nothing viable); harvest manifest + secret-scan ideas | [10](decisions/10-adr-prior-art-build-vs-adopt.md) |
| 11 | Workflow: develop-gated main; greenfield reset; migrate = only compat surface | [11](decisions/11-adr-dev-workflow-greenfield.md) |
| 12 | Engineering standards: toolchain currency, gofumpt, golangci-lint v2, lefthook, CI gates | [12](decisions/12-adr-go-engineering-standards.md) |
| 13 | History scrub of this repo after verified migration (git-filter-repo) | [13](decisions/13-adr-history-scrub-post-migration.md) |
| 14 | Reliability libraries: backoff/v5, renameio/v2, slog, gitleaks | [14](decisions/14-adr-reliability-libraries.md) |
| 15 | Testing: stdlib + go-cmp, testscript e2e, native fuzzing, real-git integration | [15](decisions/15-adr-testing-stack.md) |
| 16 | Distribution: GoReleaser v2, homebrew_casks tap, go install | [16](decisions/16-adr-distribution-release.md) |
| 17 | Config format: TOML via pelletier/go-toml/v2 | [17](decisions/17-adr-config-format-toml.md) |
| 18 | Self-update: gh-native, checksum-verified atomic swap + service restart | [18](decisions/18-adr-self-update.md) |
| 19 | Checkout maintenance: auto maintenance on but foreground-pinned (`autoDetach=false`); engine re-pins every cycle | [19](decisions/19-adr-checkout-maintenance-posture.md) |

---

## 1. Overview & goals

The bash-era system required launching sessions through an `ab-claude` wrapper,
serialized whole sessions behind a mkdir lock, and resolved concurrent edits by
overwriting — losing recent changes. v2 removes every one of those properties.

Requirement → mechanism:

- **No wrapper.** The daemon watches providers' *native* default memory paths
  (Claude Code writes `~/.claude/projects/<slug>/memory/` zero-config since
  v2.1.59). No settings injection, no provider configuration, no trust dialogs.
- **Concurrent-session safety.** A single serialized writer (the daemon), small
  frequent commits, a plaintext-aware merge driver, and retain-both conflict
  handling (ADR 03). Overlapping edits are never overwritten; worst case they are
  retained side by side.
- **Multi-provider.** A provider-adapter interface; Claude Code + Codex ship in v1
  (ADR 02).
- **Dedicated memory repo.** `agent-brain-memories`, auto-provisioned as a private
  GitHub repo, one folder per project (ADR 08).
- **Pretty CLI.** Enrollment dashboard, first-run wizard, status/conflicts/doctor
  views (ADRs 01, 05).

**Non-goals for v1:** Copilot (GitHub already cloud-syncs it), Gemini CLI (first
post-v1 adapter candidate), team/multi-user sharing, and LLM-powered semantic
merging (merges are deterministic; syncing costs no API tokens).

## 2. System architecture

One binary, four parts.

**Daemon** (`agent-brain daemon`) — the only writer. Pipeline: watch manager
(fsnotify + dynamic new-project detection + 2-second trailing debounce + polling
backstop) → event queue → sync engine (a SINGLE goroutine serializing every git and
mirror operation) → git exec wrapper (system git, carrying our filter and
merge-driver configuration). Around the pipeline: the discovered-projects registry,
the enrollment store, the per-machine manifest (distinguishing deleted-here from
new-from-remote), a periodic pull ticker, and the control-plane server (HTTP/JSON
over a 0600 unix socket; gofrs/flock guarantees a single instance — ADR 09).

**CLI** (cobra + fang; bubbletea dashboard) — a thin client of the daemon socket.
Full command tree in §7.

**Provider adapters** — each answers: which roots to watch; its scope (per-project
or user-global); how a local slug resolves to a canonical identity; how files
classify (fact / derived-index / provider-regenerated); and how its derived index
reconciles. v1 ships `claude` and `codex` (§6).

**Sync core** — a hidden checkout of `agent-brain-memories` in the platform data
directory, two-way mirrored with provider directories; mirror-out is atomic
per-file.

The sync loop: a session writes memory → watch event → debounce → mirror-in →
commit (`memory: <host> <project> <timestamp>`) → fetch + integrate (merge driver;
retain-both) → reconcile derived indexes → mirror-out → push. Push failures queue
in git itself and are retried by backoff + ticker. Idle machines stay fresh through
the ticker's fetch/integrate/mirror-out.

Activation (ADR 04): LaunchAgent on macOS, `systemd --user` on Linux, on-demand
with idle-exit on WSL2 (whose VM teardown makes residency unreliable) — all managed
by `agent-brain service` and installed by the init wizard via kardianos/service.

## 3. Data model & repo layout

```
agent-brain-memories/
├── .gitattributes              # filter/diff/merge wiring (versioned)
├── .agent-brain/
│   ├── projects.toml           # canonical project registry (plaintext, shared)
│   └── manifests/<host>.json   # per-machine sync manifests (plaintext)
├── <project>/                  # e.g. agent-brain/
│   └── claude/
│       ├── MEMORY.md           # encrypted (all memory content is)
│       └── <topic>.md
└── _global/
    └── codex/
        ├── memories/           # mirrors ~/.codex/memories/
        └── chronicle/          # mirrors ~/.codex/memories_extensions/chronicle/
```

- A per-provider subfolder under each project means a future Gemini adapter lands
  beside `claude/` without cross-provider `MEMORY.md` basename collisions.
- `_global/` holds user-global provider pools (Codex).
- Encrypted: all memory content. Plaintext: `.agent-brain/` metadata — the folder
  structure already reveals project names (ADR 00's accepted scope), and plaintext
  metadata stays debuggable and merges trivially.

**Project identity — the cross-machine linchpin.** The canonical ID is the
normalized git remote (`host/owner/repo`) when one exists — machine-independent.
The repo folder is the repo basename, with deterministic, registry-recorded
disambiguation on collision. Projects without a remote are named by the user at
enrollment. Claude's local slug is derived from the git repo *per machine* and is
NOT stable across machines, so a per-machine mapping (local slug + path ↔ canonical
ID) lives in LOCAL state, never in the repo. At enrollment the dashboard shows the
discovered slug and a best-guess path (slug reversal); the user confirms once per
machine; the daemon reads the remote URL and binds the mapping. Worktrees share
their repo's slug (July 2026 behavior), so they need no special handling.

**Local state** (outside the repo): the data dir —
`~/Library/Application Support/agent-brain/` on macOS,
`~/.local/share/agent-brain/` (XDG) on Linux — holds the `memories/` checkout,
`registry-local.toml` (slug mappings + enrollment), and `daemon.log`. Config lives
at `~/.config/agent-brain/config.toml` (TOML, ADR 17) beside the keyset (§5).
Socket and lock live in the runtime dir per ADR 09. A `registry-local.toml` unit
may carry a `repo_subdir` that maps its local root under `<folder>/<provider>/<subdir>`
in the checkout — codex enrolls two such units, `memories` and `chronicle` (the
layout above).

**File classes** (drive merge policy, §4): *fact* (Claude topic files, Codex
`raw_memories.md`, Chronicle) → 3-way merge + retain-both; *derived index*
(Claude `MEMORY.md`) → reconcile after merge; *provider-regenerated* (Codex
`memory_summary.md`, `MEMORY.md`, `rollout_summaries/*`) → newest-wins.

## 4. Sync engine & merge mechanics

A single-goroutine engine serializes these steps:

1. **Mirror-in.** Compare provider dir ↔ checkout (mtime+size, hash confirm); copy
   changes in. Deletions disambiguate via the per-machine manifest: in-manifest but
   gone locally = deleted here (`git rm`); absent from manifest but present in
   checkout = new from remote (leave; will mirror out).
2. **Commit.** `memory: <host> <project> <timestamp>`; no-op when clean.
3. **Integrate.** Fetch; rebase onto `origin/main`. The merge driver ALWAYS exits
   resolved — a clean merge or retain-both blocks — so a rebase can never strand
   mid-conflict. Unexpected driver failure → abort rebase, fall back to a merge
   commit → still failing = project marked degraded (dashboard banner) while local
   commits keep accumulating safely.
4. **Reconcile derived indexes** (refines ADR 03's "regenerate"): merge `MEMORY.md`
   as a fact file first (preserving human/agent prose), then reconcile against
   fact-file frontmatter — remove dangling index entries, append missing ones from
   topic-file descriptions, collapse duplicates. Deterministic; no LLM.
5. **Mirror-out.** Atomic per-file (renameio temp+rename); remote deletions applied
   to provider dirs only when the manifest proves they were synced here before.
6. **Push.** Failures are a git-native queue (unpushed commits) retried via
   backoff + ticker; a push race loser re-integrates and retries bounded, then
   waits for the next cycle.

**The merge driver** (`agent-brain git-merge %O %A %B %P`): git hands it the STORED
— post-clean, i.e. ciphertext — versions of base/ours/theirs. It decrypts all
three, runs `git merge-file` on plaintext, and on a clean merge re-encrypts into
%A. On true overlap it rewrites conflict hunks as **retain-both blocks**
(HTML-comment-marked, labeled with host + timestamp, both versions in full),
records the event for the dashboard conflicts view, and exits resolved. Class
policies map `.gitattributes` patterns to per-class driver modes (fact / lww);
the index class rides the fact driver at merge time, and the engine's reconcile
step (step 4 above) regenerates derived indexes afterward — reconciliation is an
engine responsibility, not a third driver mode.
Retained blocks are plain markdown, so the next agent session reads both
versions in context and can tidy naturally — an explicitly unproven hypothesis
(§11) with the conflicts view as backstop.

**Ticker:** 5-minute fetch default, configurable. **Crash safety on start:** abort
any in-progress rebase, re-run integrate, reconcile manifest against reality, full
backstop scan. **Concurrent local sessions:** the watcher coalesces events and the
engine commits the combined state — no locks, no errors surfaced; within one
machine the semantics are per-file latest-write, exactly what Claude Code gives
concurrent sessions today.

## 5. Encryption & key management

**Key model:** ONE Tink keyset shared across hosts (the shared-identity model
carried forward from v1). The keyset JSON lives at
`~/.config/agent-brain/keyset.json`, mode 0600, blocked from commits by gitignore
patterns, and NEVER rides the memories repo. `init` on the first machine generates
it with **AES256_SIV** — Tink's officially recommended Deterministic-AEAD key type
(tink-go v2.7.0, `daead` primitive; RFC 5297) — via Tink's documented
plaintext-keyset workflow (`insecurecleartextkeyset`). Tink steers *server* users
toward KMS-wrapped keysets; for a local dev tool with no KMS, a 0600 plaintext
keyset plus a password-manager copy is the accepted posture — the same class as age
identities and git-crypt keys (ADR 06).

**Onboarding:** `agent-brain key export` prints an armored (base64) keyset for
transfer over a channel the user chooses; `key import` installs it on the new
machine. The export IS the recovery artifact — `init` explicitly prompts storing a
copy in the password manager at generation time.

**Rotation:** Tink keysets are natively multi-key (a new primary encrypts; old keys
remain to decrypt history). `agent-brain key rotate` (shipped) adds a fresh
AES256_SIV primary and the daemon's single writer re-encrypts the whole repo under it
in one commit (`POST /v0/reencrypt`), then pushes; determinism resumes under the new
primary and old keys are retained so history still decrypts. Rotation is fail-closed
for the fleet: until every other machine imports the new keyset (`key rotate` prints
the `key export` / `key import --force` steps), those machines degrade on smudge and
doctor's advisory `keyset-decrypt` probe names the fix (§11).

**Filter wiring** (installed by `init`/`doctor` on every machine and re-checked
after every clone, since `.git/config` is not versioned): the versioned
`.gitattributes` scopes `filter/diff/merge=agentbrain` to memory-content paths only
(`.agent-brain/**` and `.gitattributes` itself excluded); local `.git/config` gets
clean/smudge/textconv/driver entries with **`filter.agentbrain.required = true`** —
fail-closed. Git refuses to commit plaintext when the named filter is SELECTED and
missing or broken, and a clone without the binary shows ciphertext with an erroring
smudge instead of silently degrading. `merge.renormalize = true`. Local `.git/config`
also pins git's auto maintenance to the foreground (`gc.autoDetach = false`,
`maintenance.autoDetach = false`) so a detached `gc`/`maintenance` run never races the
single-writer engine — installed by `init`, re-pinned by the engine every cycle, and
checked by `doctor` (ADR 19). The daemon refuses to sync until `doctor` passes.

**git-meta scrub contract (binding).** `required = true` does NOT save a path whose
filter is *unselected*: a `.gitattributes` below the checkout root overrides the root
file for its subtree (git's deepest-file-wins precedence), and a single `* -filter`
line unselects `agentbrain` there — no filter runs, `required` never fires, and a
sibling memory file commits as PLAINTEXT. `.gitignore` and `.git` segments are the
same class of hazard (silently unsynced files; embedded repos). Therefore **no
git-meta path may exist in the checkout below its root**, and **every engine entry
point that can create a commit scrubs the whole checkout before its first `git add`**
— `Sync` and the three admin ops (register/purge/seed) all run `prepareCheckout`
(recover + whole-checkout scrub + heal-commit) as their preamble; `Sync` additionally
re-scrubs post-integrate, because a rebase can deliver fresh poison mid-cycle. The
scrub is force-semantic: git-meta is never user data, so its removal never waits on
an up-to-date content check (a raw-pushed `.gitignore` is filter-subject and would
otherwise wedge the cycle). `doctor`'s `git-meta` check reports resident poison but
is ADVISORY and never joins `SafetyGate` — gating the cycle on it would refuse the
very sync whose scrub performs the heal. The single definition both the enforcing
engine and the observing doctor share is `repo.IsGitMetaPath`. HTTPS credential lookups in the checkout are wired to gh's own
helper repo-locally: `credential.helper` is cleared to git's empty-reset sentinel
(dropping any global/system helper — e.g. a stale keychain PAT for github.com),
then set to `!<absolute gh path> auth git-credential` in the hidden checkout's
`.git/config` **only**, never the user's global gitconfig — whose absolute-path
write is the `gh auth setup-git` synced-dotfiles hazard ADR 08 avoids
(cli/cli#9438). SSH remotes never invoke it, and `doctor --fix` re-wires it if gh
moves.

**Threat model:** protects the repo at rest on GitHub (account compromise, token
leak, GitHub-side scanning). Does NOT protect local disk — worktree and provider
dirs are plaintext by design, the same posture as today's runtime copy. Does not
hide structure, filenames, sizes, or timing, and accepts the deterministic
equality leak (identical plaintext ⇒ identical ciphertext) — ADR 00's documented
trade for mergeability and no-churn commits. The in-band magic-prefix discriminator does not weaken the absolute invariant
that plaintext memory content never reaches a git object: the clean filter
verify-decrypts any magic-prefixed input and fails closed at commit time —
blocking the commit — unless the bytes are genuine ciphertext under the local
keyset. Plaintext that merely begins with the codec's magic prefix (`agb1\x00`)
is therefore rejected, never stored unencrypted (a keyset mismatch or corrupted
ciphertext is caught the same way, rather than surfacing later as a checkout
error). Such lookalike content is unreachable for markdown memory anyway, which
never opens with a NUL-embedded header; the behavior is pinned by codec tests.

## 6. Provider adapters

**Adapter interface** (as shipped, spec §6): `Name`; `Scope` (PerProject | Global);
`Patterns` — the ordered classification table driving `Classify` → Fact |
DerivedIndex | Regenerated | Ignore and `.gitattributes` generation; `Discover` —
enumerate this machine's enrollable memory roots; `Identify` — resolve a discovered
root to its cross-machine identity (path guess + remote URL at enrollment);
`ReconcileIndex`. Adding Gemini later means implementing the interface plus a
classification table — zero core changes. `WatchRoots` was folded into enrollment
rather than shipping as a method: each enrolled unit's LocalDir IS its watch root,
and discovery-time watching of provider-parent dirs is dashboard-era work, not v1.
A global-scope provider yields one `Discovered` per root, so codex enrolls as two
`repo_subdir` units (`memories`, `chronicle`).

**Claude adapter:** watches `~/.claude/projects/` for new `<slug>/memory/`
directories; enrolled memory dirs are watched fully. Per-project scope.
Classification: `MEMORY.md` → DerivedIndex; other `*.md` → Fact; unknown files →
Fact (whole-file retain-both fallback — the safest default for data). Index
reconciliation reads topic-file frontmatter (`name`/`description`), falling back to
first heading or filename for frontmatter-less files. Doctor checks:
`autoMemoryEnabled` on (the default since v2.1.59) and
`CLAUDE_CODE_DISABLE_AUTO_MEMORY` unset.

**Codex adapter** (ships `experimental` per ADR 02 — the file layout is partly
third-party-documented): watches `$CODEX_HOME/memories/` and
`memories_extensions/chronicle/`. Global scope → `_global/codex/`, enrolled as one
pseudo-project via a single dashboard toggle. Doctor checks
`features.memories = true` in `~/.codex/config.toml`. Classification:
`raw_memories.md` → Fact (append-mostly); `memory_summary.md`, `MEMORY.md`,
`rollout_summaries/*` → Regenerated (newest-wins); `skills/**/SKILL.md` and
chronicle `*.md` → Fact. The classification table is **config-overridable**, so
upstream format drift is absorbed without a release. Codex's background
consolidator may rewrite what we mirror out; that converges through the loop
(accepted risk, §11). Provider-side pruning (`max_rollout_age_days`, default 30)
propagates as normal deletions via the manifest.

## 7. CLI & UX

Bare `agent-brain` prints help. Command tree:

- **`init`** — first-run wizard (huh forms): gh detect/auth → create-or-clone
  `agent-brain-memories` → keyset generate (first machine) or import (joining) with
  the password-manager prompt → service install → discovery scan → enrollment
  picker. Flags make it fully scriptable: `--non-interactive`,
  `--generate-key`/`--import-key` (mutually exclusive), `--skip-service`,
  `--enroll all|none`, `--repo-name`.
- **`dashboard`** — a live bubbletea (Charm v2) TUI over the running daemon's UDS
  API, four tabs on a 2-second poll: **Projects** (per-unit table — provider ·
  folder · health · watch state · last-cycle result, with a LOCAL DIR column on
  terminals ≥120 cols; `s` syncs the selected unit, `t` untracks it behind a y/N
  confirm, `a` discovers untracked memory roots and enrolls one — path confirm,
  remoteless naming, and the `named/` contract shared with `track` via
  `provider.NamedIdentity`), **Conflicts** (retained retain-both records via the `conflicts`
  loader), **Activity** (daemon uptime, quiesce deadline, the fleet watch-trigger
  count as the max over units, and the last cycle's mirror/push summary), and
  **Doctor** (the read-only `--offline` battery with per-check glyphs). A
  full-screen notice offers `s` to start the login service when the daemon is
  down. Requires an interactive terminal — `status --json` / `projects --json`
  are the scriptable equivalents.
- **`track [path] | track --all`**, **`untrack <path|folder> [--purge | --yes]`** —
  enrollment; `--purge` also removes the project folder from the repo (history
  retains it).
- **`sync [--project X]`**, **`status [--json]`**, **`projects [--json]`**,
  **`conflicts [list | show <path>]`**, **`doctor [--fix | --json | --offline]`**,
  **`scan [--project X | --json | --reveal-secrets]`** (gitleaks plaintext-leak
  scan — advisory, §5/§11), **`service install|uninstall|start|stop|status|logs [-n]`**,
  **`key export`** / **`key import [--force]`** / **`key rotate [--yes]`** (fail-closed
  fleet re-encrypt, §5), **`migrate [--skip-preflight | --yes]`** (§10),
  **`daemon run`** (foreground).
- **`update [version] [--check | --prerelease | --list [--json] | --select |
  --no-restart]`** — gh-native self-update (ADR 18): resolve the target
  release — newest by default (semver max; stable channel, `--prerelease`
  admits release candidates), an exact pin when a version is named
  (`update v2.1.0` — the channel flag doesn't apply), or an interactive pick
  via `--select` (TTY only) — download archive + checksums through
  authenticated `gh`, verify sha256, sanity-run the new binary, atomically
  swap it over the current executable, then bounce the service and confirm
  daemon readiness. `--list` prints exactly the releases a version argument
  accepts (`--json` for scripts) — the same rows the picker offers. Implicit
  resolution never downgrades; an explicitly named OLDER release installs
  after a downgrade warning (strict config parsing, ADR 17, may refuse
  newer-version state — doctor is the follow-up). Refuses unstamped dev
  builds and Homebrew-managed installs (`brew upgrade` owns those).
- Hidden plumbing invoked by git: `git-clean`, `git-smudge`, `git-textconv`,
  `git-merge`.

UX rules: mutating commands print what happened and the next step; read commands
offer `--json`; `NO_COLOR` and non-TTY degrade to plain output (fang provides
styled help/errors, shell completions, man pages); every error message names the
fix or points at `doctor`.

## 8. Code & project structure

Anchored to the official Go module-layout guidance (go.dev/doc/modules/layout):
`cmd/` for commands in a mixed repo, `internal/` for all private packages, no
`pkg/` directory (absent from official guidance; Charm's crush is internal/-only —
gh's `pkg/` is historical). Single module at the repo root —
`module github.com/Sawmonabo/agent-brain`, `go 1.26`, `toolchain go1.26.5` — so
`go install .../cmd/agent-brain@latest` works by construction.

Per ADR 11 (greenfield): the legacy bash system (`home/`, `tools/`, chezmoi
scaffolding) is DELETED on `develop`. Migration reads machine-local runtime state,
never legacy repo files (§10), so nothing in-repo needs to survive; `main` retains
the bash world until v2 merges.

```
agent-brain/
├── go.mod                     # module github.com/Sawmonabo/agent-brain
├── cmd/agent-brain/main.go    # thin: fang.Execute(cli.Root(), fang.WithVersion(cli.Version))
├── internal/
│   ├── cli/                   # cobra tree, one file per command
│   │   └── dashboard/         # bubbletea models/views
│   ├── daemon/                # lifecycle, ticker, idle-exit (WSL2)
│   │   └── api/               # UDS server + client + shared request/response types
│   ├── watch/                 # fsnotify manager, debounce, poll backstop
│   ├── engine/                # sync engine: mirror, commit, integrate, reconcile
│   ├── gitx/                  # system-git exec wrapper
│   ├── crypto/                # tink daead wrapper; clean/smudge/textconv/merge endpoints
│   ├── keys/                  # keyset generate/export/import
│   ├── provider/              # adapter interface + registry
│   │   ├── claude/
│   │   └── codex/
│   ├── repo/                  # memories-repo layout, projects registry, manifests
│   ├── config/                # config.toml, platform paths (XDG / macOS)
│   ├── service/               # kardianos install/uninstall, WSL2 spawn mode
│   ├── ghx/                   # gh CLI exec wrapper: auth, provisioning, releases
│   ├── doctor/                # check battery + the daemon's sync SafetyGate
│   └── selfupdate/            # gh-native self-update pipeline (ADR 18)
├── test/e2e/                  # testscript txtar scripts + real-git harness
├── docs/                      # this spec + decisions/ (ADRs) + plans/
├── lefthook.yml               # pre-commit/pre-push hooks (ADR 12)
├── .golangci.yml              # golangci-lint v2 config, curated set (ADR 12)
├── .goreleaser.yaml
└── .github/                   # workflows/ci.yml, dependabot.yml
```

(`testdata/` directories sit inside each package as needed.)

(The once-planned `internal/provision` package was folded into `internal/ghx`
plus `internal/cli`'s init steps — ADR 08 records the provisioning design.)

**Package boundary rule:** `engine` depends on `gitx`/`crypto`/`provider`/`repo`
interfaces — never on `cli` or `daemon`; the `daemon/api` types are the only
surface shared between daemon and CLI, so the two cannot drift. Everything under
`internal/` — zero public-API commitment, free refactoring.

## 9. Engineering standards & tooling

- **Toolchain currency:** `go.mod` declares `go 1.26` + `toolchain go1.26.5`; Go's
  automatic toolchain management builds with the pinned latest everywhere,
  regardless of package-manager lag. Dependabot bumps the toolchain patch, module
  deps, and GitHub Actions weekly. Local brew kept current.
- **Formatting:** gofmt + gofumpt, CI-enforced. **Line length: no hard limit** —
  the official Google Go style guide has none and gofmt deliberately doesn't wrap;
  ~100 columns is soft review guidance. (golines exists as a golangci-maintained
  fork if hard wrapping is ever wanted — not adopted; not the ecosystem norm.)
- **Static analysis:** the compiler plus staticcheck/govet via golangci-lint — the
  Go analog of deep type-checkers like Astral's ty. No `any` in exported surfaces;
  generics only where they delete real duplication.
- **Linting:** golangci-lint v2.12.2, curated set: govet, staticcheck, errcheck,
  revive, gosec, errorlint, misspell, unconvert, unparam, nolintlint. Every
  `//nolint` carries a linter name and reason (nolintlint enforces).
- **Hooks (lefthook v2.1.9):** pre-commit = fast only (gofumpt check, golangci-lint
  on staged files, `go mod tidy` drift); pre-push = `go test ./... -race`.
  Conventional Commits formalized.
- **CI (GitHub Actions):** PR gates to develop/main — lint, test matrix
  (macos-latest + ubuntu-latest, `-race`, coverage), govulncheck. Actions pinned by
  commit SHA. Releases via GoReleaser on tag (§13). WSL2 is not CI-able → manual
  runbook (§12).

## 10. Migration & retirement

Memories currently live in **two places** per machine: (a) `~/.agent-brain/<slug>/`
— bash-era runtime plaintext from wrapper-managed sessions; (b)
`~/.claude/projects/<slug>/memory/` — Claude's default path, holding every
plain-`claude` session plus the entire pre-v3 era (local-scope
`autoMemoryDirectory` was silently rejected). v2 watches (b) natively, so migration
only rescues (a).

**`agent-brain migrate`** — one-time, idempotent, import-only (the sole
backward-compat surface, ADR 11):

**Pre-flight (per machine, before migrate reads anything):** the bash-era
system cannot propagate deletions, so `chezmoi --config
~/.config/agent-brain/chezmoi.toml diff` may list source-only orphans —
memories deleted from `~/.agent-brain/<slug>/` that any stray `apply` would
resurrect straight into the migrate seed. The diff must be EMPTY first:
adjudicate each orphan (restore keepers to the destination, `chezmoi forget`
confirmed deletions, commit + push the legacy source). Executed on
Sawmons-MacBook-Pro 2026-07-08 (30 orphans → 28 forgotten, 2 restored); the
history scrub below is the point of no return for anything left
unadjudicated.

1. Requires `init` complete (repo + keyset + daemon).
2. Enumerates `~/.agent-brain/<slug>/` dirs (skipping `.lock`/`.sync-pending`),
   best-guess maps slugs → projects, confirms interactively (the same huh picker
   as enrollment).
3. Commits each into `agent-brain-memories/<project>/claude/` as the **seed
   layer**.
4. Enrollment's first mirror-in then overlays live default-path state as a second
   commit — **layered, not merged**: one machine means no concurrent branch, so no
   conflict; history preserves both layers. Retain-both machinery engages only
   across machines (a second machine's migrate merges normally; divergence is
   retained).
5. An imported-from marker (host + slug) in the repo manifest makes re-runs no-ops.

**Retirement checklist** (per machine, after verified import; `doctor` detects
leftovers): remove the SessionStart healthcheck hook; delete `~/.local/bin/ab-claude`
and the healthcheck script; strip `autoMemoryDirectory` from per-project
`.claude/settings.local.json`; remove `~/.config/agent-brain/chezmoi.toml`; delete
the `~/.agent-brain/` runtime dir. The age key stays archived ONLY until the scrub
below completes, then retires everywhere.

**History scrub — decided** (ADR 13): after migration is verified on every machine,
this repo's history is rewritten with git-filter-repo v2.47.0
(`--sensitive-data-removal --invert-paths --path home/dot_agent-brain`) — removing
the blobs empties the `memory:` commits so filter-repo prunes them, taking
hostname/timing metadata along. Verification = gitleaks full-history scan + manual
inspection BEFORE any push. Because GitHub retains cached views and unreachable
objects past a force-push, the chosen finish for this private, forkless repo is
**delete-and-recreate**. Gate: v2 merged to main + all machines migrated + a local
pre-scrub archive kept through the first weeks. Going public afterward becomes a
zero-cost option.

## 11. Failure modes & security

The through-line: **worst case is staleness, never loss — and provider dirs never
see partial state.**

| Failure | Behavior → recovery |
|---|---|
| Daemon dead | Sessions unaffected (agents write native paths); startup recovery scan + polling backstop catch up |
| Offline / push fails | Commits accumulate locally (git-native queue); ticker retries via cenkalti/backoff/v5 — `Permanent()` for non-retriable, `MaxElapsedTime` set explicitly per loop (defaults to 15 min otherwise) |
| Push race between machines | Fetch + rebase; driver auto-resolves; bounded retries, then next cycle |
| Merge driver failure | Rebase aborts clean → merge-commit fallback → still failing: project DEGRADED (dashboard banner + doctor guidance); local commits continue; mirror-out withheld until integrate succeeds |
| Keyset missing or stale | Missing → smudge fails fail-closed (`filter.required`); that sync pauses; provider dirs keep last-good plaintext. Stale after a fleet `key rotate` → the keyset loads but cannot decrypt the re-encrypted tip: the engine degrades fail-closed and doctor's advisory `keyset-decrypt` probe Warns with `agent-brain key import --force` (it probes `origin/main` first, since HEAD stays frozen at the last decryptable commit) |
| Filters not installed (fresh clone) | `required=true` blocks commit AND checkout; `doctor --fix` reinstalls `.git/config` wiring |
| Provider format drift | Classification table is config-driven; unknown files default to Fact (merge + retain-both — never dropped, never newest-wins); new unknowns logged |
| Provider clobbers mirror-out | Accepted risk (a) below — re-enters the loop, re-merges, converges; git retains every state |
| Partial writes / disk-full | Mirror-out = renameio/v2 atomic replace (POSIX-only targets — fine; ~20-line stdlib fallback documented in ADR 14); git ops transactional; retry next cycle |
| WSL2 VM teardown mid-sync | flock kernel-released (no orphaned-lock class); restart = crash-recovery path; runtime dir recreated 0700 each start |
| fsnotify overflow / dropped events | Debounced rescan + polling backstop self-heal |
| Clock skew | Affects only the newest-wins (provider-regenerated) class; bounded — the provider regenerates anyway |
| Repo growth | Deterministic ciphertext can't delta-compress across versions; KB-scale files make this slow-burn; periodic `git gc`; a maintenance/squash command stays on the shelf |

Daemon logging: `log/slog` (stdlib), JSON handler.

**Security posture:**

- **Threat model:** GitHub-at-rest = Tink AES-SIV ciphertext; local disk =
  plaintext BY DESIGN (agents must read it); visible metadata = filenames, sizes,
  timing, identical-file equality (ADRs 00/06).
- **Keys & surfaces:** keyset 0600, never rides any repo; export/import
  user-initiated only; password-manager copy is the recovery path. Socket 0600
  UDS; no TCP anywhere; single-instance flock. gh token borrowed at provision time
  only, never persisted; NEVER `gh auth setup-git` (ADR 08).
- **Supply chain:** SHA-pinned actions, Dependabot, govulncheck, pinned toolchain
  (ADR 12), checksummed immutable releases (§13).
- **Memory-content risk:** agents can write secrets into memories, which then sync
  (encrypted at rest, plaintext across machines). `agent-brain scan [--project]
  [--json]` runs the user's gitleaks over enrolled plaintext on demand, and doctor's
  advisory `secrets-scan` check reports whether gitleaks is installed — the awareness
  surface. Per-cycle/per-commit scanning is a **decided non-goal**: a subprocess on
  every save adds latency and false-positive fatigue for zero wire-exposure reduction
  (the wire is ciphertext regardless). (ADRs 10/14; Task 13.1 amends ADR 14's use-(1)
  "scan before commits" framing.)

**Accepted risks, formally:**

- **(a) Two-way-writing provider-owned, undocumented directories.** Mitigations:
  clobbers re-enter the loop and re-merge; git history never loses; adapters
  quarantine drift behind config-overridable classification.
- **(b) First-party Claude memory sync** (anthropics/claude-code#25739) may
  obsolete the Claude half. Hedge: multi-provider support and user-owned data.
- **(c) "The next LLM session tidies retain-both blocks" is an UNPROVEN
  hypothesis.** Backstop: the conflicts dashboard view and `conflicts` command
  keep retained blocks visible and actionable regardless.

## 12. Testing strategy

- **Unit:** stdlib `testing` + google/go-cmp for equality diffs — no assertion
  frameworks (Google's style guide permits stdlib testing only and warns against
  `reflect.DeepEqual`; ADR 15). Table-driven, `t.Parallel()`, `t.TempDir()`.
- **CLI/e2e:** rogpeppe/go-internal **testscript** (implemented, `test/e2e/`,
  `TestScripts`) — txtar scripts that drive the REAL binary as a subprocess against
  `git init --bare` remotes with a faked `gh`, zero network. Six flows:
  `init_first_machine`, `track_and_sync`, `migrate`, `doctor_fix`, `key_roundtrip`,
  `key_rotate`.
- **Adversarial containment:** a STANDING corpus (`TestAdversarialContainment`, eleven
  rows as of 2026-07-09) that raw-pushes hostile input from a clone with NO filters
  wired — an attacker who never ran agent-brain — and pins each engine containment
  invariant, every row ending on the universal no-plaintext-on-the-wire assertion.
  Later phases only APPEND rows, never delete (spec §11). The last two rows pin the
  commit boundary the other nine structurally miss: poison ALREADY RESIDENT when a
  checkout is first cloned or seeded, rather than delivered by a later integrate.
- **Integration:** real system git in `t.TempDir()` — `git init --bare` as the
  fake remote, zero network. The critical scenario: two simulated "machines" clone
  the bare repo, write divergent memory, and sync — asserting the full
  filter/merge-driver chain: clean/smudge roundtrip, retain-both blocks,
  derived-index reconcile, newest-wins classes. This is the only way to test the
  merge driver (git invokes it, not us), and it doubles as living proof of the
  concurrency guarantees.
- **Fuzzing:** native `go test -fuzz` on the crypto roundtrip (decrypt∘encrypt =
  identity; determinism: equal plaintext+key ⇒ equal ciphertext), merge-driver
  three-way inputs, and classification parsing.
- **Daemon logic:** the single-writer loop tested with an injected fake clock and
  synthetic fs events; the UDS API tested client↔server in-process over a real
  socket.
- **CI:** `-race` on all runs; coverage tracked, no hard gate in v1. **WSL2**
  cannot run in hosted CI → a manual runbook committed to the repo, executed
  before release tags touching daemon/service/watch code.
- The bats suite retires with the legacy bash tree (ADR 11).

## 13. Distribution & install

- **GoReleaser v2** on tag push (`.goreleaser.yaml` version-2 schema; the
  `release.yml` workflow runs the SHA-pinned goreleaser-action at `~> v2` — current
  GoReleaser release v2.17 as of 2026-07-10, no v3 exists): darwin and linux ×
  arm64+amd64 tar.gz archives, checksums, Conventional-Commits changelog, under
  GitHub's immutable-releases policy.
- **Darwin ad-hoc signing is mandatory to execute (decision 12), not cosmetic.**
  Apple Silicon's AMFI SIGKILLs (`killed: 9`) any binary without a valid signature,
  and a Go binary cross-compiled on a Linux runner carries only a linker signature
  macOS 26 can treat as corrupt. A GoReleaser post-build hook
  (`scripts/adhoc-sign.sh`) runs anchore/quill in ad-hoc mode (no certificate) over
  each darwin artifact; it no-ops on non-darwin so CI and local snapshot builds sign
  identically in kind. Full Developer-ID notarization is the recorded public-launch
  upgrade path (ADR 16), not shipped here.
- **Homebrew via a FORMULA (`brews`), not a cask (decision 13, reversing ADR 16's
  original cask lean).** Formulae never set `com.apple.quarantine`, so Gatekeeper
  never engages — the cask + `xattr` quarantine-strip hook the original design
  assumed is defeated on macOS 26. Personal tap `Sawmonabo/homebrew-tap`, published
  with `skip_upload: auto`: prerelease (`v2.0.0-rc.*`) tags ship release assets but
  push no formula, so `brew install sawmonabo/tap/agent-brain` activates only with
  the public `v2.0.0` tagged after the ADR-13 scrub.
- **Interim install while this repo is private** (pre-scrub, dated 2026-07-10):
  Homebrew fetches assets anonymously and this repo is private, so brew is not yet
  live. Use `gh release download <tag> -R Sawmonabo/agent-brain -p '<os>_<arch>'`
  (authenticated) or
  `go install github.com/Sawmonabo/agent-brain/cmd/agent-brain@latest` (owner git
  access, §8). No self-update code — `brew upgrade` / re-download owns that.
- **New-machine onboarding** (target: under 5 minutes; per-OS runbook in
  `docs/onboarding.md`): install → `agent-brain init` (gh auth → clone memories repo
  → `key import` from password manager → service install → enrollment picker) →
  `agent-brain migrate` if the machine has bash-era state → done.

---

## Appendix: verified version pins (go.mod + release config, verified 2026-07-10)

Go 1.26.5 (toolchain pin, go.mod) · tink-go v2.7.0 (AES256_SIV) · cobra v1.10.2 ·
fang v2.0.1 · huh v2.0.3 (`charm.land/huh/v2`) — direct. Charm v2 TUI at
`charm.land/*`, now **direct** deps (the Task-6 `dashboard` package imports them):
bubbletea v2.0.8 · lipgloss v2.0.5 · bubbles v2.1.1 (were v2.0.2 / v2.0.1 / v2.0.0
and transitive when this spec was drafted). fsnotify v1.10.1 · kardianos/service
v1.3.0 · gofrs/flock v0.13.0 · cenkalti/backoff/v5 v5.0.3 · google/renameio/v2
v2.0.2 · pelletier/go-toml/v2 v2.4.3 · google/go-cmp v0.7.0 · rogpeppe/go-internal
v1.15.0 (testscript; pulls golang.org/x/tools v0.38.0 transitively). Runtime/CI
tools, never vendored: gh ≥ 2.40 (CLI flags verified at v2.96.0) · golangci-lint
v2.12.2 (ci.yml) · gitleaks v8.30.1 (pinned in ci.yml; the lefthook hook uses the ambient install) · govulncheck v1.5.0 (ci.yml)
· GoReleaser `~> v2` (workflow pin; v2.17.0 local, 2026-07-10) · anchore/quill v0.7.1
(darwin ad-hoc signing, release.yml) · gofumpt 0.10.0 + lefthook v2.1.9 (local brew,
not repo-pinned) · git-filter-repo v2.47.0 (scrub runbook). The go.mod versions above are the resolved
graph at this tip; Dependabot keeps them current thereafter.
