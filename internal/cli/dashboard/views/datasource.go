package views

import (
	"context"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
)

// RequestTimeout bounds each background data Cmd — root's and this
// package's alike — so a wedged daemon cannot hang a poll. It is well under
// the UDS client's own 120s ceiling so the UI stays responsive even when a
// request is doomed.
const RequestTimeout = 10 * time.Second

// DataSource is the read/command seam every view renders from and acts
// through — the consumer-side interface (defined here, at the package that
// calls it, per Go's "accept interfaces" idiom). ProjectsView is the sole
// caller of the mutating methods (Sync/Track/Untrack); root fetches
// Status/Projects/Doctor/Conflicts itself and pushes results down via each
// view's Set* method. The production implementation (dashboard.apiData)
// wraps *api.Client for the daemon calls, runs the doctor battery through an
// injected closure, and reads the conflict log through
// config.ReadConflictLog (the same format owner the `conflicts` command
// reads). Tests inject a fake so no view test touches a socket, the
// filesystem, or the doctor battery — the reason the views stay pure and
// logic-heavy (task brief testing strategy).
//
// Track is part of the mirrored client mutation surface (spec §7); the
// current interactive toggle only ever calls Untrack, because the Projects
// view lists only already-enrolled units (there is no untracked row for
// Track to act on — the interactive re-track path is the `track` command).
// It stays on the interface so the seam is the full client surface the
// brief names.
type DataSource interface {
	Status(context.Context) (api.StatusResponse, error)
	Projects(context.Context) (api.ProjectsResponse, error)
	Sync(ctx context.Context, project string) (api.SyncResponse, error)
	Track(context.Context, api.TrackRequest) (api.TrackResponse, error)
	Untrack(context.Context, api.UntrackRequest) (api.UntrackResponse, error)
	// Migrate seeds one bash-era store then enrolls its live dir (spec §10) —
	// the mutating client call the Projects view's m flow drives. It joins the
	// mirrored client mutation surface alongside Track/Untrack; the production
	// apiData forwards it to api.Client.Migrate, tests inject a fake.
	Migrate(context.Context, api.MigrateRequest) (api.MigrateResponse, error)
	Doctor(context.Context) (doctor.Report, error)
	Conflicts() ([]config.ConflictRecord, error)
	// History and Blob are the read-only version surfaces the History screen
	// drills into, served through the daemon's read funnel (ADR 20
	// D3), never a mutation path. They live on the full DataSource — not only
	// the narrower HistoryDataSource the History screen consumes — so the
	// root's own m.data value (statically typed DataSource) satisfies that
	// consumer seam when the browser and reading views build a History screen.
	History(ctx context.Context, folder, path string, limit int) (api.HistoryResponse, error)
	Blob(ctx context.Context, folder, path, rev string) (api.BlobResponse, error)
}
