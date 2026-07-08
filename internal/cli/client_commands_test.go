package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
)

// startFakeDaemon serves canned API responses on a short-path socket and
// points the CLI at it via AGENT_BRAIN_RUNTIME_DIR (so
// daemon.SocketPathForClient resolves to it). t.Setenv ⇒ no t.Parallel.
func startFakeDaemon(t *testing.T, status api.StatusResponse, sync api.SyncResponse, projects api.ProjectsResponse) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", dir)

	mux := http.NewServeMux()
	mux.HandleFunc("/v0/status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(status)
	})
	mux.HandleFunc("/v0/sync", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(sync)
	})
	mux.HandleFunc("/v0/projects", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(projects)
	})
	listener, err := net.Listen("unix", filepath.Join(dir, "agent-brain.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
}

func runCommand(t *testing.T, args ...string) string {
	t.Helper()
	root := Root()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("%v: %v\noutput:\n%s", args, err, out.String())
	}
	return out.String()
}

func TestStatusCommandPrintsState(t *testing.T) {
	startFakeDaemon(t,
		api.StatusResponse{
			Version: "1.2.3", State: "ready", PID: 99,
			LastSync: &api.SyncSummary{Pushed: true, Degraded: []string{"alpha"}},
		},
		api.SyncResponse{}, api.ProjectsResponse{})
	out := runCommand(t, "status")
	for _, want := range []string{"ready", "1.2.3", "alpha"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Fatalf("status output missing %q:\n%s", want, out)
		}
	}
}

func TestSyncCommandPrintsSummary(t *testing.T) {
	startFakeDaemon(t, api.StatusResponse{},
		api.SyncResponse{Status: "completed", Summary: &api.SyncSummary{
			Commits: []string{"memory: host-a alpha 2026-07-08T12:00:00Z"}, Pushed: true,
		}}, api.ProjectsResponse{})
	out := runCommand(t, "sync")
	if !bytes.Contains([]byte(out), []byte("memory: host-a alpha")) || !bytes.Contains([]byte(out), []byte("pushed")) {
		t.Fatalf("sync output:\n%s", out)
	}
}

func TestProjectsCommandListsUnits(t *testing.T) {
	startFakeDaemon(t, api.StatusResponse{}, api.SyncResponse{},
		api.ProjectsResponse{Units: []api.UnitInfo{
			{Provider: "claude", Folder: "alpha", LocalDir: "/p/.claude/memory", Degraded: true},
		}})
	out := runCommand(t, "projects")
	for _, want := range []string{"claude", "alpha", "degraded"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Fatalf("projects output missing %q:\n%s", want, out)
		}
	}
}

func TestClientCommandsExplainDeadDaemon(t *testing.T) {
	dir, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", dir) // no socket inside

	root := Root()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"status"})
	if err := root.Execute(); err == nil {
		t.Fatal("status against dead daemon succeeded")
	} else if !bytes.Contains([]byte(err.Error()), []byte("service install")) {
		t.Fatalf("error lacks guidance: %v", err)
	}
}
