package cli

import (
	"strings"
	"testing"
)

// TestDashboardRefusesNonTTY pins the EOF/TTY contract: runCmd wires
// byte-buffer stdin/stdout (never *os.File), so the command must refuse cleanly
// rather than hang trying to drive a TUI on a pipe. There is no accessible-mode
// dashboard — the refusal points at the scriptable equivalents instead.
func TestDashboardRefusesNonTTY(t *testing.T) {
	t.Parallel()
	_, err := runCmd(t, nil, "dashboard")
	if err == nil {
		t.Fatal("dashboard did not refuse a non-tty invocation")
	}
	if !strings.Contains(err.Error(), "interactive terminal") {
		t.Errorf("error %q does not explain the tty requirement", err)
	}
	for _, want := range []string{"status --json", "projects --json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not point at the scriptable equivalent %q", err, want)
		}
	}
}

// TestDashboardHelpCrossReferencesJSON verifies the help text names the
// scriptable equivalents, so a user who cannot use the TUI is not stranded.
func TestDashboardHelpCrossReferencesJSON(t *testing.T) {
	t.Parallel()
	out, err := runCmd(t, nil, "dashboard", "--help")
	if err != nil {
		t.Fatalf("dashboard --help: %v", err)
	}
	help := string(out)
	for _, want := range []string{"status --json", "projects --json"} {
		if !strings.Contains(help, want) {
			t.Errorf("help missing scriptable equivalent %q; got:\n%s", want, help)
		}
	}
}
