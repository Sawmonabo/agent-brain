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
	At        time.Time `json:"at"`
	Commits   []string  `json:"commits,omitempty"`
	MirrorIn  Stats     `json:"mirror_in"`
	MirrorOut Stats     `json:"mirror_out"`
	Degraded  []string  `json:"degraded,omitempty"`
	// Scrubbed lists git-meta paths the post-integrate scrub removed or
	// healed — nonzero means a remote pushed something hostile or
	// corrupted (spec §5).
	Scrubbed   []string `json:"scrubbed,omitempty"`
	Pushed     bool     `json:"pushed"`
	PushQueued bool     `json:"push_queued"`
	Error      string   `json:"error,omitempty"`
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

// SyncRequest is the optional POST /v0/sync body (spec §7:
// `sync [--project X]`). An empty Project means whole-fleet; a non-empty
// one filters the triggered cycle to that repo folder (unknown folder = 400).
type SyncRequest struct {
	Project string `json:"project,omitempty"`
}

// SyncResponse answers POST /v0/sync. Status is "completed" when the
// triggered cycle finished within the wait window, "running" otherwise.
type SyncResponse struct {
	Status  string       `json:"status"`
	Summary *SyncSummary `json:"summary,omitempty"`
}

// TrackRequest enrolls one provider dir. ProjectID is "" for global scope
// (the daemon maps it to repo.GlobalFolder and skips registration);
// PreferredFolder is ignored for global scope.
type TrackRequest struct {
	Provider        string `json:"provider"`
	ProjectID       string `json:"project_id"`
	PreferredFolder string `json:"preferred_folder"`
	LocalDir        string `json:"local_dir"`
	RepoSubdir      string `json:"repo_subdir,omitempty"`
}

// TrackResponse reports the repo folder the enrollment landed in
// (collision-disambiguated).
type TrackResponse struct {
	Folder string `json:"folder"`
}

// UntrackRequest removes the enrollment for (Provider, LocalDir). Purge also
// deletes the repo folder + its projects.toml entry when this machine was its
// last local tracker.
type UntrackRequest struct {
	Provider string `json:"provider"`
	LocalDir string `json:"local_dir"`
	Purge    bool   `json:"purge"`
}

// UntrackResponse reports what untrack did.
type UntrackResponse struct {
	Removed bool `json:"removed"`
	Purged  bool `json:"purged"`
}

// MigrateRequest seeds a bash-era memory tree then enrolls the live dir.
type MigrateRequest struct {
	Provider        string `json:"provider"`
	ProjectID       string `json:"project_id"`
	PreferredFolder string `json:"preferred_folder"`
	LocalDir        string `json:"local_dir"` // live memory dir to ENROLL (may not exist yet)
	Slug            string `json:"slug"`      // bash-era slug (marker key)
	SeedDir         string `json:"seed_dir"`  // legacy tree to import
}

// MigrateResponse mirrors the engine's SeedReport.
type MigrateResponse struct {
	Folder  string `json:"folder"`
	Files   int    `json:"files"`
	Skipped bool   `json:"skipped"`
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
