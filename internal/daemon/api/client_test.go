package api

import (
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
)

// serveUDS starts an http.Server on a fresh unix socket and returns its path.
// The socket lives under a short MkdirTemp path to stay within the ~104-byte
// sun_path limit (t.TempDir names would overflow it).
func serveUDS(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socketPath := filepath.Join(dir, "d.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 2 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	return socketPath
}

func TestClientTrackRoundtrip(t *testing.T) {
	t.Parallel()
	var got TrackRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/v0/track", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(TrackResponse{Folder: "alpha-2"})
	})
	client := NewClient(serveUDS(t, mux))

	want := TrackRequest{Provider: "claude", ProjectID: "id-x", PreferredFolder: "alpha", LocalDir: "/p/.claude/memory", RepoSubdir: "memories"}
	resp, err := client.Track(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Folder != "alpha-2" {
		t.Fatalf("folder = %q, want alpha-2", resp.Folder)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("server received (-sent +got):\n%s", diff)
	}
}

func TestClientUntrackRoundtrip(t *testing.T) {
	t.Parallel()
	var got UntrackRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/v0/untrack", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(UntrackResponse{Removed: true, Purged: true})
	})
	client := NewClient(serveUDS(t, mux))

	want := UntrackRequest{Provider: "claude", LocalDir: "/p/.claude/memory", Purge: true}
	resp, err := client.Untrack(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Removed || !resp.Purged {
		t.Fatalf("resp = %+v, want removed+purged", resp)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("server received (-sent +got):\n%s", diff)
	}
}

func TestClientMigrateRoundtrip(t *testing.T) {
	t.Parallel()
	var got MigrateRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/v0/migrate", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MigrateResponse{Folder: "mig", Files: 3, Skipped: false})
	})
	client := NewClient(serveUDS(t, mux))

	want := MigrateRequest{Provider: "claude", ProjectID: "id-m", PreferredFolder: "mig", LocalDir: "/p/.claude/memory", Slug: "old-slug", SeedDir: "/legacy/old-slug"}
	resp, err := client.Migrate(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Folder != "mig" || resp.Files != 3 {
		t.Fatalf("resp = %+v, want mig/3", resp)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("server received (-sent +got):\n%s", diff)
	}
}

func TestClientSyncCarriesProjectFilter(t *testing.T) {
	t.Parallel()
	bodies := make(chan string, 2)
	mux := http.NewServeMux()
	mux.HandleFunc("/v0/sync", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		bodies <- string(data)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(SyncResponse{Status: "completed"})
	})
	client := NewClient(serveUDS(t, mux))

	// A non-empty filter travels in the request body.
	if _, err := client.Sync(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	if body := <-bodies; !strings.Contains(body, `"project":"alpha"`) {
		t.Fatalf("filtered sync body = %q, want project filter", body)
	}
	// A whole-fleet sync sends no body (nil in), preserving the pre-Task-7 wire.
	if _, err := client.Sync(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if body := <-bodies; strings.TrimSpace(body) != "" {
		t.Fatalf("whole-fleet sync body = %q, want empty", body)
	}
}

func TestClientDecodesErrorEnvelope(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v0/track", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `unknown folder "bogus"; enrolled folders: alpha, beta`, http.StatusBadRequest)
	})
	client := NewClient(serveUDS(t, mux))

	_, err := client.Track(context.Background(), TrackRequest{Provider: "claude"})
	if err == nil {
		t.Fatal("expected an error for a 400 response")
	}
	if !strings.Contains(err.Error(), "enrolled folders: alpha, beta") {
		t.Fatalf("err = %v, want the server's message body", err)
	}
}
