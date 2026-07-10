package dashboard

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/config"
)

// TestApiDataConflictsNewestFirst covers the dashboard-specific transform the
// adapter adds on top of the shared config.ReadConflictLog: records are
// returned newest-first for display, and a machine with no log yields an empty
// slice rather than an error. The parse/format behaviour itself is covered by
// config's own TestReadConflictLog. Not parallel — it sets process env.
func TestApiDataConflictsNewestFirst(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir())
	t.Setenv("AGENT_BRAIN_DATA_DIR", dataDir)

	data := &apiData{}

	// No log yet → empty, no error (the never-conflicted machine).
	got, err := data.Conflicts()
	if err != nil {
		t.Fatalf("Conflicts with no log: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Conflicts with no log = %+v, want empty", got)
	}

	// Records appended oldest-first must come back newest-first.
	logPath := filepath.Join(dataDir, "conflicts.jsonl")
	lines := `{"time":"t1","path":"a.md","mode":"fact"}` + "\n" +
		`{"time":"t2","path":"b.md","mode":"fact"}` + "\n"
	if err := os.WriteFile(logPath, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = data.Conflicts()
	if err != nil {
		t.Fatalf("Conflicts: %v", err)
	}
	want := []config.ConflictRecord{
		{Time: "t2", Path: "b.md", Mode: "fact"},
		{Time: "t1", Path: "a.md", Mode: "fact"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Conflicts order mismatch (-want +got):\n%s", diff)
	}
}
