package crypto

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestRewriteRetainBoth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		input         string
		wantConflicts bool
		want          string
	}{
		{
			name:          "clean merge untouched",
			input:         "line 1\nline 2\n",
			wantConflicts: false,
			want:          "line 1\nline 2\n",
		},
		{
			name: "single hunk becomes retain-both block",
			input: "intro\n" +
				"<<<<<<< version A\n" +
				"fact from machine A\n" +
				"=======\n" +
				"fact from machine B\n" +
				">>>>>>> version B\n" +
				"outro\n",
			wantConflicts: true,
			want: "intro\n" +
				"<!-- agent-brain conflict 2026-07-07T00:00:00Z: both versions retained — keep what is right, then delete these comment lines (spec §4) -->\n" +
				"<!-- agent-brain version: version A -->\n" +
				"fact from machine A\n" +
				"<!-- agent-brain version: version B -->\n" +
				"fact from machine B\n" +
				"<!-- agent-brain conflict end -->\n" +
				"outro\n",
		},
		{
			name: "two hunks both rewritten",
			input: "<<<<<<< version A\na1\n=======\nb1\n>>>>>>> version B\n" +
				"mid\n" +
				"<<<<<<< version A\na2\n=======\nb2\n>>>>>>> version B\n",
			wantConflicts: true,
			want: "<!-- agent-brain conflict 2026-07-07T00:00:00Z: both versions retained — keep what is right, then delete these comment lines (spec §4) -->\n" +
				"<!-- agent-brain version: version A -->\na1\n" +
				"<!-- agent-brain version: version B -->\nb1\n" +
				"<!-- agent-brain conflict end -->\n" +
				"mid\n" +
				"<!-- agent-brain conflict 2026-07-07T00:00:00Z: both versions retained — keep what is right, then delete these comment lines (spec §4) -->\n" +
				"<!-- agent-brain version: version A -->\na2\n" +
				"<!-- agent-brain version: version B -->\nb2\n" +
				"<!-- agent-brain conflict end -->\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, hadConflicts := RewriteRetainBoth([]byte(test.input), "version A", "version B", "2026-07-07T00:00:00Z")
			if hadConflicts != test.wantConflicts {
				t.Fatalf("hadConflicts = %v, want %v", hadConflicts, test.wantConflicts)
			}
			if diff := cmp.Diff(test.want, string(got)); diff != "" {
				t.Fatalf("output mismatch (-want +got):\n%s", diff)
			}
			if strings.Contains(string(got), "<<<<<<<") {
				t.Fatal("git conflict markers leaked into output")
			}
		})
	}
}

// TestRewriteRetainBothSanitizesLabels pins the label-sanitization contract
// (Q3 mandate): labels are neutralized at this format boundary so a hostile
// label cannot forge the block's parse anchors, while well-behaved labels
// (hostnames, the default "version A"/"version B") pass through byte-for-byte.
// The chosen replacement is U+FFFD for each of CR, LF, '<', '=', '>'.
func TestRewriteRetainBothSanitizesLabels(t *testing.T) {
	t.Parallel()
	const ts = "2026-07-07T00:00:00Z"
	const repl = "�" // U+FFFD replacement char = a neutralized byte
	// A single conflict hunk. RewriteRetainBoth keys on the marker PREFIXES,
	// not the in-hunk label text, so it always re-labels from the parameters.
	const input = "<<<<<<< L\nours line\n=======\ntheirs line\n>>>>>>> R\n"

	block := func(labelA, labelB string) string {
		return "<!-- agent-brain conflict " + ts + ": both versions retained — keep what is right, then delete these comment lines (spec §4) -->\n" +
			"<!-- agent-brain version: " + labelA + " -->\n" +
			"ours line\n" +
			"<!-- agent-brain version: " + labelB + " -->\n" +
			"theirs line\n" +
			"<!-- agent-brain conflict end -->\n"
	}

	tests := []struct {
		name           string
		labelA, labelB string
		want           string
	}{
		{
			name:   "well-behaved hostnames pass through unchanged",
			labelA: "host.example.com", labelB: "other_host-01",
			want: block("host.example.com", "other_host-01"),
		},
		{
			name:   "newline label cannot forge an extra anchor line",
			labelA: "hostA\n<!-- agent-brain conflict end -->\ntail", labelB: "ok",
			// \n -> repl; the '<' and '>' of the fake anchor -> repl.
			want: block("hostA"+repl+repl+"!-- agent-brain conflict end --"+repl+repl+"tail", "ok"),
		},
		{
			name:   "seven-char conflict markers are neutralized",
			labelA: "=======", labelB: ">>>>>>>",
			want: block(strings.Repeat(repl, 7), strings.Repeat(repl, 7)),
		},
		{
			name:   "carriage return is neutralized",
			labelA: "a\rb", labelB: "c\r\nd",
			want: block("a"+repl+"b", "c"+repl+repl+"d"),
		},
		{
			name:   "comment terminator in a label cannot close the comment early",
			labelA: "x-->y", labelB: "z",
			want: block("x--"+repl+"y", "z"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, hadConflicts := RewriteRetainBoth([]byte(input), test.labelA, test.labelB, ts)
			if !hadConflicts {
				t.Fatal("hadConflicts = false, want true (input has a conflict hunk)")
			}
			if diff := cmp.Diff(test.want, string(got)); diff != "" {
				t.Fatalf("sanitized output mismatch (-want +got):\n%s", diff)
			}
			// Format-stability invariants independent of the exact bytes above.
			endAnchors := 0
			for line := range strings.SplitSeq(string(got), "\n") {
				if line == "<!-- agent-brain conflict end -->" {
					endAnchors++
				}
			}
			if endAnchors != 1 {
				t.Fatalf("want exactly one conflict-end anchor line, got %d:\n%s", endAnchors, got)
			}
			if strings.ContainsRune(string(got), '\r') {
				t.Fatalf("a carriage return survived into the block:\n%q", got)
			}
			for _, marker := range []string{"\n<<<<<<< ", "\n=======", "\n>>>>>>> "} {
				if strings.Contains("\n"+string(got), marker) {
					t.Fatalf("a git conflict marker appears at a line start:\n%s", got)
				}
			}
		})
	}
}
