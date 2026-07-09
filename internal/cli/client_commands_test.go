package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

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

func TestProjectsEmptyStateNamesTrack(t *testing.T) {
	startFakeDaemon(t, api.StatusResponse{}, api.SyncResponse{}, api.ProjectsResponse{})
	out := runCommand(t, "projects")
	if !strings.Contains(out, "agent-brain track") {
		t.Fatalf("empty-projects message must name `agent-brain track`:\n%s", out)
	}
}

// TestStatusJSONDecodesToAPIType proves `status --json` marshals the exact
// daemon/api.StatusResponse the client received — not a hand-shaped subset.
func TestStatusJSONDecodesToAPIType(t *testing.T) {
	want := api.StatusResponse{
		Version: "9.9.9", State: "ready", PID: 123,
		LastSync: &api.SyncSummary{Pushed: true, Commits: []string{"memory: a"}},
	}
	startFakeDaemon(t, want, api.SyncResponse{}, api.ProjectsResponse{})
	out := runCommand(t, "status", "--json")
	var got api.StatusResponse
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("status --json output does not decode into api.StatusResponse: %v\n%s", err, out)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("status --json (-want +got):\n%s", diff)
	}
}

// TestProjectsJSONDecodesToAPIType proves `projects --json` marshals the
// exact daemon/api.ProjectsResponse, including on the empty-units case
// (--json must never substitute the friendly empty-state text).
func TestProjectsJSONDecodesToAPIType(t *testing.T) {
	want := api.ProjectsResponse{Units: []api.UnitInfo{
		{Provider: "claude", Folder: "alpha", LocalDir: "/p/.claude/memory", Degraded: true},
	}}
	startFakeDaemon(t, api.StatusResponse{}, api.SyncResponse{}, want)
	out := runCommand(t, "projects", "--json")
	var got api.ProjectsResponse
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("projects --json output does not decode into api.ProjectsResponse: %v\n%s", err, out)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("projects --json (-want +got):\n%s", diff)
	}
}

// TestNewAPIClientPreChecksSocketPath proves the socket path is validated
// BEFORE any dial is attempted: an oversized AGENT_BRAIN_RUNTIME_DIR must
// fail with guidance naming the env var, not a bare dial/EINVAL error.
func TestNewAPIClientPreChecksSocketPath(t *testing.T) {
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", strings.Repeat("x", 200))
	_, err := newAPIClient()
	if err == nil {
		t.Fatal("newAPIClient with an oversized runtime dir must fail before dialing")
	}
	if !strings.Contains(err.Error(), "AGENT_BRAIN_RUNTIME_DIR") {
		t.Fatalf("socket pre-check error must name the fix: %v", err)
	}
}

// TestSyncProjectFlagSendsFilter proves `sync --project x` reaches the
// daemon as Task 7's SyncRequest{Project: "x"} body.
func TestSyncProjectFlagSendsFilter(t *testing.T) {
	dir, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", dir)

	var gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/v0/sync", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		gotBody = string(data)
		_ = json.NewEncoder(w).Encode(api.SyncResponse{Status: "completed"})
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

	runCommand(t, "sync", "--project", "x")
	if !strings.Contains(gotBody, `"project":"x"`) {
		t.Fatalf("sync --project x did not send the filter in the request body: %q", gotBody)
	}
}

// TestStatusRendersStateDetailAndUptime pins the human surface for the two
// StatusResponse fields the daemon populates: StateDetail (which names the
// broken axis when the daemon is not ready) and StartedAt (uptime). Before
// the Phase-3 final review both reached only the daemon log and `--json`, so
// `agent-brain status` said "uninitialized" with no reason.
func TestStatusRendersStateDetailAndUptime(t *testing.T) {
	startFakeDaemon(t,
		api.StatusResponse{
			Version: "1.2.3", State: "uninitialized", PID: 99,
			StateDetail: "doctor: keyset: cannot read keyset.json",
			StartedAt:   time.Now().Add(-90 * time.Minute),
		},
		api.SyncResponse{}, api.ProjectsResponse{})
	out := runCommand(t, "status")
	for _, want := range []string{"uninitialized", "doctor: keyset", "up 1h30m"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output missing %q:\n%s", want, out)
		}
	}
}

// TestStatusOmitsUptimeWhenStartedAtIsZero: a daemon that never reported a
// start time must not render a duration measured from the zero year.
func TestStatusOmitsUptimeWhenStartedAtIsZero(t *testing.T) {
	startFakeDaemon(t,
		api.StatusResponse{Version: "1.2.3", State: "ready", PID: 99},
		api.SyncResponse{}, api.ProjectsResponse{})
	out := runCommand(t, "status")
	if strings.Contains(out, "up ") {
		t.Fatalf("zero StartedAt must render no uptime:\n%s", out)
	}
}

// TestSyncRendersScrubbedPaths pins the hostile-push operator signal on the
// HUMAN surface. SyncSummary.Scrubbed nonzero means the engine removed or
// healed git-meta someone pushed to unscope the encryption filter (spec §5) —
// the loudest thing a cycle can report, and it must not hide in `--json`.
func TestSyncRendersScrubbedPaths(t *testing.T) {
	startFakeDaemon(t, api.StatusResponse{},
		api.SyncResponse{Status: "completed", Summary: &api.SyncSummary{
			Scrubbed: []string{"alpha/.gitattributes", ".gitattributes"},
			Pushed:   true,
		}}, api.ProjectsResponse{})
	out := runCommand(t, "sync")
	for _, want := range []string{"scrubbed", "alpha/.gitattributes", "unscope"} {
		if !strings.Contains(out, want) {
			t.Fatalf("sync output missing scrub signal %q:\n%s", want, out)
		}
	}
}
