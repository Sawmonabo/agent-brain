package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestServiceLogsPrintsTail proves `service logs -n 2` on a fabricated
// 5-line daemon.log prints only the last two lines plus a trailer naming
// the log path.
func TestServiceLogsPrintsTail(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir())
	t.Setenv("AGENT_BRAIN_DATA_DIR", dataDir)
	logPath := filepath.Join(dataDir, "daemon.log")
	content := "line1\nline2\nline3\nline4\nline5\n"
	if err := os.WriteFile(logPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, nil, "service", "logs", "-n", "2")
	if err != nil {
		t.Fatalf("service logs: %v", err)
	}
	got := string(out)
	for _, want := range []string{"line4", "line5", logPath} {
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Fatalf("service logs output missing %q:\n%s", want, got)
		}
	}
	if bytes.Contains([]byte(got), []byte("line3")) {
		t.Fatalf("service logs -n 2 printed more than 2 lines:\n%s", got)
	}
}

// TestServiceLogsDefaultLineCount proves the documented default of 100
// lines: a log shorter than that prints in full without a -n flag.
func TestServiceLogsDefaultLineCount(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir())
	t.Setenv("AGENT_BRAIN_DATA_DIR", dataDir)
	logPath := filepath.Join(dataDir, "daemon.log")
	if err := os.WriteFile(logPath, []byte("only line\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, nil, "service", "logs")
	if err != nil {
		t.Fatalf("service logs: %v", err)
	}
	if !bytes.Contains(out, []byte("only line")) {
		t.Fatalf("service logs (default -n) missing content:\n%s", out)
	}
}

// TestServiceLogsMissingFile proves logs works with the daemon down —
// exactly when logs matter most — and exits 0 with a friendly message
// rather than a raw stat error.
func TestServiceLogsMissingFile(t *testing.T) {
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir())
	t.Setenv("AGENT_BRAIN_DATA_DIR", t.TempDir())

	out, err := runCmd(t, nil, "service", "logs")
	if err != nil {
		t.Fatalf("service logs on a missing file must exit 0: %v", err)
	}
	if !bytes.Contains(out, []byte("no daemon log yet")) {
		t.Fatalf("missing-log message wrong:\n%s", out)
	}
}

// TestServiceLogsNotesRotationSibling proves the trailer names the .1
// rotation generation when Task 6's mid-run rotation has produced one.
func TestServiceLogsNotesRotationSibling(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir())
	t.Setenv("AGENT_BRAIN_DATA_DIR", dataDir)
	logPath := filepath.Join(dataDir, "daemon.log")
	if err := os.WriteFile(logPath, []byte("current\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath+".1", []byte("older\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runCmd(t, nil, "service", "logs")
	if err != nil {
		t.Fatalf("service logs: %v", err)
	}
	if !bytes.Contains(out, []byte(logPath+".1")) {
		t.Fatalf("service logs trailer must name the .1 sibling when present:\n%s", out)
	}
}
