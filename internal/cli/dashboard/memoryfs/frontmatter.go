package memoryfs

import (
	"io"
	"os"
	"strings"
)

// frontmatterReadCap bounds how much of a file Meta reads. name/description
// are advisory display metadata (spec §3) — never a reason to stream an
// entire multi-megabyte memory file just to render a browser row.
const frontmatterReadCap = 4096

// Meta reads at most a memory file's first frontmatterReadCap bytes and
// extracts its display metadata. hasFrontmatter is true only when line 1 is
// exactly "---" AND a matching closing "---" line is found within the
// capped window; every other case — no opening fence, or no closing fence
// before the cap (a truncated read is indistinguishable from a genuinely
// unclosed block, and both must degrade identically) — yields
// hasFrontmatter=false and empty strings. Meta never returns an error: a
// missing, unreadable, or malformed file is ordinary input for a memory
// browser (every List caller falls back to the filename stem), not a
// failure.
//
// Line matching uses strings.TrimSpace, which strips a trailing '\r' along
// with ordinary whitespace, so CRLF-terminated lines compare equal to their
// LF counterparts without separate handling.
func Meta(path string) (name, description string, hasFrontmatter bool) {
	f, err := os.Open(path) //nolint:gosec // G304: path is caller-supplied (List's own walk, or a Memory's own composed path), not untrusted input
	if err != nil {
		return "", "", false
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, frontmatterReadCap))
	if err != nil {
		return "", "", false
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", false
	}

	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			return name, description, true
		}
		key, value, ok := splitFrontmatterLine(line)
		if !ok {
			continue
		}
		switch key {
		case "name":
			name = value
		case "description":
			description = value
		}
	}
	// Ran out of lines — either genuine EOF or the read cap — without a
	// closing fence: an incomplete block renders as no-frontmatter, never a
	// partially-trusted one.
	return "", "", false
}

// splitFrontmatterLine parses one "key: value" frontmatter line, trimming
// whitespace and a single matching pair of surrounding quotes from the
// value. This is the same tolerance internal/provider/claude/reconcile.go's
// own (unexported, adapter-internal) splitFrontmatterLine applies,
// duplicated here rather than imported: two independent packages read this
// same display-metadata shape without one depending on the other's
// adapter-internal helper.
func splitFrontmatterLine(line string) (key, value string, ok bool) {
	before, after, found := strings.Cut(line, ":")
	if !found {
		return "", "", false
	}
	key = strings.TrimSpace(before)
	value = trimMatchingQuotes(strings.TrimSpace(after))
	return key, value, true
}

// trimMatchingQuotes strips one leading and trailing '"' when both are
// present, tolerating quoted frontmatter values.
func trimMatchingQuotes(s string) string {
	if len(s) >= 2 && strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
		return s[1 : len(s)-1]
	}
	return s
}
