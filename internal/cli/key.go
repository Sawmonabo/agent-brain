package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/keys"
)

func newKeyCmd() *cobra.Command {
	keyCmd := &cobra.Command{
		Use:   "key",
		Short: "Manage the shared Tink keyset (export it for backup, import it to restore, rotate it)",
	}
	keyCmd.AddCommand(newKeyExportCmd(), newKeyImportCmd(), newKeyRotateCmd())
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

// newKeyRotateCmd rotates the primary key and has the daemon re-encrypt the
// whole repo under it (spec §5). It REFUSES when the daemon is down: rotating
// the keyset without the immediate re-encrypt would leave the repo
// mixed-primary indefinitely, silently deferring the security value the user
// just asked for. The confirmation is EOF-safe — its prefill is ABORT, so an
// unattended pipe never rotates keys — and can be skipped with --yes.
func newKeyRotateCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Rotate the primary key and re-encrypt the whole repo under it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.DefaultPaths()
			if err != nil {
				return err
			}
			client, err := newAPIClient()
			if err != nil {
				return err
			}
			confirm := func() (bool, error) { return confirmRotateInteractive(isAccessible()) }
			return runKeyRotate(cmd.Context(), client, paths.Keyset(), cmd.OutOrStdout(), cmd.ErrOrStderr(), yes, confirm)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the rotation confirmation prompt")
	return cmd
}

// runKeyRotate is the testable core: refuse if the daemon is unreachable, warn
// about the fleet-wide re-import requirement, confirm (unless yes), rotate the
// keyset, print the new armored export, then trigger the daemon-side re-encrypt
// and report it. The confirm seam lets tests drive the decision without a TTY,
// exactly as untrack's confirmPurge does.
func runKeyRotate(ctx context.Context, client *api.Client, keysetPath string, out, errOut io.Writer, yes bool, confirm func() (bool, error)) error {
	// Refuse up front when the daemon is down — before touching the keyset — so
	// a rotation is never left stranded without its re-encrypt. Only a genuine
	// "no daemon" is this refusal; any other Status error is surfaced as-is.
	if _, err := client.Status(ctx); err != nil {
		if errors.Is(err, api.ErrDaemonNotRunning) {
			return fmt.Errorf("key rotate needs the daemon running to re-encrypt immediately after rotating "+
				"(rotating alone leaves the repo mixed-primary) — start it with `agent-brain service start` "+
				"(or `agent-brain daemon run` in the foreground), then retry: %w", err)
		}
		return explainDown(err)
	}

	report := &reportWriter{w: out}
	// The fleet-ordering requirement prints BEFORE anything is touched (spec §5):
	// the moment this machine rotates and pushes, peers without the new keyset
	// fail closed on smudge — correct fail-closed behavior, but only if the user
	// knows to re-import everywhere NOW.
	report.println("key rotate: every OTHER machine must run `agent-brain key import --force` with the new keyset")
	report.println("            immediately after this, or it will fail closed on its next sync.")
	if report.err != nil {
		return report.err
	}

	if !yes {
		confirmed, err := confirm()
		if err != nil {
			return err
		}
		if !confirmed {
			_, err := fmt.Fprintln(out, "key rotate: not confirmed — aborted (the keyset was NOT changed)")
			return err
		}
	}

	if err := keys.Rotate(keysetPath); err != nil {
		return err
	}

	// Print the new armored keyset (the recovery artifact) to stdout and the
	// password-manager reminder to stderr — the same split `key export` uses, so
	// `key rotate > newkey.txt` still captures a clean keyset.
	armored, err := keys.Export(keysetPath)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, armored); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(errOut,
		"This rotated keyset IS the new recovery artifact — store it in your password manager, "+
			"and `agent-brain key import --force` it on every other machine now."); err != nil {
		return err
	}

	// Trigger the daemon-side re-encrypt. The keyset is already rotated, so a
	// failure here is NOT a clean abort: name the manual completion path.
	resp, err := client.Reencrypt(ctx)
	if err != nil {
		return fmt.Errorf("keyset rotated, but the re-encrypt failed — the repo is now mixed-primary; "+
			"once the daemon is healthy, re-run `agent-brain key rotate` to reseal every blob under the new "+
			"primary (a plain `agent-brain sync` will NOT re-encrypt the unchanged blobs): %w", err)
	}
	report.printf("key rotate: re-encrypted %d files under the new primary\n", resp.Files)
	switch {
	case resp.Pushed:
		report.println("key rotate: pushed to the remote")
	case resp.PushQueued:
		report.println("key rotate: re-encrypt commit queued — it will push on the next successful cycle")
	}
	return report.err
}

// confirmRotateInteractive asks before rotating. The prefill is false (ABORT):
// in accessible mode an exhausted stdin (EOF) keeps the prefill, so an
// unattended pipe declines rather than rotating keys unprompted (the same EOF
// contract documented on isAccessible).
func confirmRotateInteractive(accessible bool) (bool, error) {
	var confirmed bool
	err := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title("Rotate the primary key and re-encrypt the whole repo now?").
			Description("Every other machine must `key import --force` the new keyset immediately, or it fails closed.").
			Value(&confirmed),
	)).WithAccessible(accessible).Run()
	if err != nil {
		return false, err
	}
	return confirmed, nil
}
