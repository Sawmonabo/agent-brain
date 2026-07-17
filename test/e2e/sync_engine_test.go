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
// real claude/codex tables arrive later.
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
// init will) and machine B cloning it.
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
	t.Parallel()
	a, b, bare := newTwoMachines(t)

	const plaintext = "the codebase uses table-driven tests exclusively\n"
	a.write(t, "memories/testing-style.md", plaintext)
	reportA := a.sync(t)
	if !reportA.Pushed {
		t.Fatalf("A did not push: %+v", reportA)
	}

	// Wire check BEFORE B ever sees it: the blob must carry the
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

// TestHostileGitattributesCannotDisableEncryption is the spec §5 wire
// regression: a .gitattributes planted in a provider dir must never reach
// the memories repo. Were it mirrored into the unit subtree, its `* -filter`
// would override the checkout-root attributes and unset the encryption
// clean filter for its siblings — a sibling fact would then commit as
// PLAINTEXT and push to the remote in the clear.
func TestHostileGitattributesCannotDisableEncryption(t *testing.T) {
	t.Parallel()
	a, b, bare := newTwoMachines(t)

	const plaintext = "the vault combination is left-seven-right-two-two\n"
	a.write(t, ".gitattributes", "* -filter -diff -merge\n")
	a.write(t, "memories/secret-fact.md", plaintext)

	reportA := a.sync(t)
	if !reportA.Pushed {
		t.Fatalf("A did not push: %+v", reportA)
	}

	// (a) The fact is still ciphertext on the wire and never leaks plaintext.
	blob := remoteBlob(t, bare, "alpha/claude/memories/secret-fact.md")
	if !strings.HasPrefix(blob, magicPrefix) {
		t.Fatal("fact blob is not agent-brain ciphertext — the hostile .gitattributes disabled encryption")
	}
	if strings.Contains(blob, "vault combination") {
		t.Fatal("SAFETY VIOLATION: plaintext memory content reached a git object")
	}

	// (b) The hostile file never reached the remote tree at all. cat-file -e
	// exits non-zero when the path is absent from main's tree.
	if _, err := gitRunEnv(t, bare, nil, "cat-file", "-e", "main:alpha/claude/.gitattributes"); err == nil {
		t.Fatal("hostile .gitattributes reached the remote — nested attributes could override the root filter wiring")
	}

	// (c) B integrates: the fact arrives intact, the poison does not reach
	// B's provider dir.
	b.sync(t)
	if got := b.read(t, "memories/secret-fact.md"); got != plaintext {
		t.Fatalf("B's fact = %q, want %q", got, plaintext)
	}
	if _, err := os.Stat(filepath.Join(b.unit.LocalDir, ".gitattributes")); !os.IsNotExist(err) {
		t.Fatal("hostile .gitattributes propagated to B's provider dir")
	}
}

func TestConcurrentFactEditsRetainBoth(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
