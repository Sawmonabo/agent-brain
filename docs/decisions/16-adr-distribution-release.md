# ADR 16: Distribution & release — GoReleaser v2, Homebrew formula tap, go install

*Title amended 2026-07-09 (Task 7): the tap mechanism reversed from
`homebrew_casks` to a `brews` formula — decision 13 below.*

- **Status:** Accepted
- **Date:** 2026-07-07
- **Deciders:** Sawmon (pending Section 13 approval; recorded on presentation per ADR-per-decision rule)
- **Related:** ADR 12 (CI, SHA-pinned actions), ADR 13 (public-repo option post-scrub), Section 8 (cmd/ layout)

## Context

v2 must install on new machines in minutes (macOS, Linux, WSL2) and stay current.
Verified state 2026-07-07: **GoReleaser v2.16.0** (2026-05-24) is current; its
release notes carry two decision-relevant changes: releases are now published under
an **immutable-releases policy** (tag bytes can never be replaced), and the legacy
`brews` formula config is **fully deprecated in favor of `homebrew_casks`** — casks
are now the sanctioned way to ship pre-compiled binaries via Homebrew.

**Correction (2026-07-09, Task 7 / distribution research brief):** the "fully
deprecated" premise above was an overstatement, and the cask direction it justified
does not survive contact with macOS 26. `brews` is deprecated-in-favor-of but still
functional through the current v2.17 (cross-checked against the v2.16 release notes:
removal is slated only for a v3 that does not exist and is not scheduled), while
`homebrew_casks`' own unsigned-binary path — a post-install `xattr -dr
com.apple.quarantine` hook — is defeated on macOS 26 Tahoe by a SIP-protected
`com.apple.provenance` companion attribute plus quarantine-database persistence;
AppleSystemPolicy blocks execution before dyld even loads the binary. See decisions
12–13 below and the amended Decision section.

## Decision

1. **GoReleaser v2.16.0** runs on tag push in GitHub Actions (SHA-pinned, ADR 12):
   darwin/arm64+amd64 and linux/arm64+amd64 archives, checksums file, changelog
   generated from Conventional Commits. **Amended 2026-07-09 (Task 7):** GoReleaser
   v2.17 confirmed current the same week, no v3 exists or is scheduled; the release
   workflow pins `goreleaser-action@v7` with `version: '~> v2'` rather than
   transcribing a point release, so this stays current by construction.
2. ~~**Homebrew via `homebrew_casks`** (not the deprecated `brews`), published to a
   personal tap repo `Sawmonabo/homebrew-tap`:
   `brew install sawmonabo/tap/agent-brain`.~~
   **REVERSED 2026-07-09 (Task 7, decision 13 — the cask premise failed):** ships
   as a `brews` **formula**, not a cask. Formulae never set
   `com.apple.quarantine`, so Gatekeeper never engages for a `brew install`-ed
   binary — no xattr hook, nothing to defeat. This ADR's original cask +
   post-install `xattr -dr com.apple.quarantine` design is DEFEATED on macOS 26
   Tahoe (see the Context correction above); separately, Homebrew 5.0.0 deprecates
   `--no-quarantine` and is purging unsigned casks from the official tap by
   Sept 2026 (personal taps are exempt today, but the direction is explicit).
   `brews` remains fully functional through GoReleaser v2.17 despite being
   marked deprecated since v2.16 (removal slated only for a nonexistent v3);
   the recorded fallback if `brews` is ever actually removed is to hand-write
   the ~20-line formula directly in the tap repo. Config: `skip_upload: auto` —
   prerelease tags (`v2.0.0-rc.*`, used through the Task 9–11 cutover) publish
   GitHub release assets but do not push a tap formula, so no public
   `brew install` command ever points at this still-private repo's assets; the
   final `v2.0.0` (tagged post-scrub, Task 12, once the repo is public) is what
   activates `brew install sawmonabo/tap/agent-brain`. The public-launch upgrade
   path, if broad (non-personal) distribution ever matters, is cask +
   GoReleaser-native notarization (Quill-backed; needs a $99/yr Apple Developer
   ID + App Store Connect key) — never the middle road of cask + ad-hoc + xattr
   postflight, which still hits first-run friction on macOS 26.
3. **`go install github.com/Sawmonabo/agent-brain/cmd/agent-brain@latest`** works
   by construction (single root module + cmd/ layout, Section 8) — the no-brew
   fallback for Linux/WSL2.
4. ~~**Signing:** checksums + GitHub immutable releases only in v1. cosign /
   provenance attestations are YAGNI for a single-consumer personal tool —
   documented here and revisited if the repo goes public after the ADR 13
   scrub.~~
   **CORRECTED 2026-07-09 (Task 7, decision 12 — this was wrong, not merely
   conservative):** ad-hoc code signing of every darwin binary is MANDATORY in
   CI, not optional and not YAGNI. Apple Silicon's AMFI SIGKILLs (`killed: 9`)
   any Mach-O binary lacking a valid signature before it ever runs, and a Go
   binary cross-compiled for darwin on a linux CI runner carries only a
   linker-generated signature that macOS 26 can treat as corrupt
   (golang/go#42684, golang/go#56599; reproduced on a standalone Go CLI in
   openai/codex#17447). The fix is free and needs no certificate: a GoReleaser
   build post-hook runs **anchore/quill** (`quill sign --ad-hoc`, no signing
   material configured) over each darwin artifact via `scripts/adhoc-sign.sh`,
   which no-ops on non-darwin targets so one hook definition covers every build
   target and CI/local artifacts sign identically. Ad-hoc suffices to *execute*
   for personal use; a real Developer ID + notarization is what *distributing to
   other people* would require, and remains the recorded, deferred public-launch
   path — the YAGNI reasoning below now applies correctly to notarization, not
   to signing altogether. cosign / provenance attestations remain YAGNI as
   originally decided.
5. **New-machine onboarding runbook** (target: under 5 minutes):
   brew/go install → `agent-brain init` (wizard: gh auth check → clone
   `agent-brain-memories` → `key import` from password manager → service install →
   enrollment picker) → `agent-brain migrate` if the machine has bash-era state.

## Consequences

- Self-updating is delegated to the package manager (`brew upgrade` /
  `go install @latest`) — no self-update code in the binary (YAGNI; single user).
- The tap repo is one more repo to provision; `init`'s wizard does not manage it —
  it is a release-time artifact only, created once.
- WSL2 uses the Linux binary via `go install` or linuxbrew; no Windows-native
  build is shipped (all targets are POSIX — consistent with ADR 14's renameio
  constraint).
- **Added 2026-07-09 (Task 7):** the tap repo is created public and empty
  (`gh repo create --public`) — a tap must be public to `brew tap` anonymously —
  but stays empty of formulae until the first non-prerelease tag ships
  post-scrub; `skip_upload: auto` is what keeps it empty across the cutover's
  `-rc` tags while release assets still sit on the private code repo. The
  interim install path while private is `gh release download <tag>`
  (authenticated) or `go install` (owner has git access) — documented in
  Task 8's onboarding doc.

## Buy vs build

Buy entirely: GoReleaser, Homebrew tap mechanism, Dependabot. Build: nothing —
distribution is pure configuration. **Added 2026-07-09 (Task 7):** also buy
anchore/quill for ad-hoc darwin signing — free, certificate-less, runs on linux
CI runners (rcodesign is the documented equivalent alternative). Build: one
~15-line wrapper script (`scripts/adhoc-sign.sh`) so the GoReleaser hook no-ops
on non-darwin targets; still nothing resembling a custom signing pipeline.

## Sources

Search trail (WebSearch, 2026-07-07), query: `GoReleaser v2 latest version 2026
homebrew tap Go binary release`

- https://github.com/goreleaser/goreleaser/releases (v2.16.0, 2026-05-24)
- https://goreleaser.com/blog/goreleaser-v2.16/ (immutable releases; brews →
  homebrew_casks deprecation; dockers_v2 GA)
- https://goreleaser.com/blog/goreleaser-v2/
- https://goreleaser.com/
- https://github.com/goreleaser/goreleaser
- https://pkg.go.dev/github.com/goreleaser/goreleaser/v2
- https://github.com/goreleaser/goreleaser-pro/releases
- https://repology.org/project/goreleaser/versions
- https://goreleaser.com/blog/goreleaser-v2.12/
- https://goreleaser.com/blog/goreleaser-v2.14/

Post-decision verification (2026-07-09, Task 7 / distribution research brief —
the findings behind decisions 12–13 above, which reverse the cask lean and add
mandatory signing):

- GoReleaser v2.17 current, no v3 exists or is scheduled:
  https://github.com/goreleaser/goreleaser/releases
- `homebrew_casks` publisher doc — the unsigned-binary xattr hook this ADR
  originally relied on, and its Apple-version caveat:
  https://goreleaser.com/customization/publish/homebrew_casks/
- GoReleaser native notarize (Quill-backed), the recorded public-launch path:
  https://goreleaser.com/customization/sign/notarize/
- Version-embedding cookbook (`-X` ldflags stays required —
  `debug.ReadBuildInfo` never carries the semver tag on a detached release
  checkout): https://goreleaser.com/cookbooks/using-main.version/
- Cross-repo tap-push failure mode (why `GITHUB_TOKEN` cannot push the tap; the
  reason `HOMEBREW_TAP_GITHUB_TOKEN` is a separate fine-grained PAT):
  https://goreleaser.com/resources/errors/resource-not-accessible-by-integration/
- anchore/quill: https://github.com/anchore/quill · rcodesign (documented
  alternative): https://gregoryszorc.com/docs/apple-codesign/stable/
- Unsigned-darwin-binary SIGKILL reports: https://github.com/golang/go/issues/42684,
  https://github.com/golang/go/issues/56599, https://github.com/openai/codex/issues/17447
- Homebrew 5.0.0 unsigned-cask crackdown: https://workbrew.com/blog/homebrew-5-0-0
  · https://news.ycombinator.com/item?id=45907259
- Code-signing baseline (ad-hoc suffices to execute for personal use; Developer
  ID + notarization needed to distribute to others):
  https://eclecticlight.co/2026/01/17/whats-happening-with-code-signing-and-future-macos/
- Private-tap patterns for a still-private source repo — evaluated and rejected
  in favor of `skip_upload: auto` (Phase-4 plan decision 10):
  https://andre.arko.net/2023/11/24/homebrew-cask-formula-for-private-github-repo-releases/,
  https://dev.to/jhot/homebrew-and-private-github-repositories-1dfh,
  https://blog.ceejbot.com/posts/private-brew-tap/
- Action majors resolved for the release workflow (exact commit SHAs pinned via
  `gh api repos/<owner>/<repo>/git/ref/tags/<tag>` at pin time, recorded in the
  Task 7 commit body): https://github.com/actions/checkout,
  https://github.com/actions/setup-go/releases,
  https://github.com/goreleaser/goreleaser-action
- Full decision record and reasoning:
  `docs/plans/v2-phase-4-cutover-distribution.md`, "Decision record & sources
  (Phase-4 planning, 2026-07-09)", decisions 12–13.
