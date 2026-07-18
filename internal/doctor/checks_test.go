package doctor_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/crypto"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/keys"
	"github.com/Sawmonabo/agent-brain/internal/repo"
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

// TestRunMaintenancePostureHealthy pins ADR 19's foreground posture: a
// checkout wired by init/doctor (both gc.autoDetach and maintenance.autoDetach
// pinned to "false") passes the maintenance-posture check cleanly.
func TestRunMaintenancePostureHealthy(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	got := result(t, doctor.Run(context.Background(), fx.deps), "maintenance-posture")
	if got.Status != doctor.StatusOK {
		t.Fatalf("maintenance-posture = %+v, want ok on a fully wired checkout", got)
	}
}

// TestRunMaintenancePostureOneKeyDrifted warns and names ONLY the drifted key
// when a single posture key is unset — the detail must not implicate the key
// that is still correctly pinned.
func TestRunMaintenancePostureOneKeyDrifted(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	mustGit(t, fx.dir, "config", "--local", "--unset", "gc.autoDetach")

	got := result(t, doctor.Run(context.Background(), fx.deps), "maintenance-posture")
	if got.Status != doctor.StatusWarn {
		t.Fatalf("maintenance-posture = %+v, want warn when a key drifted", got)
	}
	if !strings.Contains(got.Detail, "gc.autoDetach") {
		t.Errorf("Detail %q must name the drifted key gc.autoDetach", got.Detail)
	}
	if strings.Contains(got.Detail, "maintenance.autoDetach") {
		t.Errorf("Detail %q wrongly names maintenance.autoDetach, which is still pinned", got.Detail)
	}
	if got.Fix == "" {
		t.Errorf("a drifted posture must carry a fix hint, got %+v", got)
	}
}

// TestRunMaintenancePostureBothMissingSorted warns and names BOTH keys in
// sorted order when neither is pinned — here a Deps whose checkout has no
// local config at all. Asserting the exact rendered Detail binds that the
// check sorts the drifted keys before joining them.
func TestRunMaintenancePostureBothMissingSorted(t *testing.T) {
	t.Parallel()
	got := result(t, doctor.Run(context.Background(), minimalDeps(t)), "maintenance-posture")
	if got.Status != doctor.StatusWarn {
		t.Fatalf("maintenance-posture = %+v, want warn when both keys are missing", got)
	}
	want := "git auto-maintenance is not pinned to the foreground — a detached maintenance process could race the sync engine: gc.autoDetach, maintenance.autoDetach"
	if diff := cmp.Diff(want, got.Detail); diff != "" {
		t.Errorf("maintenance-posture Detail (-want +got):\n%s", diff)
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

// remoteCheckFixture builds the minimum the remote row needs: a git
// checkout at MemoriesDir (branch main) with `origin` pointing at bareURL.
// No keyset, no enrollment — checkRemote reads none of that.
func remoteCheckFixture(t *testing.T, bareURL string) doctor.Deps {
	t.Helper()
	base := t.TempDir()
	paths := config.Paths{ConfigDir: filepath.Join(base, "cfg"), DataDir: filepath.Join(base, "data")}
	ctx := context.Background()
	if err := os.MkdirAll(paths.MemoriesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Run(ctx, paths.MemoriesDir(), "init", "--quiet", "--initial-branch=main"); err != nil {
		t.Fatal(err)
	}
	// Repo-local identity, same as newFixture: commits in this checkout must
	// never depend on (or read) the machine's global git config.
	if _, err := gitx.Run(ctx, paths.MemoriesDir(), "config", "user.name", "doctor-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Run(ctx, paths.MemoriesDir(), "config", "user.email", "doctor-test@example.invalid"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Run(ctx, paths.MemoriesDir(), "remote", "add", "origin", bareURL); err != nil {
		t.Fatal(err)
	}
	return doctor.Deps{
		Paths:    paths,
		Registry: testRegistry(t),
		Home:     filepath.Join(base, "home"),
	}
}

// TestRunRemoteReachableEmptyRemote pins the probe's reachability-only
// semantics: a brand-new remote with zero refs advertises no HEAD, which is
// not unreachability — the first push is simply still ahead. A --exit-code
// probe would exit 2 here and misreport a healthy fresh install.
func TestRunRemoteReachableEmptyRemote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "remote.git")
	if _, err := gitx.Run(ctx, filepath.Dir(bare), "init", "--quiet", "--bare", bare); err != nil {
		t.Fatal(err)
	}
	deps := remoteCheckFixture(t, bare)
	got := result(t, doctor.Run(ctx, deps), "remote")
	if got.Status != doctor.StatusOK {
		t.Fatalf("remote row for a reachable empty origin = %+v, want ok", got)
	}
}

// TestRunRemoteReachableWhenRemoteHEADDangles pins reachability against a
// remote whose HEAD symref dangles: the bare was created with a different
// default branch, then only `main` was pushed, so HEAD points at a branch
// that does not exist while main does. ls-remote advertises no HEAD for it;
// a --exit-code HEAD probe exits 2 and misreports "unreachable" even though
// every push and fetch works fine. The row must stay OK.
func TestRunRemoteReachableWhenRemoteHEADDangles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	bare := filepath.Join(t.TempDir(), "remote.git")
	if _, err := gitx.Run(ctx, filepath.Dir(bare), "init", "--quiet", "--bare", "--initial-branch=master", bare); err != nil {
		t.Fatal(err)
	}
	deps := remoteCheckFixture(t, bare)
	memories := deps.Paths.MemoriesDir()
	if err := os.WriteFile(filepath.Join(memories, "seed.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Run(ctx, memories, "add", "seed.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Run(ctx, memories, "commit", "-m", "test: seed"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Run(ctx, memories, "push", "--quiet", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	// Premise check: the scenario only exists while the remote's HEAD still
	// dangles at master after the main push. If a future git starts
	// retargeting unborn HEADs on receive, this fails loudly so the test
	// gets redesigned rather than passing vacuously.
	head, err := gitx.Run(ctx, bare, "symbolic-ref", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(head.Stdout); got != "refs/heads/master" {
		t.Fatalf("premise broken: remote HEAD = %q, want a dangling refs/heads/master", got)
	}

	got := result(t, doctor.Run(ctx, deps), "remote")
	if got.Status != doctor.StatusOK {
		t.Fatalf("remote row for a reachable origin with dangling HEAD = %+v, want ok", got)
	}
}

// writeProjectsRegistry drops a projects.toml into deps' memories checkout
// path so checkProjectIdentity has a shared registry to compare against.
func writeProjectsRegistry(t *testing.T, deps doctor.Deps, body string) {
	t.Helper()
	path := repo.NewLayout(deps.Paths.MemoriesDir()).ProjectsFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProjectIdentityMatches(t *testing.T) {
	t.Parallel()
	deps := minimalDeps(t)
	deps.Enrolled = []repo.Unit{{Provider: "claude", Folder: "myrepo", LocalDir: "/x", ProjectID: "github.com/owner/myrepo"}}
	writeProjectsRegistry(t, deps, "version = 1\n\n[projects.myrepo]\nid = \"github.com/owner/myrepo\"\n")

	got := result(t, doctor.Run(context.Background(), deps), "project-identity")
	if got.Status != doctor.StatusOK {
		t.Fatalf("project-identity = %+v, want ok", got)
	}
}

func TestProjectIdentityDriftedMapping(t *testing.T) {
	t.Parallel()
	deps := minimalDeps(t)
	deps.Enrolled = []repo.Unit{{Provider: "claude", Folder: "myrepo", LocalDir: "/x", ProjectID: "github.com/owner/myrepo"}}
	writeProjectsRegistry(t, deps, "version = 1\n\n[projects.myrepo]\nid = \"github.com/other/myrepo\"\n")

	got := result(t, doctor.Run(context.Background(), deps), "project-identity")
	if got.Status != doctor.StatusWarn {
		t.Fatalf("project-identity = %+v, want warn on a reassigned folder", got)
	}
	for _, want := range []string{"github.com/owner/myrepo", "github.com/other/myrepo", "myrepo", "crosses projects"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("Detail %q missing %q", got.Detail, want)
		}
	}
	if !strings.Contains(got.Fix, "untrack") {
		t.Errorf("Fix %q must name the untrack/re-track remediation", got.Fix)
	}
}

// TestProjectIdentityMultipleDriftsAreSortedInDetail pins slices.Sort(drifted)
// (checks.go): every other project-identity test enrolls at most one
// per-project unit, so a Detail built straight off enrollment (or map)
// order would still pass them. Enrolling "zeta-project" before
// "alpha-project" — both drifted, so their folder names sort in the
// OPPOSITE order from enrollment — and asserting the exact rendered Detail
// binds that the check sorts before joining, not merely that both folders
// are named somewhere in it.
func TestProjectIdentityMultipleDriftsAreSortedInDetail(t *testing.T) {
	t.Parallel()
	deps := minimalDeps(t)
	deps.Enrolled = []repo.Unit{
		{Provider: "claude", Folder: "zeta-project", LocalDir: "/z", ProjectID: "github.com/owner/zeta-project"},
		{Provider: "claude", Folder: "alpha-project", LocalDir: "/a", ProjectID: "github.com/owner/alpha-project"},
	}
	writeProjectsRegistry(t, deps, "version = 1\n\n"+
		"[projects.zeta-project]\nid = \"github.com/other/zeta-project\"\n\n"+
		"[projects.alpha-project]\nid = \"github.com/other/alpha-project\"\n")

	got := result(t, doctor.Run(context.Background(), deps), "project-identity")
	if got.Status != doctor.StatusWarn {
		t.Fatalf("project-identity = %+v, want warn with two drifted folders", got)
	}
	want := "project identity drift: " +
		`alpha-project (claude): registry maps it to "github.com/other/alpha-project", this machine enrolled "github.com/owner/alpha-project" — mirroring crosses projects until re-tracked; ` +
		`zeta-project (claude): registry maps it to "github.com/other/zeta-project", this machine enrolled "github.com/owner/zeta-project" — mirroring crosses projects until re-tracked`
	if diff := cmp.Diff(want, got.Detail); diff != "" {
		t.Fatalf("project-identity Detail not sorted alphabetically (-want +got):\n%s", diff)
	}
}

func TestProjectIdentityFolderMissingFromRegistry(t *testing.T) {
	t.Parallel()
	deps := minimalDeps(t)
	deps.Enrolled = []repo.Unit{{Provider: "claude", Folder: "myrepo", LocalDir: "/x", ProjectID: "github.com/owner/myrepo"}}
	writeProjectsRegistry(t, deps, "version = 1\n")

	got := result(t, doctor.Run(context.Background(), deps), "project-identity")
	if got.Status != doctor.StatusWarn {
		t.Fatalf("project-identity = %+v, want warn when the folder vanished from the registry", got)
	}
	if !strings.Contains(got.Detail, "missing") {
		t.Errorf("Detail %q should say the folder is missing from the shared registry", got.Detail)
	}
}

func TestProjectIdentityUnreadableRegistryWarns(t *testing.T) {
	t.Parallel()
	deps := minimalDeps(t)
	deps.Enrolled = []repo.Unit{{Provider: "claude", Folder: "myrepo", LocalDir: "/x", ProjectID: "github.com/owner/myrepo"}}
	writeProjectsRegistry(t, deps, "version = 99\n") // unsupported version → LoadProjects error

	got := result(t, doctor.Run(context.Background(), deps), "project-identity")
	if got.Status != doctor.StatusWarn {
		t.Fatalf("project-identity = %+v, want warn on an unreadable registry", got)
	}
	if !strings.Contains(got.Detail, "cannot read the shared project registry") {
		t.Errorf("Detail %q should say the shared registry could not be read", got.Detail)
	}
	if !strings.Contains(got.Fix, "agent-brain sync") {
		t.Errorf("Fix %q must tell the operator to run agent-brain sync", got.Fix)
	}
}

// TestProjectIdentitySkipsWithoutPerProjectUnits: global-scope units carry no
// ProjectID, so the check must not apply at all (mirrors the prereq checks'
// enrolled-scoping) — absent from the report, not a vacuous ok.
func TestProjectIdentitySkipsWithoutPerProjectUnits(t *testing.T) {
	t.Parallel()
	deps := minimalDeps(t)
	deps.Enrolled = []repo.Unit{{Provider: "codex", Folder: "_global", LocalDir: "/y"}}

	report := doctor.Run(context.Background(), deps)
	for _, res := range report.Results {
		if res.Name == "project-identity" {
			t.Fatalf("project-identity should be inapplicable with no per-project units, got %+v", res)
		}
	}
}

// TestRunFiltersComparesByFileIdentity pins the brew-symlink fix: one binary
// has many spellings (a Homebrew symlink farm entry vs its versioned Cellar
// target, case-variant paths on APFS), so checkFilters must equate the path
// RECORDED in filter.agentbrain.clean with the running deps.BinaryPath by FILE
// IDENTITY, not raw string containment. The two symlink-agnostic rows are the
// mutation-proof core: they FAIL under the old strings.Contains comparison (the
// recorded spelling is not a substring of the other spelling) and pass only
// once the comparison resolves both to the same file. The dead-path and
// different-binary rows keep the genuine failures failing; the identical-
// spelling row pins the string fast path that lets a recorded target which does
// not (yet) exist on disk still validate (the healthy fixture's own shape).
func TestRunFiltersComparesByFileIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// wire provisions any files/symlinks under base and returns the
		// spelling recorded into the filter config plus the running binary
		// path deps.BinaryPath is set to.
		wire func(t *testing.T, base string) (recorded, running string)
		want doctor.Status
	}{
		{
			// Today's broken daemon: doctor --fix (invoked via the symlink)
			// recorded the symlink spelling; the daemon was exec'd from the
			// Cellar target and hands that spelling in.
			name: "symlink recorded, cellar target running",
			wire: func(t *testing.T, base string) (string, string) {
				cellar := filepath.Join(base, "Cellar", "agent-brain", "1.0.0", "bin", "agent-brain")
				writeStandInBinary(t, cellar)
				symlink := filepath.Join(base, "opt", "bin", "agent-brain")
				if err := os.MkdirAll(filepath.Dir(symlink), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(cellar, symlink); err != nil {
					t.Fatal(err)
				}
				return symlink, cellar
			},
			want: doctor.StatusOK,
		},
		{
			// Legacy init-era wiring recorded the resolved Cellar target; a
			// later CLI/daemon runs via the upgrade-stable symlink spelling.
			name: "cellar target recorded, symlink running",
			wire: func(t *testing.T, base string) (string, string) {
				cellar := filepath.Join(base, "Cellar", "agent-brain", "1.0.0", "bin", "agent-brain")
				writeStandInBinary(t, cellar)
				symlink := filepath.Join(base, "opt", "bin", "agent-brain")
				if err := os.MkdirAll(filepath.Dir(symlink), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(cellar, symlink); err != nil {
					t.Fatal(err)
				}
				return cellar, symlink
			},
			want: doctor.StatusOK,
		},
		{
			// A Cellar path orphaned by `brew upgrade`: recorded but no longer
			// on disk. Must FAIL so doctor --fix rewrites the wiring.
			name: "dead recorded path, live binary running",
			wire: func(t *testing.T, base string) (string, string) {
				live := filepath.Join(base, "opt", "bin", "agent-brain")
				writeStandInBinary(t, live)
				dead := filepath.Join(base, "Cellar", "agent-brain", "0.9.0", "bin", "agent-brain")
				return dead, live
			},
			want: doctor.StatusFail,
		},
		{
			// Two genuinely different binaries, both on disk: not the same
			// file, so os.SameFile must reject them.
			name: "different real binaries",
			wire: func(t *testing.T, base string) (string, string) {
				running := filepath.Join(base, "opt", "bin", "agent-brain")
				writeStandInBinary(t, running)
				other := filepath.Join(base, "elsewhere", "agent-brain")
				writeStandInBinary(t, other)
				return other, running
			},
			want: doctor.StatusFail,
		},
		{
			// Identical spellings for a path that does NOT exist on disk: the
			// string fast path must accept it without an os.Stat (this is the
			// healthy fixture's own shape — its recorded target is a fabricated
			// path). Dropping the fast path would break every green fixture.
			name: "identical spelling, path absent",
			wire: func(_ *testing.T, base string) (string, string) {
				ghost := filepath.Join(base, "ghost", "agent-brain")
				return ghost, ghost
			},
			want: doctor.StatusOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fx := newFixture(t)
			recorded, running := tt.wire(t, fx.base)
			// Re-wire the whole filter chain at the recorded spelling
			// (single-valued replace: required=true and every merge/diff key
			// are re-written, so only the recorded spelling varies from
			// healthy).
			if err := gitx.InstallFilters(context.Background(), fx.dir, recorded); err != nil {
				t.Fatal(err)
			}
			fx.deps.BinaryPath = running

			got := result(t, doctor.Run(context.Background(), fx.deps), "filters")
			if got.Status != tt.want {
				t.Fatalf("filters check = %+v, want %v", got, tt.want)
			}
			if tt.want == doctor.StatusFail && got.Fix == "" {
				t.Errorf("a filters FAIL must carry the doctor --fix remedy, got %+v", got)
			}
		})
	}
}

// TestRunFiltersMixedWiringFailsNamingKey pins the all-keys comparison:
// InstallFilters writes its nine keys in a loop that can die mid-way (disk
// full, killed process), leaving clean at the new binary while a merge driver
// still points at a dead old one. A check that equated only clean would wave
// that state through, and the breakage would surface later as a failed merge
// driver. Every command-bearing key is checked by file identity, and the
// Detail names the offending key.
func TestRunFiltersMixedWiringFailsNamingKey(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	live := filepath.Join(fx.base, "opt", "bin", "agent-brain")
	writeStandInBinary(t, live)
	// The whole chain wired healthily at the live binary...
	if err := gitx.InstallFilters(context.Background(), fx.dir, live); err != nil {
		t.Fatal(err)
	}
	fx.deps.BinaryPath = live
	// ...then one command-bearing key hand-rewired to a dead path, exactly as
	// a killed mid-loop InstallFilters would leave it. The path carries no
	// quote or space, so a plain single-quoted token matches InstallFilters'
	// shellQuote output for it.
	dead := filepath.Join(fx.base, "Cellar", "agent-brain", "0.9.0", "bin", "agent-brain")
	mustGit(t, fx.dir, "config", "--local", "merge.agentbrain.driver", "'"+dead+"' git-merge --mode fact -- %O %A %B %P")

	got := result(t, doctor.Run(context.Background(), fx.deps), "filters")
	if got.Status != doctor.StatusFail {
		t.Fatalf("filters check = %+v, want fail on mixed wiring", got)
	}
	if !strings.Contains(got.Detail, "merge.agentbrain.driver") {
		t.Errorf("Detail %q must name the offending key merge.agentbrain.driver", got.Detail)
	}
	if got.Fix == "" {
		t.Errorf("a filters FAIL must carry the doctor --fix remedy, got %+v", got)
	}
}
