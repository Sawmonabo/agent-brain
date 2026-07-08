package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// SocketName and LockName live inside RuntimeDir (ADR 09).
const (
	SocketName = "agent-brain.sock"
	LockName   = "agent-brain.lock"
)

// sunPathBudget stays under the smallest sun_path limit (104 bytes on
// macOS, 108 on Linux) with headroom — a silently truncated socket path
// binds somewhere unintended.
const sunPathBudget = 100

// RuntimeDir resolves where the daemon's socket and lock live (ADR 09):
// AGENT_BRAIN_RUNTIME_DIR when set (tests, unusual layouts); $TMPDIR on
// macOS (per-user, confined — never bare /tmp); $XDG_RUNTIME_DIR on
// Linux, then /run/user/<uid> when it exists, then a per-uid dir under
// os.TempDir() so session-less environments (containers, torn-down WSL2)
// degrade instead of bricking. The DAEMON creates the dir 0700 on every
// start (WSL2 tears /run/user/<uid> down across restarts); this function
// only resolves the path.
func RuntimeDir() (string, error) {
	if dir := os.Getenv("AGENT_BRAIN_RUNTIME_DIR"); dir != "" {
		return dir, nil
	}
	if runtime.GOOS == "darwin" {
		tmp := os.Getenv("TMPDIR")
		if tmp == "" {
			tmp = os.TempDir()
		}
		return filepath.Join(tmp, "agent-brain"), nil
	}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "agent-brain"), nil
	}
	runUser := fmt.Sprintf("/run/user/%d", os.Getuid())
	if info, err := os.Stat(runUser); err == nil && info.IsDir() {
		return filepath.Join(runUser, "agent-brain"), nil
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("agent-brain-%d", os.Getuid())), nil
}

// ValidateSocketPath rejects paths sun_path would truncate. The error
// names the escape hatch because the user's only fix is a shorter dir.
func ValidateSocketPath(socketPath string) error {
	if len(socketPath) > sunPathBudget {
		return fmt.Errorf("socket path %q is %d bytes; unix sockets cap at ~104 — set AGENT_BRAIN_RUNTIME_DIR to a shorter directory", socketPath, len(socketPath))
	}
	return nil
}
