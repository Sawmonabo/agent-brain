package dashboard

import (
	"strings"
	"testing"
)

// TestAlternateScrollSequences pins the exact escapes: private mode 1007
// set/reset. Hand-typed literals, not the ansi helpers, so a helper
// regression cannot silently rewrite both sides of the comparison.
func TestAlternateScrollSequences(t *testing.T) {
	t.Parallel()
	if setAlternateScroll != "\x1b[?1007h" {
		t.Errorf("setAlternateScroll = %q, want %q", setAlternateScroll, "\x1b[?1007h")
	}
	if resetAlternateScroll != "\x1b[?1007l" {
		t.Errorf("resetAlternateScroll = %q, want %q", resetAlternateScroll, "\x1b[?1007l")
	}
}

// TestRestoreAlternateScroll pins the command-layer teardown: enabled writes
// exactly the reset sequence, disabled writes nothing (the mode was never
// set, so resetting would flip a state the user's own tooling may have set).
func TestRestoreAlternateScroll(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		enabled bool
		want    string
	}{
		{name: "enabled resets", enabled: true, want: "\x1b[?1007l"},
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
