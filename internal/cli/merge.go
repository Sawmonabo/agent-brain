package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/crypto"
)

func newGitMergeCmd() *cobra.Command {
	var mode string
	cmd := &cobra.Command{
		Use:    "git-merge --mode fact|lww -- <base> <current> <other> <pathname>",
		Short:  "Merge driver: 3-way merge on plaintext, retain-both on overlap (invoked by git)",
		Hidden: true,
		Args:   cobra.ExactArgs(4),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch mode {
			case "lww":
				return nil // keep %A: upstream side under the engine's rebase (spec §4, §11)
			case "fact":
				codec, err := loadCodec()
				if err != nil {
					return err
				}
				hadConflicts, err := crypto.MergeFact(cmd.Context(), codec,
					args[0], args[1], args[2], args[3],
					envOr("AGENT_BRAIN_MERGE_LABEL_A", "version A"),
					envOr("AGENT_BRAIN_MERGE_LABEL_B", "version B"))
				if err != nil {
					return err
				}
				if hadConflicts {
					logConflict(args[3])
				}
				return nil
			default:
				return fmt.Errorf("unknown --mode %q (want fact or lww)", mode)
			}
		},
	}
	cmd.Flags().StringVar(&mode, "mode", "fact", "merge policy: fact (3-way + retain-both) or lww (keep current)")
	return cmd
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// logConflict best-effort appends a JSON line consumed by
// `agent-brain conflicts`; it must never fail the merge, so errors are
// discarded.
func logConflict(pathname string) {
	logPath := os.Getenv("AGENT_BRAIN_CONFLICT_LOG")
	if logPath == "" {
		return
	}
	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304: logPath is the operator-set AGENT_BRAIN_CONFLICT_LOG diagnostics path (engine-controlled config), not untrusted input
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	line, err := json.Marshal(conflictRecord{
		Time: time.Now().UTC().Format(time.RFC3339),
		Path: pathname,
		Mode: "fact",
	})
	if err != nil {
		return
	}
	_, _ = file.Write(append(line, '\n'))
}
