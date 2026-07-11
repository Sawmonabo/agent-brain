# ADR 18: Self-update — gh-native release pipeline, checksum verify, atomic swap

*Amended 2026-07-10 (same day, on review): decision 2 gained explicit-version
pinning and a deliberate-rollback posture, and decision 8 (interactive picker,
TTY-only) was added — the original "rollback is manual by design" consequence
contradicted the command's own reason to exist.*

- **Status:** Accepted
- **Date:** 2026-07-10
- **Deciders:** Sawmon (requested the command during the 9.7 soak; design approved in-session)
- **Related:** ADR 16 (GoReleaser archives + checksums, brew formula, immutable
  releases), spec §7 (command surface), §6 (gh as a hard product dependency)

## Context

The T10 fleet rollout installs v2 on several machines while the repo is still
private: `brew install` cannot serve private release assets (Homebrew fetches
anonymously, ADR 16), so installs are authenticated `gh release download`
one-liners. Keeping that fleet current by hand — re-running the download on
every machine for every release — is exactly the kind of toil v2 exists to
delete. A first-class `agent-brain update` closes the loop: one command that
finds, verifies, installs, and activates the newest release.

Library landscape (WebSearch, 2026-07-10): the Go self-update ecosystem —
`creativeprojects/go-selfupdate` (the maintained lineage of the archived
`rhysd/go-github-selfupdate`) and `minio/selfupdate` — either brings its own
GitHub API client plus token plumbing to reach private repos, or handles only
the swap step and leaves resolution/download/verification to the caller. This
product already ships `gh` as a hard dependency (§6: provisioning, releases,
auth) and every installed machine is authenticated against the private repo by
construction — a third-party updater would add a second, parallel GitHub
access path with its own credential story for strictly less capability.

Homebrew publishes a convention for this exact situation: self-updating
software must not fight the package manager over files under the Homebrew
prefix — brew-managed installs are upgraded by `brew upgrade`, and updaters
are expected to detect and refuse that case.

## Decision

1. **gh-native release source.** `internal/ghx` grows two release methods —
   `ListReleases` (`gh release list --json tagName,isPrerelease,isDraft`) and
   `DownloadReleaseAssets` (`gh release download --pattern`) — and
   `internal/selfupdate` consumes them behind a `ReleaseSource` interface. No
   third-party self-update library, no bespoke HTTP client, no token plumbing:
   the same authenticated gh that installed the machine serves its updates,
   private repo included.

2. **Resolution: implicit semver-max, or an explicit pin.** With no version
   argument the target is the `golang.org/x/mod/semver` maximum over
   non-draft tags — not GitHub's publication order, which lies after re-tags
   and backports. Stable channel by default; `--prerelease` admits release
   candidates (the only channel that exists until v2.0.0 tags at T12 — the
   stable-channel error message names the flag). Implicit resolution never
   downgrades: equal or older is "already up to date", so a stale machine
   can never be walked backward by accident.
   **Amended 2026-07-10:** `update <version>` (both `2.1.0` and `v2.1.0`
   spellings) pins that exact release instead — the modern norm
   (`uv self update [TARGET_VERSION]`, `deno upgrade --version X`), and what
   staged rollouts, machine pinning, and bug reproduction need. The channel
   flag deliberately does not apply to a pin (naming a version is the
   stronger opt-in); drafts stay invisible. An explicitly named OLDER
   release IS installed — deliberate rollback is exactly why an operator
   names a version, and the explicit argument is the acknowledgment — but
   only after a loud DOWNGRADE warning: config parsing is strict
   (`DisallowUnknownFields`, ADR 17), so state written by a newer version
   may refuse to load under the older binary; the warning names
   `agent-brain doctor` and the post-restart readiness poll surfaces a
   daemon that fails to come back.

3. **Fail-closed verification pipeline, in order.** Download the platform
   archive plus the GoReleaser checksums asset into a temp dir; verify sha256
   before opening the archive; extract accepting exactly one entry — the
   top-level regular file `agent-brain` (path traversal is foreclosed by
   construction, not sanitized after the fact; a 256 MiB `io.LimitReader`
   caps decompression); sanity-exec the extracted binary (`--version` in a
   killable process group — 15 s timeout, 2 s kill grace, the migrate
   subprocess pattern) and require its output to name the target version;
   only then swap. Any failure at any stage leaves the installed binary
   byte-identical — the test suite proves this with before/after hashes.

4. **Atomic same-directory swap.** The verified binary is written to a
   `.agent-brain-update-*` temp file in the target's own directory (same
   filesystem, so `os.Rename` is atomic), chmod 0755, then renamed over the
   target. A binary being `execve`'d elsewhere keeps its old inode; new
   invocations get the new file. No partial-write window exists.

5. **Guards run before any network.** Unstamped builds (`Version = "dev"`,
   i.e. `go build` artifacts) are refused — a dev binary self-replacing with
   a release would destroy work-in-progress. Homebrew-managed binaries
   (target under `$HOMEBREW_CELLAR` or any `/Cellar/` path) are refused with
   a redirect to `brew upgrade agent-brain`, per Homebrew's convention.

6. **Service handoff completes the update.** After the swap the command
   bounces the login service through the goal-state sentinels (stop tolerates
   `ErrNotRunning`, start tolerates `ErrAlreadyRunning` — the launchd EIO
   lesson) and polls the daemon UDS (15 s / 500 ms, init's values) to print
   the daemon's now-running version. `--no-restart` skips the bounce and says
   the daemon keeps the old version until restarted; a not-installed service
   (`init --skip-service` posture) is reported and skipped, because the
   binary update itself already succeeded. `--check` reports availability and
   changes nothing.

7. **Release discovery: `--list [--json]` for scripts, `--select` for a
   terminal (added 2026-07-10).** Both surfaces print/offer the SAME rows
   from one candidate builder — non-draft semver releases, both channels
   badged, semver-descending, running version marked — which is exactly the
   set a version argument accepts. This is the enumeration companion every
   version-taking installer grows (mise `ls-remote --json`; rustup's missing
   list is a long-open feature request), and it replaces pointing users at
   raw `gh release list`, whose output also shows drafts (to maintainers)
   and non-semver tags the pin would refuse. `--list` is a guard-free read
   (a dev build may enumerate; it still may not install) and is mutually
   exclusive with every acting flag — no silent no-op combinations.
   `--select` presents the rows as a huh select on an interactive terminal —
   init's form idiom — and is deliberately REFUSED headless (piped stdin,
   CI, `ACCESSIBLE`) with the scriptable path named: huh v2.0.3's
   accessible Select backend auto-accepts the FIRST option on stdin EOF —
   a headless `--select` would silently install the newest release — and
   panics (`index out of range [-1]`, field_select.go:770) on an invalid
   line followed by EOF. Both behaviors were proven by a live probe before
   wiring; `update <version>` + `--list` is the headless- and
   screen-reader-safe pairing.

8. **Integrity model, stated honestly.** GitHub's immutable-releases policy
   (ADR 16) means a published tag's assets can never be silently replaced,
   and gh's authenticated TLS channel plus the repo's access control provide
   source authenticity while the repo is private. The sha256 gate defends the
   download path (truncation, corruption, wrong asset). Detached signing
   (minisign/cosign via GoReleaser's `signs`) is deliberately NOT built now:
   with a private single-owner repo, the signing key would live in the same
   GitHub trust domain as the assets and adds zero authenticity. Recorded
   trigger: if distribution ever broadens beyond the owner's own fleet
   (public repo at T12 is necessary but not sufficient), add offline-key
   signing then — the pipeline's verify stage is where it slots in.

## Consequences

- The private-repo fleet stays current with one command per machine; T10's
  runbook gains `agent-brain update` instead of repeat download one-liners.
- Only new dependency is `golang.org/x/mod` (semver comparison) — everything
  else rides gh and the stdlib.
- Rollback is explicit but first-class (amended 2026-07-10): implicit
  resolution refuses to downgrade, so machines never race backward — but
  `agent-brain update <older-version>` backs out a bad release deliberately,
  with the downgrade warning and doctor as the follow-up. No raw
  `gh release download` toil remains for any update-shaped operation.
- Brew-installed machines get a refusal naming `brew upgrade` — the two
  update paths cannot fight over the same binary.
- gh remains a hard runtime dependency for updates; that is already the
  product posture (§6), not a new commitment.
- `update` is CLI-only surface: the daemon never self-updates, so the single
  writer (ADR 03) is never swapped mid-cycle by its own process tree — the
  service bounce is the only activation path.

## Sources

Search trail (WebSearch, 2026-07-10), queries: `golang self-update binary
github releases library 2026 best practice`, `Homebrew self-updating software
convention Cellar refuse`

- https://github.com/creativeprojects/go-selfupdate (maintained fork lineage;
  own GitHub client + token config for private repos)
- https://github.com/rhysd/go-github-selfupdate (archived predecessor)
- https://github.com/minio/selfupdate (swap-only; resolution/verify left to caller)
- https://docs.brew.sh/FAQ (self-updating software vs brew-managed files)
- https://goreleaser.com/customization/checksum/ (checksums asset format:
  `sha256  name` lines, version-prefixed archive names)
- https://cli.github.com/manual/gh_release_list, …/gh_release_download
  (JSON fields, `--pattern` asset selection)

Amendment trail (WebSearch, 2026-07-10), queries: `uv self update specific
version deno upgrade command syntax target version 2026`, `uv CLI reference
"self update" TARGET_VERSION positional`

- https://docs.astral.sh/uv/reference/cli/ (`uv self update [TARGET_VERSION]`
  — optional positional pin, latest by default)
- https://github.com/astral-sh/uv/issues/6642 (the pin/rollback motivation)
- https://docs.deno.com/runtime/reference/cli/upgrade/ (channel args +
  `--version` pin + `--dry-run` coexist)
- huh v2.0.3 accessible-Select EOF/panic behavior: no public issue found;
  proven by local probe (see decision 7) — the TTY gate is the mitigation.
- https://rclone.org/commands/rclone_selfupdate/ (`--check`/`--version`/
  channel flags — the closest self-updater analog to this surface)
- https://mise.jdx.dev/cli/ls-remote.html (the enumeration companion norm,
  with `--json`)
- https://github.com/rust-lang/rustup/issues/1651 (a missing release list
  is a recognized gap users file)
