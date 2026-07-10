package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// magicPrefix is the on-the-wire marker of agent-brain ciphertext (crypto
// package's `magic`, mirrored here so an engine wire assertion needs no crypto
// import). A stored memory blob must begin with it and never leak plaintext.
const magicPrefix = "agb1\x00"

// engineBinaryPath is a REAL, freshly built agent-brain binary (see TestMain).
// The only engine fixtures that wire filter.agentbrain.clean/smudge
// (newEncryptedCheckout) point THAT config at THIS binary — never at the
// compiled engine.test binary. Inside a test process os.Executable() IS
// engine.test; git executing it as a clean/smudge/merge driver falls through
// to re-running the whole engine suite, and with no -test.timeout on a
// git-spawned child each nested run rewires filters at itself and recurses
// without bound (CLAUDE.md's fork-bomb rule; the 2026-07-08 incident that
// OOM-rebooted a dev machine). TestMain's tripwire below turns any recurrence
// into one loud, immediate failure instead of a repeat.
var engineBinaryPath string

// TestMain's FIRST action, before the testing package's flag parsing or
// m.Run(), must be the tripwire: a git filter invocation arrives as a bare
// positional arg, which nothing else inspects this early.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "git-clean", "git-smudge", "git-textconv", "git-merge":
			fmt.Fprintln(os.Stderr, "engine.test invoked as a git filter — a fixture wired filter config at the test binary; see engineBinaryPath's doc comment and CLAUDE.md's fork-bomb rule")
			os.Exit(1)
		}
	}
	os.Exit(testMain(m))
}

// testMain builds the real binary engineBinaryPath points at and neutralizes
// the developer's git config for every git child this package spawns, then
// runs the suite. Building once per package-test-run keeps the encrypted
// fixtures' filter wiring pointed at the same real binary at near-zero cost.
func testMain(m *testing.M) int {
	root, err := os.MkdirTemp("", "agent-brain-engine-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(root) }()

	engineBinaryPath = filepath.Join(root, "agent-brain")
	build := exec.Command("go", "build", "-o", engineBinaryPath, "../../cmd/agent-brain")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n%s", err, out)
		return 1
	}

	// Hermetic git (matches the gitx/e2e TestMains): the developer's
	// global/system config must not leak into the real-filter fixtures, whose
	// commits and pushes run through gitx's inherited environment.
	for env, value := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull,
		"GIT_CONFIG_SYSTEM": os.DevNull,
	} {
		if err := os.Setenv(env, value); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}

	return m.Run()
}

// newEncryptedCheckout builds a memories checkout whose git filters are the
// REAL agent-brain binary (engineBinaryPath), so clean/smudge genuinely
// encrypt through the keyset the process AGENT_BRAIN_CONFIG_DIR names. The
// caller sets that env (t.Setenv, hence a non-parallel test) to a PRIVATE dir
// holding its own keyset, so it can rotate the keyset in isolation from every
// other test. Shape otherwise mirrors newTestCheckout: a bare remote and a
// clone seeded the way Phase-3 init seeds it.
func newEncryptedCheckout(t *testing.T) (checkout, bare string) {
	t.Helper()
	root := t.TempDir()
	bare = filepath.Join(root, "remote.git")
	checkout = filepath.Join(root, "memories")
	mustGit(t, root, "init", "--bare", "-b", "main", bare)
	mustGit(t, root, "clone", bare, checkout)
	mustGit(t, checkout, "config", "user.name", "engine-test")
	mustGit(t, checkout, "config", "user.email", "engine-test@example.invalid")
	if err := gitx.InstallFilters(context.Background(), checkout, engineBinaryPath); err != nil {
		t.Fatal(err)
	}
	if err := repo.WriteAttributes(repo.NewLayout(checkout), testRegistry(t)); err != nil {
		t.Fatal(err)
	}
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "--quiet", "-m", "init: repo skeleton")
	mustGit(t, checkout, "push", "--quiet", "-u", "origin", "main")
	return checkout, bare
}

// readCheckout returns the worktree bytes of a repo-relative path — the
// SMUDGED (plaintext) view, the checkout-side analogue of a provider read.
func readCheckout(t *testing.T, checkout, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(checkout, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// remoteBlobBytes returns the STORED bytes of a path on the bare remote — what
// an attacker with repo access would see (ciphertext for filtered memory).
func remoteBlobBytes(t *testing.T, bare, repoPath string) string {
	t.Helper()
	return mustGit(t, bare, "cat-file", "blob", "main:"+repoPath).Stdout
}
