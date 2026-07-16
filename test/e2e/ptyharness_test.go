package e2e

// This file is the PTY transport layer for the alternate-scroll wire battery
// (pty_hub_test.go). It drives the real agent-brain binary on a real
// pseudo-terminal so the tests can assert two things no unit test can reach:
// the exact byte ORDER on the render stream (the raw log) and the rendered
// screen state under terminal-realistic input bytes (the vt10x virtual
// screen).
//
// Why vt10x exists at all: bubbletea v2's renderer emits CELL DIFFS, not whole
// frames — "what is currently on screen" is simply not recoverable by grepping
// the raw escape stream, because a later frame's content is expressed as
// cursor-move + overwrite deltas against the previous one. A VT emulator is the
// only way to reconstruct the visible grid. vt10x is the emulator the Go
// expect/survey ecosystem standardized on; it implements io.Writer, so the
// reader goroutine tees every master byte into it and term.String() dumps the
// grid.
//
// Why a shared daemon: all six scenarios are read-only against the store (the
// editor scenario exits unchanged by construction), so one hermetic daemon
// serves every parallel PTY session — the same shared-fixture economics the
// rest of this package already runs on. The daemon is the real binary's own
// `daemon run`, started once as a child process with a fully explicit
// environment, so nothing here mutates the suite's process-wide env and the
// parallel PTY sessions cannot race each other through it.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/xpty"
	"github.com/hinshun/vt10x"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/keys"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/claude"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

const (
	// pollInterval is the poll-until-predicate cadence for every wait. Fine
	// enough that a wait resolves within a frame or two of the predicate coming
	// true, coarse enough not to spin the CPU. Synchronization is ALWAYS a
	// predicate over observed output, never a bare sleep (global constraint).
	pollInterval = 10 * time.Millisecond
	// waitDeadline bounds a single predicate wait. Generous for a cold `-race`
	// run yet well under the plan's 30s ceiling, so a genuinely stuck program
	// fails loudly with a diagnostic dump rather than hanging the suite.
	waitDeadline = 20 * time.Second
	// daemonReadyDeadline bounds the shared daemon's socket coming up. The
	// child has to compile nothing (binPath is prebuilt) and only opens a
	// socket, so this is comfortably long.
	daemonReadyDeadline = 20 * time.Second
	// processExitDeadline bounds a quit's teardown-and-exit after the confirm
	// key is sent — the hub restores the terminal and returns.
	processExitDeadline = 15 * time.Second
	// tailBytes is how much of the raw log a failure message quotes. Enough to
	// show the last few frames of escape traffic without burying the message.
	tailBytes = 800
)

// seededMemoryUnit is the one enrolled project every PTY session browses. The
// index (MEMORY.md) sorts first in the browser and is default-selected; the
// long memory below it is what the wheel/editor scenarios open. Folder "alpha"
// and provider "claude" match the real claude provider's on-disk layout —
// memories live directly under the unit's LocalDir, and MEMORY.md is the
// provider's PrimaryIndexPath.
const (
	seededFolder      = "alpha"
	seededProjectID   = "id-alpha"
	longMemoryRelPath = "long-scroll-target.md"
	longMemoryName    = "long-scroll-target"
	indexRelPath      = "MEMORY.md"
	// longMemoryLineCount is how many `line-NNN` rows the long memory carries.
	// Far more than any test viewport is tall, so a scroll always has somewhere
	// to go and the top line provably advances.
	longMemoryLineCount = 200
)

// sharedHubStore is the one hermetic daemon + seeded store all PTY sessions
// share. Started once (sharedHubStoreOnce) and torn down from testMain, it
// exposes the three env vars a hub child needs to find it: its config dir
// (keyset + settings), its data dir (local registry + the seeded memories on
// disk), and its runtime dir (the socket).
type sharedHubStore struct {
	root       string
	configDir  string
	dataDir    string
	runtimeDir string
	homeDir    string
	localDir   string // the seeded unit's absolute memory dir
	daemonCmd  *exec.Cmd
	daemonPTY  xpty.Pty // the daemon runs on its own pty so its own tty probes never block
	stopOnce   sync.Once
}

var (
	sharedHubStoreOnce  sync.Once
	sharedHubStoreValue *sharedHubStore
	sharedHubStoreErr   error
)

// ensureHubStore returns the shared daemon+store, starting it exactly once
// across the whole package. A start failure is captured, not fatal to the
// starting goroutine, so every waiting test reports it through its own t
// instead of one test's t.Fatal stranding the rest on a nil store.
func ensureHubStore(t *testing.T) *sharedHubStore {
	t.Helper()
	sharedHubStoreOnce.Do(func() {
		sharedHubStoreValue, sharedHubStoreErr = startSharedHubStore()
	})
	if sharedHubStoreErr != nil {
		t.Fatalf("shared hub store: %v", sharedHubStoreErr)
	}
	return sharedHubStoreValue
}

// stopSharedHubStore kills the shared daemon and removes its root. It is
// deferred from testMain (harness_test.go) so the child never outlives the
// suite; it is a no-op when no PTY test ever ran.
func stopSharedHubStore() {
	if sharedHubStoreValue != nil {
		sharedHubStoreValue.stop()
	}
}

func (s *sharedHubStore) stop() {
	s.stopOnce.Do(func() {
		if s.daemonCmd != nil && s.daemonCmd.Process != nil {
			_ = s.daemonCmd.Process.Kill()
			_ = s.daemonCmd.Wait()
		}
		if s.daemonPTY != nil {
			_ = s.daemonPTY.Close()
		}
		if s.root != "" {
			_ = os.RemoveAll(s.root)
		}
	})
}

// startSharedHubStore provisions the hermetic store and starts `agent-brain
// daemon run` against it. It mirrors hub_semantics_test.go's newHubMachine
// setup (a memories checkout cloned from a real bare remote, a Tink keyset,
// real filter wiring at binPath, one enrolled claude unit) but for a
// LONG-LIVED, PROCESS-LEVEL daemon: t-free error returns instead of t.Fatal,
// and a fully explicit child environment instead of t.Setenv, so the daemon
// can be shared by parallel tests without touching the suite's process env.
func startSharedHubStore() (*sharedHubStore, error) {
	// A short root: the daemon's socket is <runtimeDir>/agent-brain.sock and
	// unix sun_path caps near 104 bytes, so keep the prefix tiny (the same
	// os.MkdirTemp("", "ab…") reason daemon_test.go's provisionMemories has).
	root, err := os.MkdirTemp("", "abpty")
	if err != nil {
		return nil, err
	}
	store := &sharedHubStore{
		root:       root,
		configDir:  filepath.Join(root, "cfg"),
		dataDir:    filepath.Join(root, "data"),
		runtimeDir: filepath.Join(root, "run"),
		homeDir:    filepath.Join(root, "home"),
		localDir:   filepath.Join(root, "proj", ".claude", "memory"),
	}
	for _, dir := range []string{store.configDir, store.dataDir, store.runtimeDir, store.homeDir, store.localDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			store.stop()
			return nil, err
		}
	}

	if err := keys.Generate(filepath.Join(store.configDir, "keyset.json")); err != nil {
		store.stop()
		return nil, fmt.Errorf("keyset: %w", err)
	}

	if err := store.provisionCheckout(); err != nil {
		store.stop()
		return nil, err
	}
	if err := store.enrollSeededUnit(); err != nil {
		store.stop()
		return nil, err
	}
	if err := store.seedMemories(); err != nil {
		store.stop()
		return nil, err
	}
	if err := store.startDaemon(); err != nil {
		store.stop()
		return nil, err
	}
	if err := store.waitDaemonReady(); err != nil {
		store.stop()
		return nil, err
	}
	return store, nil
}

// gitEnv is the environment every setup git command and the daemon child run
// under: the suite's hermetic git config (harness_test.go's own
// GIT_CONFIG_GLOBAL/SYSTEM pair — no developer config leaks in) plus this
// store's three agent-brain dirs and a hermetic HOME. PATH carries through so
// git and the filter binary resolve.
func (s *sharedHubStore) gitEnv() []string {
	env := append(os.Environ(), hermeticGitConfigEnv()...)
	env = append(
		env,
		"HOME="+s.homeDir,
		"AGENT_BRAIN_CONFIG_DIR="+s.configDir,
		"AGENT_BRAIN_DATA_DIR="+s.dataDir,
		"AGENT_BRAIN_RUNTIME_DIR="+s.runtimeDir,
	)
	return env
}

// provisionCheckout builds the memories checkout the daemon's engine opens: a
// clone of a real (local, network-free) bare remote with the filter/attributes
// wiring installed, committed, and pushed — the exact shape newHubMachine
// provisions, so `daemon run` starts against a valid repo.
func (s *sharedHubStore) provisionCheckout() error {
	bare := filepath.Join(s.root, "remote.git")
	if err := s.runGit(s.root, "init", "--bare", "--initial-branch=main", bare); err != nil {
		return err
	}
	checkout := filepath.Join(s.dataDir, "memories")
	if err := s.runGit(s.root, "clone", "--quiet", bare, checkout); err != nil {
		return err
	}
	if err := s.runGit(checkout, "config", "user.name", "pty-e2e"); err != nil {
		return err
	}
	if err := s.runGit(checkout, "config", "user.email", "pty-e2e@test.invalid"); err != nil {
		return err
	}
	if err := gitx.InstallFilters(suiteCtx, checkout, binPath); err != nil {
		return fmt.Errorf("install filters: %w", err)
	}
	registry, err := provider.NewRegistry(claude.New(s.homeDir))
	if err != nil {
		return fmt.Errorf("registry: %w", err)
	}
	if err := repo.WriteAttributes(repo.NewLayout(checkout), registry); err != nil {
		return fmt.Errorf("attributes: %w", err)
	}
	if err := s.runGit(checkout, "add", "-A"); err != nil {
		return err
	}
	if err := s.runGit(checkout, "commit", "--quiet", "-m", "init: repo skeleton"); err != nil {
		return err
	}
	if err := s.runGit(checkout, "push", "--quiet", "-u", "origin", "main"); err != nil {
		return err
	}
	return nil
}

// enrollSeededUnit writes the one claude unit into the daemon's local registry
// (registry-local.toml), the same file the daemon's Projects() reads to answer
// the hub's browse list. Enrolling the file directly skips the /v0/track HTTP
// round trip — already proven in daemon_test.go and irrelevant to what this
// battery pins.
func (s *sharedHubStore) enrollSeededUnit() error {
	unit := repo.Unit{
		Provider:  "claude",
		ProjectID: seededProjectID,
		Folder:    seededFolder,
		LocalDir:  s.localDir,
	}
	localRegistry := repo.NewLocalRegistry()
	if err := localRegistry.Enroll(unit); err != nil {
		return fmt.Errorf("enroll: %w", err)
	}
	paths := s.paths()
	if err := localRegistry.Save(paths.LocalRegistryFile()); err != nil {
		return fmt.Errorf("save registry: %w", err)
	}
	return nil
}

// seedMemories writes the two on-disk memory files the hub browser reads
// straight from the unit's LocalDir (memoryfs.List is a local directory walk,
// not a daemon round trip — browser.go's own note). The index sorts first; the
// long memory is a fenced code block of `line-001`…`line-200` so glamour
// renders every line on its OWN row (a code fence is preformatted — no
// paragraph reflow), which is what makes "the top line advanced by N wheel
// notches" a legible screen assertion.
func (s *sharedHubStore) seedMemories() error {
	index := "# alpha memories\n\nSee " + longMemoryName + ".\n"
	if err := os.WriteFile(filepath.Join(s.localDir, indexRelPath), []byte(index), 0o644); err != nil {
		return err
	}
	var body strings.Builder
	body.WriteString("# " + longMemoryName + "\n\n```\n")
	for lineNumber := 1; lineNumber <= longMemoryLineCount; lineNumber++ {
		fmt.Fprintf(&body, "line-%03d\n", lineNumber)
	}
	body.WriteString("```\n")
	if err := os.WriteFile(filepath.Join(s.localDir, longMemoryRelPath), []byte(body.String()), 0o644); err != nil {
		return err
	}
	return nil
}

// startDaemon launches `agent-brain daemon run` as a child on its own pty. The
// pty matters: the daemon binary is the real one, and giving it a tty slave for
// stdio keeps its startup identical to a service-managed launch while ensuring
// nothing it might probe blocks on a closed pipe. Its whole environment is
// explicit (gitEnv), so this never reads or writes the suite's process env.
func (s *sharedHubStore) startDaemon() error {
	daemonPTY, err := xpty.NewPty(80, 24)
	if err != nil {
		return fmt.Errorf("daemon pty: %w", err)
	}
	s.daemonPTY = daemonPTY
	cmd := exec.CommandContext(suiteCtx, binPath, "daemon", "run")
	cmd.Env = s.gitEnv()
	if err := daemonPTY.Start(cmd); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	s.daemonCmd = cmd
	// Drain the daemon pty so its slave never fills and blocks the daemon on a
	// write; the bytes themselves are uninteresting (the daemon is a data
	// source, not a subject of the wire assertions).
	go func() { _, _ = io.Copy(io.Discard, daemonPTY) }()
	return nil
}

// waitDaemonReady blocks until the daemon's API answers on its socket, the
// same readiness gate newHubMachine uses — poll Status until it succeeds or the
// deadline passes.
func (s *sharedHubStore) waitDaemonReady() error {
	socketPath := filepath.Join(s.runtimeDir, config.SocketName)
	client := api.NewClient(socketPath)
	deadline := time.Now().Add(daemonReadyDeadline)
	for {
		if _, err := client.Status(suiteCtx); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon socket %s never answered within %s", socketPath, daemonReadyDeadline)
		}
		time.Sleep(pollInterval)
	}
}

func (s *sharedHubStore) paths() config.Paths {
	return config.Paths{ConfigDir: s.configDir, DataDir: s.dataDir}
}

// runGit runs one git command under the store's hermetic environment, folding
// the combined output into the error on failure. A t-free sibling of
// harness_test.go's gitRun, since the shared store is provisioned once, outside
// any single test's t. The provisioning callers only care whether each step
// succeeded — the output is diagnostic, so it rides the error rather than a
// second return nobody reads.
func (s *sharedHubStore) runGit(dir string, args ...string) error {
	cmd := exec.CommandContext(suiteCtx, "git", args...)
	cmd.Dir = dir
	cmd.Env = s.gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %v in %s: %w\n%s", args, dir, err, out)
	}
	return nil
}

// sessionConfig parameterizes one PTY hub session. The zero-ish default (via
// defaultSessionConfig) opens the two-pane hub at 120x40 against the shared
// store with alternate scroll armed; individual scenarios override the pieces
// they need (a kill-switch config dir, an $EDITOR).
type sessionConfig struct {
	cols, rows int
	// configDirOverride, when set, is the hub child's AGENT_BRAIN_CONFIG_DIR
	// instead of the shared store's own. Only the kill-switch scenario uses it,
	// to feed a config.toml with alternate_scroll = false while still sharing
	// the daemon's data dir and socket.
	configDirOverride string
	// editorScript, when set, is exported as $EDITOR to the hub child so the
	// editor handoff runs it. A `#!/bin/sh\nexit 0` script exits the edit
	// unchanged, which is exactly the "no re-save" path the editor scenario
	// pins on the wire.
	editorScript string
}

func defaultSessionConfig() sessionConfig {
	// 120 columns puts the browser in its two-pane (list + preview) layout,
	// which is what arms mouse cell-motion for the click scenario and gives the
	// reading view a wide, stable wrap width; 40 rows leaves a tall viewport.
	return sessionConfig{cols: 120, rows: 40}
}

// hubSession is one running hub on a pty. The reader goroutine tees every
// master byte into BOTH the raw log (for byte-order assertions) and the vt10x
// screen (for rendered-state assertions) under a single mutex, so an assertion
// always reads a self-consistent snapshot of the two and the whole thing is
// clean under -race.
type hubSession struct {
	t   *testing.T
	pty xpty.Pty

	mu     sync.Mutex
	raw    []byte
	screen vt10x.Terminal

	readerDone chan struct{}
	procDone   chan struct{}
	stopOnce   sync.Once
}

// startHubSession spawns the real binary's hub on a fresh pty against the
// shared store and starts capturing. Every session registers its own cleanup,
// so a test that fails mid-way still kills the child and closes the pty.
func startHubSession(t *testing.T, cfg sessionConfig) *hubSession {
	t.Helper()
	store := ensureHubStore(t)

	pty, err := xpty.NewPty(cfg.cols, cfg.rows)
	if err != nil {
		t.Fatalf("open pty: %v", err)
	}

	// `dashboard` is the hub's own command and reaches the identical launchHub
	// body the bare invocation does (launchHub is the single hub entry point),
	// but without the bare root's initialized-machine probe — it needs only an
	// interactive terminal (which the pty slave is) and the running daemon, so
	// it is the stable way to open the hub under test.
	cmd := exec.CommandContext(suiteCtx, binPath, "dashboard")
	cmd.Env = store.hubChildEnv(cfg)

	session := &hubSession{
		t:          t,
		pty:        pty,
		screen:     vt10x.New(vt10x.WithSize(cfg.cols, cfg.rows)),
		readerDone: make(chan struct{}),
		procDone:   make(chan struct{}),
	}

	if err := pty.Start(cmd); err != nil {
		_ = pty.Close()
		t.Fatalf("start hub: %v", err)
	}

	go session.readLoop()
	go session.waitLoop(cmd)

	t.Cleanup(session.cleanup)
	return session
}

// hubChildEnv is the hub child's fully explicit environment: the shared store's
// data dir and socket (so it browses the seeded project through the running
// daemon), a config dir (the store's own, or a per-session override), a
// hermetic HOME and TERM, and the suite's hermetic git config. Coding-agent
// fingerprint vars are scrubbed so the hub's entry decision can never depend on
// the test process's own inherited CLAUDECODE et al. (the bare-root path gates
// on them; `dashboard` does not, but scrubbing keeps the suite honest either
// way).
func (s *sharedHubStore) hubChildEnv(cfg sessionConfig) []string {
	configDir := s.configDir
	if cfg.configDirOverride != "" {
		configDir = cfg.configDirOverride
	}
	base := scrubbedEnviron()
	base = append(
		base,
		"HOME="+s.homeDir,
		"TERM=xterm-256color",
		"AGENT_BRAIN_CONFIG_DIR="+configDir,
		"AGENT_BRAIN_DATA_DIR="+s.dataDir,
		"AGENT_BRAIN_RUNTIME_DIR="+s.runtimeDir,
	)
	base = append(base, hermeticGitConfigEnv()...)
	if cfg.editorScript != "" {
		base = append(base, "EDITOR="+cfg.editorScript)
	}
	return base
}

// killSwitchConfigDir builds a config dir whose config.toml turns alternate
// scroll OFF, copying the shared keyset in so the hub sees the identical config
// dir shape it would in production (keyset beside config.toml), differing only
// in the one setting under test.
func (s *sharedHubStore) killSwitchConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	keysetBytes, err := os.ReadFile(filepath.Join(s.configDir, "keyset.json"))
	if err != nil {
		t.Fatalf("read shared keyset: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keyset.json"), keysetBytes, 0o600); err != nil {
		t.Fatalf("write keyset: %v", err)
	}
	const disabled = "[dashboard]\nalternate_scroll = false\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(disabled), 0o644); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
	return dir
}

// writeNoopEditor writes an executable $EDITOR that exits 0 without touching its
// argument, so the edit flow's scratch round-trip returns byte-identical and
// takes the "no changes" outcome — the path the no-re-save wire pin needs (the
// editor re-arms DECSET on return but never re-saves).
func writeNoopEditor(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "noop-editor.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write editor script: %v", err)
	}
	return path
}

// scrubbedEnviron is os.Environ minus the coding-agent fingerprint vars and any
// agent-brain override the parent happens to carry, so the hub child starts
// from a known, hermetic base the session then layers its own vars onto.
func scrubbedEnviron() []string {
	drop := map[string]bool{
		"CLAUDECODE": true, "CURSOR_AGENT": true, "CODEX_SANDBOX": true,
		"CODEX_THREAD_ID": true, "CODEX_CI": true, "GEMINI_CLI": true,
		"CLINE_ACTIVE": true, "OPENCODE": true, "OPENCLAW_SHELL": true,
		"ANTIGRAVITY_CLI_ALIAS":  true,
		"AGENT_BRAIN_CONFIG_DIR": true, "AGENT_BRAIN_DATA_DIR": true,
		"AGENT_BRAIN_RUNTIME_DIR": true, "HOME": true, "TERM": true,
		"EDITOR": true, "VISUAL": true,
	}
	var out []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if drop[name] {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// readLoop tees the master stream into the raw log and the vt10x screen until
// the pty reaches EOF (which happens once the child has exited and its slave is
// closed — see waitLoop). Holding s.mu across both writes keeps the raw log and
// the screen a single consistent snapshot for any concurrent assertion.
func (s *hubSession) readLoop() {
	defer close(s.readerDone)
	buffer := make([]byte, 4096)
	for {
		n, err := s.pty.Read(buffer)
		if n > 0 {
			s.mu.Lock()
			s.raw = append(s.raw, buffer[:n]...)
			_, _ = s.screen.Write(buffer[:n])
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// waitLoop reaps the hub process and then closes the pty's slave end. Closing
// the slave is what lets readLoop drain the last buffered bytes (the teardown
// tail the CLI writes AFTER program.Run returns) and then see EOF: while THIS
// process still holds a slave fd open, the master never reports EOF even after
// the child is gone. This is the textbook pty drain, and it is why the teardown
// bytes are never lost.
func (s *hubSession) waitLoop(cmd *exec.Cmd) {
	defer close(s.procDone)
	_ = cmd.Wait()
	if unixPTY, ok := s.pty.(*xpty.UnixPty); ok {
		_ = unixPTY.Slave().Close()
	}
}

// send writes input bytes to the hub as if typed at the terminal. Wheel
// scenarios send the arrow-key ESCAPES a 1007 terminal would synthesize from a
// wheel notch; navigation scenarios send plain keys.
func (s *hubSession) send(input string) {
	s.t.Helper()
	if _, err := s.pty.Write([]byte(input)); err != nil {
		s.t.Fatalf("send %q: %v", input, err)
	}
}

// snapshotRaw returns the raw log so far.
func (s *hubSession) snapshotRaw() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.raw)
}

// snapshotScreen returns the current rendered grid.
func (s *hubSession) snapshotScreen() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.screen.String()
}

// waitRaw blocks until predicate holds over the raw log, returning the raw log
// that satisfied it. On timeout it fails with the raw tail and the current
// screen, so a stuck program is diagnosable rather than a bare deadline.
func (s *hubSession) waitRaw(predicate func(raw string) bool) string {
	s.t.Helper()
	deadline := time.Now().Add(waitDeadline)
	for {
		if raw := s.snapshotRaw(); predicate(raw) {
			return raw
		}
		if time.Now().After(deadline) {
			raw := s.snapshotRaw()
			s.t.Fatalf("waitRaw timed out after %s\nraw tail: %q\nscreen:\n%s", waitDeadline, tail(raw), s.snapshotScreen())
		}
		time.Sleep(pollInterval)
	}
}

// waitScreen blocks until predicate holds over the rendered grid, returning the
// screen that satisfied it. On timeout it fails with the screen and the raw
// tail.
func (s *hubSession) waitScreen(predicate func(screen string) bool) string {
	s.t.Helper()
	deadline := time.Now().Add(waitDeadline)
	for {
		if screen := s.snapshotScreen(); predicate(screen) {
			return screen
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("waitScreen timed out after %s\nscreen:\n%s\nraw tail: %q", waitDeadline, s.snapshotScreen(), tail(s.snapshotRaw()))
		}
		time.Sleep(pollInterval)
	}
}

// quitAndDrain ends the session with ctrl+c — bubbletea's unconditional quit,
// checked at the very top of the hub's key handler before any screen or stack
// routing (dashboard.go handleKey), so it works from whatever screen a scenario
// leaves the hub on. The interactive q→prompt→y path does NOT: a pushed screen
// (the browser or a reading view) consumes q before it can reach the root's
// quit dispatch (forwardToStack), so q there is inert and a following y hits the
// screen's own copy binding instead of confirming a prompt that never opened.
// Whatever the trigger, launchHub's RestoreAlternateScroll runs after
// program.Run returns (internal/cli/dashboard.go — "Whatever the exit path"), so
// the teardown tail is identical; the teardown-tail scenario drives the
// documented q→y path explicitly through quitViaPromptAndDrain to pin that.
// Blocks until the process has exited and the reader has drained, then returns
// the COMPLETE raw log, teardown tail included, for the exit-order assertions.
func (s *hubSession) quitAndDrain() string {
	s.t.Helper()
	s.send("\x03") // ctrl+c
	return s.drainAfterQuit()
}

// quitViaPromptAndDrain quits through the interactive confirm prompt the way a
// user at the top-level Projects tab does: esc opens "quit agent-brain? (y/n)",
// y confirms (dashboard.go — esc raises the prompt, ConfirmDecision answers it;
// q by contrast is an IMMEDIATE quit action with no prompt, and both esc and q
// reach the root only while no screen is pushed). It waits for the prompt to
// actually render before confirming, so y can never race ahead of the prompt
// opening — which would fall through to a screen or global binding for y.
func (s *hubSession) quitViaPromptAndDrain() string {
	s.t.Helper()
	s.send("\x1b") // esc raises the quit confirm prompt
	s.waitScreen(func(screen string) bool { return strings.Contains(screen, "quit agent-brain?") })
	s.send("y")
	return s.drainAfterQuit()
}

// drainAfterQuit blocks until the hub process has exited and the reader has
// drained the last teardown bytes, then returns the complete raw log. Shared by
// both quit paths so the exit-and-drain bookkeeping lives in one place.
func (s *hubSession) drainAfterQuit() string {
	s.t.Helper()
	select {
	case <-s.procDone:
	case <-time.After(processExitDeadline):
		s.t.Fatalf("hub did not exit within %s after quit\nraw tail: %q\nscreen:\n%s", processExitDeadline, tail(s.snapshotRaw()), s.snapshotScreen())
	}
	<-s.readerDone
	return s.snapshotRaw()
}

// scrollByWheel drives the reading viewport with wheel-down notches — the arrow
// escape a 1007 terminal synthesizes per wheel notch — until predicate holds
// over the rendered screen, then returns that screen. It sends one notch every
// few polls rather than a fixed count up front because the first notch(es) after
// a reading screen is pushed are DROPPED while the freshly-pushed viewport
// establishes its scroll extent (observed: a first rapid batch moves nothing, a
// second batch scrolls one line per notch). The contract under test is "wheel
// bytes scroll the reading view", not "the Nth byte is the one that moves it",
// so it drives to the observable outcome. Synchronization is the predicate poll
// on the same pollInterval cadence as waitScreen; the notch spacing is pacing,
// not a synchronization sleep.
func (s *hubSession) scrollByWheel(downByte string, predicate func(screen string) bool) string {
	s.t.Helper()
	// One notch every pollsPerNotch polls (~50ms): far enough apart that a warm
	// notch's scroll renders before the next notch is sent, so the loop stops
	// within a line or two of the predicate first coming true instead of
	// overshooting the document.
	const pollsPerNotch = 5
	deadline := time.Now().Add(waitDeadline)
	for poll := 0; ; poll++ {
		if screen := s.snapshotScreen(); predicate(screen) {
			return screen
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("reading view never reached the scroll predicate under wheel-down %q within %s\nscreen:\n%s\nraw tail: %q",
				downByte, waitDeadline, s.snapshotScreen(), tail(s.snapshotRaw()))
		}
		if poll%pollsPerNotch == 0 {
			s.send(downByte)
		}
		time.Sleep(pollInterval)
	}
}

// cleanup kills a still-running hub, drains, and closes the pty. Safe to call
// more than once (t.Cleanup plus any explicit path).
func (s *hubSession) cleanup() {
	s.stopOnce.Do(func() {
		select {
		case <-s.procDone:
		default:
			// Still running (a test bailed before quitAndDrain): kill it so
			// waitLoop's Wait returns and closes the slave.
			if unixPTY, ok := s.pty.(*xpty.UnixPty); ok {
				_ = unixPTY.Slave().Close()
			}
		}
		_ = s.pty.Close()
		<-s.procDone
		<-s.readerDone
	})
}

// tail returns the last tailBytes of s for a failure message.
func tail(s string) string {
	if len(s) <= tailBytes {
		return s
	}
	return "…" + s[len(s)-tailBytes:]
}

// lineRowOf returns the 1-based screen row (as a terminal reports mouse Y) of
// the first rendered line containing token, or 0 if none. The click scenario
// uses it to aim an SGR mouse report at a memory's actual rendered row.
func lineRowOf(screen, token string) int {
	for index, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, token) {
			return index + 1
		}
	}
	return 0
}
