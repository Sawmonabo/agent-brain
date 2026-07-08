package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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
// rlimit → engine/watch → API → loop.
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
		fileLogger, logFile, err := openLogger(d.cfg.Paths.DaemonLogFile())
		if err != nil {
			return err
		}
		defer func() { _ = logFile.Close() }()
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

	watchManager, err := watch.New(watch.Config{
		Debounce: time.Duration(d.cfg.Settings.Sync.Debounce),
		Poll:     time.Duration(d.cfg.Settings.Sync.Poll),
	})
	if err != nil {
		return err
	}
	defer func() { _ = watchManager.Close() }()
	units, err := repo.LoadLocalRegistry(d.cfg.Paths.LocalRegistryFile())
	if err != nil {
		return err
	}
	// Every Add happens before Run starts: the pump goroutine owns all
	// watch state once it is running (the watch package's contract).
	for _, u := range units.Units {
		if err := watchManager.Add(u.LocalDir); err != nil {
			logger.Warn("watch root not attached", "dir", u.LocalDir, "error", err)
		}
	}
	go func() {
		if err := watchManager.Run(ctx); err != nil {
			if ctx.Err() != nil {
				// Shutdown order cancels ctx before the deferred Close
				// releases the watcher; a stream-closed error surfacing
				// from that race is benign, not a died watcher.
				logger.Info("watch manager stopped during shutdown", "error", err)
				return
			}
			logger.Error("watch manager died", "error", err)
		}
	}()

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

	d.loop(ctx, syncEngine, watchManager, logger)

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
func (d *Daemon) loop(ctx context.Context, syncEngine *engine.Engine, watchManager *watch.Manager, logger *slog.Logger) {
	ticker := time.NewTicker(time.Duration(d.cfg.Settings.Sync.Ticker))
	defer ticker.Stop()

	retryPolicy := backoff.NewExponentialBackOff()
	retryPolicy.InitialInterval = 5 * time.Second
	retryPolicy.MaxInterval = 5 * time.Minute
	var retryC <-chan time.Time

	runCycle := func(reason string) {
		summary := d.runCycle(ctx, syncEngine, logger, reason)
		if summary != nil && summary.Error != "" {
			retryC = time.After(retryPolicy.NextBackOff())
		} else {
			retryPolicy.Reset()
			retryC = nil
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case trigger := <-watchManager.Triggers():
			runCycle(trigger.Reason)
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

// runCycle loads units fresh (Phase-3 enrollments apply without a
// restart), runs the engine, and records the outcome.
func (d *Daemon) runCycle(ctx context.Context, syncEngine *engine.Engine, logger *slog.Logger, reason string) *api.SyncSummary {
	if d.checkoutState() != "ready" {
		d.mu.Lock()
		d.state = "uninitialized"
		d.mu.Unlock()
		return nil
	}
	registry, err := repo.LoadLocalRegistry(d.cfg.Paths.LocalRegistryFile())
	if err != nil {
		logger.Error("load local registry", "error", err)
		summary := &api.SyncSummary{At: time.Now().UTC(), Error: err.Error()}
		d.record(summary)
		return summary
	}
	report, err := syncEngine.Sync(ctx, registry.Units)
	summary := toSummary(report)
	if err != nil {
		summary.Error = err.Error()
		logger.Error("sync cycle failed", "reason", reason, "error", err)
	} else {
		logger.Info("sync cycle", "reason", reason,
			"commits", len(report.Commits), "pushed", report.Pushed, "degraded", report.Degraded)
	}
	d.record(summary)
	return summary
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
		Pushed:     report.Pushed,
		PushQueued: report.PushQueued,
	}
}

func (d *Daemon) checkoutState() string {
	if info, err := os.Stat(filepath.Join(d.cfg.Paths.MemoriesDir(), ".git")); err == nil && info.IsDir() { //nolint:gosec // G703: MemoriesDir is the program-derived data-dir checkout location (config.Paths), not untrusted input
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
		return api.SyncResponse{}, errors.New("memories repo not initialized on this machine (agent-brain init arrives in Phase 3)")
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
