package service

import (
	"os"
	"strings"
)

// IsWSL2 reports whether we're inside WSL (ADR 04: service install is
// refused there; WSL2 runs on-demand mode, Phase 4).
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
