package api

import (
	"bytes"
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
	err := c.do(ctx, http.MethodGet, "/v0/status", nil, &out)
	return out, err
}

// Sync triggers a cycle and waits (bounded server-side). A non-empty project
// filters the cycle to that repo folder; "" is a whole-fleet sync and sends
// no request body (pre-Task-7 wire).
func (c *Client) Sync(ctx context.Context, project string) (SyncResponse, error) {
	var in any
	if project != "" {
		in = SyncRequest{Project: project}
	}
	var out SyncResponse
	err := c.do(ctx, http.MethodPost, "/v0/sync", in, &out)
	return out, err
}

// Projects lists enrolled units and their health.
func (c *Client) Projects(ctx context.Context) (ProjectsResponse, error) {
	var out ProjectsResponse
	err := c.do(ctx, http.MethodGet, "/v0/projects", nil, &out)
	return out, err
}

// Track enrolls a provider dir; the daemon registers the project, creates the
// folder, and enrolls the unit on its engine goroutine.
func (c *Client) Track(ctx context.Context, req TrackRequest) (TrackResponse, error) {
	var out TrackResponse
	err := c.do(ctx, http.MethodPost, "/v0/track", req, &out)
	return out, err
}

// Untrack removes an enrollment (and optionally purges the folder).
func (c *Client) Untrack(ctx context.Context, req UntrackRequest) (UntrackResponse, error) {
	var out UntrackResponse
	err := c.do(ctx, http.MethodPost, "/v0/untrack", req, &out)
	return out, err
}

// Migrate seeds a bash-era memory tree then enrolls the live dir.
func (c *Client) Migrate(ctx context.Context, req MigrateRequest) (MigrateResponse, error) {
	var out MigrateResponse
	err := c.do(ctx, http.MethodPost, "/v0/migrate", req, &out)
	return out, err
}

// GetForTest issues a bare GET so tests can probe method handling.
func (c *Client) GetForTest(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodGet, path, nil, &struct{}{})
}

// do issues one request. A nil in sends no body; a non-nil in is JSON-encoded
// with a Content-Type header. out receives the decoded 200 response.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var requestBody io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		requestBody = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://agent-brain"+path, requestBody)
	if err != nil {
		return err
	}
	if in != nil {
		request.Header.Set("Content-Type", "application/json")
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
