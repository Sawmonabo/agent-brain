package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeEncrypted runs content through git-clean and writes it where the
// driver expects a stored (post-clean) version — exactly what git hands us.
func writeEncrypted(t *testing.T, path string, content []byte) {
	t.Helper()
	ciphertext, err := runCmd(t, content, "git-clean")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, ciphertext, 0o644); err != nil {
		t.Fatal(err)
	}
}

func decryptFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := runCmd(t, data, "git-smudge")
	if err != nil {
		t.Fatal(err)
	}
	return plaintext
}

func TestGitMergeFactClean(t *testing.T) {
	setupKeyset(t)
	dir := t.TempDir()
	base, current, other := filepath.Join(dir, "O"), filepath.Join(dir, "A"), filepath.Join(dir, "B")
	writeEncrypted(t, base, []byte("top\nmiddle\nbottom\n"))
	writeEncrypted(t, current, []byte("top EDITED\nmiddle\nbottom\n"))
	writeEncrypted(t, other, []byte("top\nmiddle\nbottom EDITED\n"))

	if _, err := runCmd(t, nil, "git-merge", "--mode", "fact", "--", base, current, other, "notes.md"); err != nil {
		t.Fatalf("driver must exit resolved on mergeable input: %v", err)
	}
	got := decryptFile(t, current)
	want := []byte("top EDITED\nmiddle\nbottom EDITED\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("clean 3-way merge wrong:\n%s", got)
	}
}

func TestGitMergeFactOverlap(t *testing.T) {
	setupKeyset(t)
	dir := t.TempDir()
	base, current, other := filepath.Join(dir, "O"), filepath.Join(dir, "A"), filepath.Join(dir, "B")
	writeEncrypted(t, base, []byte("fact: original\n"))
	writeEncrypted(t, current, []byte("fact: version from machine A\n"))
	writeEncrypted(t, other, []byte("fact: version from machine B\n"))

	if _, err := runCmd(t, nil, "git-merge", "--mode", "fact", "--", base, current, other, "notes.md"); err != nil {
		t.Fatalf("driver must exit resolved even on overlap: %v", err)
	}
	got := string(decryptFile(t, current))
	for _, must := range []string{
		"agent-brain conflict",
		"fact: version from machine A",
		"fact: version from machine B",
		"agent-brain conflict end",
	} {
		if !bytes.Contains([]byte(got), []byte(must)) {
			t.Fatalf("retain-both output missing %q:\n%s", must, got)
		}
	}
	if bytes.Contains([]byte(got), []byte("<<<<<<<")) {
		t.Fatalf("git conflict markers leaked:\n%s", got)
	}
}

func TestGitMergeLwwKeepsCurrent(t *testing.T) {
	setupKeyset(t)
	dir := t.TempDir()
	base, current, other := filepath.Join(dir, "O"), filepath.Join(dir, "A"), filepath.Join(dir, "B")
	writeEncrypted(t, base, []byte("regenerated v1\n"))
	writeEncrypted(t, current, []byte("regenerated v2 (upstream)\n"))
	writeEncrypted(t, other, []byte("regenerated v2 (local replay)\n"))
	before, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, nil, "git-merge", "--mode", "lww", "--", base, current, other, "memory_summary.md"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("lww mode must leave %A unchanged")
	}
}

// TestGitMergeFactLabelOverride proves the AGENT_BRAIN_MERGE_LABEL_A/B contract
// (the Phase 2 engine sets these to host names): the override reaches the
// retain-both block labels the Phase 3 conflicts view renders.
func TestGitMergeFactLabelOverride(t *testing.T) {
	setupKeyset(t)
	t.Setenv("AGENT_BRAIN_MERGE_LABEL_A", "host-alpha")
	t.Setenv("AGENT_BRAIN_MERGE_LABEL_B", "host-beta")
	dir := t.TempDir()
	base, current, other := filepath.Join(dir, "O"), filepath.Join(dir, "A"), filepath.Join(dir, "B")
	writeEncrypted(t, base, []byte("fact: original\n"))
	writeEncrypted(t, current, []byte("fact: from A\n"))
	writeEncrypted(t, other, []byte("fact: from B\n"))

	if _, err := runCmd(t, nil, "git-merge", "--mode", "fact", "--", base, current, other, "notes.md"); err != nil {
		t.Fatalf("driver must exit resolved even on overlap: %v", err)
	}
	got := decryptFile(t, current)
	for _, must := range []string{
		"<!-- agent-brain version: host-alpha -->",
		"<!-- agent-brain version: host-beta -->",
	} {
		if !bytes.Contains(got, []byte(must)) {
			t.Fatalf("custom label %q missing from retain-both block:\n%s", must, got)
		}
	}
}

// TestGitMergeFactConflictLog proves the AGENT_BRAIN_CONFLICT_LOG contract the
// Phase 3 conflicts view consumes: a clean merge logs nothing, a conflicting
// merge appends exactly one JSON line carrying the pathname and mode.
func TestGitMergeFactConflictLog(t *testing.T) {
	setupKeyset(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "conflicts.jsonl")
	t.Setenv("AGENT_BRAIN_CONFLICT_LOG", logPath)

	// A clean 3-way merge (disjoint edits) resolves without overlap: no log.
	cleanBase, cleanCurrent, cleanOther := filepath.Join(dir, "cO"), filepath.Join(dir, "cA"), filepath.Join(dir, "cB")
	writeEncrypted(t, cleanBase, []byte("top\nmiddle\nbottom\n"))
	writeEncrypted(t, cleanCurrent, []byte("top EDITED\nmiddle\nbottom\n"))
	writeEncrypted(t, cleanOther, []byte("top\nmiddle\nbottom EDITED\n"))
	if _, err := runCmd(t, nil, "git-merge", "--mode", "fact", "--", cleanBase, cleanCurrent, cleanOther, "clean.md"); err != nil {
		t.Fatalf("clean merge must resolve: %v", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("clean merge must not create the conflict log; os.Stat err = %v", err)
	}

	// An overlapping merge conflicts: exactly one JSON line is appended.
	confBase, confCurrent, confOther := filepath.Join(dir, "kO"), filepath.Join(dir, "kA"), filepath.Join(dir, "kB")
	writeEncrypted(t, confBase, []byte("fact: original\n"))
	writeEncrypted(t, confCurrent, []byte("fact: from A\n"))
	writeEncrypted(t, confOther, []byte("fact: from B\n"))
	if _, err := runCmd(t, nil, "git-merge", "--mode", "fact", "--", confBase, confCurrent, confOther, "conflict.md"); err != nil {
		t.Fatalf("conflicting merge must still exit resolved: %v", err)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("conflict log was not written: %v", err)
	}
	lines := bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("want exactly one conflict-log line, got %d:\n%s", len(lines), raw)
	}
	var entry map[string]string
	if err := json.Unmarshal(lines[0], &entry); err != nil {
		t.Fatalf("conflict-log line is not valid JSON: %v (%q)", err, lines[0])
	}
	if entry["path"] != "conflict.md" {
		t.Fatalf("conflict-log path = %q, want %q", entry["path"], "conflict.md")
	}
	if entry["mode"] != "fact" {
		t.Fatalf("conflict-log mode = %q, want %q", entry["mode"], "fact")
	}
	if entry["time"] == "" {
		t.Fatal("conflict-log entry missing a time")
	}
}

// TestGitMergeUnknownMode guards the driver's --mode validation: an unknown
// policy is rejected with an error and writes nothing.
func TestGitMergeUnknownMode(t *testing.T) {
	t.Parallel() // no keyset or env: pure flag validation before any codec load
	out, err := runCmd(t, nil, "git-merge", "--mode", "bogus", "--", "base", "current", "other", "notes.md")
	if err == nil {
		t.Fatal("git-merge with an unknown --mode must error")
	}
	if len(out) != 0 {
		t.Fatalf("unknown --mode wrote %d bytes; must write nothing", len(out))
	}
}
