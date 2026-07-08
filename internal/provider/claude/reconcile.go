package claude

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/renameio/v2"
)

// indexEntry is one rendered MEMORY.md line: a topic file's extracted
// title and hook.
type indexEntry struct {
	file  string
	title string
	hook  string
}

// ReconcileIndex deterministically rebuilds MEMORY.md from every other
// *.md file in dir (spec §4 step 4), reading each topic file's
// frontmatter (name/description), falling back to its first heading,
// then its filename (package doc: what this regeneration costs).
//
// It writes exactly one file, MEMORY.md, never a git-meta name; safe
// under the sync.go scrub contract because Sync orders scrubIntegrated
// before reconcile.
func (a *Adapter) ReconcileIndex(_ context.Context, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing mirrored here yet — nothing to reconcile
		}
		return fmt.Errorf("claude reconcile: read %s: %w", dir, err)
	}

	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "MEMORY.md" || !strings.HasSuffix(name, ".md") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	indexPath := filepath.Join(dir, "MEMORY.md")
	if len(names) == 0 {
		if _, err := os.Stat(indexPath); os.IsNotExist(err) {
			return nil // no topic files, no existing index: never create one
		}
	}

	indexEntries := make([]indexEntry, 0, len(names))
	for _, name := range names {
		content, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // G304: name came from walking dir, an engine-constructed provider-unit path
		if err != nil {
			return fmt.Errorf("claude reconcile: read %s: %w", name, err)
		}
		title, hook := extractTitleAndHook(string(content), name)
		indexEntries = append(indexEntries, indexEntry{file: name, title: title, hook: hook})
	}

	var b strings.Builder
	b.WriteString("# Memory index\n\n")
	for _, entry := range indexEntries {
		if entry.hook != "" {
			fmt.Fprintf(&b, "- [%s](%s) — %s\n", entry.title, entry.file, entry.hook)
		} else {
			fmt.Fprintf(&b, "- [%s](%s)\n", entry.title, entry.file)
		}
	}
	rendered := b.String()

	// Skip the write when the existing bytes already match: no mtime
	// churn, so mirror-out never sees a spurious change to sync back out.
	if current, err := os.ReadFile(indexPath); err == nil && string(current) == rendered { //nolint:gosec // G304: indexPath is dir joined with the literal "MEMORY.md"
		return nil
	}
	if err := renameio.WriteFile(indexPath, []byte(rendered), 0o644); err != nil {
		return fmt.Errorf("claude reconcile: write %s: %w", indexPath, err)
	}
	return nil
}

// extractTitleAndHook derives a topic file's index title and hook (the
// text shown after the em dash) using three fallback tiers: frontmatter
// (name/description), the file's first Markdown heading, then the
// filename with its extension stripped. hook is the frontmatter
// description, or "" when there is none.
func extractTitleAndHook(content, filename string) (title, hook string) {
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		var name string
		for _, line := range lines[1:] {
			if strings.TrimSpace(line) == "---" {
				break
			}
			key, value, ok := splitFrontmatterLine(line)
			if !ok {
				continue
			}
			switch key {
			case "name":
				name = value
			case "description":
				hook = value
			}
		}
		if name != "" {
			return name, hook
		}
	}
	for _, line := range lines {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), "# "); ok {
			return strings.TrimSpace(after), hook
		}
	}
	return strings.TrimSuffix(filename, ".md"), hook
}

// splitFrontmatterLine parses one "key: value" frontmatter line,
// trimming whitespace and a single matching pair of surrounding quotes
// from the value.
func splitFrontmatterLine(line string) (key, value string, ok bool) {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:idx])
	value = trimMatchingQuotes(strings.TrimSpace(line[idx+1:]))
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
