# ADR 20: Dashboard hub — bare-command entry, $EDITOR editing, read-only history API

- **Status:** Accepted
- **Date:** 2026-07-12
- **Deciders:** Sawmon (directive: the dashboard becomes the single main
  interactive surface — all proposed capabilities in, feature-rich, no
  deferrals; edit/init behaviors to follow verified modern best practice)
- **Related:** ADR 03 (single writer), ADR 04 (resident daemon), ADR 05
  (CLI/TUI stack), ADR 09 (daemon IPC), ADR 18 (self-update), spec §7
- **Spec:** `docs/01-dashboard-hub-spec.md` (full UX and feature surface)

## Context

The v1 dashboard (spec §7) is a read-mostly monitor: four tabs
(Projects/Conflicts/Activity/Doctor) with in-TUI track/untrack/sync. Sawmon's
post-cutover direction is a hub: browsing and *reading* every project's
memories, editing them with automatic capture+push, per-memory history with
restore, global search, and the operational actions (enroll, update, doctor)
available without leaving the TUI. Three design forks needed evidence, not
taste: how editing should work, what the bare command does on an
uninitialized machine, and how the TUI may touch data without violating the
single-writer architecture. Each was researched against current
well-maintained tools before deciding (see Sources).

## Decision 1: bare `agent-brain` opens the hub; uninitialized launch is TTY- and agent-gated

`agent-brain` with no arguments launches the dashboard (the k9s/lazygit/
gh-dash pattern for interactive-first tools). Every subcommand is unchanged;
scripting surfaces stay scriptable. `agent-brain dashboard` remains as an
alias of the bare command.

On an **uninitialized** machine:

- **Human TTY:** print an explicit announcement ("agent-brain is not set up
  on this machine — starting guided setup; esc cancels"), then run the
  existing interactive `init` flow and land in the hub on success. Cancelling
  exits non-zero with the `agent-brain init` pointer. This is the current
  pattern of the tools closest to ours — Claude Code and crush auto-run
  inline onboarding; vercel and stripe announce-then-launch login — and every
  auto-launcher studied announces first and stays interruptible. Launching
  init is not a silent mutation: init is itself a step-by-step interactive
  flow with its own confirmations, so no lazygit-style y/N pre-gate is added
  in front of a flow that is all questions.
- **Non-TTY (pipe/CI):** never prompt, never hang — exit non-zero with the
  exact pointer, gh-style. clig.dev: "Only use prompts or interactive
  elements if stdin is an interactive terminal"; every surveyed tool obeys
  this without exception.
- **Detected coding agent:** exit with the pointer even on a TTY. Stripe and
  vercel fingerprint agents via env vars (`CLAUDECODE`, `CURSOR_AGENT`,
  `CODEX_*`, `GEMINI_CLI`, `CLINE_ACTIVE`, `OPENCODE`, …) and route them away
  from interactive wizards, because an agent cannot answer one; a tool whose
  purpose is serving AI coding agents adopts the same discipline.

## Decision 2: editing is a hardened $EDITOR handoff — no embedded multi-line editor

Research across the Go/charm ecosystem (lazygit, gitui, tig, k9s, glow, nap,
bubbletea's own exec example) found **zero** embedded multi-line editors for
file bodies — all suspend to the user's editor via `tea.ExecProcess`. The
note-management domain (nb, zk, jrnl, dnote, Joplin terminal) is the same;
zk states it as philosophy ("zk is not a text editor"), and the one
hand-rolled embedded editor found (basalt, "experimental") documents its own
missing basics — no undo/redo, no clipboard, no selection. Embedded editors
win only where the framework ships a serious widget (Python Textual's
TextArea); Go has none of that caliber. So:

- `e` suspends the TUI and opens the user's editor via `tea.ExecProcess`,
  resolving `$VISUAL` then `$EDITOR` (`charm.land/x/editor` provides
  resolution plus the per-editor line-jump dialect table), parsing the
  command with a POSIX-aware word splitter, never `strings.Fields`.
- The editor gets a **disposable scratch copy** outside the watched tree —
  never the live provider file. Editors' atomic-save rename semantics are a
  documented hazard class for fsnotify watchers (vim `backupcopy`, swap
  files); git/kubectl/crontab/crush all hand the editor a scratch copy.
- On return: byte-compare against the original → unchanged means "Edit
  cancelled, no changes made" (kubectl's pattern); changed content is written
  back by write-temp-in-same-dir + `os.Rename` into the provider dir — one
  atomic replace, one clean watcher event, captured and pushed by the normal
  engine cycle. The hub shows the capture confirmation as a toast.
- GUI editors (`cursor --wait`, `code --wait`) are a **configured** case, not
  auto-detected — an `editInTerminal`-style config knob following lazygit,
  since no surveyed tool has ever shipped reliable auto-detection.
- The one-frame view-leak around the exec boundary (bubbletea issue #431,
  open at decision time) is suppressed with the documented render-empty
  workaround.
- If no editor is configured, the edit binding is hinted as unavailable with
  an honest message (crush gates feature visibility the same way) — never a
  silent default.
- Single-line `textinput` fields remain fine for names/metadata (the nap
  split: inputs for one-liners, `$EDITOR` for bodies).

## Decision 3: reads split by trust domain; every mutation stays on the provider-dir path

- **Provider dirs** (plaintext, agent-owned): the hub reads them directly for
  browsing, rendering, search, links, and lint — the same trust domain as
  the agents that write them. All hub **mutations** (edit, new, delete,
  rename, restore) are writes into provider dirs, picked up by the watcher
  and committed by the daemon's single engine writer. ADR 03's invariant —
  the CLI never writes inside the memories checkout — is untouched.
- **Memories checkout** (daemon-owned): the hub never opens it. Two new
  **read-only** daemon endpoints serve history:
  - `GET /v0/history?folder=&path=` — commits touching a path (folder-wide
    with per-version changed paths when `path` is omitted, which also serves
    deleted-memory discovery), machine and timestamp parsed from the
    capture-commit subject convention
    (`memory: <host> <folder> <timestamp>`, engine/commit.go).
  - `GET /v0/blob?rev=&path=` — decrypted content at a revision (the daemon
    reads its checkout via the existing textconv/filter machinery).
  Restore is therefore not a git operation at all: the hub writes the chosen
  historical content into the provider file, and the restore becomes a new
  capture commit — history only ever grows.

## Decision 4: feature surface ships whole

The full roster is in scope for the phase plan (detail in the spec): memory
browser with glamour-rendered reading view; per-memory history/time-travel
with diff and restore; global fuzzy+full-text search; `[[link]]` navigation
with backlinks and dangling-link detection; memory lint; per-project and
fleet insights; conflict center; update banner + one-key self-update; one-key
quiesce-aware doctor --fix; full enrollment parity (init-style picker +
migrate) in-hub; gitleaks scan integration; command palette, help overlay,
and Catppuccin-consistent theming. `charm.land/glamour/v2` and
`charm.land/x/editor` join the dashboard package's allowed imports (extending
ADR 05's bubbletea/lipgloss carve-out).

## Consequences

- `internal/cli/dashboard` grows substantially and splits into focused
  subpackages; it remains the only package importing the TUI stack.
- The daemon API gains two read-only endpoints (same UDS + peer-UID
  enforcement, ADR 09); the mutation surface is unchanged.
- Root-command behavior changes (bare invocation launches a TUI); README and
  onboarding docs must say so, and `--help` remains reachable as ever.
- The uninitialized-TTY path couples the hub to the init flow's cancel
  semantics (esc-cancel, Task J) — cancelling setup must exit cleanly with
  the pointer, never strand a half-provisioned machine silently.

## Buy vs build

Buy: `charm.land/glamour/v2` (markdown render), `charm.land/x/editor`
(editor resolution + line-jump dialects), `mvdan.cc/sh/v3/shell` (POSIX word
split), existing gitleaks integration (scan). Build: history endpoints,
search/lint/links over provider dirs, and the views — nothing else exists
that speaks our daemon API.

## Sources

Editing-model research (WebSearch/WebFetch, 2026-07-12; three parallel
surveys — charm ecosystem, note-management domain, handoff pitfalls):

- https://github.com/jesseduffield/lazygit/blob/master/pkg/config/editor_presets.go (per-editor suspend table)
- https://github.com/jesseduffield/lazygit/blob/master/docs/Config.md (editPreset / editInTerminal)
- https://github.com/jesseduffield/lazygit/issues/437 and pull 454 (TTY-corruption postmortem)
- https://github.com/gitui-org/gitui/blob/master/src/popups/externaleditor.rs
- https://github.com/jonas/tig/blob/master/src/display.c (the ancestral suspend idiom)
- https://github.com/derailed/k9s/blob/master/internal/view/exec.go
- https://github.com/charmbracelet/glow/blob/master/ui/editor.go
- https://github.com/maaslalani/nap/blob/main/model.go (ExecProcess for bodies; textinput for metadata)
- https://github.com/charmbracelet/bubbletea/blob/main/examples/exec/main.go
- https://github.com/charmbracelet/bubbletea/blob/main/exec.go (v2 exec/release/restore)
- https://github.com/charmbracelet/bubbletea/issues/431 (pre-exec view leak; open) and discussions/424
- https://github.com/charmbracelet/x/blob/main/editor/editor.go (resolution + line-arg dialects)
- crush's openEditor + EnvMsg rationale: https://github.com/charmbracelet/crush
- https://github.com/xwmx/nb/blob/master/README.md · https://zk-org.github.io/zk/tips/editors-integration.html ("zk is not a text editor")
- https://jrnl.sh/en/stable/external-editors/ ("all editors must be blocking")
- https://github.com/dnote/dnote/blob/master/pkg/cli/cmd/edit/note.go
- https://joplinapp.org/help/apps/external_text_editor/
- https://github.com/erikjuhani/basalt/blob/main/docs/Known%20Limitations.md (embedded-editor costs)
- https://posting.sh/guide/external_tools/ (Textual-side hybrid; F4 escape hatch)
- https://textual.textualize.io/blog/2023/09/18/things-i-learned-while-building-textuals-textarea/ ("no clear finish line")
- kubectl edit no-op/validation loop: https://github.com/kubernetes/kubectl/blob/master/pkg/cmd/util/editor/editoptions.go
- fsnotify atomic-save hazards: https://github.com/fsnotify/fsnotify/issues/17 · /issues/372 · https://github.com/derailed/k9s/issues/1192
- vim backupcopy semantics: https://nullvm.github.io/posts/vim-backup-copy/
- GUI --wait fragility: https://github.com/microsoft/vscode/issues/269502 · https://github.com/zed-industries/zed/issues/54203 · https://github.com/anthropics/claude-code/issues/36516

Uninitialized-launch research (WebSearch/WebFetch, 2026-07-12; TUI first-run
survey + guidelines/setup-gating survey, gh behavior verified live):

- https://clig.dev/ ("Only use prompts … if stdin is an interactive terminal"; "Never require a prompt"; error-rewriting guidance)
- https://github.com/cli/cli/blob/trunk/pkg/cmd/root/help.go (gh authHelp: pointer always, never auto-launch; CI-aware wording)
- https://github.com/stripe/stripe-cli/blob/master/pkg/cmd/login_helpers.go (shouldAutoLogin: TTY ∧ no AI agent)
- https://github.com/stripe/stripe-cli/blob/master/pkg/useragent/useragent.go (agent env fingerprints)
- https://github.com/vercel/vercel/blob/main/packages/cli/src/index.ts (announce-then-launch; agent device-code branch)
- https://github.com/superfly/flyctl/blob/master/internal/command/root/root.go · https://github.com/firebase/firebase-tools/blob/main/src/requireAuth.ts · https://github.com/aws/aws-cli/blob/develop/awscli/clidriver.py (error+pointer family)
- https://github.com/tailscale/tailscale/blob/main/cmd/tailscale/cli/up.go (auth lives in its own command)
- lazygit y/N create-repo offer: https://github.com/jesseduffield/lazygit/blob/master/pkg/i18n/english.go · issues/1091 · pull/1098
- gitui hard refusal: https://github.com/gitui-org/gitui/blob/v0.27.0/src/main.rs#L128-L131
- k9s in-TUI context picker vs config panic: https://github.com/derailed/k9s/blob/master/cmd/root.go · issues/2458 · issues/267
- gh-dash unauthenticated raw-error banner: https://github.com/dlvhdr/gh-dash/blob/main/cmd/root.go
- Claude Code first-run wizard: https://code.claude.com/docs/en/quickstart
- crush onboarding auto-launch: https://github.com/charmbracelet/crush/releases/tag/v0.28.0
- aider offer→OAuth: https://aider.chat/docs/troubleshooting/models-and-keys.html
- OpenCode empty-state + /connect pointer: https://opencode.ai/docs/
- atuin/glow/zellij/helix/yazi (optional-setup control group): https://docs.atuin.sh/cli/guide/sync/ · https://github.com/charmbracelet/glow/releases/tag/v2.0.0 · https://zellij.dev/documentation/welcome-screen-alias.html · https://helix-editor.vercel.app/help/faq · https://yazi-rs.github.io/docs/configuration/overview/
- Prompt-vs-flag doctrine: https://medium.com/@jdxcode/12-factor-cli-apps-dd3c227a0e46 · https://devcenter.heroku.com/articles/cli-prompt
