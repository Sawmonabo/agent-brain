// Package e2e proves the filter/merge-driver chain through real git —
// the only way to test code that git invokes, not us (ADR 15).
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/keys"
)

var (
	binPath  string
	suiteCtx = context.Background()
)

// TestMain builds the binary once and creates the suite-wide shared keyset —
// one keyset across all "machines" is the shared-identity model (spec §5).
// os.Exit skips defers, so the real work (and the deferred cleanup) lives in
// testMain and the exit happens at top level.
func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

func testMain(m *testing.M) int {
	root, err := os.MkdirTemp("", "agent-brain-e2e-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(root) }()

	binPath = filepath.Join(root, "agent-brain")
	build := exec.Command("go", "build", "-o", binPath, "../../cmd/agent-brain")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n%s", err, out)
		return 1
	}

	keysetDir := filepath.Join(root, "config")
	if err := keys.Generate(filepath.Join(keysetDir, "keyset.json")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// Process-wide environment every git-spawned subprocess inherits:
	//   - AGENT_BRAIN_CONFIG_DIR points the clean/smudge/merge filters at the
	//     suite keyset, never ~/.config/agent-brain.
	//   - GIT_CONFIG_GLOBAL/SYSTEM neutralize the developer's real git config
	//     for git invoked through gitx (InstallFilters) as well, not only the
	//     harness's own gitRunEnv calls — hermetic isolation is absolute
	//     (ADR 15; matches internal/gitx TestMain). gitRunEnv re-asserts the
	//     two GIT_CONFIG_* vars so it is self-evidently hermetic in isolation.
	for env, value := range map[string]string{
		"AGENT_BRAIN_CONFIG_DIR": keysetDir,
		"GIT_CONFIG_GLOBAL":      "/dev/null",
		"GIT_CONFIG_SYSTEM":      "/dev/null",
	} {
		if err := os.Setenv(env, value); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}

	return m.Run()
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitRunEnv(t, dir, nil, args...)
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return out
}

func gitRunEnv(t *testing.T, dir string, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(suiteCtx, "git", args...)
	cmd.Dir = dir
	// Hermetic git (ADR 15): the developer's global/system config must not
	// leak in — commit.gpgsign would hang commits, hooksPath/defaultRemoteName
	// would corrupt the two-machine simulation. extraEnv comes last so tests
	// can still override anything (Go 1.19+: last duplicate wins).
	cmd.Env = append(
		os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func newBareRepo(t *testing.T) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "remote.git")
	gitRun(t, filepath.Dir(bare), "init", "--bare", "--initial-branch=main", bare)
	return bare
}

// gitAttributes is the memories-repo wiring (spec §5): everything filtered
// and binary-safe; `*.lww.md` is this harness's stand-in for the regenerated
// newest-wins class (spec §4 — Phase 2/3 generate the real per-provider
// patterns); the attributes file and .agent-brain/** metadata are excluded.
// Phase 2's repo package becomes the canonical home of this content.
const gitAttributes = "* filter=agentbrain diff=agentbrain merge=agentbrain -text\n" +
	"*.lww.md merge=agentbrain-lww\n" +
	".gitattributes -filter -diff -merge text eol=lf\n" +
	".agent-brain/** -filter -diff -merge text eol=lf\n"

func newMachine(t *testing.T, name, bareURL string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	gitRun(t, filepath.Dir(dir), "clone", "--quiet", bareURL, dir)
	gitRun(t, dir, "config", "user.name", name)
	gitRun(t, dir, "config", "user.email", name+"@test.invalid")
	if err := gitx.InstallFilters(suiteCtx, dir, binPath); err != nil {
		t.Fatal(err)
	}
	refreshWorktree(t, dir)
	return dir
}

// refreshWorktree forces a fresh smudge of every tracked file — required
// after InstallFilters, because clone checked files out before the filter
// wiring existed in .git/config.
func refreshWorktree(t *testing.T, dir string) {
	t.Helper()
	listed := strings.TrimSpace(gitRun(t, dir, "ls-files"))
	if listed == "" {
		return
	}
	for _, file := range strings.Split(listed, "\n") {
		if err := os.Remove(filepath.Join(dir, file)); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "checkout", "--", ".")
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// remoteBlob returns the STORED bytes of a file on the remote — what an
// attacker with GitHub access would see.
func remoteBlob(t *testing.T, bare, path string) string {
	t.Helper()
	return gitRun(t, bare, "cat-file", "blob", "main:"+path)
}

func removeAndCheckout(t *testing.T, dir, name string, extraEnv []string) error {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	_, err := gitRunEnv(t, dir, extraEnv, "checkout", "--", name)
	if err != nil {
		// Restore for subsequent assertions in the same test.
		gitRun(t, dir, "checkout", "--", name)
	}
	return err
}
