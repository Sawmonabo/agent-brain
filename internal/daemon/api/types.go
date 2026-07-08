// Package api is the daemon↔CLI wire contract (ADR 09) — the ONLY
// package both sides import (spec §8). It depends on nothing internal.
package api

import "time"

// Stats mirrors one direction of engine mirroring.
type Stats struct {
	Copied  int `json:"copied"`
	Deleted int `json:"deleted"`
	Skipped int `json:"skipped"`
}

// SyncSummary is one engine cycle's outcome, as reported over the API.
type SyncSummary struct {
	At         time.Time `json:"at"`
	Commits    []string  `json:"commits,omitempty"`
	MirrorIn   Stats     `json:"mirror_in"`
	MirrorOut  Stats     `json:"mirror_out"`
	Degraded   []string  `json:"degraded,omitempty"`
	Pushed     bool      `json:"pushed"`
	PushQueued bool      `json:"push_queued"`
	Error      string    `json:"error,omitempty"`
}

// StatusResponse answers GET /v0/status. State is "ready" when the
// memories checkout exists and cycles run, "uninitialized" when the
// daemon is up but the repo hasn't been provisioned yet (init is a
// Phase-3 command; the Phase-2 daemon must be honest about that state,
// not crash-loop on it).
type StatusResponse struct {
	Version   string       `json:"version"`
	State     string       `json:"state"`
	PID       int          `json:"pid"`
	StartedAt time.Time    `json:"started_at"`
	LastSync  *SyncSummary `json:"last_sync,omitempty"`
}

// SyncResponse answers POST /v0/sync. Status is "completed" when the
// triggered cycle finished within the wait window, "running" otherwise.
type SyncResponse struct {
	Status  string       `json:"status"`
	Summary *SyncSummary `json:"summary,omitempty"`
}

// UnitInfo is one enrolled (provider, dir) pair and its health.
type UnitInfo struct {
	Provider string `json:"provider"`
	Folder   string `json:"folder"`
	LocalDir string `json:"local_dir"`
	Degraded bool   `json:"degraded"`
}

// ProjectsResponse answers GET /v0/projects.
type ProjectsResponse struct {
	Units []UnitInfo `json:"units"`
}
