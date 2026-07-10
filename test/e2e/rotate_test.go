package e2e

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
	"github.com/Sawmonabo/agent-brain/internal/keys"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// useKeyset points the process-wide AGENT_BRAIN_CONFIG_DIR at dir, so the
// clean/smudge filter subprocesses git spawns from here on read that keyset
// (gitx has no per-call env injection — the filter resolves the keyset from
// the inherited process env). The rotation proof needs two machines on
// DISTINCT keysets, so these tests flip this between each machine's ops; that
// is only safe in a NON-parallel test (t.Setenv both asserts that and restores
// the suite keyset on cleanup).
func useKeyset(dir string) {
	_ = os.Setenv("AGENT_BRAIN_CONFIG_DIR", dir)
}

// copyKeyset installs a byte-identical copy of src's keyset.json under dst —
// the test's stand-in for `key export | key import --force`: the receiving
// machine ends up holding exactly the sender's key material.
func copyKeyset(t *testing.T, dst, src string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(src, "keyset.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "keyset.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// contains reports whether folder is in the degraded set.
func contains(folders []string, folder string) bool {
	return slices.Contains(folders, folder)
}

// doctorPathsFor bridges a syncMachine's checkout into a config.Paths for
// doctor.Run. config.Paths.MemoriesDir() is a fixed DataDir/"memories" join
// (internal/config/state.go) but checkout lives at the harness's own
// arbitrary temp path (newMachine's <tmp>/<host>) — a symlink is the least
// invasive way to satisfy that fixed join without reshaping the shared
// harness every other e2e test also relies on.
func doctorPathsFor(t *testing.T, checkout, configDir string) config.Paths {
	t.Helper()
	dataDir := t.TempDir()
	if err := os.Symlink(checkout, filepath.Join(dataDir, "memories")); err != nil {
		t.Fatal(err)
	}
	return config.Paths{ConfigDir: configDir, DataDir: dataDir}
}

// doctorResult is rotate_test.go's own copy of internal/doctor's test-only
// `result` helper — that one lives in package doctor_test and cannot be
// imported from here.
func doctorResult(t *testing.T, report doctor.Report, name string) doctor.CheckResult {
	t.Helper()
	for _, r := range report.Results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("doctor report has no check named %q; results: %+v", name, report.Results)
	return doctor.CheckResult{}
}

const (
	rotFact1 = "memories/testing-style.md"
	rotFact2 = "memories/architecture.md"
	rotText1 = "the codebase uses table-driven tests exclusively\n"
	rotText2 = "the engine is the single writer to the checkout\n"
)

// twoMachinesDistinctKeysets sets up the shared spec §5 shape for the rotation
// proofs: a bare remote plus machines A and B on SEPARATE keyset dirs (kb is a
// byte copy of ka, so both smudge the pre-rotation content). Rotation touches
// only ka, making B the stale peer. The caller must have installed the
// non-parallel + restore guard (t.Setenv AGENT_BRAIN_CONFIG_DIR) already.
func twoMachinesDistinctKeysets(t *testing.T) (a, b *syncMachine, bare, kaDir, kbDir string) {
	t.Helper()
	configRoot := t.TempDir()
	kaDir = filepath.Join(configRoot, "machine-a")
	kbDir = filepath.Join(configRoot, "machine-b")
	if err := keys.Generate(filepath.Join(kaDir, "keyset.json")); err != nil {
		t.Fatal(err)
	}
	copyKeyset(t, kbDir, kaDir)

	bare = newBareRepo(t)
	useKeyset(kaDir)
	a = newSyncMachine(t, "host-a", bare, true)
	useKeyset(kbDir)
	b = newSyncMachine(t, "host-b", bare, false)
	return a, b, bare, kaDir, kbDir
}

// TestKeyRotationReencryptsWireFailsClosed is the spec §5 rotation wire proof,
// across two machines holding SEPARATE keysets — what only the two-machine
// engine harness can express. It proves: keys.Rotate + engine.ReencryptAll
// (the exact daemon path, daemon.Reencrypt -> submitAdmin -> ReencryptAll)
// reseals EVERY memory blob under the new primary (changed on the wire AND
// still agb1\x00, no plaintext), and a peer without the new key fails closed
// (its folder degrades, the provider dir keeps its intact pre-rotation
// plaintext, and a direct smudge of the rotated blob is refused). Recovery via
// `key import` is proven in TestKeyRotationRecoveryViaKeyImport. The real
// `key rotate --yes` CLI->daemon path is proven in scripts/key_rotate.txt.
func TestKeyRotationReencryptsWireFailsClosed(t *testing.T) {
	ctx := context.Background()

	// t.Setenv: non-parallel guard + restore the suite keyset on cleanup.
	// os.Setenv (useKeyset) flips between ka/kb during the test.
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", os.Getenv("AGENT_BRAIN_CONFIG_DIR"))
	a, b, bare, kaDir, kbDir := twoMachinesDistinctKeysets(t)

	repoPath1 := "alpha/claude/" + rotFact1
	repoPath2 := "alpha/claude/" + rotFact2

	// (a) A writes two facts and syncs; B syncs and reads them in plaintext.
	useKeyset(kaDir)
	a.write(t, rotFact1, rotText1)
	a.write(t, rotFact2, rotText2)
	if r := a.sync(t); !r.Pushed {
		t.Fatalf("A initial push failed: %+v", r)
	}
	before1 := remoteBlob(t, bare, repoPath1)
	before2 := remoteBlob(t, bare, repoPath2)
	if !strings.HasPrefix(before1, magicPrefix) || !strings.HasPrefix(before2, magicPrefix) {
		t.Fatal("pre-rotation blobs are not agent-brain ciphertext")
	}
	useKeyset(kbDir)
	b.sync(t)
	if got := b.read(t, rotFact1); got != rotText1 {
		t.Fatalf("B pre-rotation fact1 = %q, want %q", got, rotText1)
	}

	// (b) A rotates and re-encrypts the whole repo under the new primary.
	useKeyset(kaDir)
	if err := keys.Rotate(filepath.Join(kaDir, "keyset.json")); err != nil {
		t.Fatal(err)
	}
	report, err := a.engine.ReencryptAll(ctx)
	if err != nil {
		t.Fatalf("ReencryptAll: %v", err)
	}
	if report.Files != 2 {
		t.Fatalf("ReencryptAll Files = %d, want 2 (both facts resealed)", report.Files)
	}
	if !report.Pushed {
		t.Fatalf("ReencryptAll did not push: %+v", report)
	}

	// Wire: every memory blob CHANGED and is still ciphertext under the new
	// primary; no plaintext anywhere in the bare repo (every object).
	after1 := remoteBlob(t, bare, repoPath1)
	after2 := remoteBlob(t, bare, repoPath2)
	if after1 == before1 || after2 == before2 {
		t.Fatal("a memory blob did not change on rotation — re-encrypt did not reseal under the new primary")
	}
	if !strings.HasPrefix(after1, magicPrefix) || !strings.HasPrefix(after2, magicPrefix) {
		t.Fatal("post-rotation blob lost the agent-brain magic prefix")
	}
	assertNoPlaintextOnWire(t, bare, rotText1, rotText2)

	// (c) B, still on the OLD keyset, syncs. Fail-closed: it cannot smudge the
	// new-primary ciphertext, so alpha degrades — gracefully (no hard error) —
	// and its provider dir keeps the intact pre-rotation plaintext (mirror-out
	// is withheld for a degraded folder, spec §11: a stale peer is never left
	// with a half-decrypted file).
	useKeyset(kbDir)
	reportC, errC := b.engine.Sync(ctx, []repo.Unit{b.unit})
	if errC != nil {
		t.Fatalf("B stale-key sync errored instead of degrading: %v", errC)
	}
	if !contains(reportC.Degraded, "alpha") {
		t.Fatalf("B stale-key sync did not degrade alpha (fail-closed): Degraded=%v", reportC.Degraded)
	}
	if got := b.read(t, rotFact1); got != rotText1 {
		t.Fatalf("B provider dir corrupted by a failed smudge: fact1 = %q, want %q", got, rotText1)
	}

	// Task 4.5: doctor's keyset-decrypt probe is the operator-guidance half
	// of this exact scenario. checkKeyset only loads the keyset file (which
	// still succeeds — B's key is stale, not corrupt), so without this probe
	// doctor reports all-OK while every sync keeps degrading. B's own HEAD
	// stays frozen pre-rotation (integrate's all-or-nothing invariant), so
	// the probe samples the fetched origin/main tracking ref rather than
	// HEAD to actually see the content B cannot decrypt.
	doctorDeps := doctor.Deps{
		Paths:    doctorPathsFor(t, b.checkout, kbDir),
		Registry: syncRegistry(t),
	}
	keysetDecrypt := doctorResult(t, doctor.Run(ctx, doctorDeps), "keyset-decrypt")
	if keysetDecrypt.Status != doctor.StatusWarn {
		t.Fatalf("keyset-decrypt on B (stale keyset, degraded) = %+v, want warn", keysetDecrypt)
	}
	if !strings.Contains(keysetDecrypt.Fix, "agent-brain key import --force") {
		t.Fatalf("keyset-decrypt fix = %q, missing verbatim `agent-brain key import --force`", keysetDecrypt.Fix)
	}

	// Direct proof the degradation CAUSE is the missing key, not an unrelated
	// conflict: checking the rotated blob out of origin/main with B's stale
	// keyset must fail closed (required smudge, missing new primary). Restore
	// B's worktree afterward.
	staleEnv := []string{"AGENT_BRAIN_CONFIG_DIR=" + kbDir}
	if _, err := gitRunEnv(t, b.checkout, staleEnv, "checkout", "origin/main", "--", repoPath1); err == nil {
		t.Fatal("smudging the rotated blob with the stale keyset succeeded — fail-closed broken")
	}
	gitRun(t, b.checkout, "checkout", "--", repoPath1)
}

// TestKeyRotationRecoveryViaKeyImport proves the recovery half of spec §5:
// once the stale peer imports the rotated keyset (`key import --force`,
// modeled by copyKeyset), it syncs cleanly and reads every fact back in
// plaintext. Importing the keyset is the ONLY change, so it is provably what
// unblocks B.
func TestKeyRotationRecoveryViaKeyImport(t *testing.T) {
	ctx := context.Background()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", os.Getenv("AGENT_BRAIN_CONFIG_DIR"))
	a, b, _, kaDir, kbDir := twoMachinesDistinctKeysets(t)

	useKeyset(kaDir)
	a.write(t, rotFact1, rotText1)
	a.write(t, rotFact2, rotText2)
	a.sync(t)
	useKeyset(kbDir)
	b.sync(t)
	if got := b.read(t, rotFact1); got != rotText1 {
		t.Fatalf("B pre-rotation fact1 = %q, want %q", got, rotText1)
	}

	useKeyset(kaDir)
	if err := keys.Rotate(filepath.Join(kaDir, "keyset.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.engine.ReencryptAll(ctx); err != nil {
		t.Fatalf("ReencryptAll: %v", err)
	}

	// B imports A's rotated keyset, then syncs: the folder recovers and BOTH
	// facts read back in plaintext.
	copyKeyset(t, kbDir, kaDir)
	useKeyset(kbDir)
	reportD := b.sync(t)
	if len(reportD.Degraded) != 0 {
		t.Fatalf("B still degraded after importing the rotated keyset: %v", reportD.Degraded)
	}
	if got := b.read(t, rotFact1); got != rotText1 {
		t.Fatalf("B fact1 after recovery = %q, want %q", got, rotText1)
	}
	if got := b.read(t, rotFact2); got != rotText2 {
		t.Fatalf("B fact2 after recovery = %q, want %q", got, rotText2)
	}
}

// TestKeyRotationDegradeThenRecoverKeepsAllFacts is the characterization test
// for the degrade->recover data-loss class (plan Task 4, integrate worktree
// heal). It runs the FULL realistic sequence a stale peer takes: a degraded
// cycle on the old key (which, pre-fix, stranded an uncommitted worktree
// deletion — a fast-forward rebase deletes the old blob, then cannot smudge the
// rotated ciphertext, and neither git abort restores it), THEN a key import and
// recovery cycle (where, pre-fix, commitProjects `git add -A` committed that
// stray deletion as a real memory deletion and mirror-out propagated it). Both
// facts must survive B's provider dir AND the wire. This FAILS before the
// integrate heal and passes after — the two component tests above never take
// the degrade-then-recover path, so only this test pins the bug.
func TestKeyRotationDegradeThenRecoverKeepsAllFacts(t *testing.T) {
	ctx := context.Background()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", os.Getenv("AGENT_BRAIN_CONFIG_DIR"))
	a, b, bare, kaDir, kbDir := twoMachinesDistinctKeysets(t)

	repoPath1 := "alpha/claude/" + rotFact1
	repoPath2 := "alpha/claude/" + rotFact2

	// A writes both facts and syncs; B (still on the shared pre-rotation key)
	// syncs and reads them.
	useKeyset(kaDir)
	a.write(t, rotFact1, rotText1)
	a.write(t, rotFact2, rotText2)
	a.sync(t)
	useKeyset(kbDir)
	b.sync(t)
	if got := b.read(t, rotFact2); got != rotText2 {
		t.Fatalf("B pre-rotation fact2 = %q, want %q", got, rotText2)
	}

	// A rotates and re-encrypts every blob under the new primary.
	useKeyset(kaDir)
	if err := keys.Rotate(filepath.Join(kaDir, "keyset.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.engine.ReencryptAll(ctx); err != nil {
		t.Fatalf("ReencryptAll: %v", err)
	}

	// (c) Degraded cycle: B is still on the old key, so its fast-forward
	// integrate cannot smudge the rotated blobs and alpha degrades. This is the
	// cycle that strands the worktree deletion of fact2 pre-fix.
	useKeyset(kbDir)
	reportC, errC := b.engine.Sync(ctx, []repo.Unit{b.unit})
	if errC != nil {
		t.Fatalf("B degraded sync errored instead of degrading: %v", errC)
	}
	if !contains(reportC.Degraded, "alpha") {
		t.Fatalf("B degraded sync did not degrade alpha: Degraded=%v", reportC.Degraded)
	}

	// (d) Recovery: B imports the rotated keyset and syncs. It must recover with
	// BOTH facts intact — not silently commit and propagate the stranded
	// deletion of fact2 that the degraded cycle left in the worktree.
	copyKeyset(t, kbDir, kaDir)
	useKeyset(kbDir)
	reportD := b.sync(t)
	if len(reportD.Degraded) != 0 {
		t.Fatalf("B still degraded after importing the rotated keyset: %v", reportD.Degraded)
	}

	// Both facts survive in B's provider dir...
	if got := b.read(t, rotFact1); got != rotText1 {
		t.Fatalf("fact1 after degrade->recover = %q, want %q", got, rotText1)
	}
	if got := b.read(t, rotFact2); got != rotText2 {
		t.Fatalf("fact2 after degrade->recover = %q, want %q — the degraded cycle's stranded worktree deletion was committed as a real deletion", got, rotText2)
	}
	// ...and on the wire both blobs are still present, ciphertext, no plaintext.
	after1 := remoteBlob(t, bare, repoPath1)
	after2 := remoteBlob(t, bare, repoPath2)
	if !strings.HasPrefix(after1, magicPrefix) || !strings.HasPrefix(after2, magicPrefix) {
		t.Fatal("a recovered blob is not agent-brain ciphertext on the wire")
	}
	assertNoPlaintextOnWire(t, bare, rotText1, rotText2)
}
