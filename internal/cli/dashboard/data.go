// Package dashboard implements `agent-brain dashboard`: a bubbletea v2 TUI
// over the running daemon (spec §7 — Projects · Conflicts · Activity · Doctor).
//
// It is the ONLY package (besides the cli root command that launches it)
// permitted to import bubbletea/bubbles/lipgloss directly (ADR 05 amendment);
// every other package keeps huh/fang. It consumes only EXISTING surfaces — the
// daemon UDS API (internal/daemon/api), the doctor battery (internal/doctor),
// and the read-only conflict-log file — and adds ZERO daemon endpoints. Every
// view refreshes on one shared tick; no view path performs I/O except through
// a bubbletea Cmd (model purity).
//
// The four tab views and their shared keymap live in the views subpackage,
// and the catppuccin-derived palette lives in theme (spec §15, split ahead of
// the dashboard-hub wave so every new screen lands in its final home from the
// start); this package is the tab-switching reducer that owns the shared
// status poll, the daemon-down/service-start flow, and this file's
// views.DataSource implementation.
package dashboard

import (
	"context"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/views"
	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
)

// apiData is the production views.DataSource: it wraps *api.Client for the
// daemon calls, runs the doctor battery through an injected closure, and
// reads the conflict log through config.ReadConflictLog (the same format
// owner the `conflicts` command reads). Tests inject a fake DataSource so no
// view test touches a socket, the filesystem, or the doctor battery — the
// reason the views stay pure and logic-heavy (task brief testing strategy).
type apiData struct {
	client    *api.Client
	runDoctor func(context.Context) (doctor.Report, error)
}

// var _ documents that apiData satisfies views.DataSource at compile time,
// rather than only where NewData happens to return it.
var _ views.DataSource = (*apiData)(nil)

// NewData builds the production data source. runDoctor is injected by the cli
// root command because a faithful doctor.Deps needs provider/ghx/registry
// composition that lives outside this package's import allowlist; the dashboard
// only ever sees the resulting doctor.Report.
func NewData(client *api.Client, runDoctor func(context.Context) (doctor.Report, error)) views.DataSource {
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

func (d *apiData) Migrate(ctx context.Context, req api.MigrateRequest) (api.MigrateResponse, error) {
	return d.client.Migrate(ctx, req)
}

func (d *apiData) Doctor(ctx context.Context) (doctor.Report, error) {
	return d.runDoctor(ctx)
}

func (d *apiData) History(ctx context.Context, folder, path string, limit int) (api.HistoryResponse, error) {
	return d.client.History(ctx, folder, path, limit)
}

func (d *apiData) Blob(ctx context.Context, folder, path, rev string) (api.BlobResponse, error) {
	return d.client.Blob(ctx, folder, path, rev)
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
