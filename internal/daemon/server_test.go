package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/engine"
)

type fakeController struct {
	status   api.StatusResponse
	sync     api.SyncResponse
	projects api.ProjectsResponse
}

func (f *fakeController) Status() api.StatusResponse { return f.status }
func (f *fakeController) TriggerSync(context.Context) (api.SyncResponse, error) {
	return f.sync, nil
}
func (f *fakeController) Projects() api.ProjectsResponse { return f.projects }

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
	syncResp, err := client.Sync(ctx)
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

func TestClientReportsDaemonNotRunning(t *testing.T) {
	t.Parallel()
	client := api.NewClient(filepath.Join(shortSocketDir(t), "absent.sock"))
	_, err := client.Status(context.Background())
	if !errors.Is(err, api.ErrDaemonNotRunning) {
		t.Fatalf("err = %v, want ErrDaemonNotRunning", err)
	}
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
