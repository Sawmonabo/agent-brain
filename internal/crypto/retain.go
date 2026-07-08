package crypto

import (
	"bytes"
	"fmt"
	"strings"
)

// RewriteRetainBoth converts `git merge-file` conflict hunks into
// retain-both blocks (spec §4): HTML-comment markers so the block is inert
// in rendered markdown, both versions in full, labels + timestamp for the
// conflicts view. Marker prefixes match merge-file's 7-char default style.
func RewriteRetainBoth(merged []byte, labelA, labelB, timestamp string) ([]byte, bool) {
	lines := strings.SplitAfter(string(merged), "\n")
	var out bytes.Buffer
	hadConflicts := false
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "<<<<<<< ") {
			out.WriteString(lines[i])
			continue
		}
		// Collect the hunk: ours until =======, theirs until >>>>>>>.
		var ours, theirs []string
		j := i + 1
		for ; j < len(lines) && !strings.HasPrefix(lines[j], "======="); j++ {
			ours = append(ours, lines[j])
		}
		k := j + 1
		for ; k < len(lines) && !strings.HasPrefix(lines[k], ">>>>>>> "); k++ {
			theirs = append(theirs, lines[k])
		}
		if j >= len(lines) || k >= len(lines) {
			// Malformed hunk (marker-like content): emit unchanged.
			out.WriteString(lines[i])
			continue
		}
		hadConflicts = true
		fmt.Fprintf(&out, "<!-- agent-brain conflict %s: both versions retained — keep what is right, then delete these comment lines (spec §4) -->\n", timestamp)
		fmt.Fprintf(&out, "<!-- agent-brain version: %s -->\n", labelA)
		out.WriteString(strings.Join(ours, ""))
		ensureNewline(&out)
		fmt.Fprintf(&out, "<!-- agent-brain version: %s -->\n", labelB)
		out.WriteString(strings.Join(theirs, ""))
		ensureNewline(&out)
		out.WriteString("<!-- agent-brain conflict end -->\n")
		i = k
	}
	return out.Bytes(), hadConflicts
}

func ensureNewline(buf *bytes.Buffer) {
	if buf.Len() > 0 && buf.Bytes()[buf.Len()-1] != '\n' {
		buf.WriteByte('\n')
	}
}
