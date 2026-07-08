package e2e

import (
	"os/exec"
	"strings"
	"testing"
)

// TestVersionOutput exercises the fang.WithVersion(cli.Version) wiring through
// the built binary — the end-to-end proof the Q1 review asked for (the flag was
// previously only checked by a manual run). cli.Version defaults to "dev"
// (release builds override it via -ldflags), so an un-stamped binary prints
// fang's "agent-brain version dev" line. Asserting the "version dev" substring
// (not a bare "dev") ties the check to that line shape, ruling out an incidental
// "dev" appearing anywhere else in the output.
func TestVersionOutput(t *testing.T) {
	t.Parallel()
	out, err := exec.CommandContext(suiteCtx, binPath, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("agent-brain --version: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "version dev") {
		t.Fatalf("--version output %q does not contain fang's %q line", out, "version dev")
	}
}
