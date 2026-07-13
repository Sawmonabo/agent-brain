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
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/gofrs/flock"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
	"github.com/Sawmonabo/agent-brain/internal/engine"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/repo"
	"github.com/Sawmonabo/agent-brain/internal/watch"
)

// ErrAlreadyRunning means another daemon holds the flock.
var ErrAlreadyRunning = errors.New("agent-brain daemon is already running")

// errNotInitialized is the actionable refusal every mutating endpoint shares
// when the checkout does not exist yet (init is a Phase-3 command).
var errNotInitialized = errors.New("memories repo not initialized on this machine — run `agent-brain init` first")

// syncWaitTimeout bounds how long POST /v0/sync waits for its cycle;
// adminWaitTimeout bounds how long track/untrack/migrate wait to be serviced
// by the engine goroutine (they queue behind any running cycle).
const (
	syncWaitTimeout  = 60 * time.Second
	adminWaitTimeout = 60 * time.Second
)

// quiesceMin and quiesceMax bound a requested quiesce TTL (Phase-4 F2): long
// enough to cover init/doctor's checkout surgery, short enough that a crashed
// caller cannot wedge the daemon for long — auto-release at the deadline is
// the backstop, and the ceiling caps the worst case.
const (
	quiesceMin = 1 * time.Second
	quiesceMax = 600 * time.Second
)

// errQuiesced is the refusal an explicit sync or a mutating op returns while a
// hold is active — it names the expiry so the caller knows when to retry, and
// says who can lift it early (the CLI that requested the hold). Silently
// queueing the op would defeat the point of quiescing.
func errQuiesced(until time.Time) error {
	return fmt.Errorf("daemon quiesced until %s — retry after, or release with the CLI that requested it", until.Format(time.RFC3339))
}

// statusError carries an HTTP status code for the server's error envelope
// (spec §7: an unknown --project folder is a 400, not a 500).
type statusError struct {
	code int
	msg  string
}

func (e statusError) Error() string { return e.msg }

// Config wires the daemon. Registry is injected so the composition
// root (cmd layer) decides which providers exist — tests run an empty
// or fake registry; production wires claude/codex.
type Config struct {
	Paths    config.Paths
	Settings config.Settings
	Registry *provider.Registry
	Version  string
	// Logger overrides the file logger (tests). nil → JSON logger on
	// the size-rotated DaemonLogFile.
	Logger *slog.Logger
	// BinaryPath overrides what doctor.SafetyGate expects the filter wiring
	// to point at. Empty (production) means Run resolves os.Executable()
	// itself. A test that spawns a real git filter subprocess must set this
	// to a genuine binary — never a compiled test binary: os.Executable()
	// inside a test process IS that test binary, and wiring a git filter at
	// it recurses the whole suite without bound (internal/daemon/
	// daemon_test.go's TestMain tripwire and testBinaryPath doc comment).
	BinaryPath string
}

type syncRequest struct {
	// filter, when non-empty, narrows the triggered cycle to one repo folder
	// AFTER the registry loads; watch/ticker cycles pass "" and stay
	// whole-fleet.
	filter string
	reply  chan syncReply
}

// syncReply carries a manual cycle's outcome back to TriggerSync. A serviced
// cycle replies with response and a nil err; err is the quiesce refusal the
// loop's syncRequests arm returns when a hold landed after TriggerSync's entry
// check but before the loop serviced the request — the same errQuiesced shape
// TriggerSync returns up front (M-N1). Mirrors adminReply's result+err split.
type syncReply struct {
	response api.SyncResponse
	err      error
}

// adminRequest is a checkout mutation (track/untrack/migrate) submitted to the
// engine goroutine. run executes on that goroutine with the live engine; its
// result is type-asserted back by the handler that built it.
type adminRequest struct {
	reason string
	run    func(context.Context, *engine.Engine) (any, error)
	reply  chan adminReply
}

type adminReply struct {
	result any
	err    error
}

// Daemon is the resident process: one engine goroutine, a watch
// manager, and the UDS API (ADR 04).
type Daemon struct {
	cfg Config

	// binaryPath is os.Executable(), resolved once at the top of Run and
	// read-only thereafter (set before the API server or loop goroutine
	// starts) — what doctor.SafetyGate checks the filter wiring points at.
	binaryPath string

	mu          sync.Mutex
	startedAt   time.Time
	state       string
	stateDetail string
	lastSync    *api.SyncSummary
	degraded    map[string]bool
	// Per-unit telemetry (Task 6.5), all read under d.mu and projected onto
	// units by Projects(). watchState/watchTriggers are keyed by root (LocalDir)
	// — the watcher attaches roots, not units; lastCycle is keyed by folder — a
	// cycle degrades folders and errors whole-fleet. watchState is replaced
	// wholesale on each watcher (re)build; watchTriggers and lastCycle accumulate
	// across cycles (watchTriggers pruned to live roots on rebuild).
	watchState    map[string]string
	watchTriggers map[string]uint64
	lastCycle     map[string]*api.UnitCycleResult
	// quiescedUntil is the deadline of an active hold (POST /v0/quiesce),
	// zero when not quiesced. It shares d.mu with the state above: "may an
	// automatic cycle start now?" must be one atomic read, or a cycle could
	// slip through between a quiesce write and the loop's check. Auto-release
	// needs no timer — the loop compares the deadline against the wall clock
	// each cycle, and a stale deadline simply reads as not-quiesced.
	quiescedUntil time.Time

	syncRequests  chan syncRequest
	adminRequests chan adminRequest
	// readRequests carries History/Blob lookups to the engine goroutine
	// (ADR 20 D3): a separate channel from adminRequests because the loop
	// arm servicing it must not run a post-op cycle or check quiesce (see
	// submitRead and the loop's readRequests arm).
	readRequests chan adminRequest
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
		cfg:           cfg,
		state:         "uninitialized",
		degraded:      map[string]bool{},
		watchState:    map[string]string{},
		watchTriggers: map[string]uint64{},
		lastCycle:     map[string]*api.UnitCycleResult{},
		syncRequests:  make(chan syncRequest),
		adminRequests: make(chan adminRequest),
		readRequests:  make(chan adminRequest),
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
	binaryPath := d.cfg.BinaryPath
	if binaryPath == "" {
		resolved, err := os.Executable()
		if err != nil {
			return fmt.Errorf("resolve binary path: %w", err)
		}
		binaryPath = resolved
	}
	d.binaryPath = binaryPath

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
	// every git child spawned during integrate inherits it;
	// `agent-brain conflicts` reads this file (the dashboard view is
	// deferred past Phase 3).
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

	// The gate must be evaluated, and d.state/d.stateDetail populated,
	// BEFORE the API server starts accepting connections: a client hitting
	// /v0/status the instant the socket opens must never observe the
	// constructor's "uninitialized" default racing a SafetyGate check that
	// spawns several git subprocesses and can take a perceptible moment,
	// rather than the real, current answer.
	d.mu.Lock()
	d.startedAt = time.Now().UTC()
	d.mu.Unlock()
	state, _ := d.refreshState(ctx)

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

	logger.Info("daemon started", "version", d.cfg.Version, "socket", socketPath, "state", state)

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
	live := rebuildWatcher(ctx, watchCfg, nil, rootsOf(initialUnits), watchDied, logger, d.setWatchStates)
	defer func() {
		if live.manager != nil {
			_ = live.manager.Close()
		}
	}()

	runCycle := func(reason, filter string) {
		summary, units, cycled := d.runCycle(ctx, syncEngine, logger, reason, filter)
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
				live = rebuildWatcher(ctx, watchCfg, live, roots, watchDied, logger, d.setWatchStates)
			}
		}
	}

	// runAutomatic gates the tick/watch/retry cycles behind the quiesce hold:
	// while quiesced they are SKIPPED (one log line), not rescheduled — the
	// next tick fires normally after the deadline passes, so auto-release
	// needs no timer. Explicit /v0/sync and mutations do NOT funnel here;
	// they are refused synchronously at their handlers (TriggerSync,
	// submitAdmin) so the caller learns the expiry instead of blocking.
	runAutomatic := func(reason, filter string) {
		if until, held := d.quiesced(time.Now()); held {
			logger.Info("cycle skipped: quiesced", "reason", reason, "until", until.Format(time.RFC3339))
			return
		}
		runCycle(reason, filter)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case trigger := <-live.triggers:
			d.recordWatchTrigger(trigger.Reason, live.watched)
			runAutomatic(trigger.Reason, "")
		case err := <-watchDied:
			logger.Error("watch manager died — rebuilding", "error", err)
			live = rebuildWatcher(ctx, watchCfg, live, live.watched, watchDied, logger, d.setWatchStates)
		case <-ticker.C:
			runAutomatic("ticker", "")
		case <-retryC:
			runAutomatic("retry", "")
		case request := <-d.syncRequests:
			// Re-check the hold HERE, on the engine goroutine: a quiesce that
			// landed after TriggerSync's entry check but before this arm serviced
			// the request would otherwise run one manual cycle inside the hold
			// (M-N1). refuseManualSyncIfQuiesced replies with the same errQuiesced
			// refusal and we skip the cycle — quiesce refuses, it never drains.
			if d.refuseManualSyncIfQuiesced(request, time.Now()) {
				continue
			}
			runCycle("manual", request.filter)
			d.mu.Lock()
			last := d.lastSync
			d.mu.Unlock()
			request.reply <- syncReply{response: api.SyncResponse{Status: "completed", Summary: last}}
		case request := <-d.adminRequests:
			// Enrollment/purge/seed run HERE, on the one engine goroutine
			// (ADR 03). Reply first (the fast local git work is done), then
			// run a full cycle: it mirrors in a freshly-tracked dir, pushes
			// the register/seed/purge commits, and — via the rebuild-on-diff
			// below runCycle — brings the watcher's roots in step with the
			// changed unit set.
			result, err := request.run(ctx, syncEngine)
			request.reply <- adminReply{result: result, err: err}
			runCycle(request.reason, "")
		case request := <-d.readRequests:
			// Read-only history/blob work runs HERE so it can never race the
			// writer (ADR 20 D3). Unlike adminRequests: no post-op cycle
			// (nothing changed) and no quiesce refusal — spec §15 greys
			// mutations only, and the quiesce window's checkout surgery
			// (.git/config/.gitattributes re-wiring) cannot corrupt a `git
			// log`/`cat-file` read; a checkout mid-init re-clone is instead
			// caught by submitRead's own refreshState "ready" gate, same as
			// every other engine-goroutine op.
			result, err := request.run(ctx, syncEngine)
			request.reply <- adminReply{result: result, err: err}
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
// recordWatch is passed d.setWatchStates so rebuildWatcher can report each root's
// posture (Task 6.5) at the one site that knows whether the watch attached — a
// pure func seam keeps rebuildWatcher testable without a live Daemon.
func rebuildWatcher(ctx context.Context, cfg watch.Config, old *liveWatcher, roots []string, watchDied chan<- error, logger *slog.Logger, recordWatch func(map[string]string)) *liveWatcher {
	if old != nil {
		if old.cancel != nil {
			old.cancel()
		}
		if old.manager != nil {
			_ = old.manager.Close()
		}
	}
	states := make(map[string]string, len(roots))
	manager, err := watch.New(cfg)
	if err != nil {
		// Exceptional (bad debounce, or fd exhaustion in fsnotify): fall
		// back to the ticker/poll backstop and let a later cycle retry. Every
		// root is unwatched, but tick sync still covers it — say so per root.
		logger.Error("watch rebuild failed — ticker/poll backstop only", "error", err)
		for _, root := range roots {
			states[root] = watchFailed("watcher unavailable: " + err.Error())
		}
		recordWatch(states)
		return &liveWatcher{watched: roots}
	}
	for _, root := range roots {
		if err := manager.Add(root); err != nil {
			logger.Warn("watch root not attached", "dir", root, "error", err)
			states[root] = watchFailed(err.Error())
		} else {
			states[root] = "watching"
		}
	}
	recordWatch(states)
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
func (d *Daemon) runCycle(ctx context.Context, syncEngine *engine.Engine, logger *slog.Logger, reason, filter string) (*api.SyncSummary, []repo.Unit, bool) {
	// Bound the conflict log before this cycle can append to it. The merge
	// driver (a git child spawned inside engine.Sync's integrate) is its
	// only writer and runs only DURING a cycle; here at the top, this single
	// engine goroutine has not entered Sync yet, so no writer holds the file
	// and renaming it is race-free. A full disk must not stop sync attempts,
	// so a rotation failure is logged, not returned.
	if err := rotateIfOversized(d.cfg.Paths.ConflictLogFile(), maxConflictLogSize); err != nil {
		logger.Warn("rotate conflict log", "error", err)
	}
	if state, _ := d.refreshState(ctx); state != "ready" {
		return nil, nil, false
	}
	registry, err := repo.LoadLocalRegistry(d.cfg.Paths.LocalRegistryFile())
	if err != nil {
		logger.Error("load local registry", "error", err)
		summary := &api.SyncSummary{At: time.Now().UTC(), Error: err.Error()}
		d.record(ctx, summary, nil)
		return summary, nil, false
	}
	// A filtered manual cycle syncs only the named folder's units, but the
	// FULL set is returned so the caller keeps the watcher whole-fleet — a
	// `sync --project X` must never shrink what the daemon watches.
	syncUnits := registry.Units
	if filter != "" {
		syncUnits = filterUnits(registry.Units, filter)
	}
	report, err := syncEngine.Sync(ctx, syncUnits)
	summary := toSummary(report)
	if err != nil {
		summary.Error = err.Error()
		logger.Error("sync cycle failed", "reason", reason, "error", err)
	} else {
		logger.Info("sync cycle", "reason", reason,
			"commits", len(report.Commits), "pushed", report.Pushed, "offline", report.Offline, "degraded", report.Degraded, "scrubbed", report.Scrubbed)
	}
	d.record(ctx, summary, syncUnits)
	return summary, registry.Units, true
}

// filterUnits keeps only the units whose repo folder matches folder.
func filterUnits(units []repo.Unit, folder string) []repo.Unit {
	filtered := make([]repo.Unit, 0, len(units))
	for _, u := range units {
		if u.Folder == folder {
			filtered = append(filtered, u)
		}
	}
	return filtered
}

// folderEnrolled reports whether any unit feeds folder.
func folderEnrolled(units []repo.Unit, folder string) bool {
	for _, u := range units {
		if u.Folder == folder {
			return true
		}
	}
	return false
}

// enrolledFolders is the sorted, de-duplicated set of repo folders in use —
// named in the 400 an unknown `sync --project` returns.
func enrolledFolders(units []repo.Unit) []string {
	seen := map[string]bool{}
	folders := make([]string, 0, len(units))
	for _, u := range units {
		if !seen[u.Folder] {
			seen[u.Folder] = true
			folders = append(folders, u.Folder)
		}
	}
	sort.Strings(folders)
	return folders
}

func (d *Daemon) record(ctx context.Context, summary *api.SyncSummary, synced []repo.Unit) {
	d.refreshState(ctx)
	d.recordOutcome(summary, synced)
}

// recordOutcome stores one cycle's result under d.mu: the last summary, and —
// scoped to the folders this cycle actually synced — each folder's degraded
// HEALTH flag and its last-cycle outcome (keyed by folder; a cycle degrades
// folders and errors whole-fleet). Scoping to synced folders is load-bearing: a
// filtered `sync --project X` must rewrite neither another folder's last-cycle
// nor its degraded flag, and synced is nil when no cycle ran (registry load
// failed), leaving both untouched.
func (d *Daemon) recordOutcome(summary *api.SyncSummary, synced []repo.Unit) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastSync = summary
	// Clear each synced folder's degraded flag, then re-mark the ones this cycle
	// reported degraded. NOT a wholesale wipe: that marked unsynced folders
	// healthy — a false HEALTH=ok contradicting their still-degraded LastCycle.
	// Orphan flags for since-untracked folders may linger, but Projects() (the
	// only reader of d.degraded) looks them up by enrolled unit, so they are
	// unreachable — not worth registry plumbing here to prune.
	for _, u := range synced {
		delete(d.degraded, u.Folder)
	}
	for _, folder := range summary.Degraded {
		d.degraded[folder] = true
	}
	for _, u := range synced {
		outcome := "ok"
		switch {
		case summary.Error != "":
			outcome = "error"
		case d.degraded[u.Folder]:
			outcome = "degraded"
		}
		d.lastCycle[u.Folder] = &api.UnitCycleResult{Outcome: outcome, FinishedAt: summary.At}
	}
}

// setWatchStates replaces the per-root watch posture wholesale — a rebuild always
// reports the full current root set — and prunes trigger counts for roots that
// dropped out (an untracked unit leaves no stale telemetry). Still-watched roots
// keep their counts, so a rebuild (watcher death, enrollment change) does not
// reset them.
func (d *Daemon) setWatchStates(states map[string]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.watchState = states
	for root := range d.watchTriggers {
		if _, ok := states[root]; !ok {
			delete(d.watchTriggers, root)
		}
	}
}

// recordWatchTrigger counts one watch-arm trigger against every currently-watched
// root. A trigger drives one whole-fleet cycle (ADR 07: the watcher never routes
// per-unit), so each watched unit genuinely participates and is counted. The
// timer backstop ("poll") is excluded — WatchTriggers is a filesystem-event
// signal, not an uptime counter.
func (d *Daemon) recordWatchTrigger(reason string, roots []string) {
	if reason == "poll" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, root := range roots {
		d.watchTriggers[root]++
	}
}

// watchFailed formats a WatchState for a unit whose watch could not be
// established or died: it names the reason AND conveys that the ticker/poll
// backstop still syncs the unit, so the daemon's log-and-continue reads as a
// degraded-but-covered unit rather than a dropped one.
func watchFailed(reason string) string {
	return "failed: " + reason + "; ticker/poll backstop still covers it"
}

func toSummary(report engine.Report) *api.SyncSummary {
	return &api.SyncSummary{
		At:         time.Now().UTC(),
		Commits:    report.Commits,
		MirrorIn:   api.Stats(report.MirrorIn),
		MirrorOut:  api.Stats(report.MirrorOut),
		Degraded:   report.Degraded,
		Offline:    report.Offline,
		Scrubbed:   report.Scrubbed,
		Pushed:     report.Pushed,
		PushQueued: report.PushQueued,
	}
}

// checkoutState IS the daemon's readiness gate (spec §5: "the daemon
// refuses to sync until doctor passes"), evaluated fresh before every
// cycle and admin op via doctor.SafetyGate's checkout+keyset+filters+
// attributes battery. detail names the broken axis (e.g. "doctor:
// keyset: ...") and is "" when state is "ready".
func (d *Daemon) checkoutState(ctx context.Context) (state, detail string) {
	if err := doctor.SafetyGate(ctx, d.cfg.Paths, d.cfg.Registry, d.binaryPath); err != nil {
		return "uninitialized", err.Error()
	}
	return "ready", ""
}

// refreshState evaluates checkoutState and records the result for Status
// (StateDetail is part of the wire contract, api.StatusResponse), then
// returns it so the caller can act on it immediately. Every call site that
// pays for a gate evaluation refreshes what Status reports through this —
// a failed TriggerSync/submitAdmin probe must be visible to the NEXT
// client that asks, not just embedded in the error the asking client got.
func (d *Daemon) refreshState(ctx context.Context) (state, detail string) {
	state, detail = d.checkoutState(ctx)
	d.mu.Lock()
	d.state = state
	d.stateDetail = detail
	d.mu.Unlock()
	return state, detail
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "unknown-host"
	}
	return name
}

// --- controller implementation (Task 10 interface) ---

// Status implements controller. QuiescedUntil is reported only while a hold
// is genuinely active (deadline in the future) — an expired deadline reads as
// nil, so status never advertises a hold auto-release already lifted.
func (d *Daemon) Status() api.StatusResponse {
	d.mu.Lock()
	defer d.mu.Unlock()
	response := api.StatusResponse{
		Version:     d.cfg.Version,
		State:       d.state,
		StateDetail: d.stateDetail,
		PID:         os.Getpid(),
		StartedAt:   d.startedAt,
		LastSync:    d.lastSync,
	}
	if !d.quiescedUntil.IsZero() && time.Now().Before(d.quiescedUntil) {
		until := d.quiescedUntil
		response.QuiescedUntil = &until
	}
	return response
}

// quiesced reports whether a hold is active as of now, returning the deadline
// for the skip-log / refusal message. The read is a single lock so a cycle
// can never slip through between a quiesce write and this check.
func (d *Daemon) quiesced(now time.Time) (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.quiescedUntil.IsZero() || !now.Before(d.quiescedUntil) {
		return time.Time{}, false
	}
	return d.quiescedUntil, true
}

// refuseManualSyncIfQuiesced closes TriggerSync's check-then-enqueue window. A
// quiesce landing after TriggerSync's entry check (d.quiesced) but before the
// loop's syncRequests arm services the request would otherwise run one manual
// cycle inside the hold. The arm calls this FIRST: a true return means it has
// already replied with the same errQuiesced refusal TriggerSync uses up front,
// so the arm skips the cycle; false means no hold is active and the cycle
// proceeds. request.reply is buffered (TriggerSync), so the send never blocks
// the loop even if the caller has already timed out and moved on.
func (d *Daemon) refuseManualSyncIfQuiesced(request syncRequest, now time.Time) bool {
	until, held := d.quiesced(now)
	if !held {
		return false
	}
	request.reply <- syncReply{err: errQuiesced(until)}
	return true
}

// Quiesce holds automatic sync cycles until now+clamp(seconds) and refuses
// explicit sync + mutations until then (Phase-4 F2). A fresh Quiesce while
// already held REPLACES the deadline — last writer wins, so the same CLI
// retrying simply resets the window rather than stacking holds. Implements
// controller.
func (d *Daemon) Quiesce(seconds int) api.QuiesceResponse {
	ttl := time.Duration(seconds) * time.Second
	ttl = min(max(ttl, quiesceMin), quiesceMax)
	until := time.Now().Add(ttl)
	d.mu.Lock()
	d.quiescedUntil = until
	d.mu.Unlock()
	return api.QuiesceResponse{Until: until}
}

// Resume lifts a hold early; idempotent — resuming a daemon that is not
// quiesced clears an already-zero deadline and returns the zero time.
// Implements controller.
func (d *Daemon) Resume() api.QuiesceResponse {
	d.mu.Lock()
	d.quiescedUntil = time.Time{}
	d.mu.Unlock()
	return api.QuiesceResponse{}
}

// TriggerSync implements controller: hand the request to the loop,
// wait bounded, report "running" on timeout. Client cancellation never
// cancels the cycle itself. A non-empty project filters the cycle to that
// repo folder; an unknown folder is a 400 naming the enrolled folders.
func (d *Daemon) TriggerSync(ctx context.Context, project string) (api.SyncResponse, error) {
	if until, held := d.quiesced(time.Now()); held {
		return api.SyncResponse{}, errQuiesced(until)
	}
	if state, detail := d.refreshState(ctx); state != "ready" {
		return api.SyncResponse{}, fmt.Errorf("%w: %s", errNotInitialized, detail)
	}
	if project != "" {
		// Validate the folder off the engine goroutine (a read, like
		// Projects) so the 400 is synchronous; the cycle re-loads and applies
		// the filter itself.
		registry, err := repo.LoadLocalRegistry(d.cfg.Paths.LocalRegistryFile())
		if err != nil {
			return api.SyncResponse{}, err
		}
		if !folderEnrolled(registry.Units, project) {
			return api.SyncResponse{}, statusError{
				code: http.StatusBadRequest,
				msg:  fmt.Sprintf("unknown folder %q; enrolled folders: %s", project, strings.Join(enrolledFolders(registry.Units), ", ")),
			}
		}
	}
	request := syncRequest{filter: project, reply: make(chan syncReply, 1)}
	timeout := time.After(syncWaitTimeout)
	select {
	case d.syncRequests <- request:
	case <-timeout:
		return api.SyncResponse{Status: "running"}, nil
	case <-ctx.Done():
		return api.SyncResponse{}, ctx.Err()
	}
	select {
	case reply := <-request.reply:
		return reply.response, reply.err
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
			Provider:      u.Provider,
			Folder:        u.Folder,
			LocalDir:      u.LocalDir,
			RepoSubdir:    u.RepoSubdir,
			Degraded:      d.degraded[u.Folder],
			WatchState:    d.watchState[u.LocalDir],
			WatchTriggers: d.watchTriggers[u.LocalDir],
			LastCycle:     d.lastCycle[u.Folder],
		})
	}
	return response
}

// submitAdmin hands an admin operation to the engine goroutine and waits
// bounded for its reply. The op itself is fast local git work; the wait
// absorbs the time it queues behind any running cycle.
func (d *Daemon) submitAdmin(ctx context.Context, reason string, run func(context.Context, *engine.Engine) (any, error)) (any, error) {
	if until, held := d.quiesced(time.Now()); held {
		return nil, errQuiesced(until)
	}
	if state, detail := d.refreshState(ctx); state != "ready" {
		return nil, fmt.Errorf("%w: %s", errNotInitialized, detail)
	}
	request := adminRequest{reason: reason, run: run, reply: make(chan adminReply, 1)}
	timeout := time.After(adminWaitTimeout)
	select {
	case d.adminRequests <- request:
	case <-timeout:
		return nil, errors.New("daemon busy with a sync cycle — try again")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case reply := <-request.reply:
		return reply.result, reply.err
	case <-timeout:
		return nil, errors.New("admin operation timed out")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// submitRead hands a read-only history/blob lookup to the engine goroutine
// and waits bounded for its reply — submitAdmin's shape, minus the quiesce
// check: spec §15 greys mutations only, and a history/blob read cannot
// corrupt or race the quiesce window's checkout surgery (ADR 20 D3), so
// there is nothing for a hold to protect a read from. The readiness gate
// stays: a checkout mid-init re-clone must still refuse, the same as every
// other engine-goroutine op.
func (d *Daemon) submitRead(ctx context.Context, reason string, run func(context.Context, *engine.Engine) (any, error)) (any, error) {
	if state, detail := d.refreshState(ctx); state != "ready" {
		return nil, fmt.Errorf("%w: %s", errNotInitialized, detail)
	}
	request := adminRequest{reason: reason, run: run, reply: make(chan adminReply, 1)}
	timeout := time.After(adminWaitTimeout)
	select {
	case d.readRequests <- request:
	case <-timeout:
		return nil, errors.New("daemon busy with a sync cycle — try again")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case reply := <-request.reply:
		return reply.result, reply.err
	case <-timeout:
		return nil, errors.New("admin operation timed out")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// resolveFolder decides the repo folder for a track/migrate request: global
// providers land in repo.GlobalFolder with no registration; per-project
// providers register (collision-disambiguated) on the engine goroutine.
func (d *Daemon) resolveFolder(ctx context.Context, e *engine.Engine, providerName, projectID, preferredFolder string) (string, error) {
	prov, ok := d.cfg.Registry.Get(providerName)
	if !ok {
		return "", fmt.Errorf("unknown provider %q", providerName)
	}
	if prov.Scope() == provider.ScopeGlobal {
		return repo.GlobalFolder, nil
	}
	return e.RegisterProject(ctx, providerName, projectID, preferredFolder)
}

// Track implements controller: register (per-project) + enroll, on the engine
// goroutine (ADR 03). The post-track cycle the loop runs mirrors the dir in
// and rebuilds the watcher.
func (d *Daemon) Track(ctx context.Context, req api.TrackRequest) (api.TrackResponse, error) {
	result, err := d.submitAdmin(ctx, "track", func(ctx context.Context, e *engine.Engine) (any, error) {
		folder, err := d.resolveFolder(ctx, e, req.Provider, req.ProjectID, req.PreferredFolder)
		if err != nil {
			return nil, err
		}
		registry, err := repo.LoadLocalRegistry(d.cfg.Paths.LocalRegistryFile())
		if err != nil {
			return nil, err
		}
		unit := repo.Unit{
			Provider:   req.Provider,
			ProjectID:  req.ProjectID,
			Folder:     folder,
			LocalDir:   req.LocalDir,
			RepoSubdir: req.RepoSubdir,
		}
		if err := registry.Enroll(unit); err != nil {
			return nil, err
		}
		if err := registry.Save(d.cfg.Paths.LocalRegistryFile()); err != nil {
			return nil, err
		}
		return api.TrackResponse{Folder: folder}, nil
	})
	if err != nil {
		return api.TrackResponse{}, err
	}
	return result.(api.TrackResponse), nil
}

// Untrack implements controller: drop the local enrollment, and — only when
// this machine was the folder's last local tracker — purge the folder and its
// registry entry. Global folders are never purged (they are shared, spec §3).
func (d *Daemon) Untrack(ctx context.Context, req api.UntrackRequest) (api.UntrackResponse, error) {
	result, err := d.submitAdmin(ctx, "untrack", func(ctx context.Context, e *engine.Engine) (any, error) {
		registry, err := repo.LoadLocalRegistry(d.cfg.Paths.LocalRegistryFile())
		if err != nil {
			return nil, err
		}
		folder := ""
		for _, u := range registry.Units {
			if u.Provider == req.Provider && u.LocalDir == req.LocalDir {
				folder = u.Folder
				break
			}
		}
		removed := registry.Remove(req.Provider, req.LocalDir)
		if removed {
			if err := registry.Save(d.cfg.Paths.LocalRegistryFile()); err != nil {
				return nil, err
			}
		}
		purged := false
		if req.Purge && removed && folder != "" && folder != repo.GlobalFolder && !folderEnrolled(registry.Units, folder) {
			if err := e.PurgeProject(ctx, folder); err != nil {
				return nil, err
			}
			purged = true
		}
		return api.UntrackResponse{Removed: removed, Purged: purged}, nil
	})
	if err != nil {
		return api.UntrackResponse{}, err
	}
	return result.(api.UntrackResponse), nil
}

// Migrate implements controller: register → seed → enroll, ORDER-SENSITIVELY
// (spec §10), so the loop's post-migrate cycle overlays live state onto the
// seed layer.
func (d *Daemon) Migrate(ctx context.Context, req api.MigrateRequest) (api.MigrateResponse, error) {
	result, err := d.submitAdmin(ctx, "migrate", func(ctx context.Context, e *engine.Engine) (any, error) {
		folder, err := d.resolveFolder(ctx, e, req.Provider, req.ProjectID, req.PreferredFolder)
		if err != nil {
			return nil, err
		}
		report, err := e.SeedProject(ctx, folder, req.Provider, req.Slug, req.SeedDir)
		if err != nil {
			return nil, err
		}
		registry, err := repo.LoadLocalRegistry(d.cfg.Paths.LocalRegistryFile())
		if err != nil {
			return nil, err
		}
		unit := repo.Unit{Provider: req.Provider, ProjectID: req.ProjectID, Folder: folder, LocalDir: req.LocalDir}
		if err := registry.Enroll(unit); err != nil {
			return nil, err
		}
		if err := registry.Save(d.cfg.Paths.LocalRegistryFile()); err != nil {
			return nil, err
		}
		return api.MigrateResponse{Folder: folder, Files: report.Files, Skipped: report.Skipped}, nil
	})
	if err != nil {
		return api.MigrateResponse{}, err
	}
	return result.(api.MigrateResponse), nil
}

// Reencrypt implements controller: re-encrypt the whole repo under the keyset's
// new primary, on the engine goroutine (ADR 03). Routing through submitAdmin
// inherits the busy-guard AND the quiesce-refusal for free — a re-encrypt is a
// mutation, refused while quiesced like track/migrate. `key rotate` calls this
// immediately after rotating the keyset, so the repo never lingers mixed-primary.
func (d *Daemon) Reencrypt(ctx context.Context) (api.ReencryptResponse, error) {
	result, err := d.submitAdmin(ctx, "reencrypt", func(ctx context.Context, e *engine.Engine) (any, error) {
		report, err := e.ReencryptAll(ctx)
		if err != nil {
			return nil, err
		}
		return api.ReencryptResponse{
			Files:      report.Files,
			Pushed:     report.Pushed,
			PushQueued: report.PushQueued,
		}, nil
	})
	if err != nil {
		return api.ReencryptResponse{}, err
	}
	return result.(api.ReencryptResponse), nil
}

// History implements controller: engine.History runs on the engine goroutine
// through the read funnel (submitRead, ADR 20 D3), so it never races the
// single writer. See mapHistoryError for the error→status mapping shared
// with Blob.
func (d *Daemon) History(ctx context.Context, folder, path string, limit int) (api.HistoryResponse, error) {
	result, err := d.submitRead(ctx, "history", func(ctx context.Context, e *engine.Engine) (any, error) {
		versions, err := e.History(ctx, folder, path, limit)
		if err != nil {
			return nil, mapHistoryError(err)
		}
		return toHistoryResponse(versions), nil
	})
	if err != nil {
		return api.HistoryResponse{}, err
	}
	return result.(api.HistoryResponse), nil
}

// Blob implements controller: engine.BlobAt runs on the engine goroutine
// through the read funnel (submitRead, ADR 20 D3), so it never races the
// single writer.
func (d *Daemon) Blob(ctx context.Context, folder, path, rev string) (api.BlobResponse, error) {
	result, err := d.submitRead(ctx, "blob", func(ctx context.Context, e *engine.Engine) (any, error) {
		content, err := e.BlobAt(ctx, folder, path, rev)
		if err != nil {
			return nil, mapHistoryError(err)
		}
		return api.BlobResponse{Content: string(content)}, nil
	})
	if err != nil {
		return api.BlobResponse{}, err
	}
	return result.(api.BlobResponse), nil
}

// mapHistoryError translates a History/BlobAt failure into the controller's
// HTTP status, shared by both since History can only ever produce the
// "everything else" case below (ErrBlobTooLarge/ErrBlobBinary are
// BlobAt-specific). Guard order: the two content caps are checked first;
// everything else — Task 1's shape validation (engine.ErrBadHistoryInput:
// a malformed folder/path/rev) and a git failure resolving an unknown rev
// or path (BlobAt's cat-file surfaces that verbatim, by design) — is the
// caller naming something that does not exist, a 400 like any other bad
// --project name (TriggerSync). A genuine infrastructure failure cannot
// reach here: submitRead's refreshState gate already refused an unhealthy
// checkout before the engine goroutine ever ran this call.
func mapHistoryError(err error) error {
	switch {
	case errors.Is(err, engine.ErrBlobTooLarge):
		return statusError{code: http.StatusRequestEntityTooLarge, msg: err.Error()}
	case errors.Is(err, engine.ErrBlobBinary):
		return statusError{code: http.StatusUnsupportedMediaType, msg: err.Error()}
	default:
		return statusError{code: http.StatusBadRequest, msg: err.Error()}
	}
}

// toHistoryResponse projects engine.HistoryVersion (Task 1) onto the wire
// shape: Timestamp is *time.Time, nil when Stamp is zero (Task 1's "not a
// capture subject" signal), rather than encoding the misleading literal
// "0001-01-01T00:00:00Z". A nil versions input (no history) still yields a
// non-nil, empty Versions slice, matching Projects()'s same
// never-null-on-the-wire convention for a list response.
func toHistoryResponse(versions []engine.HistoryVersion) api.HistoryResponse {
	response := api.HistoryResponse{Versions: make([]api.HistoryVersion, len(versions))}
	for i, v := range versions {
		wire := api.HistoryVersion{
			Rev:     v.Rev,
			Subject: v.Subject,
			Host:    v.Host,
			Paths:   v.Paths,
			Live:    v.Live,
		}
		if !v.Stamp.IsZero() {
			stamp := v.Stamp
			wire.Timestamp = &stamp
		}
		response.Versions[i] = wire
	}
	return response
}
