package dashboard

import (
	"strings"
	"testing"
)

// TestAlternateScrollSequences pins the exact escapes: private mode 1007
// set/reset, and the XTSAVE/XTRESTORE save/restore pair. Hand-typed
// literals, not the ansi helpers or the production constants, so a
// regression in either cannot silently rewrite both sides of the
// comparison.
func TestAlternateScrollSequences(t *testing.T) {
	t.Parallel()
	if setAlternateScroll != "\x1b[?1007h" {
		t.Errorf("setAlternateScroll = %q, want %q", setAlternateScroll, "\x1b[?1007h")
	}
	if resetAlternateScroll != "\x1b[?1007l" {
		t.Errorf("resetAlternateScroll = %q, want %q", resetAlternateScroll, "\x1b[?1007l")
	}
	if saveAlternateScrollState != "\x1b[?1007s" {
		t.Errorf("saveAlternateScrollState = %q, want %q", saveAlternateScrollState, "\x1b[?1007s")
	}
	if restoreAlternateScrollState != "\x1b[?1007r" {
		t.Errorf("restoreAlternateScrollState = %q, want %q", restoreAlternateScrollState, "\x1b[?1007r")
	}
}

// TestRestoreAlternateScroll pins the command-layer teardown: enabled writes
// exactly DECRST followed by XTRESTORE (reset first, so terminals without
// the XTSAVE/XTRESTORE round-trip land on the plain-reset posture; restore
// second, so terminals that do support it recover the user's pre-hub state).
// Disabled writes nothing — the mode was never set, so resetting or
// restoring would flip a state the user's own tooling may have set.
func TestRestoreAlternateScroll(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		enabled bool
		want    string
	}{
		{name: "enabled resets then restores", enabled: true, want: "\x1b[?1007l\x1b[?1007r"},
		{name: "disabled writes nothing", enabled: false, want: ""},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			RestoreAlternateScroll(&out, testCase.enabled)
			if got := out.String(); got != testCase.want {
				t.Errorf("wrote %q, want %q", got, testCase.want)
			}
		})
	}
}
