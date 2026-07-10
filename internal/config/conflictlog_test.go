package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestReadConflictLog(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string // written to the log file; "" means write no file at all
		writeNo bool   // when true, do not create the file (missing-log case)
		want    []ConflictRecord
	}{
		{
			name:    "missing log is empty, not an error",
			writeNo: true,
			want:    nil,
		},
		{
			name:    "empty file is empty",
			content: "",
			want:    nil,
		},
		{
			name: "records preserved in write order (oldest first)",
			content: `{"time":"2026-07-01T00:00:00Z","path":"a.md","mode":"fact"}` + "\n" +
				`{"time":"2026-07-02T00:00:00Z","path":"b.md","mode":"fact"}` + "\n",
			want: []ConflictRecord{
				{Time: "2026-07-01T00:00:00Z", Path: "a.md", Mode: "fact"},
				{Time: "2026-07-02T00:00:00Z", Path: "b.md", Mode: "fact"},
			},
		},
		{
			name: "blank and whitespace-only lines are skipped",
			content: `{"time":"t1","path":"a.md","mode":"fact"}` + "\n" +
				"\n" + "   \n" +
				`{"time":"t2","path":"b.md","mode":"fact"}` + "\n",
			want: []ConflictRecord{
				{Time: "t1", Path: "a.md", Mode: "fact"},
				{Time: "t2", Path: "b.md", Mode: "fact"},
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			logPath := filepath.Join(t.TempDir(), "conflicts.jsonl")
			if !testCase.writeNo {
				if err := os.WriteFile(logPath, []byte(testCase.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := ReadConflictLog(logPath)
			if err != nil {
				t.Fatalf("ReadConflictLog: %v", err)
			}
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("records mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestReadConflictLogMalformedWrapsPath proves a malformed line surfaces a
// parse error naming the log path, so the operator knows which file to inspect.
func TestReadConflictLogMalformedWrapsPath(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "conflicts.jsonl")
	if err := os.WriteFile(logPath, []byte("{not json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadConflictLog(logPath)
	if err == nil {
		t.Fatal("expected a parse error for a malformed line")
	}
	if !strings.Contains(err.Error(), logPath) {
		t.Errorf("parse error %q does not name the log path %q", err, logPath)
	}
}
