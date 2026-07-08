# ADR 15: Testing stack — stdlib + go-cmp, testscript e2e, native fuzzing, real-git integration

- **Status:** Accepted
- **Date:** 2026-07-07
- **Deciders:** Sawmon (pending Section 12 approval; recorded on presentation per ADR-per-decision rule)
- **Related:** ADR 11 (bats retires with legacy), ADR 12 (race/coverage CI gates)

## Context

v2 replaces a bats-tested bash system with Go. The test stack is a buy-vs-build
decision surface: assertion libraries, CLI test harnesses, fuzzing tools. Design
constraints: the riskiest code is the crypto filter chain and the merge driver
(both exercised *by git*, not by our code directly), plus a concurrent daemon.

## Decision

1. **Unit: stdlib `testing` + google/go-cmp.** No assertion frameworks — Google's
   style guide permits only the stdlib testing package and explicitly disallows
   assertion libraries; it recommends `cmp` for equality and warns against
   `reflect.DeepEqual` (unexported-field sensitivity). Table-driven tests,
   `t.Parallel()`, `t.TempDir()` throughout.
2. **CLI/e2e: rogpeppe/go-internal `testscript`.** txtar-format scripts; the
   harness extracted from the Go core team's own cmd/go tests; runs under
   `go test` with coverage integration and golden-file auto-update. Scripts cover
   the init/track/sync/migrate/doctor command flows.
3. **Integration: real system git in `t.TempDir()`** — `git init --bare` as the
   fake remote, zero network. The critical scenario: two simulated "machines"
   clone the bare repo, write divergent memory, sync — asserting the full
   filter/merge-driver chain (clean/smudge, retain-both blocks, derived-index
   reconcile, newest-wins class). This is the only way to test the merge driver,
   since git invokes it, not us.
4. **Fuzzing: native `go test -fuzz`** (stdlib since 1.18) on: crypto roundtrip
   (decrypt∘encrypt = identity; determinism property: equal plaintext+key ⇒ equal
   ciphertext), merge-driver three-way inputs, and provider file-classification
   parsing. No third-party fuzzer needed.
5. **Daemon logic:** engine's single-writer loop tested with injected fake clock
   and synthetic fs events; UDS API tested client↔server in-process over a real
   socket in `t.TempDir()`.
6. **CI:** `-race` everywhere, macOS + Ubuntu matrix (ADR 12). Coverage tracked;
   no hard threshold gate in v1. **WSL2** cannot run in hosted CI — a manual
   runbook (checklist committed to the repo) is executed before any release tag
   touching daemon/service/watch code.
7. The bats suite is deleted with the legacy tree (ADR 11 greenfield).

## Consequences

- Zero test-framework dependencies beyond two Google/rogpeppe modules (go-cmp,
  go-internal); everything else is stdlib.
- The merge-driver integration harness doubles as living documentation of the
  concurrency guarantees (the design's core promise).

## Buy vs build

Buy: go-cmp, testscript, native fuzzing (stdlib). Build: only the small fake-remote
git harness helpers — no existing package wraps "bare repo + two clones + our
filter config" and it's ~100 lines.

## Sources

Search trail (WebSearch, 2026-07-07):

Query: `testscript go-internal rogpeppe latest 2026 testing CLI tools txtar`
- https://pkg.go.dev/github.com/rogpeppe/go-internal/testscript
- https://rednafi.com/go/testscript-cli/
- https://github.com/fortio/testscript (fork, noted not chosen)
- https://github.com/rogpeppe/go-internal
- https://pkg.go.dev/fortio.org/testscript
- https://github.com/rogpeppe/go-internal/blob/master/cmd/txtar-c/script_test.go
- https://encore.dev/blog/testscript-hidden-testing-gem
- https://bitfieldconsulting.com/posts/testscript-tool
- https://pkg.go.dev/github.com/rogpeppe/go-internal/cmd/testscript
- https://pkg.go.dev/github.com/rogpeppe/go-internal

Query: `Google Go style guide assertion libraries testify go-cmp test best practice`
- https://google.github.io/styleguide/go/best-practices.html
- https://github.com/google/styleguide/blob/gh-pages/go/decisions.md (stdlib-only
  testing policy; prefer cmp; DeepEqual warning)
- https://lobste.rs/s/vzdoor/cult_go_test
- https://henvic.dev/posts/testing-go/
- https://www.alexedwards.net/blog/the-9-go-test-assertions-i-use
- https://pkg.go.dev/gotest.tools/v3/assert (considered, not chosen)
