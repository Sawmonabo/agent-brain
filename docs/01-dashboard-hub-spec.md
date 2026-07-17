# Dashboard hub — design spec

Companion to ADR 20 (decisions + research trail). This document is the UX and
feature surface the phase plan implements. The canonical system spec
(`00-design-spec.md`) is unchanged; §7's dashboard clause is superseded by
this document for the hub era.

## 1. Entry

- `agent-brain` (no arguments) launches the hub. `agent-brain dashboard`
  stays as an alias. Every other subcommand is unchanged.
- Uninitialized machine, human TTY: print
  `agent-brain is not set up on this machine — starting guided setup (esc cancels)`
  then run the existing interactive init flow; on success, open the hub.
  Cancel at any form exits non-zero with `run: agent-brain init`.
- Uninitialized, non-TTY (pipe/CI): exit non-zero immediately with
  `agent-brain is not initialized. To get started, run: agent-brain init`
  — no prompt, no hang (clig.dev TTY rule; gh wording model).
- Uninitialized, coding-agent environment: same pointer exit even on a TTY.
  Detection: any of `CLAUDECODE`, `CURSOR_AGENT`, `CODEX_SANDBOX`,
  `CODEX_THREAD_ID`, `CODEX_CI`, `GEMINI_CLI`, `CLINE_ACTIVE`, `OPENCODE`,
  `OPENCLAW_SHELL`, `ANTIGRAVITY_CLI_ALIAS` set and non-empty (stripe-cli's
  fingerprint table).
- Initialized but daemon unreachable: the hub still opens, with the existing
  degraded banner and Doctor tab reachable (k9s posture: fixable states stay
  in-TUI).

## 2. View map

```
Root ─ tab bar: [Projects] [Conflicts] [Activity] [Doctor]
  │    status bar: daemon state · version · gh-auth alert · update banner · toasts
  │    overlays (any view): / global search · ctrl+k palette · ? help
  │
  ├─ Projects tab (table of tracked units; a add · m migrate · u untrack · s sync)
  │    └─ Enter → Memory browser (project)
  │         ├─ list pane: memories grouped by provider, lint badges
  │         ├─ preview pane: glamour render of selection
  │         ├─ i → Project insights
  │         └─ Enter → Reading view
  │              ├─ h → History view (versions · diff · restore)
  │              └─ links: [[slug]] follow · b backlinks
  ├─ Conflicts tab → detail → e open merged file in editor
  ├─ Activity tab (sync/capture feed)
  └─ Doctor tab (checks · f fix · scan results)
```

Navigation: `tab`/`shift+tab` cycle tabs, `j/k`/arrows move, `enter` drill
in, `esc` backs out one level (from root: quit prompt), `q` quits from root
views. Every keybinding appears in the `?` overlay; contextual hints render
in each view's footer, which is anchored to the terminal's bottom row on
every frame regardless of how tall the active view's own content is.

## 3. Memory browser (per project)

- Lists every memory file under each enrolled provider dir for the project
  (`UnitInfo.LocalDir`), grouped by provider, sorted by last-modified
  (newest first; `o` toggles name/recency ordering).
- Rows show: name, one-line description (frontmatter when present), relative
  modified time, lint badge (`⚠` when §8 flags the file).
- Preview pane renders the selection with `charm.land/glamour/v2`
  (theme-aware light/dark).
- While the preview pane is shown the mouse wheel scrolls it and a click
  focuses it. That turns on terminal mouse reporting, which suppresses the
  terminal's own drag-select in the browser. While that capture is armed,
  clicking a browser list row selects it. To copy text: press `y` to copy
  the selected memory to the system clipboard (OSC 52 — carries over SSH, tmux
  with `allow-passthrough`, and WSL2); press `m` to disarm mouse capture
  entirely so the terminal's own drag-select works again (mouse reporting is
  terminal-global — there is no scoped capture to keep — so the footer discloses
  `mouse: native select (m re-arms)` every frame it is off, and `m` re-arms it);
  bypass reporting for a single selection with Option-drag (macOS
  Terminal/iTerm2) or Shift-drag (Linux terminals, Windows Terminal); or press
  `enter` into the reading view — which captures no mouse — and select normally.
- Everywhere else the hub captures no mouse. Instead it sets the terminal's
  alternate-scroll mode (DECSET 1007) for the session, so the wheel scrolls
  hub content — delivered as arrow keys — with native drag-select intact on
  every screen (ADR 21; Codex's posture). Terminals that ignore the mode:
  kitty already translates wheel to arrows in the alternate screen
  unconditionally; tmux swallows it unless the user binds
  `WheelUpPane`/`WheelDownPane` with `#{alternate_on}` send-keys (documented
  beside the OSC52 `allow-passthrough` note). Config:
  `dashboard.alternate_scroll = false` restores the terminal's raw wheel.
- In-project filter: `/` inside the browser filters the list (fuzzy on
  name + description).
- Keys: `enter` read · `e` edit · `n` new · `r` rename · `d` delete ·
  `h` history · `i` insights · `y` copy to clipboard · `m` mouse capture ·
  `esc` back.

## 4. Reading view

- Full-width scrollable glamour render (viewport; `j/k`, `ctrl+d/u`,
  `g/G` — and the wheel via alternate scroll).
- Frontmatter summarized in a header line: name · type · modified · size.
- `[[link]]` navigation: `tab`/`shift+tab` cycle link targets (highlighted
  in the render), `enter` jumps to the linked memory's reading view
  (navigation stack; `esc` returns). Dangling targets render struck-through
  with a `dangling` marker.
- `b` toggles a backlinks panel listing memories that reference this one.
- `e` edit · `h` history · `y` copy provider-file path · `Y` copy memory to
  clipboard (OSC 52) · `esc` back.

## 5. Editing ($EDITOR handoff — ADR 20 decision 2)

Flow on `e` (browser or reading view):

1. Copy the provider file to a scratch path outside any watched tree
   (`os.MkdirTemp` under the user cache dir).
2. Resolve the editor: config `editor.command` if set, else `$VISUAL`, else
   `$EDITOR` (resolution built in the owned `editorx` package; command string
   POSIX-split with `mvdan.cc/sh/v3/shell.Fields`). None set → footer
   notice: `no editor configured — set $EDITOR or editor.command in config`
   (binding visibly disabled, crush-style).
3. Suspend via `tea.ExecProcess` with the one-frame empty-View workaround
   (bubbletea #431). Config `editor.in_terminal = false` runs the child
   without releasing the terminal (GUI editors invoked with their wait
   flag); completion arrives as a message while the hub stays live.
4. On return: byte-compare scratch vs original. Unchanged → toast
   `edit cancelled, no changes made`, scratch removed. Changed → write-temp
   + `os.Rename` into the provider dir (one atomic replace), scratch
   removed.
5. The watcher/engine captures normally; the hub watches the daemon's
   activity and toasts `✓ captured + pushed` (or surfaces the degraded
   banner if the cycle fails).

New (`n`): prompt for a name (single-line textinput), create the scratch
file pre-filled with the provider-correct frontmatter skeleton (for claude:
`name`/`description`/`metadata.type` + body stub), then the same handoff;
save lands the file via atomic rename and (claude provider) reminds about
the MEMORY.md index line in the toast. Rename (`r`): textinput prefilled;
provider file renamed atomically. Delete (`d`): modal confirm (default No)
naming the file; deletion is a provider-dir remove, captured as history —
recoverable via §6 restore.

## 6. History & time-travel

- `h` on a memory calls `GET /v0/history` and lists versions: short rev,
  absolute+relative time, machine (host), and whether it's the live
  version. Data source: capture-commit subjects
  (`memory: <host> <folder> <timestamp>`).
- `enter` renders that version (via `GET /v0/blob`), `d` shows a unified
  diff against the live content (and `D` between adjacent versions).
- `R` restore, modal confirm: writes the selected version's content into
  the provider file through the §5 atomic-rename path — restore is a new
  capture, never a git rewrite; history only grows. Works for deleted
  memories too (the browser exposes `h` on a `deleted` filter view listing
  paths present in history but absent on disk).

Daemon API (read-only, UDS + peer-UID as ever; served through the engine
goroutine so reads never race the writer):

- `GET /v0/history?folder=&path=&limit=` →
  `{versions:[{rev,host,timestamp,subject,paths,live}]}` — `path` is
  `<provider>/<file>` relative to the folder's checkout dir; omitted, the
  listing is folder-wide with each version carrying its changed paths
  (the source for the deleted-memories view: paths in history, absent on
  disk).
- `GET /v0/blob?folder=&path=&rev=` → `{content}` (decrypted via the
  checkout's filter machinery; size-capped; binary refused).

## 7. Global search

- `/` from any root view opens the search overlay: one input, live results
  (debounced) across every tracked project's provider dirs.
- Match tiers: memory name → frontmatter description → body text
  (case-insensitive substring; the match line shown with highlight).
  Fuzzy matching applies to the name tier.
- Results rows: project · provider · memory · matched fragment. `enter`
  opens the memory's reading view (link stack applies), `esc` dismisses.

## 8. Memory lint ("memory doctor")

Advisory, computed client-side while browsing (and summarized in insights):

- Malformed or missing frontmatter where the provider expects it
  (claude: `name`, `description` present and non-empty).
- Dangling `[[links]]` (target resolves to no memory in the project).
- Staleness: unmodified in >90 days → `stale?` hint (threshold in config,
  `lint.stale_after_days`, 0 disables).
- Index drift (claude provider): memory files absent from MEMORY.md or
  index lines pointing at missing files.

Badges on browser rows; detail in the insights view. Never blocks anything
(advisory, like scan).

## 9. Insights

- Per project (`i` in the browser): memory count per provider, total size,
  last capture time, most-edited memories (history commit counts), stalest
  memories, lint summary, machine-activity breakdown (which hosts committed,
  from history subjects).
- Fleet header on the Projects tab: units watched, watch posture
  (`WatchState`), last sync, daemon version vs latest release.

## 10. Conflict center

- Conflicts tab lists conflict events (existing conflicts.jsonl source):
  time, project, path, resolution class.
- Detail view: the event's metadata plus the current (union-merged) content
  rendered; `e` opens the live file through the §5 edit flow to clean up a
  merge; `enter` jumps to the memory's reading view.

## 11. Update & doctor actions

- Status bar polls the release feed at most once per hub session (existing
  `update --check` resolution): newer release → banner
  `vX.Y.Z available — U to update`.
- `U` → confirm modal → runs the ADR 18 self-update (checksum, atomic swap,
  service restart) with progress in a modal; on success the hub offers `R`
  to re-exec itself on the new binary (fallback message: restart manually).
- gh-auth alert: any gh call that classifies as auth-invalid (the update check
  above, or the Doctor gh row) arms a loud, sticky status-bar segment
  `gh auth invalid — Doctor tab: f re-authenticates`. Sticky by design — an
  invalid OAuth token stays invalid until a human re-auths, so only a gh probe
  that succeeds (a passing Doctor gh row, or the re-auth handoff below) clears
  it, never mere time. A dead token silently breaks only the gh-dependent
  features (the SSH memories remote keeps syncing), so this is the one loud
  signal the product owes; a silent re-mint is impossible (GitHub's flow is
  interactive).
- Doctor tab: `r` re-runs checks; `f` on a fixable failure runs the
  quiesce-aware `doctor --fix` path and re-renders — except when gh auth is the
  invalid piece, where `f` instead hands the terminal to interactive
  `gh auth login -h github.com` (the same suspend/resume seam the `$EDITOR`
  handoff uses, ADR 21's 1007 re-assert included), re-probes on return, and
  clears the alert on success; `s` runs the gitleaks scan (advisory §12).

## 12. Scan integration

- Palette action + Doctor-tab key: runs the existing gitleaks scan per
  project, results listed (finding count per file, advisory wording).
  Never joins SafetyGate (unchanged posture).

## 13. Enrollment parity

- Projects tab `a`: full init-style enrollment — discovery, the MultiSelect
  picker (post Task-L fix), per-candidate path confirm, remoteless-folder
  naming — driven through the daemon track/migrate endpoints exactly like
  the CLI.
- `m`: migrate flow for legacy stores (spec §10 importer) with the same
  adjudication prompts.
- `u`: untrack with confirm (existing).

## 14. Palette, help, theming

- `ctrl+k` command palette: fuzzy list of every action (sync all/project,
  add, migrate, untrack, update, doctor fix, scan, search, open project,
  quit). Single-source-of-truth action registry drives palette, keymap, and
  help so they cannot drift.
- `?` help overlay: full keymap by view.
- Theming: current styles consolidated into one Catppuccin Mocha-consistent
  palette (matches Sawmon's terminal), adaptive to light terminals via
  lipgloss adaptive colors.

## 15. Architecture & invariants

- `internal/cli/dashboard` remains the only TUI-importing package; it splits
  into subpackages (`views/`, `memoryfs/` read+atomic-write helpers,
  `search/`, `lint/`, `links/`, `editorx/` handoff) — all still under the
  dashboard tree.
- Reads: provider dirs directly (plaintext, agent trust domain); checkout
  only via the two read-only daemon endpoints.
- Writes: provider dirs only, always write-temp + `os.Rename`; the CLI/TUI
  never touches the memories checkout (ADR 03 intact); daemon mutation
  endpoints unchanged.
- Quiesce respected: while quiesced, mutating actions grey out with the
  existing refusal wording.
- Non-TTY safety: the bare command never prompts or opens the TUI without a
  TTY (§1); all hub-only features have CLI equivalents for scripting
  (existing subcommands; history via future `agent-brain history` is a
  non-goal for this phase).

## 16. Non-goals (this phase)

- No embedded multi-line editor (ADR 20 decision 2).
- No remote/SSH serving of the TUI. Keyboard-first: the wheel (alternate scroll) and the browser's scoped mouse reporting are conveniences layered over complete keyboard paths, never the only path.
- No new mutating daemon endpoints; no git operations from the TUI.
- No CLI `history` subcommand (hub-only for now).
- Windows-native posture unchanged (WSL2 rules per ADR 04 stand).

## 17. Acceptance criteria (seed list for the plan)

- Bare command: TTY+initialized opens hub; TTY+uninitialized announces and
  runs init then opens hub; esc during init exits non-zero with pointer;
  non-TTY prints pointer, exit non-zero, never prompts; each agent env var
  forces the pointer path even with a TTY (table-driven).
- Edit round-trip: exactly one capture commit per changed save; byte-equal
  save produces zero commits and the cancelled toast; provider file replaced
  atomically (no intermediate partial content observable).
- New/rename/delete each produce exactly one clean capture; deleted memory
  restorable from history.
- History lists match `git log` of the checkout for the path; blob content
  at rev matches `git cat-file --textconv`; restore creates a new commit
  whose content equals the chosen version.
- Search finds a body-text needle across ≥2 projects and opens the right
  memory; dangling-link fixture flagged; stale threshold honored from
  config.
- Update banner appears against a fake release feed; doctor fix path runs
  quiesce→fix→resume against the harness daemon.
- All view renders covered by the existing CSI-strip render-test pattern;
  e2e testscript covers the non-TTY pointer and agent-env refusal.
