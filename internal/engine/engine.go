// Package engine is the sync engine (spec §4): a single-goroutine
// pipeline — mirror-in, commit, integrate, reconcile, mirror-out, push —
// and the ONLY writer to the memories checkout. It depends on gitx,
// repo, and provider only (spec §8); encryption rides the checkout's
// git filters, so the engine never sees ciphertext.
package engine

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// ErrBusy means Sync was re-entered. The daemon's single goroutine makes
// this unreachable in correct code — surfacing it loudly beats silently
// serializing a logic error.
var ErrBusy = errors.New("engine: sync already running")

// Engine runs sync cycles against one memories checkout. Not safe for
// concurrent use: Sync guards against reentry with busy and fails loudly.
type Engine struct {
	checkout string
	host     string
	layout   repo.Layout
	registry *provider.Registry
	now      func() time.Time
	// busy is the non-reentrancy guard: Sync CAS-acquires it and returns
	// ErrBusy if a second call finds it already held.
	busy atomic.Bool
}

// New validates the wiring. host must already be sanitized — it becomes
// commit messages and the manifest filename.
func New(checkout, host string, registry *provider.Registry, now func() time.Time) (*Engine, error) {
	if !filepath.IsAbs(checkout) {
		return nil, fmt.Errorf("engine: checkout %q must be absolute", checkout)
	}
	if host == "" || repo.SanitizeHostname(host) != host {
		return nil, fmt.Errorf("engine: host %q must be a sanitized hostname", host)
	}
	if registry == nil {
		return nil, fmt.Errorf("engine: registry must not be nil")
	}
	if now == nil {
		return nil, fmt.Errorf("engine: clock must not be nil")
	}
	return &Engine{
		checkout: checkout,
		host:     host,
		layout:   repo.NewLayout(checkout),
		registry: registry,
		now:      now,
	}, nil
}

// stamp is the cycle timestamp: read once per cycle, RFC 3339 UTC —
// the v1-compatible `memory: <host> <project> <timestamp>` shape.
func (e *Engine) stamp() string { return e.now().UTC().Format(time.RFC3339) }

// MirrorStats counts one direction of mirroring.
type MirrorStats struct {
	Copied  int
	Deleted int
	// Skipped counts files deliberately not mirrored: irregular files
	// (symlinks — exfiltration guard) on the way in, locally-changed
	// files (converge next cycle) on the way out.
	Skipped int
}

// Report is one cycle's outcome; the daemon exposes the latest one.
type Report struct {
	Commits    []string
	MirrorIn   MirrorStats
	MirrorOut  MirrorStats
	Degraded   []string
	Pushed     bool
	PushQueued bool
}

// localSnapshot records each synced repo-relative path's local state at
// mirror-in time. Mirror-out compares against it: a file the user (or a
// live agent session) changed mid-cycle is skipped, not overwritten —
// the change converges through the next cycle.
type localSnapshot map[string]repo.ManifestEntry
