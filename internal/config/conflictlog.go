package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// ConflictRecord is one retain-both event as recorded in the conflict log:
// the merge driver writes these (internal/cli/merge.go's logConflict), and
// `agent-brain conflicts` and the dashboard read them back. config owns the
// shape because it already owns the log's location (Paths.ConflictLogFile);
// a single format owner is what keeps the writer and every reader from drifting
// (the writer↔reader contract is pinned by a round-trip test in package cli).
type ConflictRecord struct {
	Time string `json:"time"`
	Path string `json:"path"`
	Mode string `json:"mode"`
}

// ReadConflictLog reads the newline-delimited JSON conflict log at path and
// returns its records in the order written — logConflict appends
// chronologically, so this is oldest-first. Blank lines are skipped. A missing
// log is not an error (a machine that has never conflicted has no file): the
// result is (nil, nil). Parse errors are wrapped with the path.
//
// Presentation is deliberately the caller's job, not this reader's: newest-first
// ordering, any display limit, and empty-state messaging differ between the
// `conflicts` command and the dashboard, so they stay at each call site while
// the on-disk format lives here once.
func ReadConflictLog(path string) ([]ConflictRecord, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the program-derived conflict-log location (Paths.ConflictLogFile), not untrusted input
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var records []ConflictRecord
	for _, line := range bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record ConflictRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("parse conflict log %s: %w", path, err)
		}
		records = append(records, record)
	}
	return records, nil
}
