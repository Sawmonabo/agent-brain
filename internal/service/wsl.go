package service

import (
	"os"
	"strings"
)

// IsWSL2 reports whether we're inside WSL. This drives
// kardianosController's best-effort `loginctl enable-linger` on Install
// and LingerStatus's advisory line on `service status` — WSL2 has no
// display manager or login-session keyring to keep a resident systemd
// --user unit running past logout, unlike native macOS/Linux session
// management. It no longer gates whether service install runs at all;
// ADR 04's original WSL2 on-demand-mode split is a pragmatic
// resident+linger install for now, pending re-evaluation
// against real measurements.
func IsWSL2() bool { return detectWSL2(os.ReadFile) }

// detectWSL2 takes the reader as a seam so both branches unit-test on
// any OS. Any WSL kernel (1 or 2) brands /proc/version "microsoft".
func detectWSL2(readFile func(string) ([]byte, error)) bool {
	content, err := readFile("/proc/version")
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(content)), "microsoft")
}
