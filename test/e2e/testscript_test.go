package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/provider/claude"
)

// TestScripts runs the txtar CLI flows in scripts/ against the REAL binary
// (binPath, built once by TestMain) with the fake `gh` on PATH and a real
// local `git init --bare` standing in for the remote (spec §12). Each script
// runs in its own hermetic environment (scriptSetup): a fresh HOME, all
// agent-brain paths under the script's WORK, ACCESSIBLE/NO_COLOR pinned, and
// the same hermetic git posture (setHermeticGitConfig) as every other seam in
// this suite — auto-maintenance disabled, not merely neutralized.
//
// FORK-BOMB SAFETY (CLAUDE.md): `agent-brain` on the script PATH is a real-file
// COPY of binPath — the REAL compiled binary — never this test binary. The
// scripts run agent-brain as a genuine subprocess, so init/doctor resolve their
// own os.Executable() (the copy) when wiring the git clean/smudge/merge filters;
// git therefore never executes the .test binary as a filter driver and re-runs
// the whole suite recursively.
func TestScripts(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir:   "scripts",
		Setup: scriptSetup,
		Cmds: map[string]func(*testscript.TestScript, bool, []string){
			"waitdaemon":     cmdWaitDaemon,
			"stopdaemon":     cmdStopDaemon,
			"remoteblob":     cmdRemoteBlob,
			"mkclaudememory": cmdMkClaudeMemory,
		},
	})
}

// scriptSetup gives one script its hermetic world (plan line 1425). Every path
// agent-brain resolves is redirected under the script's WORK via the
// AGENT_BRAIN_{CONFIG,DATA,RUNTIME}_DIR overrides, so no real home path is
// ever touched.
func scriptSetup(env *testscript.Env) error {
	work := env.WorkDir
	home := filepath.Join(work, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}

	// The unix socket path (RuntimeDir/agent-brain.sock) must stay under the
	// ~104-byte sun_path cap; a WORK dir this deep would blow it. A short
	// os.MkdirTemp dir keeps the socket path safe regardless of how deep WORK
	// is, and env.Defer removes it when the script finishes.
	runtimeDir, err := os.MkdirTemp("", "ab")
	if err != nil {
		return err
	}
	env.Defer(func() { _ = os.RemoveAll(runtimeDir) })

	// The shim dir goes FIRST on PATH: our fake `gh`, plus `agent-brain` as a
	// real-file COPY of the built binary (never the .test binary — fork-bomb
	// rule). A copy, not a symlink: os.Executable() resolves a symlink
	// differently on macOS (unresolved) than the daemon needs it to match
	// init's EvalSymlinks'd filter wiring, and a copy makes os.Executable()
	// identical for both the init and daemon subprocesses on every platform,
	// so the daemon's SafetyGate self-check finds its own path in
	// filter.agentbrain.clean. git and sh come from the system dirs after it.
	shimDir, err := ensureShimDir()
	if err != nil {
		return err
	}

	env.Setenv("HOME", home)
	env.Setenv("AGENT_BRAIN_CONFIG_DIR", filepath.Join(work, "config"))
	env.Setenv("AGENT_BRAIN_DATA_DIR", filepath.Join(work, "data"))
	env.Setenv("AGENT_BRAIN_RUNTIME_DIR", runtimeDir)
	env.Setenv("GH_FAKE_REMOTE", filepath.Join(work, "remote.git"))
	env.Setenv("ACCESSIBLE", "1")
	env.Setenv("NO_COLOR", "1")
	setHermeticGitConfig(env)
	// A synthetic committer identity so any raw `git commit` a script issues
	// (and the daemon's own commits, whose repo-local identity init also sets)
	// never falls through to a missing global user.name under the hermetic
	// git config above.
	env.Setenv("GIT_AUTHOR_NAME", "agent-brain e2e")
	env.Setenv("GIT_AUTHOR_EMAIL", "e2e@test.invalid")
	env.Setenv("GIT_COMMITTER_NAME", "agent-brain e2e")
	env.Setenv("GIT_COMMITTER_EMAIL", "e2e@test.invalid")
	env.Setenv("PATH", strings.Join(scriptPATH(shimDir), string(os.PathListSeparator)))
	return nil
}

// setHermeticGitConfig applies hermeticGitConfigEnv's GIT_CONFIG_GLOBAL/SYSTEM
// pair to a script's environment, so every txtar script — and the real
// agent-brain subprocess (daemon included) it execs, and any git that
// subprocess spawns in turn — runs under the identical posture as every other
// seam in this suite. testscript.Env does not inherit the test process's
// os.Environ() (by design, for isolation from the developer's own shell), so
// this call is the sole source of these two vars for everything a script
// runs, transitively; it must never reconstruct the pair independently.
func setHermeticGitConfig(env *testscript.Env) {
	for _, envPair := range hermeticGitConfigEnv() {
		key, value, _ := strings.Cut(envPair, "=")
		env.Setenv(key, value)
	}
}

// shim state is built once for the whole TestScripts run and lives beside
// binPath (under TestMain's temp root, so it is cleaned up with everything
// else). Every script's PATH points at the same shim dir: the fake gh and the
// agent-brain copy are identical across scripts; only the per-script env
// (GH_FAKE_REMOTE, the AGENT_BRAIN_*_DIR paths) differs.
var (
	shimOnce sync.Once
	shimDir  string
	shimErr  error
)

// ensureShimDir creates (once) the PATH-front directory holding the fake gh
// and a real-file copy of the built binary named agent-brain.
func ensureShimDir() (string, error) {
	shimOnce.Do(func() {
		dir := filepath.Join(filepath.Dir(binPath), "shim")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			shimErr = err
			return
		}
		if err := writeFakeGH(dir); err != nil {
			shimErr = err
			return
		}
		if err := copyExecutable(binPath, filepath.Join(dir, "agent-brain")); err != nil {
			shimErr = err
			return
		}
		shimDir = dir
	})
	return shimDir, shimErr
}

// copyExecutable copies src to dst with the executable bit set. A copy (not a
// symlink) is deliberate — see scriptSetup's shim comment.
func copyExecutable(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read binary for shim copy: %w", err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		return fmt.Errorf("write shim binary: %w", err)
	}
	return nil
}

// scriptPATH is shim dir first, then the system dirs that hold git and sh —
// the "shim + real git + system" PATH the plan pins (line 1425), without
// inheriting the developer's whole environment.
func scriptPATH(shimDir string) []string {
	dirs := []string{shimDir}
	for _, tool := range []string{"git", "sh"} {
		if resolved, err := exec.LookPath(tool); err == nil {
			dirs = append(dirs, filepath.Dir(resolved))
		}
	}
	dirs = append(dirs, "/usr/bin", "/bin")
	return dedupeKeepOrder(dirs)
}

// scriptGit runs one git command in dir and returns its combined output. The
// custom commands use it (not the harness's testing.T-bound gitRun) because
// they receive a *testscript.TestScript, not a *testing.T. cmd.Env starts
// from os.Environ() (this test process's own environment, already hermetic
// per testMain) and reasserts hermeticGitConfigEnv last, exactly like
// gitRunEnv, rather than overriding it with an independently-reconstructed
// pair.
func scriptGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), hermeticGitConfigEnv()...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func dedupeKeepOrder(items []string) []string {
	seen := make(map[string]bool, len(items))
	out := items[:0:0]
	for _, item := range items {
		if seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

// scriptClient builds a daemon API client for the socket the running script's
// AGENT_BRAIN_RUNTIME_DIR names.
func scriptClient(ts *testscript.TestScript) (*api.Client, error) {
	runtimeDir := ts.Getenv("AGENT_BRAIN_RUNTIME_DIR")
	if runtimeDir == "" {
		return nil, errors.New("AGENT_BRAIN_RUNTIME_DIR is not set")
	}
	return api.NewClient(filepath.Join(runtimeDir, config.SocketName)), nil
}

// cmdWaitDaemon blocks until the daemon answers a Status call, or fails the
// script after a bounded deadline — never an unbounded spin (brief). It is how
// a script synchronizes with the `&daemon&` background process before driving
// track/sync/migrate through it.
func cmdWaitDaemon(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("waitdaemon does not support negation")
	}
	if len(args) != 0 {
		ts.Fatalf("usage: waitdaemon (takes no arguments)")
	}
	client, err := scriptClient(ts)
	if err != nil {
		ts.Fatalf("waitdaemon: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := client.Status(ctx)
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			ts.Fatalf("waitdaemon: daemon did not become reachable within 15s: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// cmdStopDaemon terminates the `&daemon&` background process cleanly: it reads
// the daemon's own PID from its Status reply, sends SIGTERM (which the daemon
// handles as graceful shutdown), then polls until the socket stops answering.
// A clean stop before the script ends keeps testscript's end-of-script process
// reaper from having to interrupt-and-wait a still-live daemon.
func cmdStopDaemon(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("stopdaemon does not support negation")
	}
	if len(args) != 0 {
		ts.Fatalf("usage: stopdaemon (takes no arguments)")
	}
	client, err := scriptClient(ts)
	if err != nil {
		ts.Fatalf("stopdaemon: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	status, err := client.Status(ctx)
	cancel()
	if err != nil {
		ts.Fatalf("stopdaemon: daemon not reachable to stop: %v", err)
	}
	if status.PID <= 0 {
		ts.Fatalf("stopdaemon: daemon reported no pid (%d)", status.PID)
	}
	if err := syscall.Kill(status.PID, syscall.SIGTERM); err != nil {
		ts.Fatalf("stopdaemon: SIGTERM pid %d: %v", status.PID, err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := client.Status(ctx)
		cancel()
		if err != nil {
			return
		}
		if time.Now().After(deadline) {
			ts.Fatalf("stopdaemon: daemon still answering 15s after SIGTERM (pid %d)", status.PID)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// cmdRemoteBlob asserts what a given repo path stored as on the fake remote —
// the bytes an attacker with GitHub access would see. Usage:
//
//	remoteblob <repo-path> plaintext  <needle>   # blob is plaintext AND contains needle
//	remoteblob <repo-path> !plaintext <needle>   # blob is ciphertext AND lacks needle
//
// The ciphertext check is the load-bearing safety assertion: a memory blob
// must carry the agent-brain magic header and never leak its plaintext.
func cmdRemoteBlob(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("remoteblob does not support negation (use the !plaintext mode argument)")
	}
	if len(args) != 3 {
		ts.Fatalf("usage: remoteblob <repo-path> plaintext|!plaintext <needle>")
	}
	repoPath, mode, needle := args[0], args[1], args[2]
	bare := ts.Getenv("GH_FAKE_REMOTE")
	if bare == "" {
		ts.Fatalf("remoteblob: GH_FAKE_REMOTE is not set")
	}
	blob, err := scriptGit(bare, "cat-file", "blob", "main:"+repoPath)
	if err != nil {
		ts.Fatalf("remoteblob: read %s from remote: %v", repoPath, err)
	}
	switch mode {
	case "!plaintext":
		if !strings.HasPrefix(blob, magicPrefix) {
			ts.Fatalf("remoteblob: %s is not agent-brain ciphertext (magic header missing)", repoPath)
		}
		if strings.Contains(blob, needle) {
			ts.Fatalf("remoteblob: SAFETY VIOLATION — plaintext %q found in stored blob %s", needle, repoPath)
		}
	case "plaintext":
		if strings.HasPrefix(blob, magicPrefix) {
			ts.Fatalf("remoteblob: %s is ciphertext, expected plaintext", repoPath)
		}
		if !strings.Contains(blob, needle) {
			ts.Fatalf("remoteblob: %s does not contain expected plaintext %q", repoPath, needle)
		}
	default:
		ts.Fatalf("remoteblob: unknown mode %q (want plaintext or !plaintext)", mode)
	}
}

// cmdMkClaudeMemory materializes a Claude memory dir for a project path the
// way Claude Code itself would (~/.claude/projects/<slug>/memory) and, when a
// fixture is named, seeds a file into it. The slug is the dash-encoding of the
// ABSOLUTE project path — dynamic per run — so it cannot be written literally
// into a txtar script; this command computes it from the live $HOME the way
// the real MemoryDirFor does. Usage:
//
//	mkclaudememory <project-path> [<src-file> <dest-name>]
func cmdMkClaudeMemory(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("mkclaudememory does not support negation")
	}
	if len(args) != 1 && len(args) != 3 {
		ts.Fatalf("usage: mkclaudememory <project-path> [<src-file> <dest-name>]")
	}
	projectPath := ts.MkAbs(args[0])
	memoryDir := claude.MemoryDirFor(ts.Getenv("HOME"), projectPath)
	if err := os.MkdirAll(memoryDir, 0o755); err != nil {
		ts.Fatalf("mkclaudememory: %v", err)
	}
	if len(args) == 3 {
		data, err := os.ReadFile(ts.MkAbs(args[1]))
		if err != nil {
			ts.Fatalf("mkclaudememory: read fixture %s: %v", args[1], err)
		}
		if err := os.WriteFile(filepath.Join(memoryDir, args[2]), data, 0o644); err != nil {
			ts.Fatalf("mkclaudememory: write %s: %v", args[2], err)
		}
	}
}
