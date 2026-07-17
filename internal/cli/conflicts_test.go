package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/crypto"
	"github.com/Sawmonabo/agent-brain/internal/keys"
)

// newTestCodec builds a throwaway codec for fixture setup — conflicts_test.go
// only ever reads plaintext checkout copies, but building the retain-both
// fixture requires driving the real encrypted merge path (crypto.MergeFact)
// so the fixture is never a hand-typed imitation of the block format.
func newTestCodec(t *testing.T) *crypto.Codec {
	t.Helper()
	keysetPath := filepath.Join(t.TempDir(), "keyset.json")
	if err := keys.Generate(keysetPath); err != nil {
		t.Fatal(err)
	}
	primitive, err := keys.Primitive(keysetPath)
	if err != nil {
		t.Fatal(err)
	}
	return crypto.NewCodec(primitive)
}

func writeCipher(t *testing.T, codec *crypto.Codec, path string, plaintext []byte) {
	t.Helper()
	ciphertext, err := codec.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, ciphertext, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestConflictRecordRoundTrip pins the writer/reader contract: logConflict
// (merge.go) marshals config.ConflictRecord, and this test proves the bytes it
// writes unmarshal cleanly back into that same config-owned type.
func TestConflictRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "conflicts.jsonl")
	t.Setenv("AGENT_BRAIN_CONFLICT_LOG", logPath)

	logConflict("roundtrip.md")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var record config.ConflictRecord
	if err := json.Unmarshal(bytes.TrimRight(data, "\n"), &record); err != nil {
		t.Fatalf("logConflict output does not unmarshal into config.ConflictRecord: %v", err)
	}
	if record.Path != "roundtrip.md" {
		t.Fatalf("record.Path = %q, want %q", record.Path, "roundtrip.md")
	}
	if record.Mode != "fact" {
		t.Fatalf("record.Mode = %q, want %q", record.Mode, "fact")
	}
	if record.Time == "" {
		t.Fatal("record.Time is empty")
	}
}

// TestConflictsListNewestFirstWithLimit writes three records and proves
// `conflicts list --limit 2` shows only the newest two, newest first.
func TestConflictsListNewestFirstWithLimit(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir())
	t.Setenv("AGENT_BRAIN_DATA_DIR", dataDir)
	logPath := filepath.Join(dataDir, "conflicts.jsonl")
	t.Setenv("AGENT_BRAIN_CONFLICT_LOG", logPath)

	logConflict("first.md")
	logConflict("second.md")
	logConflict("third.md")

	out, err := runCmd(t, nil, "conflicts", "list", "--limit", "2")
	if err != nil {
		t.Fatalf("conflicts list: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "third.md") || !strings.Contains(got, "second.md") {
		t.Fatalf("conflicts list --limit 2 missing the newest two records:\n%s", got)
	}
	if strings.Contains(got, "first.md") {
		t.Fatalf("conflicts list --limit 2 must not include the oldest record:\n%s", got)
	}
}

// TestConflictsBareDefaultsToList proves bare `conflicts` (no subcommand)
// behaves like `conflicts list`.
func TestConflictsBareDefaultsToList(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir())
	t.Setenv("AGENT_BRAIN_DATA_DIR", dataDir)
	t.Setenv("AGENT_BRAIN_CONFLICT_LOG", filepath.Join(dataDir, "conflicts.jsonl"))

	logConflict("bare.md")

	out, err := runCmd(t, nil, "conflicts")
	if err != nil {
		t.Fatalf("bare conflicts: %v", err)
	}
	if !strings.Contains(string(out), "bare.md") {
		t.Fatalf("bare `conflicts` did not default to list:\n%s", out)
	}
}

// TestConflictsListEmptyState covers the never-conflicted machine: no log
// file exists yet.
func TestConflictsListEmptyState(t *testing.T) {
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir())
	t.Setenv("AGENT_BRAIN_DATA_DIR", t.TempDir())

	out, err := runCmd(t, nil, "conflicts", "list")
	if err != nil {
		t.Fatalf("conflicts list with no log file must exit 0: %v", err)
	}
	if !strings.Contains(string(out), "no conflicts logged") {
		t.Fatalf("empty conflicts-log message wrong:\n%s", out)
	}
}

// TestConflictsListToleratesRotationSibling proves the mid-run 5 MiB
// rotation (a .1 generation sibling of conflicts.jsonl) never breaks the
// reader — it must keep reading the live file cleanly.
func TestConflictsListToleratesRotationSibling(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir())
	t.Setenv("AGENT_BRAIN_DATA_DIR", dataDir)
	logPath := filepath.Join(dataDir, "conflicts.jsonl")
	t.Setenv("AGENT_BRAIN_CONFLICT_LOG", logPath)

	logConflict("live.md")
	rotated := `{"time":"2020-01-01T00:00:00Z","path":"old.md","mode":"fact"}` + "\n"
	if err := os.WriteFile(logPath+".1", []byte(rotated), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, nil, "conflicts", "list")
	if err != nil {
		t.Fatalf("conflicts list with a .1 sibling present: %v", err)
	}
	if !strings.Contains(string(out), "live.md") {
		t.Fatalf("conflicts list missing the live record:\n%s", out)
	}
}

// TestConflictsShowPrintsRetainedBlock drives a REAL crypto.MergeFact on
// divergent inputs (never a hand-typed imitation of the block format),
// decrypts its output the way a checkout's smudge filter would, drops that
// plaintext where a synced memory file would live, and proves `conflicts
// show` finds and prints the retain-both block.
func TestConflictsShowPrintsRetainedBlock(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir())
	t.Setenv("AGENT_BRAIN_DATA_DIR", dataDir)

	codec := newTestCodec(t)
	tmp := t.TempDir()
	base, current, other := filepath.Join(tmp, "O"), filepath.Join(tmp, "A"), filepath.Join(tmp, "B")
	writeCipher(t, codec, base, []byte("fact: original\n"))
	writeCipher(t, codec, current, []byte("fact: from A\n"))
	writeCipher(t, codec, other, []byte("fact: from B\n"))

	hadConflicts, err := crypto.MergeFact(context.Background(), codec, base, current, other, "notes.md", "host-a", "host-b")
	if err != nil {
		t.Fatal(err)
	}
	if !hadConflicts {
		t.Fatal("fixture inputs must overlap to produce a retain-both block")
	}

	mergedCiphertext, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := codec.Decrypt(mergedCiphertext)
	if err != nil {
		t.Fatal(err)
	}

	memoriesDir := filepath.Join(dataDir, "memories")
	if err := os.MkdirAll(memoriesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memoriesDir, "notes.md"), plaintext, 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, nil, "conflicts", "show", "notes.md")
	if err != nil {
		t.Fatalf("conflicts show: %v", err)
	}
	got := string(out)
	for _, must := range []string{"agent-brain conflict", "fact: from A", "fact: from B", "agent-brain conflict end"} {
		if !strings.Contains(got, must) {
			t.Fatalf("conflicts show missing %q:\n%s", must, got)
		}
	}
	if strings.Contains(got, "<<<<<<<") {
		t.Fatalf("conflicts show leaked raw git conflict markers:\n%s", got)
	}
}

// TestConflictsShowRefusesOutOfTreePaths proves conflicts show cannot be
// used to read arbitrary files outside the memories checkout. This matters
// because the argument users pass here typically comes verbatim from
// `conflicts list`'s Path column, which is populated from conflict-log
// entries recorded while processing SYNCED, remote-influenced content — not
// purely user-authored input — so a hostile remote could try to launder an
// out-of-tree path through it.
func TestConflictsShowRefusesOutOfTreePaths(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir())
	t.Setenv("AGENT_BRAIN_DATA_DIR", dataDir)
	memoriesDir := filepath.Join(dataDir, "memories")
	if err := os.MkdirAll(memoriesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memoriesDir, "clean.md"), []byte("just plain content\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A secret file OUTSIDE the checkout that no escape must ever surface.
	outsideDir := t.TempDir()
	secretPath := filepath.Join(outsideDir, "secret.md")
	if err := os.WriteFile(secretPath, []byte("outside the checkout\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	relEscape, err := filepath.Rel(memoriesDir, secretPath)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		arg       string
		wantError bool
	}{
		{"relative traversal escape", relEscape, true},
		{"absolute path", secretPath, true},
		{"valid in-tree path (happy path)", "clean.md", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			out, err := runCmd(t, nil, "conflicts", "show", testCase.arg)
			if testCase.wantError {
				if err == nil {
					t.Fatalf("conflicts show %q must be refused, got output:\n%s", testCase.arg, out)
				}
				if strings.Contains(string(out), "outside the checkout") {
					t.Fatalf("conflicts show %q leaked the out-of-tree file's content:\n%s", testCase.arg, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("conflicts show %q (valid in-tree path) must still work: %v\n%s", testCase.arg, err, out)
			}
			if !strings.Contains(string(out), "already tidied") {
				t.Fatalf("conflicts show %q unexpected output:\n%s", testCase.arg, out)
			}
		})
	}
}

// TestConflictsShowTidiedFile covers the already-resolved case: a checkout
// file with no retain-both blocks left in it.
func TestConflictsShowTidiedFile(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir())
	t.Setenv("AGENT_BRAIN_DATA_DIR", dataDir)
	memoriesDir := filepath.Join(dataDir, "memories")
	if err := os.MkdirAll(memoriesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memoriesDir, "clean.md"), []byte("just plain content\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, nil, "conflicts", "show", "clean.md")
	if err != nil {
		t.Fatalf("conflicts show on a clean file must exit 0: %v", err)
	}
	if !strings.Contains(string(out), "no retained blocks in clean.md — already tidied") {
		t.Fatalf("tidied-file message wrong:\n%s", out)
	}
}
