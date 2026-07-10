// Package daemon hosts the resident sync daemon (ADR 04): the single
// engine goroutine, the watch manager, and the UDS API server (ADR 09).
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
)

// controller is what the HTTP layer needs from the daemon core; the Daemon
// implements it, tests fake it.
type controller interface {
	Status() api.StatusResponse
	TriggerSync(ctx context.Context, project string) (api.SyncResponse, error)
	Projects() api.ProjectsResponse
	Track(ctx context.Context, req api.TrackRequest) (api.TrackResponse, error)
	Untrack(ctx context.Context, req api.UntrackRequest) (api.UntrackResponse, error)
	Migrate(ctx context.Context, req api.MigrateRequest) (api.MigrateResponse, error)
	Reencrypt(ctx context.Context) (api.ReencryptResponse, error)
	Quiesce(seconds int) api.QuiesceResponse
	Resume() api.QuiesceResponse
}

// peerUIDFunc extracts the connecting process's UID from a unix socket
// connection. It is a seam so rejection paths are testable without a
// second user account; production uses defaultPeerUID (per-OS files).
type peerUIDFunc func(net.Conn) (int, error)

type peerInfo struct {
	uid int
	err error
}

type peerKey struct{}

// listenSocket binds socketPath, replacing any stale socket left by a
// crash (the flock, not the socket, is the single-instance guard) and
// locking the file to 0600 before any request is served.
func listenSocket(socketPath string) (net.Listener, error) {
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	return listener, nil
}

// newServer wires routes, peer-UID enforcement, and explicit timeouts.
func newServer(ctrl controller, peerUID peerUIDFunc) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v0/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, ctrl.Status())
	})
	mux.HandleFunc("/v0/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// The project filter is an OPTIONAL body: whole-fleet syncs send none.
		var req api.SyncRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		response, err := ctrl.TriggerSync(r.Context(), req.Project)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, response)
	})
	mux.HandleFunc("/v0/projects", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, ctrl.Projects())
	})
	mux.HandleFunc("/v0/track", postHandler(ctrl.Track))
	mux.HandleFunc("/v0/untrack", postHandler(ctrl.Untrack))
	mux.HandleFunc("/v0/migrate", postHandler(ctrl.Migrate))
	// /v0/reencrypt is a bodyless POST (spec §5 key rotation), so it cannot use
	// postHandler (which decodes a request body and 400s on EOF). It funnels
	// through the same controller/writeError shape as the admin endpoints — the
	// busy-guard and quiesce-refusal live in the controller (submitAdmin).
	mux.HandleFunc("/v0/reencrypt", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		resp, err := ctrl.Reencrypt(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, resp)
	})
	// /v0/quiesce carries both verbs on one route: POST sets/extends the hold
	// (clamped, body api.QuiesceRequest), DELETE releases it. Both reply with
	// api.QuiesceResponse — the resulting deadline (zero = released). Clamp
	// and last-writer-wins live in the controller (daemon.go), not here.
	mux.HandleFunc("/v0/quiesce", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var req api.QuiesceRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeJSON(w, ctrl.Quiesce(req.Seconds))
		case http.MethodDelete:
			writeJSON(w, ctrl.Resume())
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	return &http.Server{
		Handler: requireSameUser(mux),
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			uid, err := peerUID(conn)
			return context.WithValue(ctx, peerKey{}, peerInfo{uid: uid, err: err})
		},
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// requireSameUser rejects any request whose connection does not carry a
// verified same-UID peer credential. Fail closed: a missing or failed
// credential read is a rejection, never a pass-through.
func requireSameUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info, ok := r.Context().Value(peerKey{}).(peerInfo)
		if !ok || info.err != nil || info.uid != os.Getuid() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// postHandler wires one JSON-in/JSON-out POST endpoint: it enforces the
// method, decodes the request body (a malformed body is a 400), and maps a
// handler error to its status via writeError. The three admin endpoints
// (track/untrack/migrate) share this exact shape.
func postHandler[Req, Resp any](handle func(context.Context, Req) (Resp, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req Req
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := handle(r.Context(), req)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, resp)
	}
}

// writeError renders err as the plain-text error envelope the client decodes,
// honoring a statusError's code (an unknown --project folder is a 400);
// everything else is a 500.
func writeError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	var se statusError
	if errors.As(err, &se) {
		code = se.code
	}
	http.Error(w, err.Error(), code)
}
