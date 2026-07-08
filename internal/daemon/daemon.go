package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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
// rlimit → engine → registry → API → loop (which owns the watcher).
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
	if err := os.Chmod(runtimeDir, 0o700); err != nil { //nolint:gosec // G302: runtime dir is a directory — 0700 (owner rwx) is least privilege that still allows traversal; the rule's 0600 bound is for files
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
		fileLogger, logWriter, err := openLogger(d.cfg.Paths.DaemonLogFile())
		if err != nil {
			return err
		}
		defer func() { _ = logWriter.Close() }()
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

	// A corrupt registry is fatal at startup: a hand-edited file naming a
	// vanished project must fail loudly, not be silently skipped. The loop
	// owns the watcher from here and re-reads the registry every cycle, so
	// enrollment changes need no restart (rebuild-on-diff, below).
	initial, err := repo.LoadLocalRegistry(d.cfg.Paths.LocalRegistryFile())
	if err != nil {
		return err
	}

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

	d.loop(ctx, syncEngine, logger, initial.Units)

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
//
// Retry policy (ADR 14, decided per this loop): unbounded exponential
// backoff, initial 5s, capped at 5m, reset on success. No elapsed-time
// stop — a resident daemon that gives up is a dead daemon, and the
// ticker/poll backstops keep firing regardless.
func (d *Daemon) loop(ctx context.Context, syncEngine *engine.Engine, logger *slog.Logger, initialUnits []repo.Unit) {
	ticker := time.NewTicker(time.Duration(d.cfg.Settings.Sync.Ticker))
	defer ticker.Stop()

	retryPolicy := backoff.NewExponentialBackOff()
	retryPolicy.InitialInterval = 5 * time.Second
	retryPolicy.MaxInterval = 5 * time.Minute
	var retryC <-chan time.Time

	watchCfg := watch.Config{
		Debounce: time.Duration(d.cfg.Settings.Sync.Debounce),
		Poll:     time.Duration(d.cfg.Settings.Sync.Poll),
	}
	// watchDied carries a spontaneous watcher failure (fd exhaustion, WSL2
	// teardown) from a watcher goroutine to this loop, which rebuilds it —
	// a died watcher must not silently degrade to ticker-only forever. Size
	// 1 + non-blocking send: coalesced failures need only one rebuild.
	watchDied := make(chan error, 1)
	live := rebuildWatcher(ctx, watchCfg, nil, rootsOf(initialUnits), watchDied, logger)
	defer func() {
		if live.manager != nil {
			_ = live.manager.Close()
		}
	}()

	runCycle := func(reason string) {
		summary, units, cycled := d.runCycle(ctx, syncEngine, logger, reason)
		if summary != nil && summary.Error != "" {
			retryC = time.After(retryPolicy.NextBackOff())
		} else {
			retryPolicy.Reset()
			retryC = nil
		}
		// Keep the watcher's roots in step with enrollment: the registry is
		// re-read every cycle, but the watcher only learns new roots by
		// being rebuilt (Add-before-Run makes replacement the only correct
		// shape). Also retry a build that previously failed (manager nil) so
		// a watcher outage self-heals instead of resting on the backstop.
		if cycled {
			if roots := rootsOf(units); live.manager == nil || !equalRoots(roots, live.watched) {
				logger.Info("watch roots changed — rebuilding", "roots", len(roots))
				live = rebuildWatcher(ctx, watchCfg, live, roots, watchDied, logger)
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case trigger := <-live.triggers:
			runCycle(trigger.Reason)
		case err := <-watchDied:
			logger.Error("watch manager died — rebuilding", "error", err)
			live = rebuildWatcher(ctx, watchCfg, live, live.watched, watchDied, logger)
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

// liveWatcher is the one running watch.Manager the loop keeps, plus the
// handle to stop it deliberately. A build failure yields a zero-manager
// value (nil triggers, so the loop's select blocks on that case and the
// ticker/poll backstop carries cycles); watched is still recorded so the
// next cycle can retry the build.
type liveWatcher struct {
	manager  *watch.Manager
	triggers <-chan watch.Trigger
	watched  []string // sorted roots currently attached
	cancel   context.CancelFunc
}

// rebuildWatcher stops old (if any) and starts a fresh watch.Manager over
// roots. Rebuild-by-replacement is mandatory: watch.Manager requires
// every Add before Run, so a running manager can never gain a root.
// Enrollment changes and watcher death both funnel here.
//
// The old manager is stopped deliberately by cancelling its own context
// first, then Close: its Run then returns via ctx (not a closed event
// stream), and the goroutine below detects the deliberate stop via
// watchCtx.Err() and stays silent — a rebuild must never masquerade as a
// death, or the loop would rebuild in a tight cycle.
func rebuildWatcher(ctx context.Context, cfg watch.Config, old *liveWatcher, roots []string, watchDied chan<- error, logger *slog.Logger) *liveWatcher {
	if old != nil {
		if old.cancel != nil {
			old.cancel()
		}
		if old.manager != nil {
			_ = old.manager.Close()
		}
	}
	manager, err := watch.New(cfg)
	if err != nil {
		// Exceptional (bad debounce, or fd exhaustion in fsnotify): fall
		// back to the ticker/poll backstop and let a later cycle retry.
		logger.Error("watch rebuild failed — ticker/poll backstop only", "error", err)
		return &liveWatcher{watched: roots}
	}
	for _, root := range roots {
		if err := manager.Add(root); err != nil {
			logger.Warn("watch root not attached", "dir", root, "error", err)
		}
	}
	watchCtx, cancel := context.WithCancel(ctx)
	go func() {
		err := manager.Run(watchCtx)
		switch {
		case err == nil:
			// Clean stop: ctx (shutdown) or watchCtx (rebuild) cancelled.
		case ctx.Err() != nil:
			logger.Info("watch manager stopped during shutdown", "error", err)
		case watchCtx.Err() != nil:
			// Deliberate rebuild; the replacement manager is already running.
		default:
			select {
			case watchDied <- err:
			default: // a death is already pending; one rebuild covers it
			}
		}
	}()
	return &liveWatcher{manager: manager, triggers: manager.Triggers(), watched: roots, cancel: cancel}
}

// rootsOf is the sorted, de-duplicated set of LocalDirs the watcher must
// cover. Sorted so equalRoots is a cheap element-wise compare; a nil
// return (no units) detaches the watcher from everything.
func rootsOf(units []repo.Unit) []string {
	if len(units) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(units))
	roots := make([]string, 0, len(units))
	for _, u := range units {
		if seen[u.LocalDir] {
			continue
		}
		seen[u.LocalDir] = true
		roots = append(roots, u.LocalDir)
	}
	sort.Strings(roots)
	return roots
}

// equalRoots reports whether two sorted root slices are identical.
func equalRoots(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// runCycle loads units fresh (Phase-3 enrollments apply without a
// restart), runs the engine, and records the outcome. It returns the unit
// set it synced and cycled=true so loop can keep the watcher's roots in
// step; cycled=false (nil units) means no cycle ran — checkout not ready,
// or the registry failed to load — so the caller must leave the watcher
// untouched rather than tear it down on a transient read error.
func (d *Daemon) runCycle(ctx context.Context, syncEngine *engine.Engine, logger *slog.Logger, reason string) (*api.SyncSummary, []repo.Unit, bool) {
	// Bound the conflict log before this cycle can append to it. The merge
	// driver (a git child spawned inside engine.Sync's integrate) is its
	// only writer and runs only DURING a cycle; here at the top, this single
	// engine goroutine has not entered Sync yet, so no writer holds the file
	// and renaming it is race-free. A full disk must not stop sync attempts,
	// so a rotation failure is logged, not returned.
	if err := rotateIfOversized(d.cfg.Paths.ConflictLogFile(), maxConflictLogSize); err != nil {
		logger.Warn("rotate conflict log", "error", err)
	}
	if d.checkoutState() != "ready" {
		d.mu.Lock()
		d.state = "uninitialized"
		d.mu.Unlock()
		return nil, nil, false
	}
	registry, err := repo.LoadLocalRegistry(d.cfg.Paths.LocalRegistryFile())
	if err != nil {
		logger.Error("load local registry", "error", err)
		summary := &api.SyncSummary{At: time.Now().UTC(), Error: err.Error()}
		d.record(summary)
		return summary, nil, false
	}
	report, err := syncEngine.Sync(ctx, registry.Units)
	summary := toSummary(report)
	if err != nil {
		summary.Error = err.Error()
		logger.Error("sync cycle failed", "reason", reason, "error", err)
	} else {
		logger.Info("sync cycle", "reason", reason,
			"commits", len(report.Commits), "pushed", report.Pushed, "degraded", report.Degraded, "scrubbed", report.Scrubbed)
	}
	d.record(summary)
	return summary, registry.Units, true
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
		Scrubbed:   report.Scrubbed,
		Pushed:     report.Pushed,
		PushQueued: report.PushQueued,
	}
}

func (d *Daemon) checkoutState() string {
	if info, err := os.Stat(filepath.Join(d.cfg.Paths.MemoriesDir(), ".git")); err == nil && info.IsDir() { //nolint:gosec // G304: MemoriesDir is the program-derived data-dir checkout location (config.Paths), not untrusted input
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
		return api.SyncResponse{}, errors.New("memories repo not initialized on this machine — run `agent-brain init` first")
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
