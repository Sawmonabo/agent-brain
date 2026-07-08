package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"syscall"
	"time"
)

// ErrDaemonNotRunning wraps connection failures that mean "no daemon on
// this socket" — the CLI turns it into actionable guidance.
var ErrDaemonNotRunning = errors.New("agent-brain daemon is not running")

// Client talks to the daemon over its unix socket.
type Client struct {
	http *http.Client
}

// NewClient dials socketPath for every request; the host in request
// URLs is a placeholder (UDS has no host).
func NewClient(socketPath string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{http: &http.Client{Transport: transport, Timeout: 120 * time.Second}}
}

// Status fetches the daemon's identity and last cycle.
func (c *Client) Status(ctx context.Context) (StatusResponse, error) {
	var out StatusResponse
	err := c.do(ctx, http.MethodGet, "/v0/status", &out)
	return out, err
}

// Sync triggers a cycle and waits (bounded server-side).
func (c *Client) Sync(ctx context.Context) (SyncResponse, error) {
	var out SyncResponse
	err := c.do(ctx, http.MethodPost, "/v0/sync", &out)
	return out, err
}

// Projects lists enrolled units and their health.
func (c *Client) Projects(ctx context.Context) (ProjectsResponse, error) {
	var out ProjectsResponse
	err := c.do(ctx, http.MethodGet, "/v0/projects", &out)
	return out, err
}

// GetForTest issues a bare GET so tests can probe method handling.
func (c *Client) GetForTest(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodGet, path, &struct{}{})
}

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	request, err := http.NewRequestWithContext(ctx, method, "http://agent-brain"+path, nil)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		if isNotRunning(err) {
			return fmt.Errorf("%w (socket dial failed: %w)", ErrDaemonNotRunning, err)
		}
		return err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon returned %s: %s", response.Status, body)
	}
	return json.Unmarshal(body, out)
}

func isNotRunning(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENOENT)
}
