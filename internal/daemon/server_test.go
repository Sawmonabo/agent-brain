package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/engine"
)

type fakeController struct {
	status    api.StatusResponse
	sync      api.SyncResponse
	projects  api.ProjectsResponse
	track     api.TrackResponse
	untrack   api.UntrackResponse
	migrate   api.MigrateResponse
	reencrypt api.ReencryptResponse
	quiesce   api.QuiesceResponse
	history   api.HistoryResponse
	blob      api.BlobResponse

	reencryptErr    error // when set, Reencrypt returns it (drives the writeError envelope test)
	quiescedSeconds int   // last Quiesce arg, for the route's method-switch test
	resumed         bool  // set by Resume
	historyErr      error // when set, History returns it (drives the error-mapping tests)
	blobErr         error // when set, Blob returns it (drives the 413/415 tests)

	// historyFolder/historyPath/historyLimit and blobFolder/blobPath/blobRev
	// record the last call's arguments, for the query-parsing tests.
	historyFolder string
	historyPath   string
	historyLimit  int
	blobFolder    string
	blobPath      string
	blobRev       string
}

func (f *fakeController) Status() api.StatusResponse { return f.status }
func (f *fakeController) TriggerSync(context.Context, string) (api.SyncResponse, error) {
	return f.sync, nil
}
func (f *fakeController) Projects() api.ProjectsResponse { return f.projects }
func (f *fakeController) Track(context.Context, api.TrackRequest) (api.TrackResponse, error) {
	return f.track, nil
}

func (f *fakeController) Untrack(context.Context, api.UntrackRequest) (api.UntrackResponse, error) {
	return f.untrack, nil
}

func (f *fakeController) Migrate(context.Context, api.MigrateRequest) (api.MigrateResponse, error) {
	return f.migrate, nil
}

func (f *fakeController) Reencrypt(context.Context) (api.ReencryptResponse, error) {
	return f.reencrypt, f.reencryptErr
}

func (f *fakeController) Quiesce(seconds int) api.QuiesceResponse {
	f.quiescedSeconds = seconds
	return f.quiesce
}

func (f *fakeController) Resume() api.QuiesceResponse {
	f.resumed = true
	return api.QuiesceResponse{}
}

func (f *fakeController) History(_ context.Context, folder, path string, limit int) (api.HistoryResponse, error) {
	f.historyFolder, f.historyPath, f.historyLimit = folder, path, limit
	return f.history, f.historyErr
}

func (f *fakeController) Blob(_ context.Context, folder, path, rev string) (api.BlobResponse, error) {
	f.blobFolder, f.blobPath, f.blobRev = folder, path, rev
	return f.blob, f.blobErr
}

// shortSocketDir avoids t.TempDir(): test names inflate the path past
// the ~104-byte sun_path limit.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func startServer(t *testing.T, ctrl controller, peerUID peerUIDFunc) string {
	t.Helper()
	socketPath := filepath.Join(shortSocketDir(t), "agent-brain.sock")
	listener, err := listenSocket(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	server := newServer(ctrl, peerUID)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	})
	return socketPath
}

func TestStatusSyncProjectsRoundtrip(t *testing.T) {
	t.Parallel()
	want := &fakeController{
		status: api.StatusResponse{Version: "test", State: "ready", PID: 42, StartedAt: time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)},
		sync: api.SyncResponse{Status: "completed", Summary: &api.SyncSummary{
			Pushed: true, Commits: []string{"memory: host-a alpha 2026-07-08T12:00:00Z"},
			Scrubbed: []string{"alpha/.gitattributes"},
		}},
		projects: api.ProjectsResponse{Units: []api.UnitInfo{
			{Provider: "claude", Folder: "alpha", LocalDir: "/p/.claude/memory", Degraded: true},
		}},
	}
	socketPath := startServer(t, want, defaultPeerUID)
	client := api.NewClient(socketPath)
	ctx := context.Background()

	status, err := client.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want.status, status); diff != "" {
		t.Fatalf("status (-want +got):\n%s", diff)
	}
	syncResp, err := client.Sync(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want.sync, syncResp); diff != "" {
		t.Fatalf("sync (-want +got):\n%s", diff)
	}
	projects, err := client.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want.projects, projects); diff != "" {
		t.Fatalf("projects (-want +got):\n%s", diff)
	}
}

// TestQuiesceRouteMethodSwitch pins /v0/quiesce's single-route/two-verb
// wiring: POST reaches Quiesce (carrying the requested seconds) and returns
// its deadline, DELETE reaches Resume, and any other verb is a 405.
func TestQuiesceRouteMethodSwitch(t *testing.T) {
	t.Parallel()
	until := time.Date(2026, 7, 9, 12, 5, 0, 0, time.UTC)
	fake := &fakeController{quiesce: api.QuiesceResponse{Until: until}}
	socketPath := startServer(t, fake, defaultPeerUID)
	client := api.NewClient(socketPath)
	ctx := context.Background()

	resp, err := client.Quiesce(ctx, 120)
	if err != nil {
		t.Fatal(err)
	}
	if fake.quiescedSeconds != 120 {
		t.Fatalf("controller saw Seconds=%d, want 120", fake.quiescedSeconds)
	}
	if !resp.Until.Equal(until) {
		t.Fatalf("Until = %s, want %s", resp.Until, until)
	}

	if _, err := client.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if !fake.resumed {
		t.Fatal("DELETE /v0/quiesce did not reach Resume")
	}

	if err := client.GetForTest(ctx, "/v0/quiesce"); err == nil ||
		!strings.Contains(err.Error(), "405") {
		t.Fatalf("GET /v0/quiesce err = %v, want a 405", err)
	}
}

// TestToSummaryIncludesScrubbed pins the engine-report → DTO mapping for
// Scrubbed: a nonzero count means a remote pushed something hostile or
// corrupted (spec §5), so it must survive the trip into api.SyncSummary
// alongside the fields toSummary already carried.
func TestToSummaryIncludesScrubbed(t *testing.T) {
	t.Parallel()
	report := engine.Report{Scrubbed: []string{"alpha/.gitattributes", ".gitattributes"}}
	got := toSummary(report)
	if diff := cmp.Diff(report.Scrubbed, got.Scrubbed); diff != "" {
		t.Fatalf("toSummary Scrubbed (-want +got):\n%s", diff)
	}
}

func TestToSummaryCarriesOffline(t *testing.T) {
	t.Parallel()
	summary := toSummary(engine.Report{Offline: true, PushQueued: true})
	if !summary.Offline || !summary.PushQueued {
		t.Fatalf("summary = %+v, want Offline and PushQueued carried through", summary)
	}
}

func TestSocketMode0600(t *testing.T) {
	t.Parallel()
	socketPath := startServer(t, &fakeController{}, defaultPeerUID)
	info, err := os.Lstat(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("socket mode = %v, want 0600", got)
	}
}

func TestPeerUIDMismatchRejected(t *testing.T) {
	t.Parallel()
	hostileUID := func(net.Conn) (int, error) { return os.Getuid() + 1, nil }
	socketPath := startServer(t, &fakeController{}, hostileUID)
	if _, err := api.NewClient(socketPath).Status(context.Background()); err == nil {
		t.Fatal("mismatched peer UID was accepted")
	}
}

func TestPeerUIDErrorFailsClosed(t *testing.T) {
	t.Parallel()
	brokenUID := func(net.Conn) (int, error) { return 0, errors.New("cred read failed") }
	socketPath := startServer(t, &fakeController{}, brokenUID)
	if _, err := api.NewClient(socketPath).Status(context.Background()); err == nil {
		t.Fatal("credential-read failure was accepted")
	}
}

func TestSyncRequiresPOST(t *testing.T) {
	t.Parallel()
	socketPath := startServer(t, &fakeController{}, defaultPeerUID)
	client := api.NewClient(socketPath)
	// Status GETs work, so the transport is fine; a GET on /v0/sync must 405.
	if err := client.GetForTest(context.Background(), "/v0/sync"); err == nil {
		t.Fatal("GET /v0/sync succeeded, want 405")
	}
}

// TestReencryptRequiresPOST pins the method guard on the bodyless-POST
// /v0/reencrypt route: a GET is a 405, the same contract TestSyncRequiresPOST
// holds for /v0/sync.
func TestReencryptRequiresPOST(t *testing.T) {
	t.Parallel()
	socketPath := startServer(t, &fakeController{}, defaultPeerUID)
	client := api.NewClient(socketPath)
	if err := client.GetForTest(context.Background(), "/v0/reencrypt"); err == nil ||
		!strings.Contains(err.Error(), "405") {
		t.Fatalf("GET /v0/reencrypt err = %v, want a 405", err)
	}
}

// TestReencryptBodylessPOSTAndErrorEnvelope pins the route's two behaviors. A
// bodyless POST (the client sends no body) reaches ctrl.Reencrypt and its
// response round-trips — the route deliberately does NOT decode a body, so
// unlike postHandler it must not 400 on the empty-body EOF. And a controller
// error surfaces through the writeError envelope (500 + message), never as a
// marshaled success body.
func TestReencryptBodylessPOSTAndErrorEnvelope(t *testing.T) {
	t.Parallel()

	ok := &fakeController{reencrypt: api.ReencryptResponse{Files: 4, Pushed: true}}
	client := api.NewClient(startServer(t, ok, defaultPeerUID))
	resp, err := client.Reencrypt(context.Background())
	if err != nil {
		t.Fatalf("bodyless POST /v0/reencrypt errored: %v", err)
	}
	if diff := cmp.Diff(ok.reencrypt, resp); diff != "" {
		t.Fatalf("reencrypt response (-want +got):\n%s", diff)
	}

	failing := &fakeController{reencryptErr: errors.New("reencrypt boom")}
	client = api.NewClient(startServer(t, failing, defaultPeerUID))
	if _, err := client.Reencrypt(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "reencrypt boom") {
		t.Fatalf("error envelope = %v, want a 500 carrying the controller message", err)
	}
}

func TestClientReportsDaemonNotRunning(t *testing.T) {
	t.Parallel()
	client := api.NewClient(filepath.Join(shortSocketDir(t), "absent.sock"))
	_, err := client.Status(context.Background())
	if !errors.Is(err, api.ErrDaemonNotRunning) {
		t.Fatalf("err = %v, want ErrDaemonNotRunning", err)
	}
}

// TestHistoryEndpointParsesQuery pins /v0/history's GET-only query-string
// contract: the exact folder/path/limit values reach the controller
// untouched, a non-GET is refused, a malformed limit is a 400 before the
// controller ever sees the request, and a controller statusError surfaces
// through the same writeError envelope every other route uses.
func TestHistoryEndpointParsesQuery(t *testing.T) {
	t.Parallel()

	t.Run("GET threads folder/path/limit to the controller", func(t *testing.T) {
		t.Parallel()
		fake := &fakeController{history: api.HistoryResponse{Versions: []api.HistoryVersion{{Rev: "abc", Live: true}}}}
		client := api.NewClient(startServer(t, fake, defaultPeerUID))

		resp, err := client.History(context.Background(), "projA", "claude/n.md", 7)
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(fake.history, resp); diff != "" {
			t.Fatalf("history response (-want +got):\n%s", diff)
		}
		if fake.historyFolder != "projA" || fake.historyPath != "claude/n.md" || fake.historyLimit != 7 {
			t.Fatalf("controller saw folder=%q path=%q limit=%d, want projA/claude/n.md/7", fake.historyFolder, fake.historyPath, fake.historyLimit)
		}
	})

	t.Run("POST is refused", func(t *testing.T) {
		t.Parallel()
		client := api.NewClient(startServer(t, &fakeController{}, defaultPeerUID))
		if err := client.PostForTest(context.Background(), "/v0/history"); err == nil ||
			!strings.Contains(err.Error(), "405") {
			t.Fatalf("POST /v0/history err = %v, want a 405", err)
		}
	})

	t.Run("a malformed limit is a 400 before the controller sees the request", func(t *testing.T) {
		t.Parallel()
		fake := &fakeController{}
		socketPath := startServer(t, fake, defaultPeerUID)
		// The typed client cannot express a malformed limit; probe the raw
		// query string directly.
		if err := api.NewClient(socketPath).GetForTest(context.Background(), "/v0/history?folder=projA&limit=notanumber"); err == nil ||
			!strings.Contains(err.Error(), "400") {
			t.Fatalf("bad limit err = %v, want a 400", err)
		}
		if fake.historyFolder != "" {
			t.Fatalf("controller was called (folder=%q) despite the malformed limit", fake.historyFolder)
		}
	})

	t.Run("a controller statusError surfaces through the error envelope", func(t *testing.T) {
		t.Parallel()
		fake := &fakeController{historyErr: statusError{code: http.StatusBadRequest, msg: "bad folder"}}
		client := api.NewClient(startServer(t, fake, defaultPeerUID))
		if _, err := client.History(context.Background(), "x", "", 0); err == nil ||
			!strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "bad folder") {
			t.Fatalf("history error envelope = %v, want a 400 carrying %q", err, "bad folder")
		}
	})
}

// TestBlobEndpointParsesQuery pins /v0/blob's GET-only query-string
// contract: folder/path/rev reach the controller untouched, a non-GET is
// refused, and BlobAt's two content guards (oversize, binary) pass through
// as 413/415 respectively via a controller statusError.
func TestBlobEndpointParsesQuery(t *testing.T) {
	t.Parallel()

	t.Run("GET threads folder/path/rev to the controller", func(t *testing.T) {
		t.Parallel()
		fake := &fakeController{blob: api.BlobResponse{Content: "hello\n"}}
		client := api.NewClient(startServer(t, fake, defaultPeerUID))

		resp, err := client.Blob(context.Background(), "projA", "claude/n.md", "abc123")
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(fake.blob, resp); diff != "" {
			t.Fatalf("blob response (-want +got):\n%s", diff)
		}
		if fake.blobFolder != "projA" || fake.blobPath != "claude/n.md" || fake.blobRev != "abc123" {
			t.Fatalf("controller saw folder=%q path=%q rev=%q, want projA/claude/n.md/abc123", fake.blobFolder, fake.blobPath, fake.blobRev)
		}
	})

	t.Run("POST is refused", func(t *testing.T) {
		t.Parallel()
		client := api.NewClient(startServer(t, &fakeController{}, defaultPeerUID))
		if err := client.PostForTest(context.Background(), "/v0/blob"); err == nil ||
			!strings.Contains(err.Error(), "405") {
			t.Fatalf("POST /v0/blob err = %v, want a 405", err)
		}
	})

	t.Run("oversize content is a 413", func(t *testing.T) {
		t.Parallel()
		fake := &fakeController{blobErr: statusError{code: http.StatusRequestEntityTooLarge, msg: "blob exceeds the API size cap"}}
		client := api.NewClient(startServer(t, fake, defaultPeerUID))
		if _, err := client.Blob(context.Background(), "x", "y", "abc"); err == nil ||
			!strings.Contains(err.Error(), "413") {
			t.Fatalf("oversize blob err = %v, want a 413", err)
		}
	})

	t.Run("binary content is a 415", func(t *testing.T) {
		t.Parallel()
		fake := &fakeController{blobErr: statusError{code: http.StatusUnsupportedMediaType, msg: "blob is not valid UTF-8 text"}}
		client := api.NewClient(startServer(t, fake, defaultPeerUID))
		if _, err := client.Blob(context.Background(), "x", "y", "abc"); err == nil ||
			!strings.Contains(err.Error(), "415") {
			t.Fatalf("binary blob err = %v, want a 415", err)
		}
	})
}

func TestListenSocketReplacesStaleSocket(t *testing.T) {
	t.Parallel()
	socketPath := filepath.Join(shortSocketDir(t), "agent-brain.sock")
	first, err := listenSocket(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash: close the listener but leave the inode via a
	// fresh file (closing removes the socket file, so recreate one).
	_ = first.Close()
	if err := os.WriteFile(socketPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := listenSocket(socketPath)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	_ = second.Close()
}
