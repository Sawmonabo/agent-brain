// Package e2e proves the filter/merge-driver chain through real git —
// the only way to test code that git invokes, not us (ADR 15).
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/gitx/gitxtest"
	"github.com/Sawmonabo/agent-brain/internal/keys"
)

var (
	binPath  string
	suiteCtx = context.Background()

	// hermeticGitConfigPath is the harness's own --global gitconfig, written
	// once by testMain. Every seam that neutralizes the developer's real git
	// config — testMain's process-wide os.Setenv loop and gitRunEnv's
	// per-command cmd.Env — reads the same GIT_CONFIG_GLOBAL/SYSTEM pair from
	// hermeticGitConfigEnv, keyed on this one path, so the two can never
	// silently drift apart.
	hermeticGitConfigPath string
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

	// The PTY battery (pty_hub_test.go) lazily starts one shared daemon child
	// against a hermetic store; stop it here so the child never outlives the
	// suite. A no-op when no PTY test ran.
	defer stopSharedHubStore()

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

	hermeticGitConfigPath = filepath.Join(root, "hermetic-gitconfig")
	if err := os.WriteFile(hermeticGitConfigPath, []byte(gitxtest.HermeticGitConfig), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	// Process-wide environment every git-spawned subprocess inherits.
	// AGENT_BRAIN_CONFIG_DIR points the clean/smudge/merge filters at the
	// suite keyset, never ~/.config/agent-brain.
	if err := os.Setenv("AGENT_BRAIN_CONFIG_DIR", keysetDir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	// GIT_CONFIG_GLOBAL/SYSTEM (hermeticGitConfigEnv) neutralize the
	// developer's real git config for git invoked through gitx
	// (InstallFilters) as well, not only the harness's own gitRunEnv calls —
	// hermetic isolation is absolute (ADR 15; matches internal/gitx TestMain).
	// gitRunEnv re-asserts the identical pair from the same function, so this
	// seam and every per-command child read from a single source instead of
	// independently-maintained literals.
	//
	// GIT_CONFIG_GLOBAL points at hermeticGitConfigPath rather than
	// /dev/null: neutralizing the config is not the same as disabling
	// maintenance, and with no config at all, gc.auto's built-in default
	// stays ON. A test that crosses the loose-object threshold in one cycle
	// can make a git child spawn `gc --auto`; autoDetach also defaults on, so
	// that child detaches from its parent and outlives the test. Its closing
	// step, update_server_info, writes .git/info/refs — racing that test's
	// t.TempDir() RemoveAll on a slow filesystem and failing the cleanup with
	// "directory not empty". gitxtest.HermeticGitConfig turns off gc.auto,
	// gc.autoDetach, and maintenance.auto so no git invocation in this suite
	// ever forks a background process that survives the test that started it.
	for _, envPair := range hermeticGitConfigEnv() {
		key, value, _ := strings.Cut(envPair, "=")
		if err := os.Setenv(key, value); err != nil {
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
	cmd.Env = append(os.Environ(), hermeticGitConfigEnv()...)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// hermeticGitConfigEnv is the GIT_CONFIG_GLOBAL/SYSTEM pair gitRunEnv appends
// to every git child it spawns — gitxtest.Env keyed on this harness's own
// hermeticGitConfigPath, so this seam and testMain's os.Setenv loop read from
// the single path var instead of two independently-maintained literals.
// Extracted to its own function (rather than inlined in gitRunEnv) so
// TestHermeticGitConfigEnv can assert on the exact entries without spawning a
// process.
func hermeticGitConfigEnv() []string {
	return gitxtest.Env(hermeticGitConfigPath)
}

// TestHermeticGitConfigEnv pins the two isolation seams described in
// testMain's comment, so a future edit that reintroduces a bare "/dev/null"
// in either one — silently re-enabling detached auto-maintenance — fails
// this test instead of surfacing as a rare teardown race on a slow runner.
func TestHermeticGitConfigEnv(t *testing.T) {
	t.Parallel()

	// Test-process seam: a child spawned with no explicit Env override
	// inherits exactly what testMain's process-wide os.Setenv put in the
	// environment. Run outside any git working tree so a repo-local config
	// cannot supply these keys instead and mask a regression here.
	want := map[string]string{
		"gc.auto":          "0",
		"gc.autoDetach":    "false",
		"maintenance.auto": "false",
	}
	for key, wantValue := range want {
		cmd := exec.CommandContext(suiteCtx, "git", "config", "--get", key)
		cmd.Dir = t.TempDir()
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git config --get %s: %v", key, err)
		}
		if got := strings.TrimSpace(string(out)); got != wantValue {
			t.Errorf("git config --get %s = %q, want %q", key, got, wantValue)
		}
	}

	// gitRunEnv's own child-env seam, inspected without spawning a process:
	// it must carry the identical GIT_CONFIG_GLOBAL entry as the seam above.
	wantEntry := "GIT_CONFIG_GLOBAL=" + hermeticGitConfigPath
	if got := hermeticGitConfigEnv(); !slices.Contains(got, wantEntry) {
		t.Errorf("hermeticGitConfigEnv() = %v, want to contain %q", got, wantEntry)
	}
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
	for file := range strings.SplitSeq(listed, "\n") {
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
