// Package dashboard implements `agent-brain dashboard`: a bubbletea v2 TUI
// over the running daemon (spec §7 — Projects · Conflicts · Activity · Doctor).
//
// It is the ONLY package (besides the cli root command that launches it)
// permitted to import bubbletea/bubbles/lipgloss directly (ADR 05 amendment);
// every other package keeps huh/fang. It consumes only EXISTING surfaces — the
// daemon UDS API (internal/daemon/api), the doctor battery (internal/doctor),
// and the read-only conflict-log file — and adds ZERO daemon endpoints. Every
// view refreshes on one shared tick; no view path performs I/O except through
// a bubbletea Cmd (model purity, enforced by the Q3 gate).
package dashboard

import (
	"context"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
)

// dashboardData is the read/command seam every view renders from. The
// production implementation (apiData) wraps *api.Client for the daemon calls,
// runs the doctor battery through an injected closure, and reads the conflict
// log through config.ReadConflictLog (the same format owner the `conflicts`
// command reads). Tests inject a fake so no view test touches a socket, the
// filesystem, or the doctor battery — the reason the models stay pure and
// logic-heavy (task brief testing strategy).
//
// Track is part of the mirrored client mutation surface (spec §7); the current
// interactive toggle only ever calls Untrack, because the Projects view lists
// only already-enrolled units (there is no untracked row for Track to act on —
// the interactive re-track path is the `track` command). It stays on the
// interface so the seam is the full client surface the brief names.
type dashboardData interface {
	Status(context.Context) (api.StatusResponse, error)
	Projects(context.Context) (api.ProjectsResponse, error)
	Sync(ctx context.Context, project string) (api.SyncResponse, error)
	Track(context.Context, api.TrackRequest) (api.TrackResponse, error)
	Untrack(context.Context, api.UntrackRequest) (api.UntrackResponse, error)
	Doctor(context.Context) (doctor.Report, error)
	Conflicts() ([]config.ConflictRecord, error)
}

// apiData is the production dashboardData.
type apiData struct {
	client    *api.Client
	runDoctor func(context.Context) (doctor.Report, error)
}

// NewData builds the production data source. runDoctor is injected by the cli
// root command because a faithful doctor.Deps needs provider/ghx/registry
// composition that lives outside this package's import allowlist; the dashboard
// only ever sees the resulting doctor.Report.
func NewData(client *api.Client, runDoctor func(context.Context) (doctor.Report, error)) dashboardData {
	return &apiData{client: client, runDoctor: runDoctor}
}

func (d *apiData) Status(ctx context.Context) (api.StatusResponse, error) {
	return d.client.Status(ctx)
}

func (d *apiData) Projects(ctx context.Context) (api.ProjectsResponse, error) {
	return d.client.Projects(ctx)
}

func (d *apiData) Sync(ctx context.Context, project string) (api.SyncResponse, error) {
	return d.client.Sync(ctx, project)
}

func (d *apiData) Track(ctx context.Context, req api.TrackRequest) (api.TrackResponse, error) {
	return d.client.Track(ctx, req)
}

func (d *apiData) Untrack(ctx context.Context, req api.UntrackRequest) (api.UntrackResponse, error) {
	return d.client.Untrack(ctx, req)
}

func (d *apiData) Doctor(ctx context.Context) (doctor.Report, error) {
	return d.runDoctor(ctx)
}

// Conflicts reads the conflict log through the shared config.ReadConflictLog
// (a pure file read; readers never violate the single-writer invariant, spec
// §5/§11) and returns records newest-first for display. The reader yields write
// order and logConflict appends chronologically, so reversing is enough — no
// timestamp parsing. An absent log is not an error — config.ReadConflictLog
// returns no records, which the Conflicts view renders as an empty state.
func (d *apiData) Conflicts() ([]config.ConflictRecord, error) {
	paths, err := config.DefaultPaths()
	if err != nil {
		return nil, err
	}
	records, err := config.ReadConflictLog(paths.ConflictLogFile())
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}
	return records, nil
}
