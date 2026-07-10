package doctor_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/crypto"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/keys"
)

// fakeGitleaksOnPath puts a trivial executable named "gitleaks" on PATH —
// checkSecretsScan only probes presence (exec.LookPath), never runs it, so
// the script's content is irrelevant (mirrors doctor_test.go's fakeGhOnPath
// for the analogous "gh" presence check).
func fakeGitleaksOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gitleaks"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// minimalDeps builds just enough Deps for checkSecretsScan, which reads
// nothing from Deps at all (a pure PATH probe) — mirrors
// TestRunCheckoutNotAGitRepo's minimal, fixture-free Deps{} style rather
// than the full newFixture machine, since no git/keyset/checkout is
// needed here.
func minimalDeps(t *testing.T) doctor.Deps {
	t.Helper()
	base := t.TempDir()
	return doctor.Deps{
		Paths:    config.Paths{ConfigDir: filepath.Join(base, "cfg"), DataDir: filepath.Join(base, "data")},
		Registry: testRegistry(t),
		Home:     filepath.Join(base, "home"),
	}
}

func TestRunSecretsScanNotInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir: no gitleaks anywhere on PATH
	got := result(t, doctor.Run(context.Background(), minimalDeps(t)), "secrets-scan")
	if got.Status != doctor.StatusWarn {
		t.Fatalf("secrets-scan check = %+v, want warn", got)
	}
	if got.Fix == "" {
		t.Fatal("secrets-scan warn result has no Fix guidance")
	}
}

func TestRunSecretsScanInstalled(t *testing.T) {
	fakeGitleaksOnPath(t)
	got := result(t, doctor.Run(context.Background(), minimalDeps(t)), "secrets-scan")
	if got.Status != doctor.StatusOK {
		t.Fatalf("secrets-scan check = %+v, want ok", got)
	}
}

// commitRawBlob stages content at relPath via plumbing (hash-object +
// update-index --cacheinfo) and commits it — this package's fixture has no
// real agent-brain binary to run as the clean filter (unlike test/e2e's
// harness), so seeding a checkout with a SPECIFIC stored blob (precomputed
// ciphertext) has to bypass the filter entirely rather than go through a
// real `git add` (which would try to invoke it). --no-filters is required:
// plain `hash-object -w <file>` applies the clean filter chosen by
// attributes for <file>'s path by default (confirmed empirically — without
// it, this fails trying to exec a filter binary that does not exist in
// this package's fixtures), so without --no-filters this would try to
// double-clean already-encrypted content and fail exactly the way a real
// `git add` would.
func commitRawBlob(t *testing.T, dir, relPath string, content []byte) {
	t.Helper()
	ctx := context.Background()
	tmp := filepath.Join(t.TempDir(), "blob-content")
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		t.Fatal(err)
	}
	hashResult, err := gitx.Run(ctx, dir, "hash-object", "-w", "--no-filters", "--", tmp)
	if err != nil {
		t.Fatal(err)
	}
	blobHash := strings.TrimSpace(hashResult.Stdout)
	if _, err := gitx.Run(ctx, dir, "update-index", "--add", "--cacheinfo", "100644,"+blobHash+","+relPath); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Run(ctx, dir, "commit", "-m", "test: seed raw blob "+relPath); err != nil {
		t.Fatal(err)
	}
}

// TestRunKeysetDecryptNoCheckout pins the first Info bucket: a keyset that
// loads fine but no checkout to sample at all.
func TestRunKeysetDecryptNoCheckout(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	paths := config.Paths{ConfigDir: filepath.Join(base, "cfg"), DataDir: filepath.Join(base, "data")}
	if err := keys.Generate(paths.Keyset()); err != nil {
		t.Fatal(err)
	}
	deps := doctor.Deps{
		Paths:    paths,
		Registry: testRegistry(t),
		Home:     filepath.Join(base, "home"),
	}
	got := result(t, doctor.Run(context.Background(), deps), "keyset-decrypt")
	if got.Status != doctor.StatusInfo {
		t.Fatalf("keyset-decrypt with no checkout = %+v, want info", got)
	}
}

// TestRunKeysetDecryptUnbornHead pins the Info bucket's other empty state,
// distinct from TestRunKeysetDecryptNoCheckout: here the checkout DIRECTORY
// exists and is a real git repository (unlike no-checkout, where the
// directory does not exist at all), but has zero commits — HEAD is
// unborn, so it resolves neither via the fetched tracking ref nor HEAD.
// Must still report Info, never Warn.
func TestRunKeysetDecryptUnbornHead(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	paths := config.Paths{ConfigDir: filepath.Join(base, "cfg"), DataDir: filepath.Join(base, "data")}
	if err := keys.Generate(paths.Keyset()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.MemoriesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Run(context.Background(), paths.MemoriesDir(), "init", "--quiet"); err != nil {
		t.Fatal(err)
	}
	deps := doctor.Deps{
		Paths:    paths,
		Registry: testRegistry(t),
		Home:     filepath.Join(base, "home"),
	}
	got := result(t, doctor.Run(context.Background(), deps), "keyset-decrypt")
	if got.Status != doctor.StatusInfo {
		t.Fatalf("keyset-decrypt with an unborn HEAD = %+v, want info", got)
	}
}

// TestRunKeysetDecryptNoEncryptedBlobsYet pins the second Info bucket: a
// real checkout whose only commit is the skeleton (.gitattributes) — no
// fact has ever been written, so there is nothing to sample. It must never
// warn on this ordinary, healthy startup state.
func TestRunKeysetDecryptNoEncryptedBlobsYet(t *testing.T) {
	t.Parallel()
	fx := newFixture(t) // the bare skeleton commit: no fact ever written
	got := result(t, doctor.Run(context.Background(), fx.deps), "keyset-decrypt")
	if got.Status != doctor.StatusInfo {
		t.Fatalf("keyset-decrypt on a fact-free repo = %+v, want info", got)
	}
}

// TestRunKeysetDecryptOK pins the happy path: this machine's own keyset
// decrypts a fact it (self-consistently) encrypted and pushed itself.
func TestRunKeysetDecryptOK(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	primitive, err := keys.Primitive(fx.deps.Paths.Keyset())
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := crypto.NewCodec(primitive).Encrypt([]byte("a fact\n"))
	if err != nil {
		t.Fatal(err)
	}
	commitRawBlob(t, fx.dir, "memories/fact.md", ciphertext)
	mustGit(t, fx.dir, "push", "origin", "main")

	got := result(t, doctor.Run(context.Background(), fx.deps), "keyset-decrypt")
	if got.Status != doctor.StatusOK {
		t.Fatalf("keyset-decrypt with a self-encrypted fact = %+v, want ok", got)
	}
}

// TestRunKeysetDecryptStaleKeysetWarns recreates, at the doctor-package
// level, the exact git topology a real fleet rotation leaves a stale peer
// in (also proven end-to-end against the real engine in
// test/e2e/rotate_test.go): this machine's OWN HEAD holds a fact it
// encrypted with its own keyset (A) and has pushed, so HEAD and origin/main
// agree — then a "peer" (a second clone of the same bare remote) re-encrypts
// the SAME fact under a brand-new keyset (B) this machine never receives,
// and pushes. A plain `git fetch` — exactly what a real sync cycle's
// integrate step does even when the rebase/merge that follows then fails
// and rolls HEAD back (engine/integrate.go's all-or-nothing invariant) —
// advances only origin/main, leaving HEAD frozen; the probe has to read
// origin/main to ever see the content keyset A cannot decrypt.
func TestRunKeysetDecryptStaleKeysetWarns(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx := context.Background()

	primitiveA, err := keys.Primitive(fx.deps.Paths.Keyset())
	if err != nil {
		t.Fatal(err)
	}
	ciphertextA, err := crypto.NewCodec(primitiveA).Encrypt([]byte("shared fact\n"))
	if err != nil {
		t.Fatal(err)
	}
	commitRawBlob(t, fx.dir, "memories/fact.md", ciphertextA)
	mustGit(t, fx.dir, "push", "origin", "main")

	bare := filepath.Join(fx.base, "remote.git")
	peerDir := filepath.Join(fx.base, "peer")
	mustGit(t, fx.base, "clone", bare, peerDir)
	mustGit(t, peerDir, "config", "user.name", "peer")
	mustGit(t, peerDir, "config", "user.email", "peer@example.invalid")

	keysetB := filepath.Join(fx.base, "peer-keyset.json")
	if err := keys.Generate(keysetB); err != nil {
		t.Fatal(err)
	}
	primitiveB, err := keys.Primitive(keysetB)
	if err != nil {
		t.Fatal(err)
	}
	ciphertextB, err := crypto.NewCodec(primitiveB).Encrypt([]byte("shared fact\n"))
	if err != nil {
		t.Fatal(err)
	}
	commitRawBlob(t, peerDir, "memories/fact.md", ciphertextB)
	mustGit(t, peerDir, "push", "origin", "main")

	// Mirrors integrate()'s fetch: advances origin/main only. This
	// machine's own HEAD/main stays exactly where it was.
	mustGit(t, fx.dir, "fetch", "origin")

	got := result(t, doctor.Run(ctx, fx.deps), "keyset-decrypt")
	if got.Status != doctor.StatusWarn {
		t.Fatalf("keyset-decrypt with a stale keyset = %+v, want warn", got)
	}
	if !strings.Contains(got.Fix, "agent-brain key import --force") {
		t.Fatalf("keyset-decrypt fix = %q, missing verbatim `agent-brain key import --force`", got.Fix)
	}
}
