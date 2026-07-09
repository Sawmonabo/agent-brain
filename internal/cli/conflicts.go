package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// conflictRecord is the one shape shared by the merge driver's writer
// (logConflict, merge.go) and this file's reader: a round-trip test pins
// that the two never drift apart.
type conflictRecord struct {
	Time string `json:"time"`
	Path string `json:"path"`
	Mode string `json:"mode"`
}

// defaultConflictsLimit bounds `conflicts list` output absent --limit.
const defaultConflictsLimit = 50

// retain-both block delimiters, exactly as crypto.RewriteRetainBoth emits
// them (internal/crypto/retain.go) — do not invent marker strings here.
// blockEndLine is checked before blockStartPrefix because it is also a
// (more specific) match for that prefix.
const (
	blockStartPrefix = "<!-- agent-brain conflict "
	blockEndLine     = "<!-- agent-brain conflict end -->"
)

func newConflictsCmd() *cobra.Command {
	var limit int
	run := func(cmd *cobra.Command, _ []string) error {
		return runConflictsList(cmd, limit)
	}
	addLimitFlag := func(c *cobra.Command) {
		c.Flags().IntVar(&limit, "limit", defaultConflictsLimit,
			"maximum number of records to show, newest first (0 or less shows all)")
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List logged retain-both conflict events, newest first",
		Args:  cobra.NoArgs,
		RunE:  run,
	}
	addLimitFlag(listCmd)

	conflictsCmd := &cobra.Command{
		Use:   "conflicts",
		Short: "Inspect retain-both conflict blocks the merge driver has left in the checkout",
		Args:  cobra.NoArgs, // bare `conflicts` behaves like `conflicts list`
		RunE:  run,
	}
	addLimitFlag(conflictsCmd)
	conflictsCmd.AddCommand(listCmd, newConflictsShowCmd())
	return conflictsCmd
}

func newConflictsShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <path>",
		Short: "Print retain-both blocks still present in the checkout copy of <path>",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConflictsShow(cmd, args[0])
		},
	}
}

// runConflictsList reads the conflict log directly (a pure file read —
// readers don't violate the single-writer invariant, spec §5/§11) and
// prints up to limit records, newest first. logConflict appends in
// chronological order, so reversing the read order is sufficient — no
// timestamp parsing/sorting is needed. It only ever reads the live log
// path, so a Task 6 `.1` rotation sibling sitting alongside it is
// automatically tolerated (never touched, never mistaken for the live file).
func runConflictsList(cmd *cobra.Command, limit int) error {
	paths, err := config.DefaultPaths()
	if err != nil {
		return err
	}
	logPath := paths.ConflictLogFile()
	data, err := os.ReadFile(logPath) //nolint:gosec // G304: logPath is the program-derived conflict-log location (config.Paths), not untrusted input
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			_, printErr := fmt.Fprintf(cmd.OutOrStdout(), "no conflicts logged at %s\n", logPath)
			return printErr
		}
		return err
	}

	var records []conflictRecord
	for _, line := range bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var record conflictRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return fmt.Errorf("parse conflict log %s: %w", logPath, err)
		}
		records = append(records, record)
	}
	if len(records) == 0 {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "no conflicts logged at %s\n", logPath)
		return err
	}

	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}
	if limit > 0 && len(records) > limit {
		records = records[:limit]
	}

	report := &reportWriter{w: cmd.OutOrStdout()}
	for _, record := range records {
		report.printf("%-25s %-40s %s\n", record.Time, record.Path, record.Mode)
	}
	return report.err
}

// runConflictsShow prints the retain-both blocks (spec §4) still present in
// the checkout's working-tree copy of relPath. That copy is the file a user
// or provider agent would actually open — plain, human-readable content
// with the retain-both HTML-comment blocks embedded, since git's smudge
// filter decrypts it on checkout — so no codec/decryption is needed here.
func runConflictsShow(cmd *cobra.Command, relPath string) error {
	paths, err := config.DefaultPaths()
	if err != nil {
		return err
	}
	fullPath, err := resolveCheckoutPath(paths, relPath)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(fullPath) //nolint:gosec // G304: fullPath is validated by resolveCheckoutPath (repo.ValidateRelPath + containment check) to resolve strictly inside the memories checkout before this read
	if err != nil {
		return err
	}

	blocks := extractRetainBlocks(data)
	if len(blocks) == 0 {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "no retained blocks in %s — already tidied\n", relPath)
		return err
	}

	report := &reportWriter{w: cmd.OutOrStdout()}
	for i, block := range blocks {
		if i > 0 {
			report.println()
		}
		report.printf("%s", block)
	}
	return report.err
}

// resolveCheckoutPath resolves relPath to a path strictly inside the
// memories checkout, refusing anything else with a clear one-line reason.
// This is not merely guarding against a careless local user (the "same
// trust boundary as cat" a first pass assumed): relPath is the value a user
// copies verbatim out of `conflicts list`'s Path column, which is populated
// from conflict-log entries the merge driver records while resolving
// SYNCED, remote-influenced content. A hostile remote controlling a
// pathname the driver logs could otherwise launder an out-of-tree read
// through this command. repo.ValidateRelPath rejects absolute paths,
// backslashes, and any '.'/'..' segment; the containment check after Join
// is defense in depth against any gap in that validation.
func resolveCheckoutPath(paths config.Paths, relPath string) (string, error) {
	if err := repo.ValidateRelPath(relPath); err != nil {
		return "", fmt.Errorf("refusing conflicts show: %w", err)
	}
	root := filepath.Clean(paths.MemoriesDir())
	full := filepath.Clean(filepath.Join(root, relPath))
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing conflicts show %q: resolves outside the memories checkout", relPath)
	}
	return full, nil
}

// extractRetainBlocks scans content for crypto.RewriteRetainBoth's blocks
// and returns each one verbatim (start marker through end marker,
// inclusive). A stray end marker with no open block is ignored: showing
// conflicts is a read-only diagnostic, not a repair tool.
func extractRetainBlocks(content []byte) []string {
	lines := strings.SplitAfter(string(content), "\n")
	var blocks []string
	var current strings.Builder
	inBlock := false
	for _, line := range lines {
		trimmed := strings.TrimRight(line, "\n")
		switch {
		case trimmed == blockEndLine:
			if inBlock {
				current.WriteString(line)
				blocks = append(blocks, current.String())
				current.Reset()
				inBlock = false
			}
		case inBlock:
			current.WriteString(line)
		case strings.HasPrefix(trimmed, blockStartPrefix):
			inBlock = true
			current.Reset()
			current.WriteString(line)
		}
	}
	return blocks
}
