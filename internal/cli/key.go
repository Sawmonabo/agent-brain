package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/keys"
)

func newKeyCmd() *cobra.Command {
	keyCmd := &cobra.Command{
		Use:   "key",
		Short: "Manage the shared Tink keyset (export it for backup, import it to restore)",
	}
	keyCmd.AddCommand(newKeyExportCmd(), newKeyImportCmd())
	return keyCmd
}

// newKeyExportCmd prints the armored keyset to stdout, and ONLY the armored
// keyset — a caller must be able to pipe it straight into a password manager
// or a file. The recovery reminder goes to stderr so it never lands in that
// pipe.
func newKeyExportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "export",
		Short: "Print the armored keyset to stdout for backup",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.DefaultPaths()
			if err != nil {
				return err
			}
			armored, err := keys.Export(paths.Keyset())
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), armored); err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.ErrOrStderr(),
				"This armored keyset IS the recovery artifact — store a copy in your password manager now.")
			return err
		},
	}
}

// newKeyImportCmd reads an armored keyset from stdin and installs it.
// Losing a keyset loses every memory encrypted under it, so this never
// clobbers an existing one silently: without --force it refuses outright;
// with --force it renames the old keyset to a .bak-<unixts> sibling first,
// so key material is moved, never destroyed.
func newKeyImportCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Read an armored keyset from stdin and install it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.DefaultPaths()
			if err != nil {
				return err
			}
			keysetPath := paths.Keyset()

			input, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return err
			}
			armored := strings.TrimSpace(string(input))

			if force {
				return forceImport(cmd, keysetPath, armored)
			}
			if err := keys.Import(keysetPath, armored); err != nil {
				if errors.Is(err, keys.ErrKeysetExists) {
					return fmt.Errorf("keyset already exists at %s — pass --force to replace it "+
						"(the old keyset can no longer decrypt new commits once replaced)", keysetPath)
				}
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "keyset imported to %s\n", keysetPath)
			return err
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"replace an existing keyset (the old one is renamed to keyset.json.bak-<unixts>, never deleted)")
	return cmd
}

// forceImport installs armored at keysetPath, replacing whatever is already
// there. It validates the armored input FIRST, at a scratch path that is
// never keysetPath itself, so a bad or corrupt import can fail without ever
// touching the working keyset — only once keys.Import has proven the new
// material valid does it back up the old keyset (never delete: a
// .bak-<unixts> sibling) and swap the validated copy into place.
func forceImport(cmd *cobra.Command, keysetPath, armored string) error {
	scratchPath := fmt.Sprintf("%s.importing-%d", keysetPath, os.Getpid())
	_ = os.Remove(scratchPath) // best-effort: clear a stale scratch file from a previous crashed attempt
	if err := keys.Import(scratchPath, armored); err != nil {
		return err // existing keyset, if any, is untouched — validation failed before anything destructive
	}
	defer func() { _ = os.Remove(scratchPath) }() // best-effort: only a leftover if the final rename below fails

	if _, statErr := os.Stat(keysetPath); statErr == nil {
		backupPath := fmt.Sprintf("%s.bak-%d", keysetPath, time.Now().Unix())
		if err := os.Rename(keysetPath, backupPath); err != nil {
			return fmt.Errorf("back up existing keyset before --force replace: %w", err)
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return statErr
	}
	if err := os.Rename(scratchPath, keysetPath); err != nil {
		return fmt.Errorf("install the validated keyset after --force replace: %w", err)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "keyset imported to %s\n", keysetPath)
	return err
}
