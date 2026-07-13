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
	// Offline means this cycle found the remote network-unreachable: the fetch
	// failed with a transport-unreachable signature, integrate was skipped, and
	// any local commits were queued. Every other fetch failure is reported as
	// an error, not offline. Additive field — absent/false from older daemons.
	Offline bool `json:"offline,omitempty"`
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
// not crash-loop on it). StateDetail names the specific broken axis (e.g.
// "doctor: keyset: ...") when State is "uninitialized" — empty when ready.
type StatusResponse struct {
	Version     string       `json:"version"`
	State       string       `json:"state"`
	StateDetail string       `json:"state_detail,omitempty"`
	PID         int          `json:"pid"`
	StartedAt   time.Time    `json:"started_at"`
	LastSync    *SyncSummary `json:"last_sync,omitempty"`
	// QuiescedUntil is the deadline of an active hold (POST /v0/quiesce):
	// while set and in the future the daemon skips tick/watch cycles and
	// refuses explicit sync + mutations. nil when not quiesced (including a
	// deadline already in the past — status reports honestly, not stale).
	QuiescedUntil *time.Time `json:"quiesced_until,omitempty"`
}

// QuiesceRequest asks the daemon to hold automatic sync cycles for a bounded
// window (POST /v0/quiesce). Seconds is clamped server-side to [1, 600]; the
// hold auto-releases at the deadline, so a crashed caller can never wedge the
// daemon permanently. init and doctor --fix use it to keep the engine off the
// checkout during their git surgery (Phase-4 F2).
type QuiesceRequest struct {
	Seconds int `json:"seconds"`
}

// QuiesceResponse reports the resulting hold deadline. A zero Until means
// released — the DELETE /v0/quiesce (resume) reply, or a resume of a daemon
// that was not quiesced.
type QuiesceResponse struct {
	Until time.Time `json:"until"`
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

// ReencryptResponse answers POST /v0/reencrypt (spec §5 key rotation): the
// daemon re-encrypted Files blobs under the new primary in one commit, and
// Pushed/PushQueued report whether that commit reached the remote. Files == 0
// means the primary was unchanged, so renormalize made no commit (a clean
// no-op). Failures (busy, quiesced, uninitialized, git errors) travel as the
// HTTP error envelope like every other endpoint, never in the success body.
type ReencryptResponse struct {
	Files      int  `json:"files"`
	Pushed     bool `json:"pushed"`
	PushQueued bool `json:"push_queued"`
}

// UnitInfo is one enrolled (provider, dir) pair and its health. The telemetry
// fields (WatchState, WatchTriggers, LastCycle) are strictly additive (Task 6.5)
// and all omitempty: a daemon that has not populated them yet, or an old client,
// is unaffected — the payload is byte-identical to before when they are unset.
type UnitInfo struct {
	Provider string `json:"provider"`
	Folder   string `json:"folder"`
	LocalDir string `json:"local_dir"`
	// RepoSubdir mirrors repo.Unit.RepoSubdir — the hub needs it to map this
	// unit's local file to its repo path (<provider>/<repo_subdir>/<file>).
	// Additive (Task-Phase-5): empty for every unit enrolled before this
	// field existed, so a pre-change payload is byte-identical.
	RepoSubdir string `json:"repo_subdir,omitempty"`
	Degraded   bool   `json:"degraded"`
	// WatchState is the unit's live watch posture: "watching" when its dir is
	// attached to the fsnotify watcher, or "failed: <reason>" when establishing
	// or running the watch failed — the ticker/poll backstop still syncs such a
	// unit, which the fallback wording conveys. Empty until the daemon's first
	// watcher build records it.
	WatchState string `json:"watch_state,omitempty"`
	// WatchTriggers counts filesystem-driven watch triggers (fs/overflow, not the
	// timer backstop) that swept this unit since its dir was first watched. A
	// watch trigger drives one whole-fleet cycle (ADR 07), so every watched unit
	// is counted; the dashboard takes the MAX over units for a fleet total — a
	// root watched since daemon start caught every trigger, so the max is the raw
	// trigger count since the longest-watched root (a sum would amplify it by
	// fleet size).
	WatchTriggers uint64 `json:"watch_triggers,omitempty"`
	// LastCycle is this unit's most recent completed cycle outcome, nil until its
	// folder has cycled at least once.
	LastCycle *UnitCycleResult `json:"last_cycle,omitempty"`
}

// UnitCycleResult is one unit's most recent completed sync-cycle outcome.
// Outcome is "ok" (folder synced clean), "degraded" (its folder withheld from
// integrate/push this cycle), or "error" (the whole cycle failed). FinishedAt is
// when that cycle completed.
type UnitCycleResult struct {
	Outcome    string    `json:"outcome"`
	FinishedAt time.Time `json:"finished_at"`
}

// ProjectsResponse answers GET /v0/projects.
type ProjectsResponse struct {
	Units []UnitInfo `json:"units"`
}

// HistoryVersion is one commit touching the queried folder/path, newest
// first (spec §6). It mirrors engine.HistoryVersion (Task 1) with one
// deliberate wire-shape difference: Timestamp is a *time.Time, omitempty,
// nil meaning "not a capture subject" — the engine's own HistoryVersion
// instead carries a zero time.Time for that same case, which would encode
// as the misleading literal "0001-01-01T00:00:00Z" over JSON.
type HistoryVersion struct {
	Rev     string `json:"rev"`
	Subject string `json:"subject"`
	// Host is the machine that made this capture; empty for a foreign
	// (non-capture-subject) commit.
	Host string `json:"host,omitempty"`
	// Timestamp is parsed from the engine's own capture-subject convention
	// (`memory: <host> <folder> <timestamp>`); nil for a foreign commit.
	Timestamp *time.Time `json:"timestamp,omitempty"`
	// Paths lists this version's folder-relative changed paths — populated
	// only in folder-wide mode (path == "" on the request), which is also
	// the source for the deleted-memories view (a path present in some
	// version's Paths but absent from HEAD).
	Paths []string `json:"paths,omitempty"`
	// Live means this rev's blob content, at the queried path, equals
	// HEAD's — a path-mode-only question; always false in folder-wide mode.
	Live bool `json:"live"`
}

// HistoryResponse answers GET /v0/history.
type HistoryResponse struct {
	Versions []HistoryVersion `json:"versions"`
}

// BlobResponse answers GET /v0/blob: decrypted content at a revision (via
// the checkout's own textconv/filter machinery), size-capped and refused
// when binary — see engine.ErrBlobTooLarge/ErrBlobBinary.
type BlobResponse struct {
	Content string `json:"content"`
}
