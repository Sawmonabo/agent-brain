# agent-brain v2 — Phase 2: Repo State, Sync Engine, Watch & Daemon — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn Phase 1's proven crypto/git plumbing into a working sync system: the memories-repo state model (layout, registries, manifests), the single-writer sync engine (mirror-in → commit → integrate → reconcile → mirror-out → push), the watch manager, and a resident daemon controllable over a 0600 unix socket and installable as a user service — proven end-to-end by two simulated machines converging **through the engine**, provider-directory to provider-directory.

**Architecture:** Everything stays under `internal/`. The engine depends on `gitx`/`crypto`/`provider`/`repo` interfaces — never on `cli` or `daemon` (spec §8). Provider *adapters* are Phase 3; Phase 2 defines the provider contract plus a test fake, so the engine, attributes generation, and watch manager are built and proven against the contract. `daemon/api` types are the only surface shared between daemon and CLI.

**Tech Stack:** Go 1.26 (toolchain go1.26.5) · Phase-1 packages (`crypto`, `gitx`, `keys`, `config`, `cli`) · fsnotify v1.10.1 · gofrs/flock · cenkalti/backoff/v5 · kardianos/service v1.3.0 · google/renameio/v2 · pelletier/go-toml/v2 · golang.org/x/sys · system git · stdlib `net/http` over UDS.

**Phase roadmap** (from the Phase-1 plan; this is plan 2 of 4):

1. **Phase 1 (done, develop=8f3df16):** greenfield reset, module + CI/tooling, config paths, keys, crypto codec, filter/merge plumbing, real-git integration proof.
2. **Phase 2 (this plan):** repo layout/registry/manifests, mirror in/out, sync engine loop, watch manager, daemon + UDS API + service install.
3. **Phase 3:** provider adapters (claude, codex), index reconciliation, full CLI/TUI (init wizard, dashboard, track/status/conflicts/doctor), testscript e2e.
4. **Phase 4:** `migrate`, retirement checks, GoReleaser + tap, onboarding/WSL2 runbooks.

Spec: `docs/00-design-spec.md` (§ references below). ADRs: `docs/decisions/`.

## Global Constraints

Every task implicitly includes these. Version pins re-verified 2026-07-08.

- Branch: ALL work lands on `develop`. Never commit to `main`.
- Module: `module github.com/Sawmonabo/agent-brain`; `go 1.26`; `toolchain go1.26.5`.
- New dependency pins: fsnotify **v1.10.1** (latest, 2026-05-04) · kardianos/service **v1.3.0** (2026-07-06) · cenkalti/backoff/v5 **v5.0.3** · gofrs/flock, golang.org/x/sys, google/renameio/v2, pelletier/go-toml/v2 at latest minor. Dependabot keeps them current afterward. **No Charm TUI deps in this phase** (huh/bubbletea are Phase 3).
- All packages under `internal/` — no public API surface. No `pkg/` directory.
- Formatting: gofmt + gofumpt (CI-enforced). Lint set unchanged (golangci-lint v2.12.2; every `//nolint` needs linter name + reason).
- Tests: stdlib `testing` + `go-cmp` only — NO assertion frameworks (ADR 15). Table-driven, `t.Parallel()`, `t.TempDir()`. Real system git for integration behavior; **no live service installs** in any test, and no writes outside test-owned temp dirs (`t.TempDir()`, or short-path `os.MkdirTemp` dirs with cleanup where the unix-socket `sun_path` limit forces it — Tasks 10–12 explain).
- Watch/daemon timing tests use REAL time with short intervals and deadline-bounded eventually-assertions (Task 9 records why a fake clock is wrong when real kernel event latency is in the loop). No bare fixed-sleep-then-assert chains; brief quiet-window waits and `-count=5` flake-shaking are the sanctioned patterns.
- Commits: Conventional Commits. One commit per task minimum.
- Safety invariants (spec §5, §11): the keyset NEVER enters any repo; plaintext memory content NEVER reaches a git object (e2e asserts ciphertext on the wire); `filter.agentbrain.required = true` fail-closed; the merge driver ALWAYS exits resolved on mergeable input; **mirror-out is withheld for a project whose integrate failed** (degraded), and **provider dirs never see partial writes** (renameio atomic replace).
- Single-writer invariant (ADR 03): the engine is the ONLY component that touches the memories checkout; the daemon's one engine goroutine is the ONLY caller of engine cycles. The engine defends this with a busy-guard that fails loudly (never blocks silently).
- Engine package boundary (spec §8): `engine` imports `gitx`/`provider`/`repo` (+ stdlib/renameio) only — never `cli`, `daemon`, `watch`, or `crypto` (encryption rides the git filters; the exit criteria grep enforces this). `daemon/api` is the only daemon↔CLI shared surface and imports nothing internal.
- The daemon binds its socket in the runtime dir per ADR 09 (never bare `/tmp`; dir `0700` recreated every start; socket `0600`; `sun_path` limit ~104 bytes respected). Peer-UID verification (SO_PEERCRED / LOCAL_PEERCRED) ships NOW, not deferred.
- The age key, `~/.config/agent-brain/` contents, and `main`'s bash system stay untouched. Tests inject config/data/runtime dirs via the env overrides `internal/config` already honors — never the real home paths.

---

### Orientation: what Phase 1 already provides

Phase 2 tasks consume these existing surfaces (do not re-implement them; import them):

- `internal/crypto` — the codec and the git plumbing endpoints (clean/smudge/textconv/merge). Git invokes them through the filters `gitx.InstallFilters` wires; the engine NEVER calls crypto directly in this phase.
- `internal/gitx` — `InstallFilters(ctx, repoDir, binPath)` (filter + merge-driver `.git/config` wiring, fail-closed `required=true`) and the exec runner used for all git operations. Signal-killed git children surface as errors, never as data (Phase-1 hardening).
- `internal/keys` — keyset generate/load; `AGENT_BRAIN_CONFIG_DIR` override honored process-wide (the e2e suite relies on this).
- `internal/config` — platform paths (XDG + macOS asymmetry) with env overrides for tests.
- `test/e2e` — the real-git harness: `newBareRepo`, `newMachine` (clone + identity + filters + re-smudged worktree), `gitRun`/`gitRunEnv` (hermetic: `GIT_CONFIG_GLOBAL/SYSTEM=/dev/null`), `writeFile`/`readFile`, `remoteBlob` (attacker's view of the bare remote), the `gitAttributes` const (incl. the `*.lww.md` newest-wins class), `binPath`, `TestMain` (builds the binary, shared keyset, hermetic env).

Exact signatures for anything you touch: read the package's source first; the task briefs below quote the load-bearing ones.

### Task 1: Provider contract — classes, patterns, registry, test fake (spec §6, §8; ADRs 02/03)

The adapter *interface* ships now so the engine, attributes generation, and tests build against the contract; the real `claude`/`codex` adapters are Phase 3. Interface segregation is deliberate: this phase defines ONLY the methods Phase-2 consumers call (`Name`, `Scope`, `Patterns`→`Classify`, `ReconcileIndex`). `DiscoverProjects`/`ResolveIdentity` from spec §6 join the interface in Phase 3 when enrollment (their first consumer) exists — adding methods to an `internal/` interface with one fake and two adapters is a compile-checked, mechanical change, not a compat risk.

**Files:**
- Create: `internal/provider/provider.go`, `internal/provider/match.go`, `internal/provider/registry.go`
- Create: `internal/provider/providertest/fake.go`
- Test: `internal/provider/provider_test.go`, `internal/provider/match_test.go`, `internal/provider/registry_test.go`, `internal/provider/fuzz_test.go`

**Interfaces:**
- Consumes: nothing new (stdlib + `context`).
- Produces (later tasks rely on these exact names):
  - `type Class int` — `ClassFact`, `ClassDerivedIndex`, `ClassRegenerated`, `ClassIgnore` (+ `String()`)
  - `type Scope int` — `ScopePerProject`, `ScopeGlobal`
  - `type Pattern struct { Glob string; Class Class }`
  - `type Provider interface { Name() string; Scope() Scope; Patterns() []Pattern; ReconcileIndex(ctx context.Context, dir string) error }`
  - `func Classify(p Provider, rel string) Class`
  - `func Match(glob, rel string) bool` · `func ValidateGlob(glob string) error`
  - `type Registry` · `func NewRegistry(providers ...Provider) (*Registry, error)` · `(*Registry).Get(name string) (Provider, bool)` · `(*Registry).All() []Provider`
  - `providertest.Fake` implementing `Provider` with recorded `ReconcileIndex` calls

- [ ] **Step 1: Write the failing tests** — `internal/provider/provider_test.go`:

```go
package provider_test

import (
	"context"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/providertest"
)

// claudeLikeTable mirrors the spec §6 Claude classification so the contract
// is exercised against the real Phase-3 shape without shipping the adapter.
func claudeLikeTable() []provider.Pattern {
	return []provider.Pattern{
		{Glob: "MEMORY.md", Class: provider.ClassDerivedIndex},
		{Glob: "*.md", Class: provider.ClassFact},
	}
}

func TestClassifyFirstMatchWins(t *testing.T) {
	t.Parallel()
	fake := providertest.New("claude", provider.ScopePerProject, claudeLikeTable())

	tests := []struct {
		name string
		rel  string
		want provider.Class
	}{
		{"derived index beats star-md", "MEMORY.md", provider.ClassDerivedIndex},
		{"topic file is fact", "debugging.md", provider.ClassFact},
		{"nested topic file is fact via default", "notes/deep.md", provider.ClassFact},
		{"unknown extension defaults to fact", "scratch.txt", provider.ClassFact},
		{"unmatched never drops data", "bin/blob.dat", provider.ClassFact},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := provider.Classify(fake, tt.rel); got != tt.want {
				t.Fatalf("Classify(%q) = %v, want %v", tt.rel, got, tt.want)
			}
		})
	}
}

func TestClassifyIgnoreAndRegenerated(t *testing.T) {
	t.Parallel()
	fake := providertest.New("codexlike", provider.ScopeGlobal, []provider.Pattern{
		{Glob: ".lock/**", Class: provider.ClassIgnore},
		{Glob: "memory_summary.md", Class: provider.ClassRegenerated},
		{Glob: "rollout_summaries/*", Class: provider.ClassRegenerated},
		{Glob: "skills/**/SKILL.md", Class: provider.ClassFact},
	})

	tests := []struct {
		rel  string
		want provider.Class
	}{
		{".lock/pid", provider.ClassIgnore},
		{"memory_summary.md", provider.ClassRegenerated},
		{"rollout_summaries/2026-07-08.md", provider.ClassRegenerated},
		{"skills/git/SKILL.md", provider.ClassFact},
		{"skills/SKILL.md", provider.ClassFact}, // ** matches zero segments
		{"raw_memories.md", provider.ClassFact}, // default
	}
	for _, tt := range tests {
		if got := provider.Classify(fake, tt.rel); got != tt.want {
			t.Fatalf("Classify(%q) = %v, want %v", tt.rel, got, tt.want)
		}
	}
}

func TestFakeRecordsReconcileCalls(t *testing.T) {
	t.Parallel()
	fake := providertest.New("claude", provider.ScopePerProject, nil)
	if err := fake.ReconcileIndex(context.Background(), "/tmp/x"); err != nil {
		t.Fatal(err)
	}
	if got := fake.ReconcileCalls(); len(got) != 1 || got[0] != "/tmp/x" {
		t.Fatalf("ReconcileCalls() = %v, want [/tmp/x]", got)
	}
}
```

`internal/provider/match_test.go`:

```go
package provider_test

import (
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/provider"
)

func TestMatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		glob, rel string
		want      bool
	}{
		// Single-segment wildcards stay within a segment.
		{"*.md", "MEMORY.md", true},
		{"*.md", "notes/deep.md", false},
		{"rollout_summaries/*", "rollout_summaries/a.md", true},
		{"rollout_summaries/*", "rollout_summaries/sub/a.md", false},
		// '**' as a full segment spans zero or more segments (git semantics).
		{"skills/**/SKILL.md", "skills/git/SKILL.md", true},
		{"skills/**/SKILL.md", "skills/a/b/SKILL.md", true},
		{"skills/**/SKILL.md", "skills/SKILL.md", true},
		{"**", "anything/at/all.md", true},
		{".lock/**", ".lock/pid", true},
		{".lock/**", ".lock", false}, // '**' needs the prefix segment to exist as a dir path
		// Literals.
		{"MEMORY.md", "MEMORY.md", true},
		{"MEMORY.md", "memory.md", false},
	}
	for _, tt := range tests {
		if got := provider.Match(tt.glob, tt.rel); got != tt.want {
			t.Fatalf("Match(%q, %q) = %v, want %v", tt.glob, tt.rel, got, tt.want)
		}
	}
}

func TestValidateGlob(t *testing.T) {
	t.Parallel()
	valid := []string{"skills/**/SKILL.md", "*.md", "rollout_summaries/*", "MEMORY.md"}
	for _, glob := range valid {
		if err := provider.ValidateGlob(glob); err != nil {
			t.Fatalf("valid glob %q rejected: %v", glob, err)
		}
	}
	// Rejections fail fast at construction, not silently at match time.
	// Whitespace / '#' / leading '!' / empty would corrupt or invert the
	// generated .gitattributes lines this glob eventually becomes
	// (Task 2's repo.GenerateAttributes) — the same table drives both.
	invalid := []string{"bad[range.md", "has space.md", "has\ttab.md", "#comment.md", "!negated.md", ""}
	for _, glob := range invalid {
		if err := provider.ValidateGlob(glob); err == nil {
			t.Fatalf("ValidateGlob(%q) = nil, want error", glob)
		}
	}
}
```

`internal/provider/registry_test.go`:

```go
package provider_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/providertest"
)

func TestRegistryDeterministicOrderAndLookup(t *testing.T) {
	t.Parallel()
	codex := providertest.New("codex", provider.ScopeGlobal, nil)
	claude := providertest.New("claude", provider.ScopePerProject, nil)

	reg, err := provider.NewRegistry(codex, claude)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range reg.All() {
		names = append(names, p.Name())
	}
	if diff := cmp.Diff([]string{"claude", "codex"}, names); diff != "" {
		t.Fatalf("All() order (-want +got):\n%s", diff)
	}
	if _, ok := reg.Get("claude"); !ok {
		t.Fatal("Get(claude) = false, want true")
	}
	if _, ok := reg.Get("gemini"); ok {
		t.Fatal("Get(gemini) = true, want false")
	}
}

func TestRegistryRejectsDuplicatesBadNamesAndBadGlobs(t *testing.T) {
	t.Parallel()
	a := providertest.New("claude", provider.ScopePerProject, nil)
	b := providertest.New("claude", provider.ScopePerProject, nil)
	if _, err := provider.NewRegistry(a, b); err == nil {
		t.Fatal("duplicate provider name accepted; want error")
	}
	bad := providertest.New("bad", provider.ScopePerProject, []provider.Pattern{
		{Glob: "bad[range.md", Class: provider.ClassFact},
	})
	if _, err := provider.NewRegistry(bad); err == nil {
		t.Fatal("malformed glob accepted at construction; want error")
	}
	// Provider names become repo path segments (<project>/<name>/) and
	// .gitattributes pattern segments — the interface contract says
	// lowercase and path-safe, and the registry is where it's enforced.
	for _, name := range []string{"", "Claude", "co dex", "a/b", "..", "_global", ".hidden"} {
		p := providertest.New(name, provider.ScopePerProject, nil)
		if _, err := provider.NewRegistry(p); err == nil {
			t.Fatalf("provider name %q accepted; want error", name)
		}
	}
}
```

`internal/provider/fuzz_test.go`:

```go
package provider_test

import (
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/provider"
)

// FuzzMatch pins two properties: Match never panics on arbitrary inputs,
// and it is deterministic (same inputs ⇒ same answer).
func FuzzMatch(f *testing.F) {
	f.Add("skills/**/SKILL.md", "skills/a/SKILL.md")
	f.Add("*.md", "MEMORY.md")
	f.Add("**", "")
	f.Add("a[", "a")
	f.Fuzz(func(t *testing.T, glob, rel string) {
		first := provider.Match(glob, rel)
		if second := provider.Match(glob, rel); first != second {
			t.Fatalf("Match(%q, %q) nondeterministic: %v then %v", glob, rel, first, second)
		}
	})
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/provider/... 2>&1 | head -20`
Expected: compile errors (`package provider` does not exist yet).

- [ ] **Step 3: Implement** — `internal/provider/provider.go`:

```go
// Package provider defines the contract every memory-provider adapter
// implements, plus the class/pattern model driving merge policy and
// .gitattributes generation (spec §6; ADRs 02/03). Phase 2 ships the
// contract and a test fake; the claude/codex adapters are Phase 3.
// DiscoverProjects/ResolveIdentity (spec §6) join the interface in
// Phase 3 alongside enrollment, their first consumer.
package provider

import "context"

// Class is a file's merge-policy class (spec §3 "File classes").
type Class int

const (
	// ClassFact merges 3-way with retain-both on overlap — the default
	// for anything unrecognized: data is never dropped, never newest-wins.
	ClassFact Class = iota
	// ClassDerivedIndex rides the fact driver at merge time; the engine's
	// reconcile step regenerates it afterward (spec §4 step 4).
	ClassDerivedIndex
	// ClassRegenerated is provider-rebuildable output — newest-wins
	// (merge=agentbrain-lww in generated attributes).
	ClassRegenerated
	// ClassIgnore is never mirrored into the repo (locks, scratch).
	ClassIgnore
)

func (c Class) String() string {
	switch c {
	case ClassFact:
		return "fact"
	case ClassDerivedIndex:
		return "derived-index"
	case ClassRegenerated:
		return "regenerated"
	case ClassIgnore:
		return "ignore"
	default:
		return "unknown"
	}
}

// Scope is where a provider keeps memory (ADR 02).
type Scope int

const (
	// ScopePerProject providers key memory by project (Claude Code).
	ScopePerProject Scope = iota
	// ScopeGlobal providers keep one user-global pool (Codex → _global/).
	ScopeGlobal
)

func (s Scope) String() string {
	switch s {
	case ScopePerProject:
		return "per-project"
	case ScopeGlobal:
		return "global"
	default:
		return "unknown"
	}
}

// Pattern binds a slash-separated glob (relative to a memory root) to a
// Class. Tables are ordered; the first matching pattern wins.
type Pattern struct {
	Glob  string
	Class Class
}

// Provider is the Phase-2 adapter contract. Implementations must be
// safe for concurrent use by multiple goroutines (the daemon reads them
// from its API server while the engine syncs).
type Provider interface {
	// Name is the stable identifier used in repo paths (<project>/<name>/)
	// and registries. Lowercase, path-safe, never empty.
	Name() string
	// Scope reports whether memory is per-project or user-global.
	Scope() Scope
	// Patterns returns the ordered classification table. One table, two
	// consumers: Classify (mirror decisions) and repo attribute
	// generation (merge-driver wiring).
	Patterns() []Pattern
	// ReconcileIndex deterministically rebuilds the provider's derived
	// index inside dir — a <project>/<provider>/ dir in the checkout —
	// after integrate (spec §4 step 4). No-op when nothing applies.
	ReconcileIndex(ctx context.Context, dir string) error
}

// Classify resolves rel (slash-separated, relative to the memory root)
// against p's table. Unmatched paths are ClassFact — the safest default
// for data (spec §6).
func Classify(p Provider, rel string) Class {
	for _, pat := range p.Patterns() {
		if Match(pat.Glob, rel) {
			return pat.Class
		}
	}
	return ClassFact
}
```

`internal/provider/match.go`:

```go
package provider

import (
	"fmt"
	"path"
	"strings"
)

// Match reports whether rel (slash-separated, no leading slash) matches
// glob. Semantics — the documented contract shared with attribute
// generation (repo package):
//
//   - Segments are split on '/'.
//   - A segment of exactly "**" matches zero or more whole segments.
//   - Any other segment matches one segment via path.Match ('*', '?',
//     '[...]' within the segment; '*' never crosses '/').
//   - Malformed per-segment patterns match nothing; ValidateGlob rejects
//     them up front so registries fail fast instead.
func Match(glob, rel string) bool {
	return matchSegments(strings.Split(glob, "/"), strings.Split(rel, "/"))
}

func matchSegments(globParts, relParts []string) bool {
	if len(globParts) == 0 {
		return len(relParts) == 0
	}
	head, rest := globParts[0], globParts[1:]
	if head == "**" {
		// Zero segments…
		if matchSegments(rest, relParts) {
			return true
		}
		// …or consume one and stay on '**'.
		if len(relParts) > 0 {
			return matchSegments(globParts, relParts[1:])
		}
		return false
	}
	if len(relParts) == 0 {
		return false
	}
	ok, err := path.Match(head, relParts[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(rest, relParts[1:])
}

// ValidateGlob rejects globs whose segments path.Match cannot parse, and
// characters that would corrupt the .gitattributes lines the same glob
// becomes in repo.GenerateAttributes: whitespace splits an attributes
// line into pattern+attrs, '#' comments it out, leading '!' inverts it.
// Registries call this at construction so a bad pattern is a loud
// startup error — never a silently-never-matching or file-corrupting rule.
func ValidateGlob(glob string) error {
	if glob == "" {
		return fmt.Errorf("empty glob")
	}
	if strings.HasPrefix(glob, "!") {
		return fmt.Errorf("glob %q: leading '!' would negate a .gitattributes pattern", glob)
	}
	if strings.ContainsAny(glob, " \t\n\r\"#") {
		return fmt.Errorf("glob %q: whitespace, quotes, and '#' are unrepresentable in .gitattributes lines", glob)
	}
	for _, seg := range strings.Split(glob, "/") {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, "probe"); err != nil {
			return fmt.Errorf("glob %q: segment %q: %w", glob, seg, err)
		}
	}
	return nil
}
```

`internal/provider/registry.go`:

```go
package provider

import (
	"fmt"
	"regexp"
	"sort"
)

// Registry holds the configured providers with deterministic iteration
// order (sorted by name). Immutable after construction.
type Registry struct {
	byName  map[string]Provider
	ordered []Provider
}

// nameRE pins the Provider.Name contract: lowercase, path-safe, starts
// alphanumeric — names become repo path segments and .gitattributes
// pattern segments, so '_'-prefixed (reserved: _global), '.'-prefixed,
// separator-bearing, and uppercase names are all construction errors.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// NewRegistry validates and indexes providers: names must be unique and
// satisfy the name contract; every pattern glob must validate. Fail-fast
// contract — a bad table is a startup error, never a silent
// misclassification or a corrupted attributes file.
func NewRegistry(providers ...Provider) (*Registry, error) {
	registry := &Registry{byName: make(map[string]Provider, len(providers))}
	for _, p := range providers {
		name := p.Name()
		if !nameRE.MatchString(name) {
			return nil, fmt.Errorf("provider name %q violates the name contract (%s)", name, nameRE)
		}
		if _, dup := registry.byName[name]; dup {
			return nil, fmt.Errorf("duplicate provider %q", name)
		}
		for _, pat := range p.Patterns() {
			if err := ValidateGlob(pat.Glob); err != nil {
				return nil, fmt.Errorf("provider %q: %w", name, err)
			}
		}
		registry.byName[name] = p
		registry.ordered = append(registry.ordered, p)
	}
	sort.Slice(registry.ordered, func(i, j int) bool {
		return registry.ordered[i].Name() < registry.ordered[j].Name()
	})
	return registry, nil
}

// Get returns the provider registered under name.
func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.byName[name]
	return p, ok
}

// All returns every provider, sorted by name. Callers must not mutate
// the returned slice.
func (r *Registry) All() []Provider {
	return r.ordered
}
```

`internal/provider/providertest/fake.go`:

```go
// Package providertest provides a configurable in-memory Provider for
// engine, repo, and daemon tests.
package providertest

import (
	"context"
	"sync"

	"github.com/Sawmonabo/agent-brain/internal/provider"
)

// Fake implements provider.Provider with a fixed table and recorded
// ReconcileIndex calls. Safe for concurrent use.
type Fake struct {
	name     string
	scope    provider.Scope
	patterns []provider.Pattern

	mu             sync.Mutex
	reconcileCalls []string
	// ReconcileFunc, when non-nil, runs inside ReconcileIndex after the
	// call is recorded — lets tests mutate the dir or inject errors.
	ReconcileFunc func(ctx context.Context, dir string) error
}

// New constructs a Fake. patterns may be nil (everything ClassFact).
func New(name string, scope provider.Scope, patterns []provider.Pattern) *Fake {
	return &Fake{name: name, scope: scope, patterns: patterns}
}

func (f *Fake) Name() string                 { return f.name }
func (f *Fake) Scope() provider.Scope        { return f.scope }
func (f *Fake) Patterns() []provider.Pattern { return f.patterns }

func (f *Fake) ReconcileIndex(ctx context.Context, dir string) error {
	f.mu.Lock()
	f.reconcileCalls = append(f.reconcileCalls, dir)
	fn := f.ReconcileFunc
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, dir)
	}
	return nil
}

// ReconcileCalls returns the dirs ReconcileIndex was called with, in order.
func (f *Fake) ReconcileCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reconcileCalls...)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/provider/... -race -v 2>&1 | tail -15`
Expected: all PASS.

- [ ] **Step 5: Fuzz smoke**

Run: `go test ./internal/provider/ -fuzz FuzzMatch -fuzztime 20s`
Expected: no crashers. (`-fuzz` takes exactly ONE package.)

- [ ] **Step 6: Lint, format, commit**

```bash
gofumpt -l -w . && golangci-lint run && go test ./... -race
git add internal/provider/
git commit -m "feat(provider): adapter contract, class/pattern model, registry, test fake (spec §6, ADRs 02/03)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: Memories-repo layout, name contracts, canonical `.gitattributes` generation (spec §3, §5)

The repo package becomes the canonical home of the attributes content the Phase-1 e2e harness carries as a const (Task 10's brief said exactly this). Generation is driven by the SAME provider pattern tables that drive classification — one table, two consumers, no drift.

**Files:**
- Create: `internal/repo/layout.go`, `internal/repo/names.go`, `internal/repo/attributes.go`
- Test: `internal/repo/layout_test.go`, `internal/repo/names_test.go`, `internal/repo/attributes_test.go`

**Interfaces:**
- Consumes: `provider.Registry`, `provider.Pattern`, `provider.ClassRegenerated` (Task 1); `github.com/google/renameio/v2`.
- Produces (later tasks rely on these exact names):
  - Constants: `MetaDirName = ".agent-brain"`, `GlobalFolder = "_global"`
  - `type Layout struct{ … }` · `func NewLayout(root string) Layout`
  - `(Layout) Root() string` · `MetaDir() string` · `ProjectsFile() string` · `ManifestDir() string` · `ManifestFile(host string) string` · `AttributesFile() string` · `UnitDir(folder, providerName string) string`
  - `func ValidateFolderName(name string) error` · `func SanitizeHostname(host string) string`
  - `func GenerateAttributes(reg *provider.Registry) string` · `func WriteAttributes(l Layout, reg *provider.Registry) error`

- [ ] **Step 1: Write the failing tests** — `internal/repo/names_test.go`:

```go
package repo_test

import (
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func TestValidateFolderName(t *testing.T) {
	t.Parallel()
	valid := []string{"agent-brain", "ai-sidekicks", "Repo.Name", "a", "x_y", "proj-2"}
	for _, name := range valid {
		if err := repo.ValidateFolderName(name); err != nil {
			t.Fatalf("valid folder %q rejected: %v", name, err)
		}
	}
	invalid := []string{
		"",            // empty
		".",           // path special
		"..",          // traversal
		".hidden",     // leading dot collides with meta/VCS space
		"_global",     // reserved for global-scope pools
		"_anything",   // whole '_' prefix reserved
		".agent-brain",// reserved meta dir
		".git",        // VCS
		"a/b",         // separator
		`a\b`,         // separator (windows-style)
		"has space",   // breaks .gitattributes and CLI ergonomics
		"a\x00b",      // control byte
		strings.Repeat("x", 101), // length cap
	}
	for _, name := range invalid {
		if err := repo.ValidateFolderName(name); err == nil {
			t.Fatalf("ValidateFolderName(%q) = nil, want error", name)
		}
	}
}

func TestSanitizeHostname(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"Sawmons-MacBook-Pro", "Sawmons-MacBook-Pro"},
		{"host.local", "host.local"},
		{"weird host\n", "weird-host-"},
		{"a/b:c", "a-b-c"},
		{"", "unknown-host"},
		{strings.Repeat("h", 200), strings.Repeat("h", 100)},
	}
	for _, tt := range tests {
		if got := repo.SanitizeHostname(tt.in); got != tt.want {
			t.Fatalf("SanitizeHostname(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
```

`internal/repo/layout_test.go`:

```go
package repo_test

import (
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func TestLayoutPaths(t *testing.T) {
	t.Parallel()
	l := repo.NewLayout("/data/memories")

	tests := []struct{ name, got, want string }{
		{"root", l.Root(), "/data/memories"},
		{"meta", l.MetaDir(), filepath.Join("/data/memories", ".agent-brain")},
		{"projects", l.ProjectsFile(), filepath.Join("/data/memories", ".agent-brain", "projects.toml")},
		{"manifests", l.ManifestDir(), filepath.Join("/data/memories", ".agent-brain", "manifests")},
		{"manifest file sanitizes", l.ManifestFile("host/../x"), filepath.Join("/data/memories", ".agent-brain", "manifests", "host-..-x.json")},
		{"attributes", l.AttributesFile(), filepath.Join("/data/memories", ".gitattributes")},
		{"project unit", l.UnitDir("agent-brain", "claude"), filepath.Join("/data/memories", "agent-brain", "claude")},
		{"global unit", l.UnitDir(repo.GlobalFolder, "codex"), filepath.Join("/data/memories", "_global", "codex")},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Fatalf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}
```

Note the manifest-file case: `ManifestFile` must route through `SanitizeHostname` so a hostile hostname can NEVER traverse out of the manifests dir — `/` is not in the sanitizer's allowed set, so `..` never survives adjacent to a separator.

`internal/repo/attributes_test.go`:

```go
package repo_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/providertest"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func testRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	claude := providertest.New("claude", provider.ScopePerProject, []provider.Pattern{
		{Glob: "MEMORY.md", Class: provider.ClassDerivedIndex},
		{Glob: "*.md", Class: provider.ClassFact},
	})
	codex := providertest.New("codex", provider.ScopeGlobal, []provider.Pattern{
		{Glob: "raw_memories.md", Class: provider.ClassFact},
		{Glob: "memory_summary.md", Class: provider.ClassRegenerated},
		{Glob: "MEMORY.md", Class: provider.ClassRegenerated},
		{Glob: "rollout_summaries/*", Class: provider.ClassRegenerated},
	})
	reg, err := provider.NewRegistry(claude, codex)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// TestGenerateAttributesGolden pins the exact canonical file. Ordering is
// load-bearing three ways: gitattributes resolves per attribute with the
// LAST matching line winning, so the '*' default comes first, lww
// overrides follow, and the meta/self exclusions come last (they must
// beat everything). The default and exclusion lines are byte-identical
// with the Phase-1 e2e harness const — continuity of the proven wiring.
func TestGenerateAttributesGolden(t *testing.T) {
	t.Parallel()
	want := `# Generated by agent-brain; do not edit (init/doctor rewrite this file).
* filter=agentbrain diff=agentbrain merge=agentbrain -text
**/codex/memory_summary.md merge=agentbrain-lww
**/codex/MEMORY.md merge=agentbrain-lww
**/codex/rollout_summaries/* merge=agentbrain-lww
.gitattributes -filter -diff -merge text eol=lf
.agent-brain/** -filter -diff -merge text eol=lf
`
	got := repo.GenerateAttributes(testRegistry(t))
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("GenerateAttributes mismatch (-want +got):\n%s", diff)
	}
	// Determinism: byte-identical on repeat (no map-order leakage).
	if again := repo.GenerateAttributes(testRegistry(t)); again != got {
		t.Fatal("GenerateAttributes is nondeterministic")
	}
}

func TestWriteAttributes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	l := repo.NewLayout(root)
	if err := repo.WriteAttributes(l, testRegistry(t)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".gitattributes"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != repo.GenerateAttributes(testRegistry(t)) {
		t.Fatal("written attributes differ from generated content")
	}
	// Idempotent overwrite (renameio replace, not append).
	if err := repo.WriteAttributes(l, testRegistry(t)); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(filepath.Join(root, ".gitattributes"))
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(data) {
		t.Fatal("rewrite is not idempotent")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/repo/... 2>&1 | head -20`
Expected: compile errors (package does not exist).

- [ ] **Step 3: Add the renameio dependency (skip if already in go.mod)**

Run: `grep renameio go.mod || go get github.com/google/renameio/v2@latest`

- [ ] **Step 4: Implement** — `internal/repo/names.go`:

```go
// Package repo models the agent-brain-memories repository: its on-disk
// layout, name contracts, canonical .gitattributes, registries, and
// per-host manifests (spec §3).
package repo

import (
	"fmt"
	"regexp"
	"strings"
)

// folderRE pins project-folder names: start alphanumeric, then a
// path-safe set, capped at 100. Leading '.' and '_' are excluded by the
// start class — '.' collides with meta/VCS space, '_' is the reserved
// prefix (GlobalFolder).
var folderRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// reservedFolders are rejected regardless of charset. folderRE already
// blocks '.'/'_' starts; these entries document the collision class and
// defend it even if the regexp is ever loosened.
var reservedFolders = map[string]bool{
	".":            true,
	"..":           true,
	MetaDirName:    true,
	GlobalFolder:   true,
	".git":         true,
	".gitattributes": true,
}

// ValidateFolderName gates every project-folder name before it becomes a
// repo path segment: registry writes, enrollment, and migration all call
// this. POSIX-only targets (macOS/Linux/WSL2-ext4), so Windows reserved
// device names are deliberately out of scope.
func ValidateFolderName(name string) error {
	if reservedFolders[name] {
		return fmt.Errorf("folder name %q is reserved", name)
	}
	if !folderRE.MatchString(name) {
		return fmt.Errorf("folder name %q violates the name contract (%s)", name, folderRE)
	}
	return nil
}

// hostSafe is the byte set preserved by SanitizeHostname.
var hostSafe = regexp.MustCompile(`[^A-Za-z0-9.-]`)

// SanitizeHostname makes a hostname safe for manifest filenames and
// commit-message tokens: every byte outside [A-Za-z0-9.-] becomes '-',
// output is capped at 100 bytes, and empty input becomes "unknown-host".
// '/' is outside the allowed set, so no output can traverse directories.
func SanitizeHostname(host string) string {
	cleaned := hostSafe.ReplaceAllString(host, "-")
	if len(cleaned) > 100 {
		cleaned = cleaned[:100]
	}
	if cleaned == "" || strings.Trim(cleaned, ".-") == "" {
		return "unknown-host"
	}
	return cleaned
}
```

`internal/repo/layout.go`:

```go
package repo

import "path/filepath"

const (
	// MetaDirName holds plaintext machine-shared metadata inside the
	// memories repo (registry + manifests) — excluded from filtering by
	// the generated .gitattributes.
	MetaDirName = ".agent-brain"
	// GlobalFolder holds user-global provider pools (Codex) — spec §3.
	GlobalFolder = "_global"

	projectsFileName = "projects.toml"
	manifestDirName  = "manifests"
)

// Layout resolves every path inside a memories checkout. It is pure path
// arithmetic: callers own validation (ValidateFolderName, the provider
// name contract) at the boundaries where names enter the system.
type Layout struct {
	root string
}

// NewLayout wraps the checkout root (absolute path).
func NewLayout(root string) Layout { return Layout{root: root} }

func (l Layout) Root() string    { return l.root }
func (l Layout) MetaDir() string { return filepath.Join(l.root, MetaDirName) }

func (l Layout) ProjectsFile() string { return filepath.Join(l.MetaDir(), projectsFileName) }
func (l Layout) ManifestDir() string  { return filepath.Join(l.MetaDir(), manifestDirName) }

// ManifestFile routes through SanitizeHostname so hostile hostnames can
// never escape the manifests dir.
func (l Layout) ManifestFile(host string) string {
	return filepath.Join(l.ManifestDir(), SanitizeHostname(host)+".json")
}

func (l Layout) AttributesFile() string { return filepath.Join(l.root, ".gitattributes") }

// UnitDir is the checkout dir for one sync unit: <folder>/<provider>.
// Pass GlobalFolder for global-scope providers.
func (l Layout) UnitDir(folder, providerName string) string {
	return filepath.Join(l.root, folder, providerName)
}
```

`internal/repo/attributes.go`:

```go
package repo

import (
	"fmt"
	"strings"

	"github.com/google/renameio/v2"

	"github.com/Sawmonabo/agent-brain/internal/provider"
)

// GenerateAttributes emits the canonical .gitattributes for a memories
// repo (spec §5). Line order is load-bearing: gitattributes resolves
// each attribute with the LAST matching line winning, so the filtered
// default comes first, per-provider newest-wins overrides follow, and
// the self/meta exclusions close the file so nothing re-filters them.
// The default and exclusion lines are byte-identical with the wiring the
// Phase-1 e2e suite proved.
//
// Inputs are pre-validated (provider.NewRegistry rejects names and globs
// that cannot appear in an attributes line), so this function is total.
func GenerateAttributes(reg *provider.Registry) string {
	var b strings.Builder
	b.WriteString("# Generated by agent-brain; do not edit (init/doctor rewrite this file).\n")
	b.WriteString("* filter=agentbrain diff=agentbrain merge=agentbrain -text\n")
	for _, p := range reg.All() { // deterministic: sorted by name
		for _, pat := range p.Patterns() { // deterministic: table order
			if pat.Class != provider.ClassRegenerated {
				continue
			}
			fmt.Fprintf(&b, "**/%s/%s merge=agentbrain-lww\n", p.Name(), pat.Glob)
		}
	}
	b.WriteString(".gitattributes -filter -diff -merge text eol=lf\n")
	b.WriteString(".agent-brain/** -filter -diff -merge text eol=lf\n")
	return b.String()
}

// WriteAttributes atomically replaces the checkout's .gitattributes with
// the canonical content (temp file + rename; no partial-state window).
func WriteAttributes(l Layout, reg *provider.Registry) error {
	if err := renameio.WriteFile(l.AttributesFile(), []byte(GenerateAttributes(reg)), 0o644); err != nil {
		return fmt.Errorf("write attributes: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/repo/... -race -v 2>&1 | tail -15`
Expected: all PASS.

- [ ] **Step 6: Lint, format, full suite, commit**

```bash
gofumpt -l -w . && golangci-lint run && go test ./... -race
git add internal/repo/ go.mod go.sum
git commit -m "feat(repo): memories-repo layout, name contracts, canonical .gitattributes generation (spec §3, §5)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: Project registry (`projects.toml`) + local unit registry (`registry-local.toml`) (spec §3; ADR 17)

Two registries, two audiences. `projects.toml` lives IN the memories repo (plaintext, machine-shared, machine-owned — comment-free per ADR 17) and maps canonical project IDs to repo folders with deterministic collision handling. `registry-local.toml` lives in the machine's data dir and binds enrolled local dirs to repo units — it NEVER enters the repo (spec §3: local slugs/paths are machine-specific). Both APIs take explicit file paths; the daemon/CLI composition layer supplies them (the repo package stays env-free).

**Files:**
- Create: `internal/repo/projects.go`, `internal/repo/local.go`
- Test: `internal/repo/projects_test.go`, `internal/repo/local_test.go`

**Interfaces:**
- Consumes: `ValidateFolderName`, `GlobalFolder` (Task 2); `pelletier/go-toml/v2`; `renameio/v2`.
- Produces (later tasks rely on these exact names):
  - `type Projects struct { Version int; Entries map[string]ProjectEntry }` (TOML: `version`, `projects`) · `type ProjectEntry struct { ID string }`
  - `func LoadProjects(path string) (*Projects, error)` · `(*Projects).Save(path string) error` · `(*Projects).FolderFor(id string) (string, bool)` · `(*Projects).Add(id, preferredFolder string) (string, error)`
  - `type Unit struct { Provider, ProjectID, Folder, LocalDir string }`
  - `type LocalRegistry struct { Version int; Units []Unit }`
  - `func LoadLocalRegistry(path string) (*LocalRegistry, error)` · `(*LocalRegistry).Save(path string) error` · `(*LocalRegistry).Enroll(u Unit) error` · `(*LocalRegistry).Remove(providerName, localDir string) bool`
  - `const RegistryVersion = 1`

- [ ] **Step 1: Write the failing tests** — `internal/repo/projects_test.go`:

```go
package repo_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func TestProjectsAddIsIdempotentAndDisambiguates(t *testing.T) {
	t.Parallel()
	p := repo.NewProjects()

	folder, err := p.Add("github.com/sawmonabo/agent-brain", "agent-brain")
	if err != nil {
		t.Fatal(err)
	}
	if folder != "agent-brain" {
		t.Fatalf("first Add folder = %q, want agent-brain", folder)
	}

	// Same ID again → same folder, no growth (idempotent re-enrollment).
	again, err := p.Add("github.com/sawmonabo/agent-brain", "agent-brain")
	if err != nil {
		t.Fatal(err)
	}
	if again != "agent-brain" {
		t.Fatalf("re-Add folder = %q, want agent-brain", again)
	}

	// Different ID, colliding basename → deterministic -2 suffix,
	// recorded in the registry (spec §3: registry-recorded disambiguation).
	other, err := p.Add("gitlab.com/other/agent-brain", "agent-brain")
	if err != nil {
		t.Fatal(err)
	}
	if other != "agent-brain-2" {
		t.Fatalf("colliding Add folder = %q, want agent-brain-2", other)
	}

	if got, ok := p.FolderFor("gitlab.com/other/agent-brain"); !ok || got != "agent-brain-2" {
		t.Fatalf("FolderFor = %q,%v", got, ok)
	}
}

func TestProjectsAddRejectsInvalidInputs(t *testing.T) {
	t.Parallel()
	p := repo.NewProjects()
	if _, err := p.Add("", "x"); err == nil {
		t.Fatal("empty id accepted")
	}
	if _, err := p.Add("id", "_global"); err == nil {
		t.Fatal("reserved folder accepted")
	}
	if _, err := p.Add("id", "a/b"); err == nil {
		t.Fatal("separator folder accepted")
	}
}

func TestProjectsRoundtripDeterministic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "projects.toml")

	p := repo.NewProjects()
	for _, add := range []struct{ id, folder string }{
		{"github.com/sawmonabo/zeta", "zeta"},
		{"github.com/sawmonabo/alpha", "alpha"},
	} {
		if _, err := p.Add(add.id, add.folder); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Save(path); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Deterministic bytes: save again, expect identical output — the
	// file lives in a git repo; nondeterministic key order = diff churn.
	if err := p.Save(path); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("Save is nondeterministic:\n--- first\n%s\n--- second\n%s", first, second)
	}

	loaded, err := repo.LoadProjects(path)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(p, loaded); diff != "" {
		t.Fatalf("roundtrip mismatch (-saved +loaded):\n%s", diff)
	}
}

func TestLoadProjectsMissingFileIsEmptyRegistry(t *testing.T) {
	t.Parallel()
	p, err := repo.LoadProjects(filepath.Join(t.TempDir(), "nope", "projects.toml"))
	if err != nil {
		t.Fatalf("missing file must yield an empty registry (first machine), got %v", err)
	}
	if p.Version != repo.RegistryVersion || len(p.Entries) != 0 {
		t.Fatalf("empty registry expected, got %+v", p)
	}
}

func TestLoadProjectsRejectsUnknownVersionAndCorruptTOML(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	vpath := filepath.Join(dir, "v99.toml")
	if err := os.WriteFile(vpath, []byte("version = 99\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LoadProjects(vpath); err == nil {
		t.Fatal("unknown version accepted; want explicit error (forward-compat contract)")
	}

	cpath := filepath.Join(dir, "corrupt.toml")
	if err := os.WriteFile(cpath, []byte("version = [broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LoadProjects(cpath); err == nil {
		t.Fatal("corrupt TOML accepted; want error")
	}
}
```

`internal/repo/local_test.go`:

```go
package repo_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func validUnit() repo.Unit {
	return repo.Unit{
		Provider:  "claude",
		ProjectID: "github.com/sawmonabo/agent-brain",
		Folder:    "agent-brain",
		LocalDir:  "/home/u/.claude/projects/-home-u-dev-agent-brain/memory",
	}
}

func TestLocalRegistryEnrollValidatesAndDedupes(t *testing.T) {
	t.Parallel()
	r := repo.NewLocalRegistry()

	if err := r.Enroll(validUnit()); err != nil {
		t.Fatal(err)
	}
	// Same (provider, local dir) again → idempotent no-op.
	if err := r.Enroll(validUnit()); err != nil {
		t.Fatal(err)
	}
	if len(r.Units) != 1 {
		t.Fatalf("dedupe failed: %d units", len(r.Units))
	}

	// A second local dir feeding the SAME (provider, folder) would make
	// two sources mirror into one checkout dir — ping-pong. Reject.
	dup := validUnit()
	dup.LocalDir = "/home/u/elsewhere/memory"
	if err := r.Enroll(dup); err == nil {
		t.Fatal("second local dir for same (provider, folder) accepted; want error")
	}

	bad := []repo.Unit{
		{Provider: "", ProjectID: "x", Folder: "f", LocalDir: "/abs"},
		{Provider: "claude", ProjectID: "x", Folder: "_global2", LocalDir: "/abs"}, // '_' reserved
		{Provider: "claude", ProjectID: "x", Folder: "ok", LocalDir: "relative/dir"},
	}
	for i, u := range bad {
		if err := r.Enroll(u); err == nil {
			t.Fatalf("bad unit %d accepted: %+v", i, u)
		}
	}

	// Global-scope pseudo-project: GlobalFolder IS valid here (and only
	// here — user-facing folder validation still rejects it; the
	// registry accepts it for ScopeGlobal units with empty ProjectID).
	global := repo.Unit{Provider: "codex", Folder: repo.GlobalFolder, LocalDir: "/home/u/.codex/memories"}
	if err := r.Enroll(global); err != nil {
		t.Fatalf("global unit rejected: %v", err)
	}
}

func TestLocalRegistryRoundtripAndRemove(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "deep", "registry-local.toml")

	r := repo.NewLocalRegistry()
	if err := r.Enroll(validUnit()); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("registry-local mode = %o, want 0600", got)
	}

	loaded, err := repo.LoadLocalRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(r, loaded); diff != "" {
		t.Fatalf("roundtrip mismatch:\n%s", diff)
	}

	if !loaded.Remove("claude", validUnit().LocalDir) {
		t.Fatal("Remove returned false for enrolled unit")
	}
	if loaded.Remove("claude", validUnit().LocalDir) {
		t.Fatal("Remove returned true for absent unit")
	}
}

func TestLoadLocalRegistryMissingAndInvalid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	r, err := repo.LoadLocalRegistry(filepath.Join(dir, "absent.toml"))
	if err != nil || len(r.Units) != 0 {
		t.Fatalf("missing file: got %+v, %v; want empty registry, nil", r, err)
	}

	// Corrupt entries must fail loudly at load, naming the entry — a
	// silently-skipped unit is a project that silently stops syncing.
	bad := filepath.Join(dir, "bad.toml")
	content := "version = 1\n\n[[units]]\nprovider = \"claude\"\nproject_id = \"x\"\nfolder = \"ok\"\nlocal_dir = \"not-absolute\"\n"
	if err := os.WriteFile(bad, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LoadLocalRegistry(bad); err == nil {
		t.Fatal("invalid unit accepted at load; want error")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/repo/... 2>&1 | head -20`
Expected: compile errors (new identifiers).

- [ ] **Step 3: Add the go-toml dependency (skip if already in go.mod)**

Run: `grep 'pelletier/go-toml' go.mod || go get github.com/pelletier/go-toml/v2@latest`

- [ ] **Step 4: Implement** — `internal/repo/projects.go`:

```go
package repo

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/google/renameio/v2"
)

// RegistryVersion is the current schema version of both registries.
// Loaders reject anything else explicitly — an older binary must fail
// loudly on a newer file, never misread it.
const RegistryVersion = 1

// ProjectEntry is one canonical project in the shared registry.
type ProjectEntry struct {
	ID string `toml:"id"`
}

// Projects is the machine-shared project registry stored at
// .agent-brain/projects.toml inside the memories repo. Machine-owned and
// comment-free (ADR 17); keys are repo folder names.
type Projects struct {
	Version int                     `toml:"version"`
	Entries map[string]ProjectEntry `toml:"projects"`
}

// NewProjects returns an empty registry at the current version.
func NewProjects() *Projects {
	return &Projects{Version: RegistryVersion, Entries: map[string]ProjectEntry{}}
}

// LoadProjects reads path. A missing file is an empty registry (the
// first machine ever); anything unreadable, unparseable, or at an
// unknown version is an explicit error.
func LoadProjects(path string) (*Projects, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return NewProjects(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read projects registry: %w", err)
	}
	var p Projects
	if err := toml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse projects registry %s: %w", path, err)
	}
	if p.Version != RegistryVersion {
		return nil, fmt.Errorf("projects registry %s: unsupported version %d (this binary supports %d)", path, p.Version, RegistryVersion)
	}
	if p.Entries == nil {
		p.Entries = map[string]ProjectEntry{}
	}
	for folder, entry := range p.Entries {
		if err := ValidateFolderName(folder); err != nil {
			return nil, fmt.Errorf("projects registry %s: %w", path, err)
		}
		if entry.ID == "" {
			return nil, fmt.Errorf("projects registry %s: folder %q has empty id", path, folder)
		}
	}
	return &p, nil
}

// Save atomically writes the registry with deterministic bytes (folders
// emitted in sorted order) — the file lives in git; churn is a defect.
func (p *Projects) Save(path string) error {
	folders := make([]string, 0, len(p.Entries))
	for folder := range p.Entries {
		folders = append(folders, folder)
	}
	sort.Strings(folders)

	// Emit deterministically ourselves rather than depending on the
	// library's map-ordering behavior.
	buf := []byte(fmt.Sprintf("version = %d\n", p.Version))
	for _, folder := range folders {
		entry, err := toml.Marshal(map[string]map[string]ProjectEntry{"projects": {folder: p.Entries[folder]}})
		if err != nil {
			return fmt.Errorf("encode projects registry: %w", err)
		}
		buf = append(buf, '\n')
		buf = append(buf, entry...)
	}
	if err := renameio.WriteFile(path, buf, 0o644); err != nil {
		return fmt.Errorf("write projects registry: %w", err)
	}
	return nil
}

// FolderFor returns the repo folder recorded for a canonical ID.
func (p *Projects) FolderFor(id string) (string, bool) {
	for folder, entry := range p.Entries {
		if entry.ID == id {
			return folder, true
		}
	}
	return "", false
}

// Add registers id under preferredFolder, disambiguating deterministically
// on collision (folder, folder-2, folder-3, …) and returning the folder
// actually recorded. Re-adding an existing id is idempotent (spec §3).
func (p *Projects) Add(id, preferredFolder string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("empty project id")
	}
	if existing, ok := p.FolderFor(id); ok {
		return existing, nil
	}
	if err := ValidateFolderName(preferredFolder); err != nil {
		return "", err
	}
	candidate := preferredFolder
	for suffix := 2; ; suffix++ {
		if _, taken := p.Entries[candidate]; !taken {
			break
		}
		candidate = fmt.Sprintf("%s-%d", preferredFolder, suffix)
		if err := ValidateFolderName(candidate); err != nil {
			return "", err
		}
	}
	p.Entries[candidate] = ProjectEntry{ID: id}
	return candidate, nil
}
```

`internal/repo/local.go`:

```go
package repo

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/google/renameio/v2"
)

// Unit binds one enrolled local provider directory to its repo location.
// This is the engine's work item: mirror LocalDir ↔ <Folder>/<Provider>.
type Unit struct {
	Provider  string `toml:"provider"`
	ProjectID string `toml:"project_id"` // canonical id; empty for global scope
	Folder    string `toml:"folder"`     // repo folder, or GlobalFolder
	LocalDir  string `toml:"local_dir"`  // absolute, machine-local
}

func (u Unit) validate() error {
	if u.Provider == "" {
		return fmt.Errorf("unit has empty provider")
	}
	if u.Folder != GlobalFolder {
		if err := ValidateFolderName(u.Folder); err != nil {
			return err
		}
	}
	if !filepath.IsAbs(u.LocalDir) {
		return fmt.Errorf("unit local_dir %q is not absolute", u.LocalDir)
	}
	return nil
}

// LocalRegistry is this machine's enrollment state, stored at
// <data-dir>/registry-local.toml. It NEVER enters the memories repo —
// local slugs and paths are machine-specific (spec §3).
type LocalRegistry struct {
	Version int    `toml:"version"`
	Units   []Unit `toml:"units"`
}

// NewLocalRegistry returns an empty registry at the current version.
func NewLocalRegistry() *LocalRegistry {
	return &LocalRegistry{Version: RegistryVersion}
}

// LoadLocalRegistry reads path; a missing file is an empty registry.
// Corrupt or invalid content fails loudly, naming the offending unit —
// a silently-dropped unit is a project that silently stops syncing.
func LoadLocalRegistry(path string) (*LocalRegistry, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return NewLocalRegistry(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read local registry: %w", err)
	}
	var r LocalRegistry
	if err := toml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse local registry %s: %w", path, err)
	}
	if r.Version != RegistryVersion {
		return nil, fmt.Errorf("local registry %s: unsupported version %d (this binary supports %d)", path, r.Version, RegistryVersion)
	}
	for i, u := range r.Units {
		if err := u.validate(); err != nil {
			return nil, fmt.Errorf("local registry %s: unit %d: %w", path, i, err)
		}
	}
	return &r, nil
}

// Save atomically writes the registry (0600 — it maps this machine's
// private filesystem layout), creating the parent dir 0700 when needed.
// Units are sorted for deterministic bytes.
func (r *LocalRegistry) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create local-registry dir: %w", err)
	}
	sort.Slice(r.Units, func(i, j int) bool {
		a, b := r.Units[i], r.Units[j]
		if a.Folder != b.Folder {
			return a.Folder < b.Folder
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.LocalDir < b.LocalDir
	})
	data, err := toml.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode local registry: %w", err)
	}
	if err := renameio.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write local registry: %w", err)
	}
	return nil
}

// Enroll adds u. Re-enrolling the same (provider, local dir) is an
// idempotent no-op. A DIFFERENT local dir for an already-fed
// (provider, folder) is rejected: two local sources mirroring into one
// checkout dir would ping-pong overwrite each other.
func (r *LocalRegistry) Enroll(u Unit) error {
	if err := u.validate(); err != nil {
		return err
	}
	for _, existing := range r.Units {
		if existing.Provider == u.Provider && existing.LocalDir == u.LocalDir {
			return nil
		}
		if existing.Provider == u.Provider && existing.Folder == u.Folder {
			return fmt.Errorf("folder %q already fed by %s on this machine (%s); untrack it first", u.Folder, u.Provider, existing.LocalDir)
		}
	}
	r.Units = append(r.Units, u)
	return nil
}

// Remove drops the unit for (providerName, localDir), reporting whether
// anything was removed.
func (r *LocalRegistry) Remove(providerName, localDir string) bool {
	for i, u := range r.Units {
		if u.Provider == providerName && u.LocalDir == localDir {
			r.Units = append(r.Units[:i], r.Units[i+1:]...)
			return true
		}
	}
	return false
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/repo/... -race -v 2>&1 | tail -20`
Expected: all PASS. If the `Projects.Save` golden/determinism test fails on TOML shape (e.g. `[projects.agent-brain]` header form), adjust the emission in `Save` — the deterministic-bytes and roundtrip properties are the contract, the exact header style is not.

- [ ] **Step 6: Lint, format, full suite, commit**

```bash
gofumpt -l -w . && golangci-lint run && go test ./... -race
git add internal/repo/ go.mod go.sum
git commit -m "feat(repo): shared project registry + machine-local unit registry with fail-fast validation (spec §3, ADR 17)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: Per-host sync manifests (spec §3, §4; ADR 17 keeps these JSON)

The manifest is the deletion-disambiguation ledger: it records, per repo-relative path, the state this host last synced. Mirror-in uses it to tell "deleted here" (in manifest, gone locally, present in checkout → `git rm`) from "new from remote" (absent from manifest, present in checkout → mirror out). Mirror-out applies remote deletions to provider dirs ONLY when the manifest proves the file was synced here before. One file per host inside the repo (`.agent-brain/manifests/<host>.json`) — each host writes only its own file, so manifests never merge-conflict.

**Files:**
- Create: `internal/repo/manifest.go`
- Test: `internal/repo/manifest_test.go`

**Interfaces:**
- Consumes: `Layout.ManifestFile` (Task 2); stdlib `encoding/json`, `crypto/sha256`; `renameio/v2`.
- Produces (the engine relies on these exact names):
  - `type ManifestEntry struct { Size int64; MTimeUnixNano int64; SHA256 string }` (JSON: `size`, `mtime_unix_nano`, `sha256`)
  - `type Manifest struct { Version int; Files map[string]ManifestEntry }` (JSON: `version`, `files`; keys are slash-separated repo-relative paths)
  - `func NewManifest() *Manifest` · `func LoadManifest(path string) (*Manifest, error)` · `(*Manifest).Save(path string) error`
  - `(*Manifest).Has(rel string) bool` · `(*Manifest).Get(rel string) (ManifestEntry, bool)` · `(*Manifest).Set(rel string, e ManifestEntry) error` · `(*Manifest).Delete(rel string)`
  - `func HashFile(path string) (ManifestEntry, error)` · `func ValidateRelPath(rel string) error`

- [ ] **Step 1: Write the failing tests** — `internal/repo/manifest_test.go`:

```go
package repo_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func TestManifestRoundtripDeterministic(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "deep", "host.json")

	m := repo.NewManifest()
	for _, rel := range []string{"zeta/claude/notes.md", "alpha/claude/MEMORY.md"} {
		if err := m.Set(rel, repo.ManifestEntry{Size: 3, MTimeUnixNano: 42, SHA256: "abc"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The manifest is committed to git — nondeterministic bytes = churn.
	if string(first) != string(second) {
		t.Fatal("Save is nondeterministic")
	}

	loaded, err := repo.LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(m, loaded); diff != "" {
		t.Fatalf("roundtrip mismatch:\n%s", diff)
	}
}

func TestManifestMissingUnknownVersionCorrupt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	m, err := repo.LoadManifest(filepath.Join(dir, "absent.json"))
	if err != nil || len(m.Files) != 0 {
		t.Fatalf("missing manifest: got %+v, %v; want empty, nil", m, err)
	}

	v99 := filepath.Join(dir, "v99.json")
	if err := os.WriteFile(v99, []byte(`{"version":99,"files":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LoadManifest(v99); err == nil {
		t.Fatal("unknown version accepted; want error")
	}

	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte(`{"version":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LoadManifest(corrupt); err == nil {
		t.Fatal("corrupt JSON accepted; want error")
	}

	traversal := filepath.Join(dir, "traversal.json")
	payload := `{"version":1,"files":{"../escape.md":{"size":1,"mtime_unix_nano":1,"sha256":"x"}}}`
	if err := os.WriteFile(traversal, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LoadManifest(traversal); err == nil {
		t.Fatal("traversal path in manifest accepted; want error (repo file is remote-influenced input)")
	}
}

func TestValidateRelPath(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"a/claude/x.md", "x.md", "_global/codex/raw_memories.md"} {
		if err := repo.ValidateRelPath(ok); err != nil {
			t.Fatalf("valid rel %q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/abs.md", "../up.md", "a/../b.md", "a//b.md", `a\b.md`, "./x.md", "a/./b.md"} {
		if err := repo.ValidateRelPath(bad); err == nil {
			t.Fatalf("ValidateRelPath(%q) = nil, want error", bad)
		}
	}
}

func TestHashFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "f.md")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry, err := repo.HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// sha256("hello\n")
	want := "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"
	if entry.SHA256 != want {
		t.Fatalf("SHA256 = %s, want %s", entry.SHA256, want)
	}
	if entry.Size != 6 || entry.MTimeUnixNano == 0 {
		t.Fatalf("Size/MTime not populated: %+v", entry)
	}
	if !strings.EqualFold(entry.SHA256, want) {
		t.Fatal("hash must be lowercase hex")
	}
	if _, err := repo.HashFile(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("HashFile(absent) = nil error")
	}
}

func TestManifestSetGetDelete(t *testing.T) {
	t.Parallel()
	m := repo.NewManifest()
	if err := m.Set("../bad.md", repo.ManifestEntry{}); err == nil {
		t.Fatal("Set accepted traversal path")
	}
	if err := m.Set("p/claude/a.md", repo.ManifestEntry{Size: 1, MTimeUnixNano: 2, SHA256: "x"}); err != nil {
		t.Fatal(err)
	}
	if !m.Has("p/claude/a.md") {
		t.Fatal("Has = false after Set")
	}
	if entry, ok := m.Get("p/claude/a.md"); !ok || entry.Size != 1 {
		t.Fatalf("Get = %+v, %v", entry, ok)
	}
	m.Delete("p/claude/a.md")
	if m.Has("p/claude/a.md") {
		t.Fatal("Has = true after Delete")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/repo/ -run 'TestManifest|TestValidateRelPath|TestHashFile' 2>&1 | head -20`
Expected: compile errors.

- [ ] **Step 3: Implement** — `internal/repo/manifest.go`:

```go
package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/renameio/v2"
)

// ManifestEntry is the state of one file as this host last synced it.
type ManifestEntry struct {
	Size          int64  `json:"size"`
	MTimeUnixNano int64  `json:"mtime_unix_nano"`
	SHA256        string `json:"sha256"`
}

// Manifest is this host's sync ledger (spec §4): which repo paths this
// machine has synced, and in what state. It disambiguates deletions —
// "in manifest but gone locally" is deleted-here; "in checkout but not
// in manifest" is new-from-remote — and gates mirror-out deletions
// (remote deletions apply to provider dirs only for paths the manifest
// proves were synced here before). Stored at
// .agent-brain/manifests/<host>.json; each host writes ONLY its own
// file, so manifests never merge-conflict.
type Manifest struct {
	Version int                      `json:"version"`
	Files   map[string]ManifestEntry `json:"files"`
}

// NewManifest returns an empty manifest at the current version.
func NewManifest() *Manifest {
	return &Manifest{Version: RegistryVersion, Files: map[string]ManifestEntry{}}
}

// LoadManifest reads path; a missing file is an empty manifest (first
// sync on this host). The file rides the shared repo, so its content is
// remote-influenced input: unknown versions, corrupt JSON, and unsafe
// paths are explicit errors, never best-effort skips.
func LoadManifest(manifestPath string) (*Manifest, error) {
	data, err := os.ReadFile(manifestPath)
	if errors.Is(err, fs.ErrNotExist) {
		return NewManifest(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", manifestPath, err)
	}
	if m.Version != RegistryVersion {
		return nil, fmt.Errorf("manifest %s: unsupported version %d (this binary supports %d)", manifestPath, m.Version, RegistryVersion)
	}
	if m.Files == nil {
		m.Files = map[string]ManifestEntry{}
	}
	for rel := range m.Files {
		if err := ValidateRelPath(rel); err != nil {
			return nil, fmt.Errorf("manifest %s: %w", manifestPath, err)
		}
	}
	return &m, nil
}

// Save atomically writes the manifest. encoding/json sorts map keys, and
// indentation keeps repo diffs reviewable.
func (m *Manifest) Save(manifestPath string) error {
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		return fmt.Errorf("create manifest dir: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := renameio.WriteFile(manifestPath, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// Has reports whether rel was synced by this host before.
func (m *Manifest) Has(rel string) bool {
	_, ok := m.Files[rel]
	return ok
}

// Get returns the recorded state for rel.
func (m *Manifest) Get(rel string) (ManifestEntry, bool) {
	e, ok := m.Files[rel]
	return e, ok
}

// Set records rel at state e. rel must be a clean slash-separated
// repo-relative path — the same contract LoadManifest enforces.
func (m *Manifest) Set(rel string, e ManifestEntry) error {
	if err := ValidateRelPath(rel); err != nil {
		return err
	}
	m.Files[rel] = e
	return nil
}

// Delete removes rel from the ledger.
func (m *Manifest) Delete(rel string) {
	delete(m.Files, rel)
}

// ValidateRelPath admits only clean, slash-separated, repo-relative
// paths: non-empty, not absolute, no backslashes, no '.'/'..' segments,
// and already in path.Clean form (no '//', no './').
func ValidateRelPath(rel string) error {
	if rel == "" {
		return fmt.Errorf("empty relative path")
	}
	if strings.HasPrefix(rel, "/") {
		return fmt.Errorf("path %q is absolute", rel)
	}
	if strings.Contains(rel, `\`) {
		return fmt.Errorf("path %q contains a backslash", rel)
	}
	if path.Clean(rel) != rel {
		return fmt.Errorf("path %q is not in clean form", rel)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("path %q contains a %q segment", rel, seg)
		}
	}
	return nil
}

// HashFile computes the manifest entry for an on-disk file.
func HashFile(filePath string) (ManifestEntry, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return ManifestEntry{}, fmt.Errorf("hash %s: %w", filePath, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return ManifestEntry{}, fmt.Errorf("stat %s: %w", filePath, err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ManifestEntry{}, fmt.Errorf("hash %s: %w", filePath, err)
	}
	return ManifestEntry{
		Size:          info.Size(),
		MTimeUnixNano: info.ModTime().UnixNano(),
		SHA256:        hex.EncodeToString(h.Sum(nil)),
	}, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/repo/... -race -v 2>&1 | tail -20`
Expected: all PASS.

- [ ] **Step 5: Lint, format, full suite, commit**

```bash
gofumpt -l -w . && golangci-lint run && go test ./... -race
git add internal/repo/
git commit -m "feat(repo): per-host sync manifests — the deletion-disambiguation ledger (spec §3, §4)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: Config — runtime dir (ADR 09), settings file (ADR 17), state-path helpers (spec §3)

Pure additions beside the existing `paths.go` — `Paths{ConfigDir, DataDir}`, `DefaultPaths()`, and the `AGENT_BRAIN_CONFIG_DIR`/`AGENT_BRAIN_DATA_DIR` overrides stay untouched (zero ripple into Phase-1 tests). The runtime dir is deliberately NOT part of `Paths`: it never depends on `$HOME`, and its ADR 09 rules (socket-length limit, WSL2 teardown, 0700-per-start) are their own contract.

**Files:**
- Create: `internal/config/runtime.go`, `internal/config/settings.go`, `internal/config/state.go`
- Test: `internal/config/runtime_test.go`, `internal/config/settings_test.go`, `internal/config/state_test.go`

**Interfaces:**
- Consumes: existing `config.Paths`; `pelletier/go-toml/v2` (added in Task 3).
- Produces (daemon/service/e2e rely on these exact names):
  - `func RuntimeDir() (string, error)` — override `AGENT_BRAIN_RUNTIME_DIR`; darwin `$TMPDIR`; linux `$XDG_RUNTIME_DIR`, then `/run/user/<uid>` when it exists, then `os.TempDir()/agent-brain-<uid>` (containers/WSL2 without a session dir must not brick the daemon)
  - `const SocketName = "agent-brain.sock"` · `const LockName = "agent-brain.lock"`
  - `func ValidateSocketPath(socketPath string) error` — `sun_path` is 104 bytes on macOS / 108 on Linux; enforce ≤100 with an error that names the `AGENT_BRAIN_RUNTIME_DIR` escape hatch
  - `type Settings struct { Sync SyncSettings }` · `type SyncSettings struct { Ticker, Debounce, Poll Duration }` · `type Duration time.Duration` (TextUnmarshaler)
  - `func DefaultSettings() Settings` (ticker 5m — spec §4; debounce 2s — ADR 07; poll 45s backstop) · `func LoadSettings(path string) (Settings, error)` — missing file ⇒ defaults; unknown keys ⇒ error (a typo'd key silently ignored is a config that silently doesn't apply); floors enforced (ticker ≥ 30s, debounce ≥ 100ms, poll ≥ 5s)
  - Path helpers on `Paths`: `SettingsFile()`, `MemoriesDir()`, `LocalRegistryFile()`, `DaemonLogFile()`, `ConflictLogFile()`

- [ ] **Step 1: Write the failing tests** — `internal/config/runtime_test.go`:

```go
package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/config"
)

// Runtime-dir resolution is env-driven; t.Setenv forbids t.Parallel.
func TestRuntimeDirOverrideWins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", dir)
	got, err := config.RuntimeDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Fatalf("RuntimeDir() = %q, want override %q", got, dir)
	}
}

func TestRuntimeDirPlatformDefaults(t *testing.T) {
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", "")
	switch runtime.GOOS {
	case "darwin":
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		got, err := config.RuntimeDir()
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(tmp, "agent-brain"); got != want {
			t.Fatalf("darwin RuntimeDir() = %q, want %q", got, want)
		}
	case "linux":
		xdg := t.TempDir()
		t.Setenv("XDG_RUNTIME_DIR", xdg)
		got, err := config.RuntimeDir()
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(xdg, "agent-brain"); got != want {
			t.Fatalf("linux RuntimeDir() = %q, want %q", got, want)
		}
	}
}

func TestRuntimeDirLinuxFallbackWithoutXDG(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only fallback chain")
	}
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	got, err := config.RuntimeDir()
	if err != nil {
		t.Fatal(err)
	}
	runUser := fmt.Sprintf("/run/user/%d", os.Getuid())
	if _, statErr := os.Stat(runUser); statErr == nil {
		if want := filepath.Join(runUser, "agent-brain"); got != want {
			t.Fatalf("RuntimeDir() = %q, want %q", got, want)
		}
	} else if !strings.Contains(got, "agent-brain-") {
		t.Fatalf("RuntimeDir() = %q, want temp-dir fallback containing agent-brain-<uid>", got)
	}
}

func TestValidateSocketPath(t *testing.T) {
	t.Parallel()
	if err := config.ValidateSocketPath("/tmp/agent-brain/agent-brain.sock"); err != nil {
		t.Fatalf("short path rejected: %v", err)
	}
	long := "/" + strings.Repeat("x", 120) + "/agent-brain.sock"
	err := config.ValidateSocketPath(long)
	if err == nil {
		t.Fatal("101+ byte socket path accepted; sun_path would truncate it")
	}
	if !strings.Contains(err.Error(), "AGENT_BRAIN_RUNTIME_DIR") {
		t.Fatalf("error must name the escape hatch, got: %v", err)
	}
}
```

`internal/config/settings_test.go`:

```go
package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/config"
)

func TestLoadSettingsMissingFileYieldsDefaults(t *testing.T) {
	t.Parallel()
	got, err := config.LoadSettings(filepath.Join(t.TempDir(), "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(config.DefaultSettings(), got); diff != "" {
		t.Fatalf("defaults mismatch (-want +got):\n%s", diff)
	}
	if time.Duration(got.Sync.Ticker) != 5*time.Minute {
		t.Fatalf("default ticker = %v, want 5m", got.Sync.Ticker)
	}
}

func TestLoadSettingsParsesAndValidates(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, content string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	good, err := config.LoadSettings(write(t, "[sync]\nticker = \"1m\"\ndebounce = \"500ms\"\npoll = \"30s\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(good.Sync.Ticker) != time.Minute || time.Duration(good.Sync.Debounce) != 500*time.Millisecond {
		t.Fatalf("parsed settings wrong: %+v", good)
	}

	cases := []struct{ name, content string }{
		{"unknown key", "[sync]\ntikcer = \"1m\"\n"},
		{"unknown table", "[sink]\nticker = \"1m\"\n"},
		{"bad duration", "[sync]\nticker = \"soon\"\n"},
		{"ticker under floor", "[sync]\nticker = \"5s\"\n"},
		{"debounce under floor", "[sync]\ndebounce = \"1ms\"\n"},
		{"poll under floor", "[sync]\npoll = \"1s\"\n"},
		{"corrupt", "[sync\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := config.LoadSettings(write(t, tc.content)); err == nil {
				t.Fatalf("LoadSettings accepted %s; want error", tc.name)
			}
		})
	}
}
```

`internal/config/state_test.go`:

```go
package config_test

import (
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/config"
)

func TestStatePathHelpers(t *testing.T) {
	t.Parallel()
	p := config.Paths{ConfigDir: "/cfg", DataDir: "/data"}
	tests := []struct{ name, got, want string }{
		{"settings", p.SettingsFile(), filepath.Join("/cfg", "config.toml")},
		{"memories", p.MemoriesDir(), filepath.Join("/data", "memories")},
		{"local registry", p.LocalRegistryFile(), filepath.Join("/data", "registry-local.toml")},
		{"daemon log", p.DaemonLogFile(), filepath.Join("/data", "daemon.log")},
		{"conflict log", p.ConflictLogFile(), filepath.Join("/data", "conflicts.jsonl")},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Fatalf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/... 2>&1 | head -20`
Expected: compile errors (new identifiers).

- [ ] **Step 3: Implement** — `internal/config/runtime.go`:

```go
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const (
	// SocketName and LockName live inside RuntimeDir (ADR 09).
	SocketName = "agent-brain.sock"
	LockName   = "agent-brain.lock"

	// sunPathBudget stays under the smallest sun_path limit (104 bytes on
	// macOS, 108 on Linux) with headroom — a silently truncated socket
	// path binds somewhere unintended.
	sunPathBudget = 100
)

// RuntimeDir resolves where the daemon's socket and lock live (ADR 09):
// AGENT_BRAIN_RUNTIME_DIR when set (tests, unusual layouts); $TMPDIR on
// macOS (per-user, confined — never bare /tmp); $XDG_RUNTIME_DIR on
// Linux, then /run/user/<uid> when it exists, then a per-uid dir under
// os.TempDir() so session-less environments (containers, torn-down WSL2)
// degrade instead of bricking. The DAEMON creates the dir 0700 on every
// start (WSL2 tears /run/user/<uid> down across restarts); this function
// only resolves the path.
func RuntimeDir() (string, error) {
	if dir := os.Getenv("AGENT_BRAIN_RUNTIME_DIR"); dir != "" {
		return dir, nil
	}
	if runtime.GOOS == "darwin" {
		tmp := os.Getenv("TMPDIR")
		if tmp == "" {
			tmp = os.TempDir()
		}
		return filepath.Join(tmp, "agent-brain"), nil
	}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "agent-brain"), nil
	}
	runUser := fmt.Sprintf("/run/user/%d", os.Getuid())
	if info, err := os.Stat(runUser); err == nil && info.IsDir() {
		return filepath.Join(runUser, "agent-brain"), nil
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("agent-brain-%d", os.Getuid())), nil
}

// ValidateSocketPath rejects paths sun_path would truncate. The error
// names the escape hatch because the user's only fix is a shorter dir.
func ValidateSocketPath(socketPath string) error {
	if len(socketPath) > sunPathBudget {
		return fmt.Errorf("socket path %q is %d bytes; unix sockets cap at ~104 — set AGENT_BRAIN_RUNTIME_DIR to a shorter directory", socketPath, len(socketPath))
	}
	return nil
}
```

`internal/config/settings.go`:

```go
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// Duration is a time.Duration that unmarshals from TOML strings ("5m").
type Duration time.Duration

// UnmarshalText implements encoding.TextUnmarshaler (go-toml v2 honors it).
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// SyncSettings tunes the engine/watch cadence.
type SyncSettings struct {
	// Ticker is the idle fetch/integrate interval (spec §4: 5m default).
	Ticker Duration `toml:"ticker"`
	// Debounce is the watch trailing-quiet window (ADR 07: 2s default).
	Debounce Duration `toml:"debounce"`
	// Poll is the backstop rescan interval (ADR 07).
	Poll Duration `toml:"poll"`
}

// Settings is ~/.config/agent-brain/config.toml — user-edited, read-only
// to the program (ADR 17: init writes it once from a template in Phase 3;
// nothing ever rewrites it, so user comments survive).
type Settings struct {
	Sync SyncSettings `toml:"sync"`
}

// DefaultSettings returns the documented defaults.
func DefaultSettings() Settings {
	return Settings{Sync: SyncSettings{
		Ticker:   Duration(5 * time.Minute),
		Debounce: Duration(2 * time.Second),
		Poll:     Duration(45 * time.Second),
	}}
}

// LoadSettings reads path. A missing file is the default configuration; a
// present file must parse strictly — an unknown key is an error, because
// a typo'd setting silently ignored is a setting that silently doesn't
// apply. Floors keep pathological values from wedging the daemon.
func LoadSettings(path string) (Settings, error) {
	settings := DefaultSettings()
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the program-derived settings location, not untrusted input
	if errors.Is(err, fs.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("read settings: %w", err)
	}
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return Settings{}, fmt.Errorf("parse settings %s: %w", path, err)
	}
	if err := settings.validate(); err != nil {
		return Settings{}, fmt.Errorf("settings %s: %w", path, err)
	}
	return settings, nil
}

func (s Settings) validate() error {
	checks := []struct {
		name  string
		value time.Duration
		floor time.Duration
	}{
		{"sync.ticker", time.Duration(s.Sync.Ticker), 30 * time.Second},
		{"sync.debounce", time.Duration(s.Sync.Debounce), 100 * time.Millisecond},
		{"sync.poll", time.Duration(s.Sync.Poll), 5 * time.Second},
	}
	for _, c := range checks {
		if c.value < c.floor {
			return fmt.Errorf("%s = %s is below the %s floor", c.name, c.value, c.floor)
		}
	}
	return nil
}
```

`internal/config/state.go`:

```go
package config

import "path/filepath"

// SettingsFile is the user-edited configuration (ADR 17).
func (p Paths) SettingsFile() string { return filepath.Join(p.ConfigDir, "config.toml") }

// MemoriesDir is the hidden agent-brain-memories checkout (spec §3).
func (p Paths) MemoriesDir() string { return filepath.Join(p.DataDir, "memories") }

// LocalRegistryFile is the machine-local enrollment registry (spec §3).
func (p Paths) LocalRegistryFile() string { return filepath.Join(p.DataDir, "registry-local.toml") }

// DaemonLogFile is the daemon's structured log (spec §3).
func (p Paths) DaemonLogFile() string { return filepath.Join(p.DataDir, "daemon.log") }

// ConflictLogFile is where the merge driver records retain-both events
// (spec §4: "records the event for the dashboard conflicts view") when
// the daemon exports AGENT_BRAIN_CONFLICT_LOG. Phase 3's conflicts view
// reads it.
func (p Paths) ConflictLogFile() string { return filepath.Join(p.DataDir, "conflicts.jsonl") }
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/... -race -v 2>&1 | tail -20`
Expected: all PASS (existing Phase-1 path tests included, untouched).

- [ ] **Step 5: Lint, format, full suite, commit**

```bash
gofumpt -l -w . && golangci-lint run && go test ./... -race
git add internal/config/
git commit -m "feat(config): runtime dir resolution, strict settings loading, state-path helpers (ADRs 09, 17)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 6: Engine — mirror-in and per-project commits (spec §4 steps 1–2)

First engine task. `internal/engine` is the single writer to the memories checkout (spec §8) and depends ONLY on `gitx`, `repo`, `provider`, and stdlib — never `cli`, `daemon`, or `crypto` (encryption rides the git filters installed in the checkout; the engine never touches ciphertext).

Design decisions binding this and later engine tasks:
- **No Git interface.** Engine calls `gitx.Run`/`gitx.RunStatus` directly; tests use real system git against a `git init --bare` fake remote (repo testing convention). The fake-able seams are providers, clocks, and the service controller — not git.
- **White-box step tests.** Steps are unexported methods; step tests live in `package engine` (not `engine_test`). The exported `Sync` (Task 8) gets black-box coverage in the Task 13 e2e.
- **Step tests run without filters/keyset.** The engine is filter-agnostic — `git add` invokes the clean filter when configured, and step tests exercise engine logic in plaintext repos. Task 13 runs the full pipeline WITH filters and asserts ciphertext on the wire (spec §5); that split is deliberate, not a coverage gap.
- **Two commit points, both no-op when clean.** `commitProjects` commits per project folder — message exactly `memory: <host> <project> <timestamp>` (spec §4 step 2) — and `commitMeta` commits `.agent-brain/**` changes as `memory: <host> manifest <timestamp>`. The spec doesn't enumerate the manifest commit; it is the implied requirement of "rebase needs a clean tree" (§4 step 3) — the manifest file spans projects, so it gets its own deterministic commit instead of riding an arbitrary project's.
- **One timestamp per cycle** (`now()` read once, RFC 3339 UTC, matching the v1 message shape) — deterministic tests, coherent history.
- **Symlinks and irregular files in provider dirs are never followed.** Copying through a planted symlink would encrypt and commit an arbitrary reachable file — an exfiltration vector. `Lstat`-gate to regular files; count skips.

**Files:**
- Create: `internal/engine/engine.go`, `internal/engine/mirror_in.go`, `internal/engine/commit.go`
- Test: `internal/engine/helpers_test.go`, `internal/engine/mirror_in_test.go`, `internal/engine/commit_test.go`

**Interfaces:**
- Consumes: `gitx.Run`/`RunStatus`; `repo.Layout`, `repo.Unit`, `repo.Manifest`/`LoadManifest`/`HashFile`/`ManifestEntry`, `repo.SanitizeHostname`; `provider.Registry`/`Classify`/`ClassIgnore`; `providertest.Fake`.
- Produces (Tasks 7, 8, 11, 13 rely on these):
  - `type Engine struct` (unexported fields) · `func New(checkout, host string, registry *provider.Registry, now func() time.Time) (*Engine, error)`
  - `type Report struct { Commits []string; MirrorIn, MirrorOut MirrorStats; Degraded []string; Pushed, PushQueued bool }` · `type MirrorStats struct { Copied, Deleted, Skipped int }`
  - `type localSnapshot map[string]repo.ManifestEntry` (repo-relative path → local state at cycle start; mirror-out's skip gate)
  - unexported: `(e *Engine) mirrorIn(ctx, units, manifest) (MirrorStats, localSnapshot, error)` · `(e *Engine) commitProjects(ctx, stamp) ([]string, error)` · `(e *Engine) commitMeta(ctx, stamp) (string, error)` · `(e *Engine) stamp() string`

- [ ] **Step 1: Write the test helpers** — `internal/engine/helpers_test.go`:

```go
package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/providertest"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// fixedNow keeps commit messages deterministic across a test.
var fixedNow = func() time.Time { return time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC) }

const fixedStamp = "2026-07-08T12:00:00Z"

func testRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	fake := providertest.New("claude", provider.ScopePerProject, []provider.Pattern{
		{Glob: "MEMORY.md", Class: provider.ClassDerivedIndex},
		{Glob: "memories/**", Class: provider.ClassFact},
		{Glob: "*.tmp", Class: provider.ClassIgnore},
	})
	registry, err := provider.NewRegistry(fake)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func mustGit(t *testing.T, dir string, args ...string) gitx.Result {
	t.Helper()
	res, err := gitx.Run(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v\nstderr: %s", args, err, res.Stderr)
	}
	return res
}

// newTestCheckout builds the two-repo shape every engine test needs: a
// bare "remote" and a cloned checkout seeded the way Phase-3 init will
// seed it (attributes file committed on main, pushed upstream).
func newTestCheckout(t *testing.T) (checkout, bare string) {
	t.Helper()
	root := t.TempDir()
	bare = filepath.Join(root, "remote.git")
	checkout = filepath.Join(root, "memories")
	mustGit(t, root, "init", "--bare", "-b", "main", bare)
	mustGit(t, root, "clone", bare, checkout)
	mustGit(t, checkout, "config", "user.name", "engine-test")
	mustGit(t, checkout, "config", "user.email", "engine-test@example.invalid")
	if err := repo.WriteAttributes(repo.NewLayout(checkout), testRegistry(t)); err != nil {
		t.Fatal(err)
	}
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "-m", "init: repo skeleton")
	mustGit(t, checkout, "push", "-u", "origin", "main")
	return checkout, bare
}

func newTestEngine(t *testing.T, checkout string) *Engine {
	t.Helper()
	engine, err := New(checkout, "host-a", testRegistry(t), fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

// unit enrolls a provider dir under t.TempDir and returns it.
func unit(t *testing.T, folder string) repo.Unit {
	t.Helper()
	localDir := filepath.Join(t.TempDir(), "project", ".claude", "memory")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return repo.Unit{Provider: "claude", ProjectID: "id-" + folder, Folder: folder, LocalDir: localDir}
}

func writeLocal(t *testing.T, u repo.Unit, rel, content string) {
	t.Helper()
	full := filepath.Join(u.LocalDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Write the failing tests** — `internal/engine/mirror_in_test.go`:

```go
package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func TestMirrorInCopiesChangedFilesAndSkipsIgnored(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	writeLocal(t, u, "memories/go-style.md", "# fact\n")
	writeLocal(t, u, "scratch.tmp", "never syncs\n")

	manifest := repo.NewManifest()
	stats, snapshot, err := engine.mirrorIn(context.Background(), []repo.Unit{u}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 1 {
		t.Fatalf("Copied = %d, want 1", stats.Copied)
	}
	copied := filepath.Join(checkout, "alpha", "claude", "memories", "go-style.md")
	data, err := os.ReadFile(copied)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "# fact\n" {
		t.Fatalf("checkout content = %q", data)
	}
	if _, err := os.Stat(filepath.Join(checkout, "alpha", "claude", "scratch.tmp")); !os.IsNotExist(err) {
		t.Fatal("ClassIgnore file reached the checkout")
	}
	if !manifest.Has("alpha/claude/memories/go-style.md") {
		t.Fatal("manifest missing the synced path")
	}
	if _, ok := snapshot["alpha/claude/memories/go-style.md"]; !ok {
		t.Fatal("snapshot missing the synced path")
	}
}

func TestMirrorInSecondRunIsNoop(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	writeLocal(t, u, "memories/a.md", "content\n")

	manifest := repo.NewManifest()
	if _, _, err := engine.mirrorIn(context.Background(), []repo.Unit{u}, manifest); err != nil {
		t.Fatal(err)
	}
	stats, _, err := engine.mirrorIn(context.Background(), []repo.Unit{u}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 0 || stats.Deleted != 0 {
		t.Fatalf("second run stats = %+v, want zero copies/deletes", stats)
	}
}

func TestMirrorInDeletesViaManifestOnly(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	writeLocal(t, u, "memories/gone.md", "will be deleted\n")

	manifest := repo.NewManifest()
	ctx := context.Background()
	if _, _, err := engine.mirrorIn(ctx, []repo.Unit{u}, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.commitProjects(ctx, fixedStamp); err != nil {
		t.Fatal(err)
	}

	// Case 1: in manifest + gone locally = deleted here → git rm.
	if err := os.Remove(filepath.Join(u.LocalDir, "memories", "gone.md")); err != nil {
		t.Fatal(err)
	}
	// Case 2: in checkout + NOT in manifest = new from remote → untouched.
	fromRemote := filepath.Join(checkout, "alpha", "claude", "memories", "remote-new.md")
	if err := os.WriteFile(fromRemote, []byte("landed via integrate\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stats, _, err := engine.mirrorIn(ctx, []repo.Unit{u}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deleted != 1 {
		t.Fatalf("Deleted = %d, want 1", stats.Deleted)
	}
	if _, err := os.Stat(filepath.Join(checkout, "alpha", "claude", "memories", "gone.md")); !os.IsNotExist(err) {
		t.Fatal("deleted-here file still in checkout")
	}
	if manifest.Has("alpha/claude/memories/gone.md") {
		t.Fatal("manifest still lists the deleted path")
	}
	if _, err := os.Stat(fromRemote); err != nil {
		t.Fatal("new-from-remote file was wrongly removed:", err)
	}
}

func TestMirrorInRefusesSymlinks(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")

	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("keyset material\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(u.LocalDir, "memories"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(u.LocalDir, "memories", "planted.md")); err != nil {
		t.Fatal(err)
	}

	manifest := repo.NewManifest()
	stats, _, err := engine.mirrorIn(context.Background(), []repo.Unit{u}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 0 || stats.Skipped != 1 {
		t.Fatalf("stats = %+v, want 0 copied / 1 skipped", stats)
	}
	if _, err := os.Stat(filepath.Join(checkout, "alpha", "claude", "memories", "planted.md")); !os.IsNotExist(err) {
		t.Fatal("symlink target content reached the checkout — exfiltration path")
	}
}

func TestMirrorInUnknownProviderIsError(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	bad := repo.Unit{Provider: "gemini", ProjectID: "x", Folder: "alpha", LocalDir: t.TempDir()}
	if _, _, err := engine.mirrorIn(context.Background(), []repo.Unit{bad}, repo.NewManifest()); err == nil {
		t.Fatal("unenrollable provider silently skipped; want loud error")
	}
}
```

`internal/engine/commit_test.go`:

```go
package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func TestCommitProjectsOneCommitPerProject(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	ctx := context.Background()

	alpha, beta := unit(t, "alpha"), unit(t, "beta")
	writeLocal(t, alpha, "memories/a.md", "A\n")
	writeLocal(t, beta, "memories/b.md", "B\n")
	manifest := repo.NewManifest()
	if _, _, err := engine.mirrorIn(ctx, []repo.Unit{alpha, beta}, manifest); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Save(engine.layout.ManifestFile(engine.host)); err != nil {
		t.Fatal(err)
	}

	subjects, err := engine.commitProjects(ctx, fixedStamp)
	if err != nil {
		t.Fatal(err)
	}
	wantSubjects := []string{
		"memory: host-a alpha " + fixedStamp,
		"memory: host-a beta " + fixedStamp,
	}
	if len(subjects) != 2 || subjects[0] != wantSubjects[0] || subjects[1] != wantSubjects[1] {
		t.Fatalf("subjects = %v, want %v", subjects, wantSubjects)
	}

	metaSubject, err := engine.commitMeta(ctx, fixedStamp)
	if err != nil {
		t.Fatal(err)
	}
	if want := "memory: host-a manifest " + fixedStamp; metaSubject != want {
		t.Fatalf("meta subject = %q, want %q", metaSubject, want)
	}

	status := mustGit(t, checkout, "status", "--porcelain")
	if strings.TrimSpace(status.Stdout) != "" {
		t.Fatalf("tree dirty after commits:\n%s", status.Stdout)
	}
	log := mustGit(t, checkout, "log", "--format=%s", "-n", "3")
	got := strings.Split(strings.TrimSpace(log.Stdout), "\n")
	want := []string{
		"memory: host-a manifest " + fixedStamp,
		"memory: host-a beta " + fixedStamp,
		"memory: host-a alpha " + fixedStamp,
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("log[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCommitsAreNoopsWhenClean(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	ctx := context.Background()

	subjects, err := engine.commitProjects(ctx, fixedStamp)
	if err != nil {
		t.Fatal(err)
	}
	metaSubject, err := engine.commitMeta(ctx, fixedStamp)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 0 || metaSubject != "" {
		t.Fatalf("clean tree produced commits: %v %q", subjects, metaSubject)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/engine/ 2>&1 | head -20`
Expected: compile errors (package doesn't exist yet).

- [ ] **Step 4: Implement** — `internal/engine/engine.go`:

```go
// Package engine is the sync engine (spec §4): a single-goroutine
// pipeline — mirror-in, commit, integrate, reconcile, mirror-out, push —
// and the ONLY writer to the memories checkout. It depends on gitx,
// repo, and provider only (spec §8); encryption rides the checkout's
// git filters, so the engine never sees ciphertext.
package engine

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// Engine runs sync cycles against one memories checkout. Not safe for
// concurrent use: Sync (Task 8) guards against reentry and fails loudly.
type Engine struct {
	checkout string
	host     string
	layout   repo.Layout
	registry *provider.Registry
	now      func() time.Time
}

// New validates the wiring. host must already be sanitized — it becomes
// commit messages and the manifest filename.
func New(checkout, host string, registry *provider.Registry, now func() time.Time) (*Engine, error) {
	if !filepath.IsAbs(checkout) {
		return nil, fmt.Errorf("engine: checkout %q must be absolute", checkout)
	}
	if host == "" || repo.SanitizeHostname(host) != host {
		return nil, fmt.Errorf("engine: host %q must be a sanitized hostname", host)
	}
	if registry == nil {
		return nil, fmt.Errorf("engine: registry must not be nil")
	}
	if now == nil {
		return nil, fmt.Errorf("engine: clock must not be nil")
	}
	return &Engine{
		checkout: checkout,
		host:     host,
		layout:   repo.NewLayout(checkout),
		registry: registry,
		now:      now,
	}, nil
}

// stamp is the cycle timestamp: read once per cycle, RFC 3339 UTC —
// the v1-compatible `memory: <host> <project> <timestamp>` shape.
func (e *Engine) stamp() string { return e.now().UTC().Format(time.RFC3339) }

// MirrorStats counts one direction of mirroring.
type MirrorStats struct {
	Copied  int
	Deleted int
	// Skipped counts files deliberately not mirrored: irregular files
	// (symlinks — exfiltration guard) on the way in, locally-changed
	// files (converge next cycle) on the way out.
	Skipped int
}

// Report is one cycle's outcome; the daemon exposes the latest one.
type Report struct {
	Commits    []string
	MirrorIn   MirrorStats
	MirrorOut  MirrorStats
	Degraded   []string
	Pushed     bool
	PushQueued bool
}

// localSnapshot records each synced repo-relative path's local state at
// mirror-in time. Mirror-out compares against it: a file the user (or a
// live agent session) changed mid-cycle is skipped, not overwritten —
// the change converges through the next cycle.
type localSnapshot map[string]repo.ManifestEntry
```

`internal/engine/mirror_in.go`:

```go
package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/renameio/v2"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// mirrorIn implements spec §4 step 1 for every unit: compare provider
// dir ↔ checkout (manifest mtime+size fast path, hash confirm), copy
// local changes in, and resolve deletions through the manifest —
// in-manifest + gone-locally is deleted-here (git rm); in-checkout +
// absent-from-manifest is new-from-remote (left for mirror-out).
func (e *Engine) mirrorIn(ctx context.Context, units []repo.Unit, manifest *repo.Manifest) (MirrorStats, localSnapshot, error) {
	var stats MirrorStats
	snapshot := localSnapshot{}
	for _, u := range units {
		prov, ok := e.registry.Get(u.Provider)
		if !ok {
			return stats, nil, fmt.Errorf("mirror-in %s: provider %q not registered", u.Folder, u.Provider)
		}
		if err := e.mirrorInUnit(ctx, u, prov, manifest, &stats, snapshot); err != nil {
			return stats, nil, fmt.Errorf("mirror-in %s/%s: %w", u.Folder, u.Provider, err)
		}
	}
	return stats, snapshot, nil
}

func (e *Engine) mirrorInUnit(ctx context.Context, u repo.Unit, prov provider.Provider, manifest *repo.Manifest, stats *MirrorStats, snapshot localSnapshot) error {
	unitDir := e.layout.UnitDir(u.Folder, u.Provider)
	unitPrefix := path.Join(u.Folder, u.Provider) + "/"

	// Pass 1: local → checkout.
	localFiles := map[string]bool{}
	err := filepath.WalkDir(u.LocalDir, func(fullPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			// Symlinks and other irregular files never sync: copying
			// through a planted link would commit an arbitrary
			// reachable file into the (shared) repo.
			stats.Skipped++
			return nil
		}
		rel, err := filepath.Rel(u.LocalDir, fullPath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if provider.Classify(prov, rel) == provider.ClassIgnore {
			return nil
		}
		localFiles[rel] = true
		repoRel := unitPrefix + rel

		// Fast path (spec §4: "mtime+size, hash confirm"): a manifest
		// entry matching size+mtime means unchanged since last sync.
		if entry, ok := manifest.Get(repoRel); ok {
			if info, statErr := d.Info(); statErr == nil &&
				info.Size() == entry.Size && info.ModTime().UnixNano() == entry.MTimeUnixNano {
				snapshot[repoRel] = entry
				return nil
			}
		}
		entry, err := repo.HashFile(fullPath)
		if err != nil {
			return err
		}
		snapshot[repoRel] = entry
		if prev, ok := manifest.Get(repoRel); ok && prev.SHA256 == entry.SHA256 {
			// Touched but content-identical: refresh the ledger only.
			return manifest.Set(repoRel, entry)
		}
		content, err := os.ReadFile(fullPath) //nolint:gosec // G304: path came from walking the enrolled provider dir
		if err != nil {
			return err
		}
		dest := filepath.Join(unitDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := renameio.WriteFile(dest, content, 0o644); err != nil {
			return err
		}
		stats.Copied++
		return manifest.Set(repoRel, entry)
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		// A missing LocalDir is not an error: enrollment outlives an
		// agent dir that hasn't been recreated yet on this machine.
		return err
	}

	// Pass 2: deletions. Walk the checkout side of the unit.
	err = filepath.WalkDir(unitDir, func(fullPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		rel, err := filepath.Rel(unitDir, fullPath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		repoRel := unitPrefix + rel
		if localFiles[rel] || !manifest.Has(repoRel) {
			// Present locally, or never synced by this host
			// (new-from-remote): not ours to delete.
			return nil
		}
		if _, err := gitx.Run(ctx, e.checkout, "rm", "--quiet", "--ignore-unmatch", "--", repoRel); err != nil {
			return err
		}
		manifest.Delete(repoRel)
		stats.Deleted++
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	// Pass 3: ledger hygiene — entries whose file is gone from BOTH
	// sides (deleted remotely while also deleted here) would otherwise
	// linger forever ("reconcile manifest against reality", spec §4).
	for repoRel := range manifest.Files {
		if !strings.HasPrefix(repoRel, unitPrefix) {
			continue
		}
		rel := strings.TrimPrefix(repoRel, unitPrefix)
		if localFiles[rel] {
			continue
		}
		if _, err := os.Lstat(filepath.Join(unitDir, filepath.FromSlash(rel))); errors.Is(err, fs.ErrNotExist) {
			manifest.Delete(repoRel)
		}
	}
	return nil
}
```

`internal/engine/commit.go`:

```go
package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// commitProjects implements spec §4 step 2: one commit per project
// folder with changes, message exactly `memory: <host> <project>
// <timestamp>`; a clean tree is a no-op. Returns the commit subjects.
func (e *Engine) commitProjects(ctx context.Context, stamp string) ([]string, error) {
	folders, err := e.changedTopLevels(ctx)
	if err != nil {
		return nil, err
	}
	var subjects []string
	for _, folder := range folders {
		if folder == repo.MetaDirName {
			continue // .agent-brain commits via commitMeta
		}
		subject := fmt.Sprintf("memory: %s %s %s", e.host, folder, stamp)
		created, err := e.commitPaths(ctx, subject, folder)
		if err != nil {
			return subjects, err
		}
		if created {
			subjects = append(subjects, subject)
		}
	}
	return subjects, nil
}

// commitMeta commits .agent-brain/** (manifest and registry deltas)
// under its own deterministic subject — the manifest spans projects, so
// it never rides an arbitrary project's commit. Returns "" when clean.
func (e *Engine) commitMeta(ctx context.Context, stamp string) (string, error) {
	subject := fmt.Sprintf("memory: %s manifest %s", e.host, stamp)
	created, err := e.commitPaths(ctx, subject, repo.MetaDirName)
	if err != nil || !created {
		return "", err
	}
	return subject, nil
}

// changedTopLevels parses `git status --porcelain -z` into the sorted
// set of top-level path segments with any change (worktree or index).
func (e *Engine) changedTopLevels(ctx context.Context) ([]string, error) {
	res, err := gitx.Run(ctx, e.checkout, "status", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	// -z: NUL-terminated `XY path` records; rename records carry a
	// second NUL-terminated origin path immediately after.
	fields := strings.Split(res.Stdout, "\x00")
	for i := 0; i < len(fields); i++ {
		record := fields[i]
		if len(record) < 4 {
			continue
		}
		statusXY, changedPath := record[:2], record[3:]
		set[topSegment(changedPath)] = true
		if statusXY[0] == 'R' || statusXY[0] == 'C' {
			i++ // consume the rename/copy origin path record
			if i < len(fields) && fields[i] != "" {
				set[topSegment(fields[i])] = true
			}
		}
	}
	folders := make([]string, 0, len(set))
	for folder := range set {
		folders = append(folders, folder)
	}
	sort.Strings(folders)
	return folders, nil
}

func topSegment(p string) string {
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return p
}

// commitPaths stages pathspec and commits when anything is staged.
func (e *Engine) commitPaths(ctx context.Context, subject, pathspec string) (bool, error) {
	if _, err := gitx.Run(ctx, e.checkout, "add", "-A", "--", pathspec); err != nil {
		return false, err
	}
	staged, err := gitx.RunStatus(ctx, e.checkout, "diff", "--cached", "--quiet")
	if err != nil {
		return false, err
	}
	if staged.ExitCode == 0 {
		return false, nil // nothing staged
	}
	if _, err := gitx.Run(ctx, e.checkout, "commit", "--quiet", "-m", subject); err != nil {
		return false, err
	}
	return true, nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/engine/ -race -v 2>&1 | tail -20`
Expected: all PASS.

- [ ] **Step 6: Lint, format, full suite, commit**

```bash
gofumpt -l -w . && golangci-lint run && go test ./... -race
git add internal/engine/
git commit -m "feat(engine): mirror-in with manifest deletion semantics, per-project commits (spec §4 steps 1-2)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 7: Engine — integrate and push (spec §4 steps 3 and 6)

Integrate is all-or-nothing: a rebase or merge either lands `origin/main` in full or aborts back to the pre-integrate state — there is no partial integration. "Degraded" therefore marks which projects OWNED the conflicting paths (dashboard banner + mirror-out withheld, spec §11); a conflict under `.agent-brain/` or an unattributable failure degrades everything, conservatively.

Contracts this task pins:
- **Offline is not an error.** A failed `git fetch` means the cycle runs local-only (commit, reconcile, mirror-out) and push queues — spec §11's git-native queue. Errors are reserved for infrastructure failures (spawn failures, an abort that itself fails and strands the repo).
- **Failure ladder is exactly spec §4 step 3:** rebase → capture conflicted paths → `rebase --abort` → `merge --no-edit` fallback → still failing → capture paths → `merge --abort` → degrade owners of the conflicted paths.
- **Push race loser re-integrates and retries bounded** (`pushRaceRetries = 3`), then queues for the next cycle (spec §4 step 6). Only a *rejection* (non-fast-forward) triggers the retry loop; network failures queue immediately.
- Branch and remote are constants: `defaultBranch = "main"`, `remoteName = "origin"` (init creates this shape in Phase 3; the e2e harness creates it in tests).

**Files:**
- Create: `internal/engine/integrate.go`, `internal/engine/push.go`
- Test: `internal/engine/integrate_test.go`, `internal/engine/push_test.go`

**Interfaces:**
- Consumes: Task 6's `Engine`, helpers; `gitx.Run`/`RunStatus`.
- Produces (Task 8 orchestrates these):
  - `type integrateOutcome struct { Offline, Integrated, DegradedAll bool; Degraded []string }`
  - `type pushOutcome struct { Pushed, Queued, DegradedAll bool; Degraded []string }`
  - unexported: `(e *Engine) integrate(ctx) (integrateOutcome, error)` · `(e *Engine) push(ctx) (pushOutcome, error)`

- [ ] **Step 1: Write the failing tests** — `internal/engine/integrate_test.go`:

```go
package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// secondClone clones the same bare remote as an independent "machine".
func secondClone(t *testing.T, bare string) string {
	t.Helper()
	other := filepath.Join(t.TempDir(), "memories-b")
	mustGit(t, t.TempDir(), "clone", bare, other)
	mustGit(t, other, "config", "user.name", "engine-test-b")
	mustGit(t, other, "config", "user.email", "engine-test-b@example.invalid")
	return other
}

func commitFileOn(t *testing.T, checkout, rel, content, message string) {
	t.Helper()
	full := filepath.Join(checkout, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, checkout, "add", "-A", "--", rel)
	mustGit(t, checkout, "commit", "--quiet", "-m", message)
}

func TestIntegrateFastForwardsWhenBehind(t *testing.T) {
	t.Parallel()
	checkout, bare := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	other := secondClone(t, bare)
	commitFileOn(t, other, "alpha/claude/memories/from-b.md", "B's fact\n", "memory: host-b alpha ts")
	mustGit(t, other, "push", "origin", "main")

	outcome, err := engine.integrate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Integrated || outcome.Offline || len(outcome.Degraded) != 0 {
		t.Fatalf("outcome = %+v, want clean integration", outcome)
	}
	if _, err := os.Stat(filepath.Join(checkout, "alpha", "claude", "memories", "from-b.md")); err != nil {
		t.Fatal("remote file did not land:", err)
	}
}

func TestIntegrateRebasesLocalCommitsLinearly(t *testing.T) {
	t.Parallel()
	checkout, bare := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	commitFileOn(t, checkout, "alpha/claude/memories/local.md", "local fact\n", "memory: host-a alpha ts")
	other := secondClone(t, bare)
	commitFileOn(t, other, "beta/claude/memories/remote.md", "remote fact\n", "memory: host-b beta ts")
	mustGit(t, other, "push", "origin", "main")

	outcome, err := engine.integrate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Integrated {
		t.Fatalf("outcome = %+v, want Integrated", outcome)
	}
	for _, rel := range []string{"alpha/claude/memories/local.md", "beta/claude/memories/remote.md"} {
		if _, err := os.Stat(filepath.Join(checkout, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("%s missing after integrate: %v", rel, err)
		}
	}
	// Rebase, not merge: exactly the one local commit sits atop origin/main.
	ahead := mustGit(t, checkout, "rev-list", "--count", "origin/main..HEAD")
	if got := strings.TrimSpace(ahead.Stdout); got != "1" {
		t.Fatalf("commits ahead = %s, want 1 (linear rebase)", got)
	}
}

func TestIntegrateOfflineIsNotAnError(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	mustGit(t, checkout, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "vanished.git"))

	outcome, err := engine.integrate(context.Background())
	if err != nil {
		t.Fatal("offline fetch must not error:", err)
	}
	if !outcome.Offline || outcome.Integrated {
		t.Fatalf("outcome = %+v, want Offline", outcome)
	}
}

// TestIntegrateDriverFailureDegradesProject forces the exact spec §4
// scenario: the merge driver unexpectedly fails (driver = `false`), the
// rebase aborts clean, the merge-commit fallback also fails, and the
// owning project is degraded while the checkout returns to its
// pre-integrate state.
func TestIntegrateDriverFailureDegradesProject(t *testing.T) {
	t.Parallel()
	checkout, bare := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	mustGit(t, checkout, "config", "merge.agentbrain.name", "failing driver")
	mustGit(t, checkout, "config", "merge.agentbrain.driver", "false")
	mustGit(t, checkout, "config", "merge.agentbrain-lww.name", "failing driver")
	mustGit(t, checkout, "config", "merge.agentbrain-lww.driver", "false")

	conflictPath := "alpha/claude/memories/clash.md"
	commitFileOn(t, checkout, conflictPath, "ours\n", "memory: host-a alpha ts")
	other := secondClone(t, bare)
	commitFileOn(t, other, conflictPath, "theirs\n", "memory: host-b alpha ts")
	mustGit(t, other, "push", "origin", "main")

	outcome, err := engine.integrate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Integrated || outcome.DegradedAll {
		t.Fatalf("outcome = %+v, want project-scoped degradation", outcome)
	}
	if len(outcome.Degraded) != 1 || outcome.Degraded[0] != "alpha" {
		t.Fatalf("Degraded = %v, want [alpha]", outcome.Degraded)
	}
	// Aborts restored the local state: no rebase/merge in progress, ours intact.
	gitDir := filepath.Join(checkout, ".git")
	for _, marker := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD"} {
		if _, err := os.Lstat(filepath.Join(gitDir, marker)); !os.IsNotExist(err) {
			t.Fatalf("stranded %s after aborts", marker)
		}
	}
	data, err := os.ReadFile(filepath.Join(checkout, filepath.FromSlash(conflictPath)))
	if err != nil || string(data) != "ours\n" {
		t.Fatalf("local content = %q, %v; want pre-integrate state", data, err)
	}
}

func TestIntegrateMetaConflictDegradesAll(t *testing.T) {
	t.Parallel()
	checkout, bare := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	// .agent-brain/** carries `-merge` (Task 2 attributes): an add/add
	// there cannot content-merge, forcing the degrade-all path.
	notePath := repo.MetaDirName + "/note.txt"
	commitFileOn(t, checkout, notePath, "ours\n", "meta ours")
	other := secondClone(t, bare)
	commitFileOn(t, other, notePath, "theirs\n", "meta theirs")
	mustGit(t, other, "push", "origin", "main")

	outcome, err := engine.integrate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Integrated || !outcome.DegradedAll {
		t.Fatalf("outcome = %+v, want DegradedAll", outcome)
	}
}
```

`internal/engine/push_test.go`:

```go
package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestPushNothingToPush(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	outcome, err := engine.push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Pushed || outcome.Queued {
		t.Fatalf("outcome = %+v, want no-op", outcome)
	}
}

func TestPushDeliversLocalCommits(t *testing.T) {
	t.Parallel()
	checkout, bare := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	commitFileOn(t, checkout, "alpha/claude/memories/a.md", "fact\n", "memory: host-a alpha ts")

	outcome, err := engine.push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Pushed || outcome.Queued {
		t.Fatalf("outcome = %+v, want Pushed", outcome)
	}
	remoteLog := mustGit(t, bare, "log", "--format=%s", "-n", "1", "main")
	if got := strings.TrimSpace(remoteLog.Stdout); got != "memory: host-a alpha ts" {
		t.Fatalf("remote tip = %q", got)
	}
}

func TestPushOfflineQueues(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	commitFileOn(t, checkout, "alpha/claude/memories/a.md", "fact\n", "memory: host-a alpha ts")
	mustGit(t, checkout, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "vanished.git"))

	outcome, err := engine.push(context.Background())
	if err != nil {
		t.Fatal("offline push must queue, not error:", err)
	}
	if outcome.Pushed || !outcome.Queued {
		t.Fatalf("outcome = %+v, want Queued", outcome)
	}
}

// TestPushRaceLoserReintegratesAndWins is spec §4 step 6 end to end:
// the other machine pushes first, our push is rejected non-fast-forward,
// we re-integrate (rebase) and the retry lands both histories.
func TestPushRaceLoserReintegratesAndWins(t *testing.T) {
	t.Parallel()
	checkout, bare := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	commitFileOn(t, checkout, "alpha/claude/memories/ours.md", "ours\n", "memory: host-a alpha ts")
	other := secondClone(t, bare)
	commitFileOn(t, other, "beta/claude/memories/theirs.md", "theirs\n", "memory: host-b beta ts")
	mustGit(t, other, "push", "origin", "main")

	outcome, err := engine.push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Pushed {
		t.Fatalf("outcome = %+v, want Pushed after race retry", outcome)
	}
	remoteFiles := mustGit(t, bare, "ls-tree", "-r", "--name-only", "main")
	for _, rel := range []string{"alpha/claude/memories/ours.md", "beta/claude/memories/theirs.md"} {
		if !strings.Contains(remoteFiles.Stdout, rel) {
			t.Fatalf("remote missing %s after race resolution:\n%s", rel, remoteFiles.Stdout)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/engine/ 2>&1 | head -20`
Expected: compile errors (`integrate`/`push` undefined).

- [ ] **Step 3: Implement** — `internal/engine/integrate.go`:

```go
package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

const (
	remoteName    = "origin"
	defaultBranch = "main"
	upstreamRef   = remoteName + "/" + defaultBranch
)

// integrateOutcome reports spec §4 step 3. Integrate is all-or-nothing:
// Integrated means HEAD now contains origin/main; otherwise the checkout
// is back at its pre-integrate state and Degraded names the project
// folders whose paths conflicted (mirror-out withheld for them, §11).
type integrateOutcome struct {
	Offline     bool
	Integrated  bool
	DegradedAll bool
	Degraded    []string
}

// integrate fetches and rebases onto origin/main, falling back per the
// spec §4 ladder: rebase → abort → merge commit → abort → degraded.
// Offline (fetch failure) is a normal outcome, not an error; errors are
// infrastructure failures only.
func (e *Engine) integrate(ctx context.Context) (integrateOutcome, error) {
	if fetch, err := gitx.RunStatus(ctx, e.checkout, "fetch", "--quiet", remoteName); err != nil {
		return integrateOutcome{}, fmt.Errorf("integrate: fetch: %w", err)
	} else if fetch.ExitCode != 0 {
		return integrateOutcome{Offline: true}, nil
	}

	behind, err := gitx.Run(ctx, e.checkout, "rev-list", "--count", "HEAD.."+upstreamRef)
	if err != nil {
		return integrateOutcome{}, fmt.Errorf("integrate: behind count: %w", err)
	}
	if strings.TrimSpace(behind.Stdout) == "0" {
		return integrateOutcome{Integrated: true}, nil
	}

	rebase, err := gitx.RunStatus(ctx, e.checkout, "rebase", upstreamRef)
	if err != nil {
		return integrateOutcome{}, fmt.Errorf("integrate: rebase: %w", err)
	}
	if rebase.ExitCode == 0 {
		return integrateOutcome{Integrated: true}, nil
	}

	// Rebase failed (spec: "unexpected driver failure"). Capture the
	// conflicted paths for attribution, abort clean, try a merge commit.
	rebaseConflicts, _ := e.conflictedPaths(ctx)
	if _, err := gitx.RunStatus(ctx, e.checkout, "rebase", "--abort"); err != nil {
		return integrateOutcome{}, fmt.Errorf("integrate: rebase --abort: %w", err)
	}

	merge, err := gitx.RunStatus(ctx, e.checkout, "merge", "--no-edit", upstreamRef)
	if err != nil {
		return integrateOutcome{}, fmt.Errorf("integrate: merge fallback: %w", err)
	}
	if merge.ExitCode == 0 {
		return integrateOutcome{Integrated: true}, nil
	}

	mergeConflicts, _ := e.conflictedPaths(ctx)
	if _, err := gitx.RunStatus(ctx, e.checkout, "merge", "--abort"); err != nil {
		return integrateOutcome{}, fmt.Errorf("integrate: merge --abort: %w", err)
	}

	conflicts := mergeConflicts
	if len(conflicts) == 0 {
		conflicts = rebaseConflicts
	}
	return degradeByPaths(conflicts), nil
}

// conflictedPaths lists unmerged paths while a rebase/merge conflict is
// live. Best-effort: attribution failing must not mask the abort.
func (e *Engine) conflictedPaths(ctx context.Context) ([]string, error) {
	res, err := gitx.RunStatus(ctx, e.checkout, "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil || res.ExitCode != 0 {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(res.Stdout, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// degradeByPaths maps conflicted paths to project folders. A conflict
// under .agent-brain/ (shared metadata) or an empty attribution is not
// project-scoped — degrade everything, conservatively.
func degradeByPaths(paths []string) integrateOutcome {
	if len(paths) == 0 {
		return integrateOutcome{DegradedAll: true}
	}
	set := map[string]bool{}
	for _, p := range paths {
		folder := topSegment(p)
		if folder == repo.MetaDirName {
			return integrateOutcome{DegradedAll: true}
		}
		set[folder] = true
	}
	folders := make([]string, 0, len(set))
	for folder := range set {
		folders = append(folders, folder)
	}
	sort.Strings(folders)
	return integrateOutcome{Degraded: folders}
}
```

`internal/engine/push.go`:

```go
package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
)

// pushRaceRetries bounds the reject → re-integrate → retry loop
// (spec §4 step 6); after that the commits wait for the next cycle.
const pushRaceRetries = 3

// pushOutcome reports spec §4 step 6. Queued means unpushed commits
// remain — the git-native queue; never an error state.
type pushOutcome struct {
	Pushed      bool
	Queued      bool
	DegradedAll bool
	Degraded    []string
}

// push delivers local commits. Only a non-fast-forward REJECTION enters
// the race-retry loop; network failures queue immediately for the
// backoff/ticker to retry (spec §11).
func (e *Engine) push(ctx context.Context) (pushOutcome, error) {
	for attempt := 0; ; attempt++ {
		ahead, err := gitx.Run(ctx, e.checkout, "rev-list", "--count", upstreamRef+"..HEAD")
		if err != nil {
			return pushOutcome{}, fmt.Errorf("push: ahead count: %w", err)
		}
		if strings.TrimSpace(ahead.Stdout) == "0" {
			return pushOutcome{}, nil // nothing to push
		}

		res, err := gitx.RunStatus(ctx, e.checkout, "push", "--quiet", remoteName, defaultBranch)
		if err != nil {
			return pushOutcome{}, fmt.Errorf("push: %w", err)
		}
		if res.ExitCode == 0 {
			return pushOutcome{Pushed: true}, nil
		}
		if !isRejection(res.Stderr) || attempt >= pushRaceRetries {
			return pushOutcome{Queued: true}, nil
		}

		// Race lost: someone pushed since our fetch. Re-integrate and retry.
		integ, err := e.integrate(ctx)
		if err != nil {
			return pushOutcome{Queued: true}, err
		}
		if !integ.Integrated {
			return pushOutcome{
				Queued:      true,
				DegradedAll: integ.DegradedAll,
				Degraded:    integ.Degraded,
			}, nil
		}
	}
}

// isRejection detects a non-fast-forward push rejection (as opposed to
// a transport failure). Git's phrasing is stable across versions:
// "[rejected]" plus "fetch first" / "non-fast-forward" hints.
func isRejection(stderr string) bool {
	return strings.Contains(stderr, "[rejected]") ||
		strings.Contains(stderr, "non-fast-forward") ||
		strings.Contains(stderr, "fetch first")
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/engine/ -race -v 2>&1 | tail -30`
Expected: all PASS (Task 6 tests included).

- [ ] **Step 5: Lint, format, full suite, commit**

```bash
gofumpt -l -w . && golangci-lint run && go test ./... -race
git add internal/engine/
git commit -m "feat(engine): integrate with degrade ladder, push with bounded race retries (spec §4 steps 3, 6)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 8: Engine — reconcile, mirror-out, and the `Sync` orchestration (spec §4 steps 4–5 + cycle contract)

The exported surface of the engine. `Sync` runs the full §4 pipeline with three cross-cutting contracts:

- **Self-healing preamble every cycle, not just daemon start.** `recoverState` aborts any stranded rebase/merge before work begins. Spec §4 says "crash safety on start"; running it per-cycle is strictly stronger — a mid-cycle SIGKILL (WSL2 teardown, OOM) heals on the next tick instead of the next daemon restart. A dirty tree from a crashed mirror-in needs no special handling: mirror-in is idempotent and `commitProjects` sweeps strays.
- **Non-reentrant, loudly.** `Sync` guards with an `atomic.Bool`; a second concurrent call returns `ErrBusy`. The daemon's single goroutine means this should never fire — if it does, it's a programming error and must not be silently serialized.
- **Mirror-out never destroys local work.** Overwrites are gated on the mirror-in snapshot (changed-since-snapshot ⇒ skip, converge next cycle); remote deletions are gated on BOTH the manifest (proves prior sync here, spec §4 step 5) and an unchanged-since-snapshot check (a user edit racing a remote deletion survives, and mirrors back in next cycle).

Manifest note: the in-memory manifest stays authoritative across integrate — `manifests/<host>.json` is written only by this host, so a rebase can never change it under us (that's the "one manifest file per host" design earning its keep).

Push is attempted only when integrate succeeded; on degraded or offline cycles the commits queue (`PushQueued`) without burning a known-doomed push + retry loop.

**Files:**
- Create: `internal/engine/recover.go`, `internal/engine/reconcile.go`, `internal/engine/mirror_out.go`, `internal/engine/sync.go`
- Modify: `internal/engine/engine.go` (add `busy atomic.Bool` field to `Engine`, `ErrBusy`)
- Test: `internal/engine/recover_test.go`, `internal/engine/reconcile_test.go`, `internal/engine/mirror_out_test.go`, `internal/engine/sync_test.go`

**Interfaces:**
- Consumes: Tasks 6–7 internals; `providertest.Fake.ReconcileCalls`.
- Produces (daemon Task 11, e2e Task 13 rely on these — the engine's ONLY exported behavior surface):
  - `var ErrBusy error`
  - `func (e *Engine) Sync(ctx context.Context, units []repo.Unit) (Report, error)`
  - unexported: `recoverState(ctx)` · `reconcile(ctx, units, skip map[string]bool)` · `mirrorOut(ctx, units, manifest, snapshot, skip map[string]bool) (MirrorStats, error)`

- [ ] **Step 1: Write the failing tests** — `internal/engine/recover_test.go`:

```go
package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestRecoverAbortsStrandedRebase manufactures the crash the spec's
// recovery contract exists for: a rebase that stopped mid-conflict
// (driver failure) and was never aborted — daemon killed, WSL2 torn
// down. recoverState must return the checkout to a clean state.
func TestRecoverAbortsStrandedRebase(t *testing.T) {
	t.Parallel()
	checkout, bare := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	mustGit(t, checkout, "config", "merge.agentbrain.driver", "false")
	mustGit(t, checkout, "config", "merge.agentbrain-lww.driver", "false")

	conflictPath := "alpha/claude/memories/clash.md"
	commitFileOn(t, checkout, conflictPath, "ours\n", "ours")
	other := secondClone(t, bare)
	commitFileOn(t, other, conflictPath, "theirs\n", "theirs")
	mustGit(t, other, "push", "origin", "main")

	mustGit(t, checkout, "fetch", "origin")
	// Raw rebase, deliberately NOT aborted — the stranded state.
	if res, err := gitx.RunStatus(context.Background(), checkout, "rebase", "origin/main"); err != nil || res.ExitCode == 0 {
		t.Fatalf("expected rebase to stop on conflict, got exit %d err %v", res.ExitCode, err)
	}

	if err := engine.recoverState(context.Background()); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(checkout, ".git")
	for _, marker := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD"} {
		if _, err := os.Lstat(filepath.Join(gitDir, marker)); !os.IsNotExist(err) {
			t.Fatalf("%s still present after recovery", marker)
		}
	}
}

func TestRecoverIsNoopOnCleanCheckout(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	if err := engine.recoverState(context.Background()); err != nil {
		t.Fatal(err)
	}
}
```

Add `"github.com/Sawmonabo/agent-brain/internal/gitx"` to this file's imports.

`internal/engine/reconcile_test.go`:

```go
package engine

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func TestReconcileCallsEachUnitAndSkipsDegraded(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	alpha, beta := unit(t, "alpha"), unit(t, "beta")

	err := engine.reconcile(context.Background(), []repo.Unit{alpha, beta}, map[string]bool{"alpha": true})
	if err != nil {
		t.Fatal(err)
	}
	fakeProv, _ := engine.registry.Get("claude")
	fake := fakeProv.(interface{ ReconcileCalls() []string })
	want := []string{engine.layout.UnitDir("beta", "claude")}
	if diff := cmp.Diff(want, fake.ReconcileCalls()); diff != "" {
		t.Fatalf("reconcile calls (-want +got):\n%s", diff)
	}
}
```

`internal/engine/mirror_out_test.go`:

```go
package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// seedSyncedFile establishes "this host synced rel before": content in
// LocalDir and checkout, manifest entry, snapshot entry.
func seedSyncedFile(t *testing.T, engine *Engine, u repo.Unit, manifest *repo.Manifest, snapshot localSnapshot, rel, content string) {
	t.Helper()
	writeLocal(t, u, rel, content)
	dest := filepath.Join(engine.layout.UnitDir(u.Folder, u.Provider), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entry, err := repo.HashFile(filepath.Join(u.LocalDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	repoRel := u.Folder + "/" + u.Provider + "/" + rel
	if err := manifest.Set(repoRel, entry); err != nil {
		t.Fatal(err)
	}
	snapshot[repoRel] = entry
}

func TestMirrorOutAppliesRemoteAddsAndManifestGatedDeletions(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	manifest, snapshot := repo.NewManifest(), localSnapshot{}

	// Previously synced file that the remote has since deleted:
	// present locally + in manifest, absent from checkout.
	seedSyncedFile(t, engine, u, manifest, snapshot, "memories/deleted-remotely.md", "old\n")
	if err := os.Remove(filepath.Join(engine.layout.UnitDir("alpha", "claude"), "memories", "deleted-remotely.md")); err != nil {
		t.Fatal(err)
	}
	// New-from-remote file: in checkout, absent locally and from manifest.
	remoteNew := filepath.Join(engine.layout.UnitDir("alpha", "claude"), "memories", "remote-new.md")
	if err := os.MkdirAll(filepath.Dir(remoteNew), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remoteNew, []byte("from B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Never-synced local-absent checkout file in an UNMANIFESTED unit
	// dir on another machine's project must NOT be deleted locally —
	// covered by the manifest gate below.

	stats, err := engine.mirrorOut(context.Background(), []repo.Unit{u}, manifest, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 1 || stats.Deleted != 1 {
		t.Fatalf("stats = %+v, want 1 copied / 1 deleted", stats)
	}
	if _, err := os.Stat(filepath.Join(u.LocalDir, "memories", "deleted-remotely.md")); !os.IsNotExist(err) {
		t.Fatal("remote deletion not applied locally")
	}
	data, err := os.ReadFile(filepath.Join(u.LocalDir, "memories", "remote-new.md"))
	if err != nil || string(data) != "from B\n" {
		t.Fatalf("remote-new content = %q, %v", data, err)
	}
	if manifest.Has("alpha/claude/memories/deleted-remotely.md") {
		t.Fatal("manifest still lists deleted path")
	}
	if !manifest.Has("alpha/claude/memories/remote-new.md") {
		t.Fatal("manifest missing newly mirrored-out path")
	}
}

func TestMirrorOutNeverOverwritesMidCycleLocalEdits(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	manifest, snapshot := repo.NewManifest(), localSnapshot{}
	seedSyncedFile(t, engine, u, manifest, snapshot, "memories/racy.md", "synced\n")

	// Remote updated the checkout copy...
	dest := filepath.Join(engine.layout.UnitDir("alpha", "claude"), "memories", "racy.md")
	if err := os.WriteFile(dest, []byte("remote change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// ...while a live agent session ALSO wrote locally mid-cycle.
	writeLocal(t, u, "memories/racy.md", "local mid-cycle edit\n")

	stats, err := engine.mirrorOut(context.Background(), []repo.Unit{u}, manifest, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Skipped != 1 || stats.Copied != 0 {
		t.Fatalf("stats = %+v, want 1 skipped / 0 copied", stats)
	}
	data, err := os.ReadFile(filepath.Join(u.LocalDir, "memories", "racy.md"))
	if err != nil || string(data) != "local mid-cycle edit\n" {
		t.Fatalf("local edit destroyed: %q, %v", data, err)
	}
}

func TestMirrorOutDeletionSkippedWhenLocalChanged(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	manifest, snapshot := repo.NewManifest(), localSnapshot{}
	seedSyncedFile(t, engine, u, manifest, snapshot, "memories/edited.md", "synced\n")

	// Remote deleted it, but the user edited it locally mid-cycle.
	if err := os.Remove(filepath.Join(engine.layout.UnitDir("alpha", "claude"), "memories", "edited.md")); err != nil {
		t.Fatal(err)
	}
	writeLocal(t, u, "memories/edited.md", "user's new thoughts\n")

	stats, err := engine.mirrorOut(context.Background(), []repo.Unit{u}, manifest, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deleted != 0 || stats.Skipped != 1 {
		t.Fatalf("stats = %+v, want deletion skipped", stats)
	}
	if _, err := os.Stat(filepath.Join(u.LocalDir, "memories", "edited.md")); err != nil {
		t.Fatal("user's local edit was deleted:", err)
	}
}

func TestMirrorOutWithheldForDegradedProjects(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	manifest, snapshot := repo.NewManifest(), localSnapshot{}
	remoteNew := filepath.Join(engine.layout.UnitDir("alpha", "claude"), "memories", "remote-new.md")
	if err := os.MkdirAll(filepath.Dir(remoteNew), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remoteNew, []byte("from B\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stats, err := engine.mirrorOut(context.Background(), []repo.Unit{u}, manifest, snapshot, map[string]bool{"alpha": true})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 0 {
		t.Fatalf("stats = %+v; degraded project must not mirror out", stats)
	}
	if _, err := os.Stat(filepath.Join(u.LocalDir, "memories", "remote-new.md")); !os.IsNotExist(err) {
		t.Fatal("degraded project mirrored out anyway")
	}
}
```

`internal/engine/sync_test.go`:

```go
package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func TestSyncFullCycleLocalToRemote(t *testing.T) {
	t.Parallel()
	checkout, bare := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	writeLocal(t, u, "memories/fact.md", "a fact\n")

	report, err := engine.Sync(context.Background(), []repo.Unit{u})
	if err != nil {
		t.Fatal(err)
	}
	if report.MirrorIn.Copied != 1 || !report.Pushed || report.PushQueued {
		t.Fatalf("report = %+v, want 1 copied + pushed", report)
	}
	wantSubject := "memory: host-a alpha " + fixedStamp
	found := false
	for _, s := range report.Commits {
		if s == wantSubject {
			found = true
		}
	}
	if !found {
		t.Fatalf("Commits = %v, want to include %q", report.Commits, wantSubject)
	}
	remoteFiles := mustGit(t, bare, "ls-tree", "-r", "--name-only", "main")
	if !strings.Contains(remoteFiles.Stdout, "alpha/claude/memories/fact.md") {
		t.Fatalf("remote tree missing synced file:\n%s", remoteFiles.Stdout)
	}
	if !strings.Contains(remoteFiles.Stdout, ".agent-brain/manifests/host-a.json") {
		t.Fatalf("remote tree missing host manifest:\n%s", remoteFiles.Stdout)
	}
	// Second cycle with no changes is a true no-op.
	second, err := engine.Sync(context.Background(), []repo.Unit{u})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Commits) != 0 || second.Pushed || second.PushQueued {
		t.Fatalf("idle cycle produced work: %+v", second)
	}
}

func TestSyncTwoCheckoutsConverge(t *testing.T) {
	t.Parallel()
	checkoutA, bare := newTestCheckout(t)
	engineA := newTestEngine(t, checkoutA)
	unitA := unit(t, "alpha")
	writeLocal(t, unitA, "memories/from-a.md", "A's fact\n")
	if _, err := engineA.Sync(context.Background(), []repo.Unit{unitA}); err != nil {
		t.Fatal(err)
	}

	// "Machine B": its own clone, host identity, and provider dir.
	checkoutB := secondClone(t, bare)
	engineB, err := New(checkoutB, "host-b", testRegistry(t), fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	unitB := repo.Unit{Provider: "claude", ProjectID: "id-alpha", Folder: "alpha", LocalDir: filepath.Join(t.TempDir(), "memory")}
	if err := os.MkdirAll(unitB.LocalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reportB, err := engineB.Sync(context.Background(), []repo.Unit{unitB})
	if err != nil {
		t.Fatal(err)
	}
	if reportB.MirrorOut.Copied != 1 {
		t.Fatalf("reportB = %+v, want A's file mirrored out on B", reportB)
	}
	data, err := os.ReadFile(filepath.Join(unitB.LocalDir, "memories", "from-a.md"))
	if err != nil || string(data) != "A's fact\n" {
		t.Fatalf("B's provider dir = %q, %v", data, err)
	}
}

func TestSyncRefusesReentry(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	engine.busy.Store(true)
	if _, err := engine.Sync(context.Background(), nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/engine/ 2>&1 | head -20`
Expected: compile errors.

- [ ] **Step 3: Implement** — add to `Engine` struct in `internal/engine/engine.go`: field `busy atomic.Bool` (import `"sync/atomic"`) and, at package level:

```go
// ErrBusy means Sync was re-entered. The daemon's single goroutine makes
// this unreachable in correct code — surfacing it loudly beats silently
// serializing a logic error.
var ErrBusy = errors.New("engine: sync already running")
```

(import `"errors"`.) Then `internal/engine/recover.go`:

```go
package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
)

// recoverState aborts any rebase or merge a previous crash left behind
// (spec §4 crash safety). Runs at the top of every cycle: a mid-cycle
// SIGKILL heals on the next tick, not the next daemon restart.
func (e *Engine) recoverState(ctx context.Context) error {
	gitDir := filepath.Join(e.checkout, ".git")
	steps := []struct {
		marker string
		abort  []string
	}{
		{"rebase-merge", []string{"rebase", "--abort"}},
		{"rebase-apply", []string{"rebase", "--abort"}},
		{"MERGE_HEAD", []string{"merge", "--abort"}},
	}
	for _, s := range steps {
		if _, err := os.Lstat(filepath.Join(gitDir, s.marker)); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if _, err := gitx.Run(ctx, e.checkout, s.abort...); err != nil {
			return fmt.Errorf("recover: git %v: %w", s.abort, err)
		}
	}
	return nil
}
```

`internal/engine/reconcile.go`:

```go
package engine

import (
	"context"
	"fmt"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// reconcile runs each provider's derived-index reconciliation (spec §4
// step 4) on every non-degraded unit. Reconcilers are deterministic; a
// failure is a bug or corrupted state and fails the cycle loudly.
func (e *Engine) reconcile(ctx context.Context, units []repo.Unit, skip map[string]bool) error {
	for _, u := range units {
		if skip[u.Folder] {
			continue
		}
		prov, ok := e.registry.Get(u.Provider)
		if !ok {
			return fmt.Errorf("reconcile %s: provider %q not registered", u.Folder, u.Provider)
		}
		if err := prov.ReconcileIndex(ctx, e.layout.UnitDir(u.Folder, u.Provider)); err != nil {
			return fmt.Errorf("reconcile %s/%s: %w", u.Folder, u.Provider, err)
		}
	}
	return nil
}
```

`internal/engine/mirror_out.go`:

```go
package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/renameio/v2"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// mirrorOut implements spec §4 step 5: checkout → provider dirs,
// atomic per file. Two hard gates protect local work: overwrites and
// deletions are both skipped when the local file changed since the
// cycle's mirror-in snapshot (converge next cycle), and deletions
// additionally require the manifest to prove this host synced the path
// before. Degraded projects (skip set) are withheld entirely (§11).
func (e *Engine) mirrorOut(ctx context.Context, units []repo.Unit, manifest *repo.Manifest, snapshot localSnapshot, skip map[string]bool) (MirrorStats, error) {
	var stats MirrorStats
	for _, u := range units {
		if skip[u.Folder] {
			continue
		}
		if err := e.mirrorOutUnit(ctx, u, manifest, snapshot, &stats); err != nil {
			return stats, fmt.Errorf("mirror-out %s/%s: %w", u.Folder, u.Provider, err)
		}
	}
	return stats, nil
}

func (e *Engine) mirrorOutUnit(_ context.Context, u repo.Unit, manifest *repo.Manifest, snapshot localSnapshot, stats *MirrorStats) error {
	unitDir := e.layout.UnitDir(u.Folder, u.Provider)
	unitPrefix := path.Join(u.Folder, u.Provider) + "/"

	// localUnchanged reports whether the provider file at rel is safe to
	// replace: absent entirely, or byte-identical to the cycle snapshot.
	localUnchanged := func(rel, repoRel string) (bool, error) {
		localPath := filepath.Join(u.LocalDir, filepath.FromSlash(rel))
		if _, err := os.Lstat(localPath); errors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		snap, ok := snapshot[repoRel]
		if !ok {
			return false, nil // appeared mid-cycle: hands off
		}
		current, err := repo.HashFile(localPath)
		if err != nil {
			return false, err
		}
		return current.SHA256 == snap.SHA256, nil
	}

	// Pass 1: checkout → local (adds and updates).
	inCheckout := map[string]bool{}
	err := filepath.WalkDir(unitDir, func(fullPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		rel, err := filepath.Rel(unitDir, fullPath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		repoRel := unitPrefix + rel
		inCheckout[rel] = true

		checkoutEntry, err := repo.HashFile(fullPath)
		if err != nil {
			return err
		}
		if snap, ok := snapshot[repoRel]; ok && snap.SHA256 == checkoutEntry.SHA256 {
			return nil // local state already matches what this cycle mirrored in
		}
		safe, err := localUnchanged(rel, repoRel)
		if err != nil {
			return err
		}
		if !safe {
			stats.Skipped++
			return nil
		}
		content, err := os.ReadFile(fullPath) //nolint:gosec // G304: path came from walking the unit's checkout dir
		if err != nil {
			return err
		}
		localPath := filepath.Join(u.LocalDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
			return err
		}
		if err := renameio.WriteFile(localPath, content, 0o644); err != nil {
			return err
		}
		written, err := repo.HashFile(localPath)
		if err != nil {
			return err
		}
		stats.Copied++
		return manifest.Set(repoRel, written)
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	// Pass 2: remote deletions — manifest-gated (spec §4 step 5).
	for repoRel := range manifest.Files {
		if !strings.HasPrefix(repoRel, unitPrefix) {
			continue
		}
		rel := strings.TrimPrefix(repoRel, unitPrefix)
		if inCheckout[rel] {
			continue
		}
		localPath := filepath.Join(u.LocalDir, filepath.FromSlash(rel))
		if _, err := os.Lstat(localPath); errors.Is(err, fs.ErrNotExist) {
			manifest.Delete(repoRel) // gone everywhere; drop the entry
			continue
		}
		safe, err := localUnchanged(rel, repoRel)
		if err != nil {
			return err
		}
		if !safe {
			// User edited while remote deleted: keep the edit; it
			// mirrors back in next cycle as a fresh fact.
			stats.Skipped++
			continue
		}
		if err := os.Remove(localPath); err != nil {
			return err
		}
		removeEmptyParents(localPath, u.LocalDir)
		manifest.Delete(repoRel)
		stats.Deleted++
	}
	return nil
}

// removeEmptyParents tidies now-empty directories up to (not including)
// stop. Best-effort: the first non-empty ancestor ends the walk.
func removeEmptyParents(deleted, stop string) {
	stop = filepath.Clean(stop)
	for dir := filepath.Dir(deleted); dir != stop && strings.HasPrefix(dir, stop+string(filepath.Separator)); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			return
		}
	}
}
```

`internal/engine/sync.go`:

```go
package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// Sync runs one full cycle (spec §4): recover → mirror-in → commit →
// integrate → reconcile → commit → mirror-out → commit meta → push.
// It is the engine's only exported behavior and must never run
// concurrently with itself.
func (e *Engine) Sync(ctx context.Context, units []repo.Unit) (Report, error) {
	if !e.busy.CompareAndSwap(false, true) {
		return Report{}, ErrBusy
	}
	defer e.busy.Store(false)

	var report Report
	stamp := e.stamp()

	if err := e.recoverState(ctx); err != nil {
		return report, err
	}

	manifestPath := e.layout.ManifestFile(e.host)
	manifest, err := repo.LoadManifest(manifestPath)
	if err != nil {
		return report, err
	}

	inStats, snapshot, err := e.mirrorIn(ctx, units, manifest)
	report.MirrorIn = inStats
	if err != nil {
		return report, err
	}
	if err := manifest.Save(manifestPath); err != nil {
		return report, err
	}
	subjects, err := e.commitProjects(ctx, stamp)
	report.Commits = append(report.Commits, subjects...)
	if err != nil {
		return report, err
	}
	if metaSubject, err := e.commitMeta(ctx, stamp); err != nil {
		return report, err
	} else if metaSubject != "" {
		report.Commits = append(report.Commits, metaSubject)
	}

	integ, err := e.integrate(ctx)
	if err != nil {
		return report, err
	}
	// The in-memory manifest stays authoritative across integrate:
	// manifests/<host>.json is written only by this host, so a rebase
	// cannot change it underneath us.
	skip := map[string]bool{}
	for _, folder := range integ.Degraded {
		skip[folder] = true
	}
	if integ.DegradedAll {
		for _, u := range units {
			skip[u.Folder] = true
		}
	}
	report.Degraded = sortedKeys(skip)

	if integ.Integrated {
		if err := e.reconcile(ctx, units, skip); err != nil {
			return report, err
		}
		subjects, err := e.commitProjects(ctx, stamp)
		report.Commits = append(report.Commits, subjects...)
		if err != nil {
			return report, err
		}
	}

	outStats, err := e.mirrorOut(ctx, units, manifest, snapshot, skip)
	report.MirrorOut = outStats
	if err != nil {
		return report, err
	}
	if err := manifest.Save(manifestPath); err != nil {
		return report, err
	}
	if metaSubject, err := e.commitMeta(ctx, stamp); err != nil {
		return report, err
	} else if metaSubject != "" {
		report.Commits = append(report.Commits, metaSubject)
	}

	if !integ.Integrated {
		// Offline or degraded: a push is known-doomed; queue instead
		// of burning the retry loop (git-native queue, spec §11).
		queued, err := e.hasUnpushed(ctx)
		if err != nil {
			return report, err
		}
		report.PushQueued = queued
		return report, nil
	}
	pushed, err := e.push(ctx)
	if err != nil {
		return report, err
	}
	report.Pushed = pushed.Pushed
	report.PushQueued = pushed.Queued
	for _, folder := range pushed.Degraded {
		if !skip[folder] {
			skip[folder] = true
		}
	}
	if pushed.DegradedAll {
		for _, u := range units {
			skip[u.Folder] = true
		}
	}
	report.Degraded = sortedKeys(skip)
	return report, nil
}

func (e *Engine) hasUnpushed(ctx context.Context) (bool, error) {
	ahead, err := gitx.Run(ctx, e.checkout, "rev-list", "--count", upstreamRef+"..HEAD")
	if err != nil {
		return false, fmt.Errorf("unpushed count: %w", err)
	}
	return strings.TrimSpace(ahead.Stdout) != "0", nil
}

func sortedKeys(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
```

Note for the implementer: race-retry degradation discovered inside `push` arrives AFTER this cycle's mirror-out has already run — that is correct, not a bug. The retry's integrate either landed cleanly (no degradation) or aborted back (nothing new reached the checkout, so mirror-out had nothing degraded to apply); the report still surfaces the degraded projects for the dashboard.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/engine/ -race -v 2>&1 | tail -40`
Expected: all PASS (Tasks 6–8 suites).

- [ ] **Step 5: Lint, format, full suite, commit**

```bash
gofumpt -l -w . && golangci-lint run && go test ./... -race
git add internal/engine/
git commit -m "feat(engine): reconcile, guarded mirror-out, Sync orchestration with crash recovery (spec §4 steps 4-5)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 9: Watch manager — fsnotify with coalescing, dynamic subtree attach, poll backstop (ADR 07)

`internal/watch` turns raw filesystem events into a coalesced "run a cycle" signal. It routes NOTHING per-unit: any event under any watched root produces one global trigger, and the engine cycle scans everything (a cycle over quiet units is cheap; per-unit routing is complexity with no payoff — decided in planning, YAGNI).

Contracts:
- **fsnotify v1.10.1, `NewBufferedWatcher(64)`, NO recursive watching** (ADR 07). `Add` walks the tree and attaches each directory; a `Create` of a directory attaches it (and its subtree) dynamically. The created-with-contents race — files landing inside a new dir before its watch attaches — is closed by the trigger semantics, not by enumeration: the `Create` event itself triggers a full engine cycle, which scans those files regardless of missed events.
- **Missing roots watch their nearest existing ancestor** (a provider dir deleted and recreated re-attaches automatically). The ancestor watch exists only to notice the root appearing; it attaches the root, never the ancestor's siblings.
- **Trigger channel has capacity 1 and drops when full.** This is the coalescing invariant: a buffered, untaken trigger already guarantees a future cycle will see any change that happens before the consumer takes it; events arriving DURING a cycle debounce into the next buffered trigger. No event can be lost, only merged.
- **`Chmod` events are ignored** (kqueue/atime noise produces cycle storms; every content change also emits `Write`/`Create`). Watcher overflow or error emits a trigger (`Reason: "overflow"`) — a full cycle IS the rescan that self-heals a lossy watch.
- **Real clock, short durations in tests** — a deliberate revision of the earlier "injected clock" note, with reasoning: fsnotify's kernel latency is already real-time and unfakeable, so a fake clock adds a second time domain that must interleave with real event delivery — a known flake source. Tests use 40ms debounce with eventually-style deadline waits (2s), which is robust on slow CI runners. The daemon's OWN timers (Task 11) inject trigger channels instead, where time is the only async source.
- Poll backstop (ADR 07: self-heal + WSL2 `/mnt/c` where inotify misses events): a plain ticker emitting `Reason: "poll"`; `Poll = 0` disables it (unit tests).

**Files:**
- Create: `internal/watch/watch.go`
- Test: `internal/watch/watch_test.go`
- Modify: `go.mod` (`go get github.com/fsnotify/fsnotify@v1.10.1`)

**Interfaces:**
- Consumes: `fsnotify` only (no internal deps — `watch` is a leaf package).
- Produces (daemon Task 11 relies on these):
  - `type Config struct { Debounce, Poll time.Duration }` (Debounce must be > 0; Poll 0 disables)
  - `type Trigger struct { Reason string }` — `"fs"`, `"poll"`, or `"overflow"`
  - `func New(config Config) (*Manager, error)` · `(m *Manager) Add(root string) error` · `(m *Manager) Triggers() <-chan Trigger` · `(m *Manager) Run(ctx context.Context) error` (returns nil on ctx cancel) · `(m *Manager) Close() error`

- [ ] **Step 1: Write the failing tests** — `internal/watch/watch_test.go`:

```go
package watch_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/watch"
)

const testDebounce = 40 * time.Millisecond

func startManager(t *testing.T, config watch.Config, roots ...string) *watch.Manager {
	t.Helper()
	manager, err := watch.New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	for _, root := range roots {
		if err := manager.Add(root); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		if err := manager.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	return manager
}

func awaitTrigger(t *testing.T, manager *watch.Manager, within time.Duration) watch.Trigger {
	t.Helper()
	select {
	case trigger := <-manager.Triggers():
		return trigger
	case <-time.After(within):
		t.Fatal("no trigger within deadline")
		return watch.Trigger{}
	}
}

func assertQuiet(t *testing.T, manager *watch.Manager, within time.Duration) {
	t.Helper()
	select {
	case trigger := <-manager.Triggers():
		t.Fatalf("unexpected trigger %+v", trigger)
	case <-time.After(within):
	}
}

func TestWriteTriggersOnceAfterDebounce(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	manager := startManager(t, watch.Config{Debounce: testDebounce}, root)

	if err := os.WriteFile(filepath.Join(root, "MEMORY.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	trigger := awaitTrigger(t, manager, 2*time.Second)
	if trigger.Reason != "fs" {
		t.Fatalf("Reason = %q, want fs", trigger.Reason)
	}
	assertQuiet(t, manager, 6*testDebounce)
}

func TestBurstCoalescesToOneTrigger(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	manager := startManager(t, watch.Config{Debounce: testDebounce}, root)

	for i := 0; i < 5; i++ {
		name := filepath.Join(root, "memories", "topic-"+string(rune('a'+i))+".md")
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte("fact\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	awaitTrigger(t, manager, 2*time.Second)
	assertQuiet(t, manager, 6*testDebounce)
}

// TestNewSubdirectoryGetsWatched proves dynamic attach: a write INSIDE a
// directory created after startup still triggers (fsnotify itself is
// non-recursive — this is the manager's added value).
func TestNewSubdirectoryGetsWatched(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	manager := startManager(t, watch.Config{Debounce: testDebounce}, root)

	subdir := filepath.Join(root, "rollout_summaries")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	awaitTrigger(t, manager, 2*time.Second) // the mkdir itself

	// Wait out the debounce, then write inside the NEW directory.
	time.Sleep(4 * testDebounce)
	if err := os.WriteFile(filepath.Join(subdir, "s.md"), []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitTrigger(t, manager, 2*time.Second)
}

// TestMissingRootAttachesWhenCreated covers the deleted-and-recreated
// provider dir: Add on a nonexistent root watches the nearest existing
// ancestor and attaches the root when it appears.
func TestMissingRootAttachesWhenCreated(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, ".claude", "memory")
	manager := startManager(t, watch.Config{Debounce: testDebounce}, root)

	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	awaitTrigger(t, manager, 2*time.Second) // creation trigger

	time.Sleep(4 * testDebounce)
	if err := os.WriteFile(filepath.Join(root, "MEMORY.md"), []byte("z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitTrigger(t, manager, 2*time.Second) // proves the root is attached
}

func TestPollBackstopFires(t *testing.T) {
	t.Parallel()
	manager := startManager(t, watch.Config{Debounce: testDebounce, Poll: 100 * time.Millisecond}, t.TempDir())
	trigger := awaitTrigger(t, manager, 2*time.Second)
	if trigger.Reason != "poll" {
		t.Fatalf("Reason = %q, want poll", trigger.Reason)
	}
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()
	if _, err := watch.New(watch.Config{Debounce: 0}); err == nil {
		t.Fatal("zero debounce accepted")
	}
	if _, err := watch.New(watch.Config{Debounce: time.Second, Poll: -time.Second}); err == nil {
		t.Fatal("negative poll accepted")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go get github.com/fsnotify/fsnotify@v1.10.1 && go test ./internal/watch/ 2>&1 | head -10`
Expected: compile errors.

- [ ] **Step 3: Implement** — `internal/watch/watch.go`:

```go
// Package watch coalesces filesystem events under enrolled provider
// dirs into engine-cycle triggers (ADR 07). It never routes per-unit:
// one global trigger, one full cycle — the cycle is the rescan.
package watch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Config tunes the manager. Debounce is the trailing quiet window;
// Poll is the backstop rescan interval (0 disables — unit tests).
type Config struct {
	Debounce time.Duration
	Poll     time.Duration
}

// Trigger asks the daemon for one engine cycle.
type Trigger struct {
	// Reason is "fs" (debounced events), "poll" (backstop tick), or
	// "overflow" (the watcher lost events; the cycle IS the rescan).
	Reason string
}

// Manager owns one fsnotify watcher and the coalescing state machine.
type Manager struct {
	config   Config
	watcher  *fsnotify.Watcher
	triggers chan Trigger
	// pendingRoots maps a watched ancestor to the not-yet-existing
	// roots waiting under it (deleted/recreated provider dirs).
	pendingRoots map[string][]string
}

// New creates the manager with a buffered watcher (ADR 07: burst
// absorption while the pump goroutine is mid-iteration).
func New(config Config) (*Manager, error) {
	if config.Debounce <= 0 {
		return nil, fmt.Errorf("watch: debounce must be positive, got %v", config.Debounce)
	}
	if config.Poll < 0 {
		return nil, fmt.Errorf("watch: poll must be >= 0, got %v", config.Poll)
	}
	watcher, err := fsnotify.NewBufferedWatcher(64)
	if err != nil {
		return nil, fmt.Errorf("watch: %w", err)
	}
	return &Manager{
		config:       config,
		watcher:      watcher,
		triggers:     make(chan Trigger, 1),
		pendingRoots: map[string][]string{},
	}, nil
}

// Add watches root and its whole subtree. A missing root watches its
// nearest existing ancestor instead, attaching when the root appears.
// Call before Run; the pump owns all state afterwards.
func (m *Manager) Add(root string) error {
	root = filepath.Clean(root)
	if info, err := os.Stat(root); err == nil && info.IsDir() {
		return m.attachTree(root)
	}
	ancestor := filepath.Dir(root)
	for {
		if info, err := os.Stat(ancestor); err == nil && info.IsDir() {
			break
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return fmt.Errorf("watch: no existing ancestor for %s", root)
		}
		ancestor = parent
	}
	if err := m.watcher.Add(ancestor); err != nil {
		return fmt.Errorf("watch %s: %w", ancestor, err)
	}
	m.pendingRoots[ancestor] = append(m.pendingRoots[ancestor], root)
	return nil
}

// attachTree adds a watch on every directory under (and including) dir.
// fsnotify is non-recursive by design (ADR 07); this walk is the
// recursion, and dir-Create events extend it dynamically.
func (m *Manager) attachTree(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil // raced with a delete; the next event re-attaches
			}
			return walkErr
		}
		if !d.IsDir() {
			return nil
		}
		if err := m.watcher.Add(p); err != nil {
			return fmt.Errorf("watch %s: %w", p, err)
		}
		return nil
	})
}

// Triggers is the coalesced output. Capacity 1, drop-when-full: an
// untaken buffered trigger already guarantees a future cycle sees any
// change that lands before the consumer takes it — merged, never lost.
func (m *Manager) Triggers() <-chan Trigger { return m.triggers }

// Close releases the underlying watcher.
func (m *Manager) Close() error { return m.watcher.Close() }

// Run pumps events until ctx is cancelled (returns nil) or the watcher
// dies (returns the error).
func (m *Manager) Run(ctx context.Context) error {
	debounce := time.NewTimer(m.config.Debounce)
	if !debounce.Stop() {
		<-debounce.C
	}
	debouncing := false

	var pollC <-chan time.Time
	if m.config.Poll > 0 {
		poll := time.NewTicker(m.config.Poll)
		defer poll.Stop()
		pollC = poll.C
	}

	for {
		select {
		case <-ctx.Done():
			debounce.Stop()
			return nil

		case event, ok := <-m.watcher.Events:
			if !ok {
				return errors.New("watch: event stream closed")
			}
			if event.Op == fsnotify.Chmod {
				continue // atime/permission noise; content changes emit Write/Create
			}
			if event.Op.Has(fsnotify.Create) {
				m.handleCreate(event.Name)
			}
			if debouncing && !debounce.Stop() {
				<-debounce.C
			}
			debounce.Reset(m.config.Debounce)
			debouncing = true

		case <-debounce.C:
			debouncing = false
			m.emit("fs")

		case <-pollC:
			m.emit("poll")

		case err, ok := <-m.watcher.Errors:
			if !ok {
				return errors.New("watch: error stream closed")
			}
			_ = err // overflow or transient watch error
			m.emit("overflow")
		}
	}
}

// handleCreate attaches newly created directories (and pending roots
// that just appeared). Files that landed inside before the watch
// attached are covered by the trigger this very event produces.
func (m *Manager) handleCreate(name string) {
	if roots, ok := m.pendingRoots[filepath.Dir(name)]; ok {
		remaining := roots[:0]
		for _, root := range roots {
			if root == name || isUnder(root, name) {
				if info, err := os.Stat(root); err == nil && info.IsDir() {
					_ = m.attachTree(root)
					continue
				}
			}
			remaining = append(remaining, root)
		}
		if len(remaining) == 0 {
			delete(m.pendingRoots, filepath.Dir(name))
		} else {
			m.pendingRoots[filepath.Dir(name)] = remaining
		}
	}
	if info, err := os.Stat(name); err == nil && info.IsDir() {
		_ = m.attachTree(name)
	}
}

func isUnder(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) &&
		rel != "." && !hasDotDotPrefix(rel)
}

func hasDotDotPrefix(rel string) bool {
	return rel == ".." || len(rel) > 2 && rel[:3] == ".."+string(filepath.Separator)
}

func (m *Manager) emit(reason string) {
	select {
	case m.triggers <- Trigger{Reason: reason}:
	default: // coalesce into the already-buffered trigger
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/watch/ -race -count=5 -v 2>&1 | tail -20`
Expected: all PASS across 5 repetitions (`-count=5` shakes out timing flakes NOW, not in CI later).

- [ ] **Step 5: Lint, format, full suite, commit**

```bash
gofumpt -l -w . && golangci-lint run && go test ./... -race
git add internal/watch/ go.mod go.sum
git commit -m "feat(watch): coalescing fsnotify manager with dynamic attach and poll backstop (ADR 07)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 10: Daemon API — UDS server core, peer-UID enforcement, typed client (ADR 09)

Two packages with a hard boundary (spec §8): `internal/daemon/api` holds the wire types and the client — the ONLY surface the CLI may import — and `internal/daemon` holds the server. `api` imports NOTHING internal (not even `engine`); the daemon converts `engine.Report` into `api.SyncSummary`, keeping engine types out of the CLI's dependency tree.

Security contracts (ADR 09, shipping NOW, not deferred):
- Socket file is chmod'd 0600 immediately after listen; stale sockets are removed first (the Task 11 flock guarantees no live daemon owns one).
- **Peer-UID verification on every request, fail closed.** Credentials are read at accept time via `ConnContext` (`SO_PEERCRED`/`getsockopt Ucred` on Linux, `LOCAL_PEERCRED`/`Xucred` on macOS; other platforms refuse). A UID mismatch is 403; a credential-read error rejects too. The extractor is a seam (`peerUIDFunc`) so the mismatch and fail-closed paths are unit-testable without a second UID.
- Server timeouts are explicit: `ReadHeaderTimeout` 5s, `ReadTimeout` 30s, `WriteTimeout` 120s (a sync response can legitimately take tens of seconds), `IdleTimeout` 120s.

Endpoints (`/v0` prefix; version bumps are additive):
- `GET /v0/status` → `api.StatusResponse`
- `POST /v0/sync` → `api.SyncResponse` (Task 11 implements wait-up-to-60s-then-`"running"`)
- `GET /v0/projects` → `api.ProjectsResponse`

Test-harness note: UDS paths must stay under the ~104-byte `sun_path` limit, and `t.TempDir()` embeds long test names — helpers use `os.MkdirTemp("", "ab")` (short) with explicit cleanup instead.

**Files:**
- Create: `internal/daemon/api/types.go`, `internal/daemon/api/client.go`, `internal/daemon/server.go`, `internal/daemon/peercred_linux.go`, `internal/daemon/peercred_darwin.go`, `internal/daemon/peercred_other.go`
- Test: `internal/daemon/server_test.go`
- Modify: `go.mod` (`go get golang.org/x/sys@v0.42.0` — promotes the existing indirect to direct)

**Interfaces:**
- Consumes: `config.SocketName` (path composition happens in Task 11/tests).
- Produces (Tasks 11–12 and the CLI rely on these):
  - `api.StatusResponse{Version, State string; PID int; StartedAt time.Time; LastSync *SyncSummary}` (`State` ∈ `"ready"`, `"uninitialized"`) · `api.SyncSummary{At time.Time; Commits []string; MirrorIn, MirrorOut Stats; Degraded []string; Pushed, PushQueued bool; Error string}` · `api.Stats{Copied, Deleted, Skipped int}` · `api.SyncResponse{Status string; Summary *SyncSummary}` (`Status` ∈ `"completed"`, `"running"`) · `api.ProjectsResponse{Units []UnitInfo}` · `api.UnitInfo{Provider, Folder, LocalDir string; Degraded bool}`
  - `api.NewClient(socketPath string) *Client` · `(c *Client) Status(ctx) (StatusResponse, error)` · `(c *Client) Sync(ctx) (SyncResponse, error)` · `(c *Client) Projects(ctx) (ProjectsResponse, error)` · `var api.ErrDaemonNotRunning error`
  - `daemon.controller` interface `{ Status() api.StatusResponse; TriggerSync(ctx context.Context) (api.SyncResponse, error); Projects() api.ProjectsResponse }` (Task 11's `Daemon` implements it)
  - `daemon.newServer(ctrl controller, peerUID peerUIDFunc) *http.Server` · `daemon.listenSocket(socketPath string) (net.Listener, error)` · `type peerUIDFunc func(net.Conn) (int, error)` · `var defaultPeerUID peerUIDFunc` (per-OS)

- [ ] **Step 1: Write the failing tests** — `internal/daemon/server_test.go`:

```go
package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
)

type fakeController struct {
	status   api.StatusResponse
	sync     api.SyncResponse
	projects api.ProjectsResponse
}

func (f *fakeController) Status() api.StatusResponse { return f.status }
func (f *fakeController) TriggerSync(context.Context) (api.SyncResponse, error) {
	return f.sync, nil
}
func (f *fakeController) Projects() api.ProjectsResponse { return f.projects }

// shortSocketDir avoids t.TempDir(): test names inflate the path past
// the ~104-byte sun_path limit.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func startServer(t *testing.T, ctrl controller, peerUID peerUIDFunc) string {
	t.Helper()
	socketPath := filepath.Join(shortSocketDir(t), "agent-brain.sock")
	listener, err := listenSocket(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	server := newServer(ctrl, peerUID)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	})
	return socketPath
}

func TestStatusSyncProjectsRoundtrip(t *testing.T) {
	t.Parallel()
	want := &fakeController{
		status: api.StatusResponse{Version: "test", State: "ready", PID: 42, StartedAt: time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)},
		sync: api.SyncResponse{Status: "completed", Summary: &api.SyncSummary{
			Pushed: true, Commits: []string{"memory: host-a alpha 2026-07-08T12:00:00Z"},
		}},
		projects: api.ProjectsResponse{Units: []api.UnitInfo{
			{Provider: "claude", Folder: "alpha", LocalDir: "/p/.claude/memory", Degraded: true},
		}},
	}
	socketPath := startServer(t, want, defaultPeerUID)
	client := api.NewClient(socketPath)
	ctx := context.Background()

	status, err := client.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want.status, status); diff != "" {
		t.Fatalf("status (-want +got):\n%s", diff)
	}
	syncResp, err := client.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want.sync, syncResp); diff != "" {
		t.Fatalf("sync (-want +got):\n%s", diff)
	}
	projects, err := client.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want.projects, projects); diff != "" {
		t.Fatalf("projects (-want +got):\n%s", diff)
	}
}

func TestSocketMode0600(t *testing.T) {
	t.Parallel()
	socketPath := startServer(t, &fakeController{}, defaultPeerUID)
	info, err := os.Lstat(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("socket mode = %v, want 0600", got)
	}
}

func TestPeerUIDMismatchRejected(t *testing.T) {
	t.Parallel()
	hostileUID := func(net.Conn) (int, error) { return os.Getuid() + 1, nil }
	socketPath := startServer(t, &fakeController{}, hostileUID)
	if _, err := api.NewClient(socketPath).Status(context.Background()); err == nil {
		t.Fatal("mismatched peer UID was accepted")
	}
}

func TestPeerUIDErrorFailsClosed(t *testing.T) {
	t.Parallel()
	brokenUID := func(net.Conn) (int, error) { return 0, errors.New("cred read failed") }
	socketPath := startServer(t, &fakeController{}, brokenUID)
	if _, err := api.NewClient(socketPath).Status(context.Background()); err == nil {
		t.Fatal("credential-read failure was accepted")
	}
}

func TestSyncRequiresPOST(t *testing.T) {
	t.Parallel()
	socketPath := startServer(t, &fakeController{}, defaultPeerUID)
	client := api.NewClient(socketPath)
	// Status GETs work, so the transport is fine; a GET on /v0/sync must 405.
	if err := client.GetForTest(context.Background(), "/v0/sync"); err == nil {
		t.Fatal("GET /v0/sync succeeded, want 405")
	}
}

func TestClientReportsDaemonNotRunning(t *testing.T) {
	t.Parallel()
	client := api.NewClient(filepath.Join(shortSocketDir(t), "absent.sock"))
	_, err := client.Status(context.Background())
	if !errors.Is(err, api.ErrDaemonNotRunning) {
		t.Fatalf("err = %v, want ErrDaemonNotRunning", err)
	}
}

func TestListenSocketReplacesStaleSocket(t *testing.T) {
	t.Parallel()
	socketPath := filepath.Join(shortSocketDir(t), "agent-brain.sock")
	first, err := listenSocket(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash: close the listener but leave the inode via a
	// fresh file (closing removes the socket file, so recreate one).
	_ = first.Close()
	if err := os.WriteFile(socketPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := listenSocket(socketPath)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	_ = second.Close()
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go get golang.org/x/sys@v0.42.0 && go test ./internal/daemon/... 2>&1 | head -10`
Expected: compile errors.

- [ ] **Step 3: Implement** — `internal/daemon/api/types.go`:

```go
// Package api is the daemon↔CLI wire contract (ADR 09) — the ONLY
// package both sides import (spec §8). It depends on nothing internal.
package api

import "time"

// Stats mirrors one direction of engine mirroring.
type Stats struct {
	Copied  int `json:"copied"`
	Deleted int `json:"deleted"`
	Skipped int `json:"skipped"`
}

// SyncSummary is one engine cycle's outcome, as reported over the API.
type SyncSummary struct {
	At         time.Time `json:"at"`
	Commits    []string  `json:"commits,omitempty"`
	MirrorIn   Stats     `json:"mirror_in"`
	MirrorOut  Stats     `json:"mirror_out"`
	Degraded   []string  `json:"degraded,omitempty"`
	Pushed     bool      `json:"pushed"`
	PushQueued bool      `json:"push_queued"`
	Error      string    `json:"error,omitempty"`
}

// StatusResponse answers GET /v0/status. State is "ready" when the
// memories checkout exists and cycles run, "uninitialized" when the
// daemon is up but the repo hasn't been provisioned yet (init is a
// Phase-3 command; the Phase-2 daemon must be honest about that state,
// not crash-loop on it).
type StatusResponse struct {
	Version   string       `json:"version"`
	State     string       `json:"state"`
	PID       int          `json:"pid"`
	StartedAt time.Time    `json:"started_at"`
	LastSync  *SyncSummary `json:"last_sync,omitempty"`
}

// SyncResponse answers POST /v0/sync. Status is "completed" when the
// triggered cycle finished within the wait window, "running" otherwise.
type SyncResponse struct {
	Status  string       `json:"status"`
	Summary *SyncSummary `json:"summary,omitempty"`
}

// UnitInfo is one enrolled (provider, dir) pair and its health.
type UnitInfo struct {
	Provider string `json:"provider"`
	Folder   string `json:"folder"`
	LocalDir string `json:"local_dir"`
	Degraded bool   `json:"degraded"`
}

// ProjectsResponse answers GET /v0/projects.
type ProjectsResponse struct {
	Units []UnitInfo `json:"units"`
}
```

`internal/daemon/api/client.go`:

```go
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"syscall"
	"time"
)

// ErrDaemonNotRunning wraps connection failures that mean "no daemon on
// this socket" — the CLI turns it into actionable guidance.
var ErrDaemonNotRunning = errors.New("agent-brain daemon is not running")

// Client talks to the daemon over its unix socket.
type Client struct {
	http *http.Client
}

// NewClient dials socketPath for every request; the host in request
// URLs is a placeholder (UDS has no host).
func NewClient(socketPath string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{http: &http.Client{Transport: transport, Timeout: 120 * time.Second}}
}

// Status fetches the daemon's identity and last cycle.
func (c *Client) Status(ctx context.Context) (StatusResponse, error) {
	var out StatusResponse
	err := c.do(ctx, http.MethodGet, "/v0/status", &out)
	return out, err
}

// Sync triggers a cycle and waits (bounded server-side).
func (c *Client) Sync(ctx context.Context) (SyncResponse, error) {
	var out SyncResponse
	err := c.do(ctx, http.MethodPost, "/v0/sync", &out)
	return out, err
}

// Projects lists enrolled units and their health.
func (c *Client) Projects(ctx context.Context) (ProjectsResponse, error) {
	var out ProjectsResponse
	err := c.do(ctx, http.MethodGet, "/v0/projects", &out)
	return out, err
}

// GetForTest issues a bare GET so tests can probe method handling.
func (c *Client) GetForTest(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodGet, path, &struct{}{})
}

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	request, err := http.NewRequestWithContext(ctx, method, "http://agent-brain"+path, nil)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		if isNotRunning(err) {
			return fmt.Errorf("%w (socket dial failed: %v)", ErrDaemonNotRunning, err)
		}
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon returned %s: %s", response.Status, body)
	}
	return json.Unmarshal(body, out)
}

func isNotRunning(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENOENT)
}
```

`internal/daemon/server.go`:

```go
// Package daemon hosts the resident sync daemon (ADR 04): the single
// engine goroutine, the watch manager, and the UDS API server (ADR 09).
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
)

// controller is what the HTTP layer needs from the daemon core; Task 11's
// Daemon implements it, tests fake it.
type controller interface {
	Status() api.StatusResponse
	TriggerSync(ctx context.Context) (api.SyncResponse, error)
	Projects() api.ProjectsResponse
}

// peerUIDFunc extracts the connecting process's UID from a unix socket
// connection. It is a seam so rejection paths are testable without a
// second user account; production uses defaultPeerUID (per-OS files).
type peerUIDFunc func(net.Conn) (int, error)

type peerInfo struct {
	uid int
	err error
}

type peerKey struct{}

// listenSocket binds socketPath, replacing any stale socket left by a
// crash (the flock, not the socket, is the single-instance guard) and
// locking the file to 0600 before any request is served.
func listenSocket(socketPath string) (net.Listener, error) {
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	return listener, nil
}

// newServer wires routes, peer-UID enforcement, and explicit timeouts.
func newServer(ctrl controller, peerUID peerUIDFunc) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v0/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, ctrl.Status())
	})
	mux.HandleFunc("/v0/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		response, err := ctrl.TriggerSync(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, response)
	})
	mux.HandleFunc("/v0/projects", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, ctrl.Projects())
	})

	return &http.Server{
		Handler: requireSameUser(mux),
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			uid, err := peerUID(conn)
			return context.WithValue(ctx, peerKey{}, peerInfo{uid: uid, err: err})
		},
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// requireSameUser rejects any request whose connection does not carry a
// verified same-UID peer credential. Fail closed: a missing or failed
// credential read is a rejection, never a pass-through.
func requireSameUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info, ok := r.Context().Value(peerKey{}).(peerInfo)
		if !ok || info.err != nil || info.uid != os.Getuid() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
```

`internal/daemon/peercred_linux.go`:

```go
//go:build linux

package daemon

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// defaultPeerUID reads SO_PEERCRED: the kernel-attested UID of the
// process that connected (ADR 09).
var defaultPeerUID peerUIDFunc = func(conn net.Conn) (int, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("peer credentials: not a unix connection (%T)", conn)
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid int
	var credErr error
	controlErr := raw.Control(func(fd uintptr) {
		ucred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			credErr = err
			return
		}
		uid = int(ucred.Uid)
	})
	if controlErr != nil {
		return 0, controlErr
	}
	return uid, credErr
}
```

`internal/daemon/peercred_darwin.go`:

```go
//go:build darwin

package daemon

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// defaultPeerUID reads LOCAL_PEERCRED: the kernel-attested effective
// UID of the connecting process (ADR 09).
var defaultPeerUID peerUIDFunc = func(conn net.Conn) (int, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("peer credentials: not a unix connection (%T)", conn)
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid int
	var credErr error
	controlErr := raw.Control(func(fd uintptr) {
		xucred, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil {
			credErr = err
			return
		}
		uid = int(xucred.Uid)
	})
	if controlErr != nil {
		return 0, controlErr
	}
	return uid, credErr
}
```

`internal/daemon/peercred_other.go`:

```go
//go:build !linux && !darwin

package daemon

import (
	"errors"
	"net"
)

// defaultPeerUID fails closed on platforms without a verified peer
// credential API — every request is rejected rather than trusted.
var defaultPeerUID peerUIDFunc = func(net.Conn) (int, error) {
	return 0, errors.New("peer credentials unsupported on this platform")
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/daemon/... -race -v 2>&1 | tail -20`
Expected: all PASS.

- [ ] **Step 5: Lint, format, full suite, commit**

```bash
gofumpt -l -w . && golangci-lint run && go test ./... -race
git add internal/daemon/ go.mod go.sum
git commit -m "feat(daemon): UDS API server with peer-UID enforcement and typed client (ADR 09)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 11: Daemon lifecycle — flock, logging, the single engine goroutine (ADRs 04, 09, 14)

`daemon.Daemon` composes everything: runtime dir (0700 every start — WSL2 tears it down), `gofrs/flock` single-instance guard (kernel-released on any death), size-rotated `slog` JSON logging, RLIMIT_NOFILE raise (kqueue consumes one fd per watched dir on macOS), the watch manager, the UDS server, and THE single goroutine that owns the engine.

Explicit decisions (each one is an ADR 14 "decide per loop" obligation):
- **Retry policy: unbounded, capped-interval backoff.** `cenkalti/backoff/v5` `ExponentialBackOff` (initial 5s, max interval 5m), reset on success. `MaxElapsedTime` is deliberately not set: this is a resident daemon — giving up equals a dead daemon, and the ticker/poll backstops keep firing regardless. That IS the explicit per-loop decision ADR 14 demands, with reasons recorded here.
- **Units re-load from `registry-local.toml` every cycle** (a cheap TOML read), so Phase-3 enrollments take effect without a daemon restart. Watch ROOTS attach at startup only in Phase 2 — a newly enrolled dir still syncs via ticker/poll until restart; dynamic watch-root refresh lands with the Phase-3 enroll UX. Documented limitation, not an accident.
- **Uninitialized is a first-class state.** No memories checkout (`MemoriesDir/.git` absent) → daemon runs, `Status.State = "uninitialized"`, cycles skip, `POST /v0/sync` returns an actionable error. The Phase-2 daemon must be honest about not being provisioned, not crash-loop.
- **Manual sync waits bounded.** `TriggerSync` hands the request to the loop and waits ≤ 60s for the cycle; still running → `{Status: "running"}` and the client checks status later. Client impatience never cancels an engine cycle (a half-cancelled cycle exercises crash recovery for no reason).

**Files:**
- Create: `internal/daemon/daemon.go`, `internal/daemon/logging.go`, `internal/daemon/rlimit_unix.go`
- Test: `internal/daemon/daemon_test.go`
- Modify: `go.mod` (`go get github.com/gofrs/flock@latest github.com/cenkalti/backoff/v5@v5.0.3` — record the flock version go resolves; the v0.12+ line is current)

**Interfaces:**
- Consumes: everything prior — `config` (Paths, Settings, RuntimeDir, ValidateSocketPath, SocketName, LockName), `engine.New`/`Sync`/`Report`/`ErrBusy`, `watch`, `repo.LoadLocalRegistry`/`SanitizeHostname`, Task 10 server pieces.
- Produces (Task 12 CLI relies on these):
  - `type Config struct { Paths config.Paths; Settings config.Settings; Registry *provider.Registry; Version string; Logger *slog.Logger }` (nil Logger → JSON logger on the rotated `DaemonLogFile`)
  - `func New(cfg Config) (*Daemon, error)` · `func (d *Daemon) Run(ctx context.Context) error` (blocks; nil on graceful shutdown)
  - `var ErrAlreadyRunning error`
  - `func SocketPathForClient() (string, error)` — `config.RuntimeDir()` + `config.SocketName`; the one shared path derivation the CLI uses
  - `Run` exports `AGENT_BRAIN_CONFLICT_LOG=<DataDir>/conflicts.jsonl` process-wide before the first cycle. The Phase-1 merge driver records retain-both events **only when this variable is set** (spec §4: the driver "records the event for the dashboard conflicts view") — without it, daemon-driven merges would silently drop conflict history that Phase 3's conflicts view needs.

- [ ] **Step 1: Write the failing tests** — `internal/daemon/daemon_test.go`:

```go
package daemon_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/providertest"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// Settings floors are a LoadSettings contract; tests construct the
// struct directly to run fast cycles.
func fastSettings() config.Settings {
	return config.Settings{Sync: config.SyncSettings{
		Ticker:   config.Duration(time.Hour), // ticker quiet; tests drive via watch/manual
		Debounce: config.Duration(50 * time.Millisecond),
		Poll:     config.Duration(0),
	}}
}

func testRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	fake := providertest.New("claude", provider.ScopePerProject, []provider.Pattern{
		{Glob: "memories/**", Class: provider.ClassFact},
	})
	registry, err := provider.NewRegistry(fake)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if res, err := gitx.Run(context.Background(), dir, args...); err != nil {
		t.Fatalf("git %v: %v\nstderr: %s", args, err, res.Stderr)
	}
}

// newDaemonEnv provisions Paths under short temp dirs (sun_path limit),
// a seeded memories checkout with a bare remote, and one enrolled unit.
func newDaemonEnv(t *testing.T) (config.Paths, repo.Unit) {
	t.Helper()
	base, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	paths := config.Paths{ConfigDir: filepath.Join(base, "cfg"), DataDir: filepath.Join(base, "data")}
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", filepath.Join(base, "run"))

	bare := filepath.Join(base, "remote.git")
	checkout := paths.MemoriesDir()
	mustGit(t, base, "init", "--bare", "-b", "main", bare)
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mustGit(t, base, "clone", bare, checkout)
	mustGit(t, checkout, "config", "user.name", "daemon-test")
	mustGit(t, checkout, "config", "user.email", "daemon-test@example.invalid")
	if err := repo.WriteAttributes(repo.NewLayout(checkout), testRegistry(t)); err != nil {
		t.Fatal(err)
	}
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "-m", "init: repo skeleton")
	mustGit(t, checkout, "push", "-u", "origin", "main")

	localDir := filepath.Join(base, "proj", ".claude", "memory")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	unit := repo.Unit{Provider: "claude", ProjectID: "id-alpha", Folder: "alpha", LocalDir: localDir}
	registry := repo.NewLocalRegistry()
	if err := registry.Enroll(unit); err != nil {
		t.Fatal(err)
	}
	if err := registry.Save(paths.LocalRegistryFile()); err != nil {
		t.Fatal(err)
	}
	return paths, unit
}

func startDaemon(t *testing.T, paths config.Paths) *api.Client {
	t.Helper()
	d, err := daemon.New(daemon.Config{
		Paths:    paths,
		Settings: fastSettings(),
		Registry: testRegistry(t),
		Version:  "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v on graceful shutdown", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("daemon did not shut down within 5s")
		}
	})

	socketPath, err := daemon.SocketPathForClient()
	if err != nil {
		t.Fatal(err)
	}
	client := api.NewClient(socketPath)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := client.Status(context.Background()); err == nil {
			return client
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon API never came up")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDaemonWatchesSyncsAndReports(t *testing.T) {
	paths, unit := newDaemonEnv(t)
	client := startDaemon(t, paths)

	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "ready" || status.Version != "test" {
		t.Fatalf("status = %+v, want ready/test", status)
	}

	// Run exports the conflict-log path process-wide so the Phase-1 merge
	// driver (a git child of integrate) records retain-both events for the
	// Phase-3 conflicts view (spec §4). Daemon tests are serial (newDaemonEnv
	// uses t.Setenv), so the live daemon's value is deterministic here.
	if got := os.Getenv("AGENT_BRAIN_CONFLICT_LOG"); got != paths.ConflictLogFile() {
		t.Fatalf("AGENT_BRAIN_CONFLICT_LOG = %q, want %q", got, paths.ConflictLogFile())
	}

	// A file written into the enrolled dir must flow through watch →
	// debounce → cycle → commit → push, no manual trigger involved.
	if err := os.MkdirAll(filepath.Join(unit.LocalDir, "memories"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unit.LocalDir, "memories", "fact.md"), []byte("watched\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, err := client.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if status.LastSync != nil && status.LastSync.Pushed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pushed cycle within deadline; last status %+v", status)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Manual trigger returns a completed cycle synchronously.
	syncResp, err := client.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if syncResp.Status != "completed" || syncResp.Summary == nil {
		t.Fatalf("sync = %+v, want completed with summary", syncResp)
	}

	projects, err := client.Projects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(projects.Units) != 1 || projects.Units[0].Folder != "alpha" {
		t.Fatalf("projects = %+v", projects)
	}
}

func TestSecondDaemonRefusesToStart(t *testing.T) {
	paths, _ := newDaemonEnv(t)
	startDaemon(t, paths)

	second, err := daemon.New(daemon.Config{Paths: paths, Settings: fastSettings(), Registry: testRegistry(t), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Run(context.Background()); !errors.Is(err, daemon.ErrAlreadyRunning) {
		t.Fatalf("second Run = %v, want ErrAlreadyRunning", err)
	}
}

func TestDaemonUninitializedRepoIsHonest(t *testing.T) {
	base, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	paths := config.Paths{ConfigDir: filepath.Join(base, "cfg"), DataDir: filepath.Join(base, "data")}
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", filepath.Join(base, "run"))

	client := startDaemon(t, paths)
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "uninitialized" {
		t.Fatalf("State = %q, want uninitialized", status.State)
	}
	if _, err := client.Sync(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("sync on uninitialized repo: err = %v, want actionable message", err)
	}
}

func TestLogRotationOnStart(t *testing.T) {
	paths, _ := newDaemonEnv(t)
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 11<<20)
	if err := os.WriteFile(paths.DaemonLogFile(), big, 0o600); err != nil {
		t.Fatal(err)
	}
	startDaemon(t, paths)
	rotated, err := os.Stat(paths.DaemonLogFile() + ".1")
	if err != nil {
		t.Fatal("oversized log was not rotated:", err)
	}
	if rotated.Size() != int64(len(big)) {
		t.Fatalf("rotated size = %d, want %d", rotated.Size(), len(big))
	}
	current, err := os.Stat(paths.DaemonLogFile())
	if err != nil {
		t.Fatal(err)
	}
	if current.Size() >= int64(len(big)) {
		t.Fatal("fresh log did not start small")
	}
}
```

(These tests use `t.Setenv`, so no `t.Parallel()` in this file.)

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go get github.com/gofrs/flock@latest github.com/cenkalti/backoff/v5@v5.0.3 && go test ./internal/daemon/ 2>&1 | head -10`
Expected: compile errors.

- [ ] **Step 3: Implement** — `internal/daemon/logging.go`:

```go
package daemon

import (
	"fmt"
	"log/slog"
	"os"
)

// maxLogSize triggers start-time rotation; one .1 generation is kept.
// A resident single-user daemon does not need a log-management stack.
const maxLogSize = 10 << 20

// openLogger rotates an oversized log and returns a JSON slog logger
// plus the file to close on shutdown.
func openLogger(logPath string) (*slog.Logger, *os.File, error) {
	if info, err := os.Stat(logPath); err == nil && info.Size() > maxLogSize {
		if err := os.Rename(logPath, logPath+".1"); err != nil {
			return nil, nil, fmt.Errorf("rotate log: %w", err)
		}
	}
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open log: %w", err)
	}
	return slog.New(slog.NewJSONHandler(file, nil)), file, nil
}
```

`internal/daemon/rlimit_unix.go`:

```go
//go:build unix

package daemon

import "golang.org/x/sys/unix"

// raiseFDLimit lifts RLIMIT_NOFILE toward 4096 (capped by the hard
// limit). kqueue on macOS consumes one descriptor per watched
// directory (ADR 07); default soft limits (256 on macOS) are too low
// for comfort. Best-effort: failure is logged, not fatal.
func raiseFDLimit() error {
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		return err
	}
	const want = 4096
	if limit.Cur >= want {
		return nil
	}
	limit.Cur = want
	if limit.Max < want {
		limit.Cur = limit.Max
	}
	return unix.Setrlimit(unix.RLIMIT_NOFILE, &limit)
}
```

`internal/daemon/daemon.go`:

```go
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/gofrs/flock"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/engine"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/repo"
	"github.com/Sawmonabo/agent-brain/internal/watch"
)

// ErrAlreadyRunning means another daemon holds the flock.
var ErrAlreadyRunning = errors.New("agent-brain daemon is already running")

// syncWaitTimeout bounds how long POST /v0/sync waits for its cycle.
const syncWaitTimeout = 60 * time.Second

// Config wires the daemon. Registry is injected so the composition
// root (cmd layer) decides which providers exist — Phase 2 runs an
// empty or fake registry; Phase 3 plugs in claude/codex.
type Config struct {
	Paths    config.Paths
	Settings config.Settings
	Registry *provider.Registry
	Version  string
	// Logger overrides the file logger (tests). nil → JSON logger on
	// the size-rotated DaemonLogFile.
	Logger *slog.Logger
}

type syncRequest struct {
	reply chan api.SyncResponse
}

// Daemon is the resident process: one engine goroutine, a watch
// manager, and the UDS API (ADR 04).
type Daemon struct {
	cfg Config

	mu        sync.Mutex
	startedAt time.Time
	state     string
	lastSync  *api.SyncSummary
	degraded  map[string]bool

	syncRequests chan syncRequest
}

// New validates config; all I/O happens in Run.
func New(cfg Config) (*Daemon, error) {
	if cfg.Registry == nil {
		return nil, errors.New("daemon: registry must not be nil")
	}
	if cfg.Paths.ConfigDir == "" || cfg.Paths.DataDir == "" {
		return nil, errors.New("daemon: paths must be populated")
	}
	return &Daemon{
		cfg:          cfg,
		state:        "uninitialized",
		degraded:     map[string]bool{},
		syncRequests: make(chan syncRequest),
	}, nil
}

// SocketPathForClient derives the socket path the CLI dials — the one
// path derivation shared by both sides (ADR 09).
func SocketPathForClient() (string, error) {
	runtimeDir, err := config.RuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(runtimeDir, config.SocketName), nil
}

// Run blocks until ctx is cancelled (graceful shutdown, returns nil) or
// startup fails. Startup order matters: runtime dir → flock → logging →
// rlimit → engine/watch → API → loop.
func (d *Daemon) Run(ctx context.Context) error {
	runtimeDir, err := config.RuntimeDir()
	if err != nil {
		return err
	}
	// 0700 every start: WSL2 tears the runtime dir down across
	// restarts, and a pre-existing looser mode must be corrected.
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return fmt.Errorf("runtime dir: %w", err)
	}
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		return fmt.Errorf("runtime dir mode: %w", err)
	}
	socketPath := filepath.Join(runtimeDir, config.SocketName)
	if err := config.ValidateSocketPath(socketPath); err != nil {
		return err
	}

	lock := flock.New(filepath.Join(runtimeDir, config.LockName))
	locked, err := lock.TryLock()
	if err != nil {
		return fmt.Errorf("lock: %w", err)
	}
	if !locked {
		return ErrAlreadyRunning
	}
	defer func() { _ = lock.Unlock() }()

	// The data dir hosts the daemon log AND the conflict log — create it
	// unconditionally, not only when this process owns the logger.
	if err := os.MkdirAll(d.cfg.Paths.DataDir, 0o700); err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	logger := d.cfg.Logger
	if logger == nil {
		fileLogger, logFile, err := openLogger(d.cfg.Paths.DaemonLogFile())
		if err != nil {
			return err
		}
		defer logFile.Close()
		logger = fileLogger
	}
	if err := raiseFDLimit(); err != nil {
		logger.Warn("raise fd limit failed", "error", err)
	}

	// The Phase-1 merge driver records retain-both events only when
	// AGENT_BRAIN_CONFLICT_LOG is set (spec §4: the driver "records the
	// event for the dashboard conflicts view"). Export it process-wide so
	// every git child spawned during integrate inherits it; Phase 3's
	// conflicts view reads this file.
	if err := os.Setenv("AGENT_BRAIN_CONFLICT_LOG", d.cfg.Paths.ConflictLogFile()); err != nil {
		return fmt.Errorf("conflict log env: %w", err)
	}

	host := repo.SanitizeHostname(hostname())
	syncEngine, err := engine.New(d.cfg.Paths.MemoriesDir(), host, d.cfg.Registry, time.Now)
	if err != nil {
		return err
	}

	watchManager, err := watch.New(watch.Config{
		Debounce: time.Duration(d.cfg.Settings.Sync.Debounce),
		Poll:     time.Duration(d.cfg.Settings.Sync.Poll),
	})
	if err != nil {
		return err
	}
	defer watchManager.Close()
	units, err := repo.LoadLocalRegistry(d.cfg.Paths.LocalRegistryFile())
	if err != nil {
		return err
	}
	for _, u := range units.Units {
		if err := watchManager.Add(u.LocalDir); err != nil {
			logger.Warn("watch root not attached", "dir", u.LocalDir, "error", err)
		}
	}
	go func() {
		if err := watchManager.Run(ctx); err != nil {
			logger.Error("watch manager died", "error", err)
		}
	}()

	listener, err := listenSocket(socketPath)
	if err != nil {
		return err
	}
	server := newServer(d, defaultPeerUID)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("api server died", "error", err)
		}
	}()

	d.mu.Lock()
	d.startedAt = time.Now().UTC()
	d.state = d.checkoutState()
	d.mu.Unlock()
	logger.Info("daemon started", "version", d.cfg.Version, "socket", socketPath, "state", d.checkoutState())

	d.loop(ctx, syncEngine, watchManager, logger)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("api shutdown", "error", err)
	}
	logger.Info("daemon stopped")
	return nil
}

// loop is THE engine goroutine: every cycle — watch, ticker, manual —
// funnels through this single select (spec §4 single-writer rule).
func (d *Daemon) loop(ctx context.Context, syncEngine *engine.Engine, watchManager *watch.Manager, logger *slog.Logger) {
	ticker := time.NewTicker(time.Duration(d.cfg.Settings.Sync.Ticker))
	defer ticker.Stop()

	retryPolicy := backoff.NewExponentialBackOff()
	retryPolicy.InitialInterval = 5 * time.Second
	retryPolicy.MaxInterval = 5 * time.Minute
	var retryC <-chan time.Time

	runCycle := func(reason string) {
		summary := d.runCycle(ctx, syncEngine, logger, reason)
		if summary != nil && summary.Error != "" {
			retryC = time.After(retryPolicy.NextBackOff())
		} else {
			retryPolicy.Reset()
			retryC = nil
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case trigger := <-watchManager.Triggers():
			runCycle(trigger.Reason)
		case <-ticker.C:
			runCycle("ticker")
		case <-retryC:
			runCycle("retry")
		case request := <-d.syncRequests:
			runCycle("manual")
			d.mu.Lock()
			last := d.lastSync
			d.mu.Unlock()
			request.reply <- api.SyncResponse{Status: "completed", Summary: last}
		}
	}
}

// runCycle loads units fresh (Phase-3 enrollments apply without a
// restart), runs the engine, and records the outcome.
func (d *Daemon) runCycle(ctx context.Context, syncEngine *engine.Engine, logger *slog.Logger, reason string) *api.SyncSummary {
	if d.checkoutState() != "ready" {
		d.mu.Lock()
		d.state = "uninitialized"
		d.mu.Unlock()
		return nil
	}
	registry, err := repo.LoadLocalRegistry(d.cfg.Paths.LocalRegistryFile())
	if err != nil {
		logger.Error("load local registry", "error", err)
		summary := &api.SyncSummary{At: time.Now().UTC(), Error: err.Error()}
		d.record(summary)
		return summary
	}
	report, err := syncEngine.Sync(ctx, registry.Units)
	summary := toSummary(report)
	if err != nil {
		summary.Error = err.Error()
		logger.Error("sync cycle failed", "reason", reason, "error", err)
	} else {
		logger.Info("sync cycle", "reason", reason,
			"commits", len(report.Commits), "pushed", report.Pushed, "degraded", report.Degraded)
	}
	d.record(summary)
	return summary
}

func (d *Daemon) record(summary *api.SyncSummary) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.state = d.checkoutState()
	d.lastSync = summary
	d.degraded = map[string]bool{}
	for _, folder := range summary.Degraded {
		d.degraded[folder] = true
	}
}

func toSummary(report engine.Report) *api.SyncSummary {
	return &api.SyncSummary{
		At:         time.Now().UTC(),
		Commits:    report.Commits,
		MirrorIn:   api.Stats(report.MirrorIn),
		MirrorOut:  api.Stats(report.MirrorOut),
		Degraded:   report.Degraded,
		Pushed:     report.Pushed,
		PushQueued: report.PushQueued,
	}
}

func (d *Daemon) checkoutState() string {
	if info, err := os.Stat(filepath.Join(d.cfg.Paths.MemoriesDir(), ".git")); err == nil && info.IsDir() {
		return "ready"
	}
	return "uninitialized"
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "unknown-host"
	}
	return name
}

// --- controller implementation (Task 10 interface) ---

// Status implements controller.
func (d *Daemon) Status() api.StatusResponse {
	d.mu.Lock()
	defer d.mu.Unlock()
	return api.StatusResponse{
		Version:   d.cfg.Version,
		State:     d.state,
		PID:       os.Getpid(),
		StartedAt: d.startedAt,
		LastSync:  d.lastSync,
	}
}

// TriggerSync implements controller: hand the request to the loop,
// wait bounded, report "running" on timeout. Client cancellation never
// cancels the cycle itself.
func (d *Daemon) TriggerSync(ctx context.Context) (api.SyncResponse, error) {
	if d.checkoutState() != "ready" {
		return api.SyncResponse{}, errors.New("memories repo not initialized on this machine (agent-brain init arrives in Phase 3)")
	}
	request := syncRequest{reply: make(chan api.SyncResponse, 1)}
	timeout := time.After(syncWaitTimeout)
	select {
	case d.syncRequests <- request:
	case <-timeout:
		return api.SyncResponse{Status: "running"}, nil
	case <-ctx.Done():
		return api.SyncResponse{}, ctx.Err()
	}
	select {
	case response := <-request.reply:
		return response, nil
	case <-timeout:
		return api.SyncResponse{Status: "running"}, nil
	case <-ctx.Done():
		return api.SyncResponse{}, ctx.Err()
	}
}

// Projects implements controller.
func (d *Daemon) Projects() api.ProjectsResponse {
	response := api.ProjectsResponse{Units: []api.UnitInfo{}}
	registry, err := repo.LoadLocalRegistry(d.cfg.Paths.LocalRegistryFile())
	if err != nil {
		return response
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, u := range registry.Units {
		response.Units = append(response.Units, api.UnitInfo{
			Provider: u.Provider,
			Folder:   u.Folder,
			LocalDir: u.LocalDir,
			Degraded: d.degraded[u.Folder],
		})
	}
	return response
}
```

Implementer notes:
- `api.Stats(report.MirrorIn)` is a legal struct conversion only if `api.Stats` and `engine.MirrorStats` keep identical field names/types/order — they do by construction; if the compiler disagrees, write the three fields explicitly rather than changing either type.
- `backoff/v5` moved elapsed-time stops into `Retry*` options; `ExponentialBackOff` used directly as an interval source (as here) has no elapsed cap — which is the decision documented above, not an accident to "fix".
- If `flock.TryLock` semantics differ on the resolved version (it has been stable for years), the two-daemon test is the contract; adjust the call, not the test.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/daemon/... -race -v 2>&1 | tail -30`
Expected: all PASS (Task 10 suite included).

- [ ] **Step 5: Lint, format, full suite, commit**

```bash
gofumpt -l -w . && golangci-lint run && go test ./... -race
git add internal/daemon/ go.mod go.sum
git commit -m "feat(daemon): lifecycle with flock, log rotation, single engine goroutine, bounded manual sync (ADRs 04, 09, 14)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 12: Service install + CLI commands (`daemon run`, `service …`, `sync`, `status`, `projects`) (ADRs 04, 05, 08)

Two pieces: `internal/service` wraps kardianos/service v1.3.0 behind a narrow `Controller` interface (so CLI tests fake it — **no live service installs in tests, ever**), and `internal/cli` gains the commands that make the daemon operable. Output is deliberately plain `fmt` text — the Charm TUI layer is Phase 3 (roadmap); these commands exist so Phase 2 exit criteria can exercise the full binary end to end.

Contracts:
- **kardianos config (ADR 04):** `UserService: true`; launchd gets explicit `RunAtLoad: true` (kardianos defaults it false; a login-started daemon is the product) and `KeepAlive: true`; `Arguments: ["daemon", "run"]` — the service manager just runs our binary; kardianos's own Run/Interface machinery is unused beyond a no-op program (its `Install` doesn't need it).
- **WSL2 refuses service install** with guidance (ADR 04: WSL2 is on-demand mode, Phase 4). Detection reads `/proc/version` for "microsoft" (case-insensitive) through an injected reader so both branches are unit-tested on any OS.
- **Binary path resolution:** `os.Executable()` + `filepath.EvalSymlinks` — a service pointing at a temp `go run` path or an unresolved symlink is a support ticket.
- CLI client commands derive the socket via `daemon.SocketPathForClient()` and turn `api.ErrDaemonNotRunning` into actionable text (`agent-brain service install`, or `agent-brain daemon run` in a terminal).
- Registry composition happens HERE (single site): Phase 2 passes an empty `provider.NewRegistry()`; Phase 3 registers claude/codex adapters at this same call site.

**Files:**
- Create: `internal/service/service.go`, `internal/service/wsl.go`, `internal/cli/daemon.go`, `internal/cli/service.go`, `internal/cli/client_commands.go`
- Modify: `internal/cli/root.go` (AddCommand calls only), `go.mod` (`go get github.com/kardianos/service@v1.3.0`)
- Test: `internal/service/service_test.go`, `internal/cli/client_commands_test.go`

**Interfaces:**
- Consumes: `daemon.New`/`Run`/`Config`/`SocketPathForClient`/`ErrAlreadyRunning`, `api.NewClient`/`ErrDaemonNotRunning`, `config.DefaultPaths`/`LoadSettings`, `provider.NewRegistry`, `cli.Version`.
- Produces:
  - `service.Controller` interface `{ Install() error; Uninstall() error; Start() error; Stop() error; Status() (Status, error) }` · `service.Status` (`StatusUnknown`/`StatusRunning`/`StatusStopped`/`StatusNotInstalled`) · `func service.NewController(binaryPath string) (Controller, error)` · `func service.IsWSL2() bool`
  - cobra commands: `agent-brain daemon run`, `agent-brain service install|uninstall|start|stop|status`, `agent-brain sync`, `agent-brain status`, `agent-brain projects`

- [ ] **Step 1: Write the failing tests** — `internal/service/service_test.go`:

```go
package service

import (
	"runtime"
	"testing"
)

func TestDetectWSL2(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		content string
		readErr bool
		want    bool
	}{
		{"wsl2 kernel", "Linux version 5.15.167.4-microsoft-standard-WSL2", false, true},
		{"wsl1 kernel", "Linux version 4.4.0-19041-Microsoft", false, true},
		{"native linux", "Linux version 6.8.0-45-generic (buildd@lcy02)", false, false},
		{"unreadable", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			read := func(string) ([]byte, error) {
				if tc.readErr {
					return nil, errFake
				}
				return []byte(tc.content), nil
			}
			if got := detectWSL2(read); got != tc.want {
				t.Fatalf("detectWSL2 = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewControllerConstructsWithoutTouchingSystem(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("phase 2 targets darwin/linux")
	}
	controller, err := NewController("/usr/local/bin/agent-brain")
	if err != nil {
		t.Fatal(err)
	}
	if controller == nil {
		t.Fatal("nil controller")
	}
	// Construction only — Install/Start would touch the live system and
	// are exercised manually (exit criteria), never in tests.
}

func TestNewControllerRejectsRelativePath(t *testing.T) {
	t.Parallel()
	if _, err := NewController("agent-brain"); err == nil {
		t.Fatal("relative binary path accepted")
	}
}
```

(Define `var errFake = errors.New("fake")` in the test file; import `"errors"`.)

`internal/cli/client_commands_test.go` — the CLI talks to a tiny in-test fake daemon over a real UDS:

```go
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
)

// startFakeDaemon serves canned API responses on a short-path socket and
// points the CLI at it via AGENT_BRAIN_RUNTIME_DIR (so
// daemon.SocketPathForClient resolves to it). t.Setenv ⇒ no t.Parallel.
func startFakeDaemon(t *testing.T, status api.StatusResponse, sync api.SyncResponse, projects api.ProjectsResponse) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", dir)

	mux := http.NewServeMux()
	mux.HandleFunc("/v0/status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(status)
	})
	mux.HandleFunc("/v0/sync", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(sync)
	})
	mux.HandleFunc("/v0/projects", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(projects)
	})
	listener, err := net.Listen("unix", filepath.Join(dir, "agent-brain.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
}

func runCommand(t *testing.T, args ...string) string {
	t.Helper()
	root := Root()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("%v: %v\noutput:\n%s", args, err, out.String())
	}
	return out.String()
}

func TestStatusCommandPrintsState(t *testing.T) {
	startFakeDaemon(t,
		api.StatusResponse{Version: "1.2.3", State: "ready", PID: 99,
			LastSync: &api.SyncSummary{Pushed: true, Degraded: []string{"alpha"}}},
		api.SyncResponse{}, api.ProjectsResponse{})
	out := runCommand(t, "status")
	for _, want := range []string{"ready", "1.2.3", "alpha"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Fatalf("status output missing %q:\n%s", want, out)
		}
	}
}

func TestSyncCommandPrintsSummary(t *testing.T) {
	startFakeDaemon(t, api.StatusResponse{},
		api.SyncResponse{Status: "completed", Summary: &api.SyncSummary{
			Commits: []string{"memory: host-a alpha 2026-07-08T12:00:00Z"}, Pushed: true,
		}}, api.ProjectsResponse{})
	out := runCommand(t, "sync")
	if !bytes.Contains([]byte(out), []byte("memory: host-a alpha")) || !bytes.Contains([]byte(out), []byte("pushed")) {
		t.Fatalf("sync output:\n%s", out)
	}
}

func TestProjectsCommandListsUnits(t *testing.T) {
	startFakeDaemon(t, api.StatusResponse{}, api.SyncResponse{},
		api.ProjectsResponse{Units: []api.UnitInfo{
			{Provider: "claude", Folder: "alpha", LocalDir: "/p/.claude/memory", Degraded: true},
		}})
	out := runCommand(t, "projects")
	for _, want := range []string{"claude", "alpha", "degraded"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Fatalf("projects output missing %q:\n%s", want, out)
		}
	}
}

func TestClientCommandsExplainDeadDaemon(t *testing.T) {
	dir, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", dir) // no socket inside

	root := Root()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"status"})
	if err := root.Execute(); err == nil {
		t.Fatal("status against dead daemon succeeded")
	} else if !bytes.Contains([]byte(err.Error()), []byte("service install")) {
		t.Fatalf("error lacks guidance: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go get github.com/kardianos/service@v1.3.0 && go test ./internal/service/ ./internal/cli/ 2>&1 | head -10`
Expected: compile errors.

- [ ] **Step 3: Implement** — `internal/service/wsl.go`:

```go
package service

import (
	"os"
	"strings"
)

// IsWSL2 reports whether we're inside WSL (ADR 04: service install is
// refused there; WSL2 runs on-demand mode, Phase 4).
func IsWSL2() bool { return detectWSL2(os.ReadFile) }

// detectWSL2 takes the reader as a seam so both branches unit-test on
// any OS. Any WSL kernel (1 or 2) brands /proc/version "microsoft".
func detectWSL2(readFile func(string) ([]byte, error)) bool {
	content, err := readFile("/proc/version")
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(content)), "microsoft")
}
```

`internal/service/service.go`:

```go
// Package service manages the login-started daemon via
// kardianos/service (ADR 04), wrapped in a narrow interface so nothing
// else in the codebase touches the live service manager — and tests
// never do.
package service

import (
	"fmt"
	"path/filepath"

	kardianos "github.com/kardianos/service"
)

// Status is the coarse service state the CLI reports.
type Status int

const (
	StatusUnknown Status = iota
	StatusRunning
	StatusStopped
	StatusNotInstalled
)

func (s Status) String() string {
	switch s {
	case StatusRunning:
		return "running"
	case StatusStopped:
		return "stopped"
	case StatusNotInstalled:
		return "not installed"
	default:
		return "unknown"
	}
}

// Controller is the fake-able surface over the service manager.
type Controller interface {
	Install() error
	Uninstall() error
	Start() error
	Stop() error
	Status() (Status, error)
}

// noopProgram satisfies kardianos.Interface; install/uninstall/start/
// stop never invoke it — the daemon process is self-sufficient
// (`agent-brain daemon run`), the service manager just launches it.
type noopProgram struct{}

func (noopProgram) Start(kardianos.Service) error { return nil }
func (noopProgram) Stop(kardianos.Service) error  { return nil }

type kardianosController struct {
	svc kardianos.Service
}

// NewController builds the launchd/systemd-user controller for the
// given absolute binary path (ADR 04: UserService; launchd RunAtLoad
// must be set explicitly — kardianos defaults it false).
func NewController(binaryPath string) (Controller, error) {
	if !filepath.IsAbs(binaryPath) {
		return nil, fmt.Errorf("service: binary path %q must be absolute", binaryPath)
	}
	cfg := &kardianos.Config{
		Name:        "agent-brain",
		DisplayName: "agent-brain memory sync",
		Description: "Syncs AI coding agents' per-project memory across machines.",
		Executable:  binaryPath,
		Arguments:   []string{"daemon", "run"},
		Option: kardianos.KeyValue{
			"UserService": true,
			"RunAtLoad":   true,
			"KeepAlive":   true,
		},
	}
	svc, err := kardianos.New(noopProgram{}, cfg)
	if err != nil {
		return nil, fmt.Errorf("service: %w", err)
	}
	return &kardianosController{svc: svc}, nil
}

func (c *kardianosController) Install() error   { return c.svc.Install() }
func (c *kardianosController) Uninstall() error { return c.svc.Uninstall() }
func (c *kardianosController) Start() error     { return c.svc.Start() }
func (c *kardianosController) Stop() error      { return c.svc.Stop() }

func (c *kardianosController) Status() (Status, error) {
	status, err := c.svc.Status()
	if err == kardianos.ErrNotInstalled {
		return StatusNotInstalled, nil
	}
	if err != nil {
		return StatusUnknown, err
	}
	switch status {
	case kardianos.StatusRunning:
		return StatusRunning, nil
	case kardianos.StatusStopped:
		return StatusStopped, nil
	default:
		return StatusUnknown, nil
	}
}
```

(If the resolved kardianos version reports not-installed differently — `errors.Is` vs equality — follow the library; the CLI contract is that `status` on a never-installed service prints "not installed" without erroring.)

`internal/cli/daemon.go`:

```go
package cli

import (
	"fmt"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon"
	"github.com/Sawmonabo/agent-brain/internal/provider"
)

// buildRegistry is THE provider composition site. Phase 2 ships no
// adapters; Phase 3 registers claude and codex here and nowhere else.
func buildRegistry() (*provider.Registry, error) {
	return provider.NewRegistry()
}

func newDaemonCmd() *cobra.Command {
	daemonCmd := &cobra.Command{
		Use:   "daemon",
		Short: "Daemon process control",
	}
	daemonCmd.AddCommand(&cobra.Command{
		Use:   "run",
		Short: "Run the sync daemon in the foreground (the service manager invokes this)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.DefaultPaths()
			if err != nil {
				return err
			}
			settings, err := config.LoadSettings(paths.SettingsFile())
			if err != nil {
				return err
			}
			registry, err := buildRegistry()
			if err != nil {
				return err
			}
			d, err := daemon.New(daemon.Config{
				Paths:    paths,
				Settings: settings,
				Registry: registry,
				Version:  Version,
			})
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			fmt.Fprintln(cmd.OutOrStdout(), "agent-brain daemon starting (Ctrl-C to stop)")
			return d.Run(ctx)
		},
	})
	return daemonCmd
}
```

`internal/cli/service.go`:

```go
package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/service"
)

// resolveBinary pins the service definition to the real installed
// binary, not a symlink or a go-run temp path.
func resolveBinary() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(executable)
}

func newServiceCmd() *cobra.Command {
	serviceCmd := &cobra.Command{
		Use:   "service",
		Short: "Install or control the login-started daemon service",
	}
	controllerFor := func() (service.Controller, error) {
		binaryPath, err := resolveBinary()
		if err != nil {
			return nil, err
		}
		return service.NewController(binaryPath)
	}
	run := func(action string, act func(service.Controller) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, _ []string) error {
			controller, err := controllerFor()
			if err != nil {
				return err
			}
			if err := act(controller); err != nil {
				return fmt.Errorf("service %s: %w", action, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "service %s: ok\n", action)
			return nil
		}
	}

	serviceCmd.AddCommand(
		&cobra.Command{
			Use:   "install",
			Short: "Install the user service (launchd / systemd --user)",
			RunE: func(cmd *cobra.Command, args []string) error {
				if service.IsWSL2() {
					return fmt.Errorf("service install is not supported on WSL2 — WSL lacks a reliable login service manager; on-demand mode arrives in Phase 4. Run `agent-brain daemon run` in a terminal for now")
				}
				return run("install", service.Controller.Install)(cmd, args)
			},
		},
		&cobra.Command{Use: "uninstall", Short: "Remove the user service", RunE: run("uninstall", service.Controller.Uninstall)},
		&cobra.Command{Use: "start", Short: "Start the service", RunE: run("start", service.Controller.Start)},
		&cobra.Command{Use: "stop", Short: "Stop the service", RunE: run("stop", service.Controller.Stop)},
		&cobra.Command{
			Use:   "status",
			Short: "Report service state",
			RunE: func(cmd *cobra.Command, _ []string) error {
				controller, err := controllerFor()
				if err != nil {
					return err
				}
				status, err := controller.Status()
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "service: %s\n", status)
				return nil
			},
		},
	)
	return serviceCmd
}
```

`internal/cli/client_commands.go`:

```go
package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/daemon"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
)

// newAPIClient dials the daemon, translating a dead socket into
// guidance instead of a raw dial error.
func newAPIClient() (*api.Client, error) {
	socketPath, err := daemon.SocketPathForClient()
	if err != nil {
		return nil, err
	}
	return api.NewClient(socketPath), nil
}

func explainDown(err error) error {
	if errors.Is(err, api.ErrDaemonNotRunning) {
		return fmt.Errorf("%w\nStart it with `agent-brain service install` (login service) or `agent-brain daemon run` (foreground)", err)
	}
	return err
}

func printSummary(cmd *cobra.Command, summary *api.SyncSummary) {
	if summary == nil {
		return
	}
	if summary.Error != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "  error: %s\n", summary.Error)
	}
	for _, subject := range summary.Commits {
		fmt.Fprintf(cmd.OutOrStdout(), "  commit: %s\n", subject)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "  in: %d copied / %d deleted / %d skipped\n",
		summary.MirrorIn.Copied, summary.MirrorIn.Deleted, summary.MirrorIn.Skipped)
	fmt.Fprintf(cmd.OutOrStdout(), "  out: %d copied / %d deleted / %d skipped\n",
		summary.MirrorOut.Copied, summary.MirrorOut.Deleted, summary.MirrorOut.Skipped)
	fmt.Fprintf(cmd.OutOrStdout(), "  pushed: %v  queued: %v\n", summary.Pushed, summary.PushQueued)
	if len(summary.Degraded) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "  degraded: %v\n", summary.Degraded)
	}
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show daemon state and the last sync cycle",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newAPIClient()
			if err != nil {
				return err
			}
			status, err := client.Status(cmd.Context())
			if err != nil {
				return explainDown(err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "daemon: %s (version %s, pid %d)\n", status.State, status.Version, status.PID)
			if status.LastSync == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "last sync: never")
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "last sync: %s\n", status.LastSync.At.Format("2006-01-02 15:04:05 MST"))
			printSummary(cmd, status.LastSync)
			return nil
		},
	}
}

func newSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Trigger a sync cycle now and report the outcome",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newAPIClient()
			if err != nil {
				return err
			}
			response, err := client.Sync(cmd.Context())
			if err != nil {
				return explainDown(err)
			}
			if response.Status == "running" {
				fmt.Fprintln(cmd.OutOrStdout(), "sync still running — check `agent-brain status`")
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), "sync completed")
			printSummary(cmd, response.Summary)
			return nil
		},
	}
}

func newProjectsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "projects",
		Short: "List enrolled projects and their health",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newAPIClient()
			if err != nil {
				return err
			}
			projects, err := client.Projects(cmd.Context())
			if err != nil {
				return explainDown(err)
			}
			if len(projects.Units) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no projects enrolled (enrollment arrives with Phase 3's init/track)")
				return nil
			}
			for _, unit := range projects.Units {
				health := "ok"
				if unit.Degraded {
					health = "degraded"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-8s %-24s %-9s %s\n", unit.Provider, unit.Folder, health, unit.LocalDir)
			}
			return nil
		},
	}
}
```

Modify `internal/cli/root.go` — extend the existing AddCommand call (SilenceUsage/SilenceErrors stay untouched):

```go
	root.AddCommand(
		newGitCleanCmd(),
		newGitSmudgeCmd(),
		newGitTextconvCmd(),
		newGitMergeCmd(),
		newDaemonCmd(),
		newServiceCmd(),
		newStatusCmd(),
		newSyncCmd(),
		newProjectsCmd(),
	)
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/service/ ./internal/cli/ -race -v 2>&1 | tail -25`
Expected: all PASS.

- [ ] **Step 5: Lint, format, full suite, commit**

```bash
gofumpt -l -w . && golangci-lint run && go test ./... -race
git add internal/service/ internal/cli/ go.mod go.sum
git commit -m "feat(cli,service): daemon run, service control with WSL2 refusal, plain status/sync/projects (ADRs 04, 05)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 13: End-to-end — two machines, real crypto, full engine pipeline (spec §12's "only way to test the whole loop")

The money test: two simulated machines (independent clones + provider homes + hosts) share one bare remote and one Tink keyset, with the REAL agent-brain binary installed as clean/smudge/merge driver — the complete spec §4+§5 pipeline driven through `engine.Sync`. Asserts:

1. **Convergence** — a fact written in A's provider dir appears in B's provider dir after `A.Sync; B.Sync`.
2. **Ciphertext on the wire** — the bare remote's blob for a memory file contains no plaintext (safety invariant), while `.agent-brain/**` (manifests) is readable plaintext (the Task 2 attributes exclusions doing their job).
3. **Retain-both** — a true concurrent edit of a fact file converges on BOTH machines to content holding both versions inside `agent-brain conflict` markers (the Phase-1 driver's format).
4. **LWW** — a concurrent edit of a Regenerated-class file converges with NO markers, both machines identical, content equal to exactly one of the two versions (which writer wins is the Phase-1 merge contract, not re-asserted here).
5. **Deletion via manifest** — a file deleted on A disappears from B's provider dir.

Degraded-withholds-mirror-out is deliberately NOT re-tested here: with the real driver installed, integration cannot be made to fail without sabotaging the driver config, and Tasks 7–8 already unit-test that ladder exhaustively.

This file REUSES the Phase-1 harness (`harness_test.go`, same package): `TestMain` already builds the binary into `binPath`, generates the suite-wide shared keyset, and exports hermetic env (`AGENT_BRAIN_CONFIG_DIR`, `GIT_CONFIG_GLOBAL/SYSTEM=/dev/null`) process-wide — duplicating any of that would fork the hermeticity guarantees. New helpers use collision-free names (`syncMachine`, `newSyncMachine`, `newTwoMachines`, `syncRegistry`) beside the harness's package-level `newMachine`/`writeFile`/`readFile`/`gitRun`/`newBareRepo`/`remoteBlob`.

**Files:**
- Create: `test/e2e/sync_engine_test.go`

**Interfaces:**
- Consumes: `engine.New`/`Sync`/`Report`, `repo` (NewLayout/WriteAttributes/Unit), `provider`/`providertest`; harness helpers `newBareRepo`, `newMachine`, `gitRun`, `remoteBlob` (+ `binPath`/keyset/hermetic env via `TestMain`).

- [ ] **Step 1: Write the test** — `test/e2e/sync_engine_test.go`:

```go
package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/engine"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/providertest"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// syncRegistry is the provider table these engine tests run under; the
// real claude/codex tables arrive in Phase 3.
func syncRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	fake := providertest.New("claude", provider.ScopePerProject, []provider.Pattern{
		{Glob: "MEMORY.md", Class: provider.ClassDerivedIndex},
		{Glob: "memories/**", Class: provider.ClassFact},
		{Glob: "summary.md", Class: provider.ClassRegenerated},
	})
	registry, err := provider.NewRegistry(fake)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

// syncMachine is one simulated machine: a filtered clone (harness
// newMachine — binary, shared keyset, and hermetic git env all come
// from TestMain), an engine bound to it, and a provider home dir.
type syncMachine struct {
	engine   *engine.Engine
	unit     repo.Unit
	checkout string
}

func newSyncMachine(t *testing.T, host, bare string, seed bool) *syncMachine {
	t.Helper()
	checkout := newMachine(t, host, bare)
	if seed {
		if err := repo.WriteAttributes(repo.NewLayout(checkout), syncRegistry(t)); err != nil {
			t.Fatal(err)
		}
		gitRun(t, checkout, "add", "-A")
		gitRun(t, checkout, "commit", "--quiet", "-m", "init: repo skeleton")
		gitRun(t, checkout, "push", "--quiet", "-u", "origin", "main")
	}
	syncEngine, err := engine.New(checkout, host, syncRegistry(t), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	localDir := filepath.Join(t.TempDir(), host, ".claude", "memory")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return &syncMachine{
		engine:   syncEngine,
		checkout: checkout,
		unit:     repo.Unit{Provider: "claude", ProjectID: "id-alpha", Folder: "alpha", LocalDir: localDir},
	}
}

// newTwoMachines is the full spec §3/§5 shape: one bare remote, the
// suite's shared keyset, machine A seeding the repo skeleton (as
// Phase-3 init will) and machine B cloning it.
func newTwoMachines(t *testing.T) (a, b *syncMachine, bare string) {
	t.Helper()
	bare = newBareRepo(t)
	a = newSyncMachine(t, "host-a", bare, true)
	b = newSyncMachine(t, "host-b", bare, false)
	return a, b, bare
}

func (m *syncMachine) sync(t *testing.T) engine.Report {
	t.Helper()
	report, err := m.engine.Sync(context.Background(), []repo.Unit{m.unit})
	if err != nil {
		t.Fatalf("sync %s: %v", m.checkout, err)
	}
	return report
}

func (m *syncMachine) write(t *testing.T, rel, content string) {
	t.Helper()
	full := filepath.Join(m.unit.LocalDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (m *syncMachine) read(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(m.unit.LocalDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func TestTwoMachineConvergenceWithCiphertextOnTheWire(t *testing.T) {
	a, b, bare := newTwoMachines(t)

	const plaintext = "the codebase uses table-driven tests exclusively\n"
	a.write(t, "memories/testing-style.md", plaintext)
	reportA := a.sync(t)
	if !reportA.Pushed {
		t.Fatalf("A did not push: %+v", reportA)
	}

	// Wire check BEFORE B ever sees it: the blob must carry the Phase-1
	// ciphertext magic (magicPrefix — package-level const in
	// roundtrip_test.go, same package) and must not leak plaintext.
	blob := remoteBlob(t, bare, "alpha/claude/memories/testing-style.md")
	if !strings.HasPrefix(blob, magicPrefix) {
		t.Fatal("remote blob is not agent-brain ciphertext (magic header missing)")
	}
	if strings.Contains(blob, "table-driven") {
		t.Fatal("SAFETY VIOLATION: plaintext memory content in a git object")
	}
	// ...while the manifest (attributes-excluded) is readable JSON.
	manifest := remoteBlob(t, bare, ".agent-brain/manifests/host-a.json")
	if !strings.Contains(manifest, "alpha/claude/memories/testing-style.md") {
		t.Fatalf("manifest not plaintext on the wire:\n%s", manifest)
	}

	b.sync(t)
	if got := b.read(t, "memories/testing-style.md"); got != plaintext {
		t.Fatalf("B's provider dir = %q, want %q", got, plaintext)
	}
}

func TestConcurrentFactEditsRetainBoth(t *testing.T) {
	a, b, _ := newTwoMachines(t)

	a.write(t, "memories/shared.md", "base version\n")
	a.sync(t)
	b.sync(t) // both machines now hold the base

	a.write(t, "memories/shared.md", "version from machine A\n")
	b.write(t, "memories/shared.md", "version from machine B\n")
	a.sync(t) // A wins the push race
	b.sync(t) // B integrates: driver emits retain-both, exits resolved
	a.sync(t) // A picks up the resolution

	for name, m := range map[string]*syncMachine{"A": a, "B": b} {
		content := m.read(t, "memories/shared.md")
		for _, want := range []string{"version from machine A", "version from machine B", "agent-brain conflict"} {
			if !strings.Contains(content, want) {
				t.Fatalf("machine %s missing %q:\n%s", name, want, content)
			}
		}
	}
	if a.read(t, "memories/shared.md") != b.read(t, "memories/shared.md") {
		t.Fatal("machines did not converge on identical retained content")
	}
}

func TestConcurrentRegeneratedEditsResolveLWW(t *testing.T) {
	a, b, _ := newTwoMachines(t)

	a.write(t, "summary.md", "base summary\n")
	a.sync(t)
	b.sync(t)

	a.write(t, "summary.md", "summary regenerated on A\n")
	b.write(t, "summary.md", "summary regenerated on B\n")
	a.sync(t)
	b.sync(t)
	a.sync(t)

	contentA, contentB := a.read(t, "summary.md"), b.read(t, "summary.md")
	if contentA != contentB {
		t.Fatalf("machines diverged:\nA: %q\nB: %q", contentA, contentB)
	}
	if strings.Contains(contentA, "conflict") {
		t.Fatalf("lww class produced conflict markers:\n%s", contentA)
	}
	if contentA != "summary regenerated on A\n" && contentA != "summary regenerated on B\n" {
		t.Fatalf("lww result is neither input: %q", contentA)
	}
}

func TestDeletionPropagatesViaManifest(t *testing.T) {
	a, b, _ := newTwoMachines(t)

	a.write(t, "memories/ephemeral.md", "short-lived\n")
	a.sync(t)
	b.sync(t)
	if _, err := os.Stat(filepath.Join(b.unit.LocalDir, "memories", "ephemeral.md")); err != nil {
		t.Fatal("file never reached B:", err)
	}

	if err := os.Remove(filepath.Join(a.unit.LocalDir, "memories", "ephemeral.md")); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	b.sync(t)
	if _, err := os.Stat(filepath.Join(b.unit.LocalDir, "memories", "ephemeral.md")); !os.IsNotExist(err) {
		t.Fatal("deletion did not propagate to B's provider dir")
	}
}
```

- [ ] **Step 2: Run the tests**

Run: `go test ./test/e2e/ -run 'TestTwoMachine|TestConcurrent|TestDeletion' -race -v 2>&1 | tail -30`
Expected: all PASS. These tests exercise every prior task at once — failures here localize to whichever assertion names the broken step.

- [ ] **Step 3: Run the complete suite, lint, commit**

```bash
gofumpt -l -w . && golangci-lint run && go test ./... -race
git add test/e2e/
git commit -m "test(e2e): two-machine engine pipeline with real crypto — convergence, retain-both, lww, manifest deletions (spec §12)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

## Phase 2 exit criteria

Every box checks before Phase 2 is called done (same bar as Phase 1):

- [ ] `go test ./... -race` fully green, including the two-machine e2e; `golangci-lint run` and `gofumpt -l .` clean; CI green on ubuntu + macos including govulncheck.
- [ ] **Zero plaintext on the wire**, engine-driven: the e2e's `cat-file` assertion passes — memory content blobs are ciphertext while `.agent-brain/**` stays readable.
- [ ] **Package boundaries hold** (spec §8): `go list -deps ./internal/engine | grep -E 'internal/(cli|daemon|crypto)$'` prints nothing; `go list -deps ./internal/daemon/api | grep internal/` prints only `internal/daemon/api` itself.
- [ ] **Daemon manual smoke on this Mac (human-run, not automated):** `go build ./... && ./agent-brain daemon run` in one terminal; in another: `agent-brain status` shows `uninitialized` honestly (no memories repo exists yet in production), `agent-brain projects` prints the empty-enrollment guidance, Ctrl-C shuts down gracefully. Optionally `service install` + `service status` + `service uninstall` — human judgment, and uninstall MUST be verified before calling it done.
- [ ] **Single-instance guard proven:** second `daemon run` while the first runs exits with the already-running error.
- [ ] Ledger (`.superpowers/sdd/progress.md`) records per-task commits and review outcomes; all task commits on `develop`; **nothing merged to `main`** (cutover order: retire the old system on all machines first).

## Phase 3 preview (context for reviewers; NOT in scope)

Phase 3 lights this machinery up for real users: provider adapters (claude, codex) with real `Patterns` tables and `ReconcileIndex` implementations registered at `buildRegistry()`; `DiscoverProjects`/`ResolveIdentity` joining the provider interface alongside the enrollment UX; `init` (repo provisioning via `gh`, keyset generate/import, config.toml template, service install); `track`/`untrack`; the Charm TUI dashboard (status/conflicts/doctor); dynamic watch-root refresh on enroll; testscript-driven CLI e2e. Phase 4 adds `migrate` from the bash-era system, retirement checks, GoReleaser + tap, and the WSL2 on-demand mode.













