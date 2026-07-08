// Package daemon hosts the resident sync daemon (ADR 04): the single
// engine goroutine, the watch manager, and the UDS API server (ADR 09).
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
)

// controller is what the HTTP layer needs from the daemon core; Task 11's
// Daemon implements it, tests fake it.
type controller interface {
	Status() api.StatusResponse
	TriggerSync(ctx context.Context) (api.SyncResponse, error)
	Projects() api.ProjectsResponse
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
		response, err := ctrl.TriggerSync(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
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
