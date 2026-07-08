package e2e

import (
	"os/exec"
	"strings"
	"testing"
)

// TestVersionOutput exercises the fang.WithVersion(cli.Version) wiring through
// the built binary — the end-to-end proof the Q1 review asked for (the flag was
// previously only checked by a manual run). cli.Version defaults to "dev"
// (release builds override it via -ldflags), so an un-stamped binary must print
// that default in its --version output.
func TestVersionOutput(t *testing.T) {
	t.Parallel()
	out, err := exec.CommandContext(suiteCtx, binPath, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("agent-brain --version: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "dev") {
		t.Fatalf("--version output %q does not contain the default version %q", out, "dev")
	}
}
