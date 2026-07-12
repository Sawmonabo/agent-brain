package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/claude"
)

func newMigrateCmd() *cobra.Command {
	var skipPreflight, yes bool
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "One-time import of the bash-era ~/.agent-brain memory tree (spec §10)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			deps, err := buildTrackDeps()
			if err != nil {
				return err
			}
			client, err := newAPIClient()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			if skipPreflight {
				if err := printSkipPreflightWarning(out); err != nil {
					return err
				}
			} else {
				settings, err := config.LoadSettings(deps.paths.SettingsFile())
				if err != nil {
					return err
				}
				chezmoiConfigPath := filepath.Join(deps.paths.ConfigDir, "chezmoi.toml")
				preflightTimeout := time.Duration(settings.Migrate.PreflightTimeout)
				if err := runMigratePreflight(cmd.Context(), chezmoiConfigPath, preflightTimeout); err != nil {
					return err
				}
			}

			accessible := isAccessible()
			callbacks := migrateCallbacks{
				confirmProjectPath: func(guess string) (string, error) {
					return confirmProjectPathInteractive(guess, accessible)
				},
				nameRemotelessFolder: func(hint string) (string, error) {
					return nameRemotelessFolderInteractive(hint, accessible)
				},
			}
			if yes {
				callbacks.confirmProjectPath = func(guess string) (string, error) { return guess, nil }
			}

			return runMigrate(cmd.Context(), deps, client, callbacks, out)
		},
	}
	cmd.Flags().BoolVar(&skipPreflight, "skip-preflight", false,
		"skip the chezmoi pre-flight gate (spec §10) — only if every orphan has already been adjudicated by hand; the history scrub is the point of no return")
	cmd.Flags().BoolVar(&yes, "yes", false,
		"auto-accept every path guess without an edit prompt (remoteless projects still prompt for a folder name — there is no guess to accept)")
	return cmd
}

// migrateCallbacks bundles migrate's two human-interaction seams.
// Unlike enrollCallbacks, there is no "skip remoteless" mode: every
// legacy project must be accounted for, so nameRemotelessFolder always
// prompts (spec §10 step 3), never returns errSkipRemoteless.
type migrateCallbacks struct {
	confirmProjectPath   func(guess string) (string, error)
	nameRemotelessFolder func(hint string) (string, error)
}

// runMigratePreflight enforces spec §10's pre-flight: the bash-era system
// cannot propagate deletions, so a stray `chezmoi apply` could resurrect a
// source-only orphan straight into the seed this command is about to
// read. Absent config (this machine never ran the bash system, or already
// retired it) passes silently; present config demands an EMPTY diff —
// anything else, or a missing chezmoi binary, refuses. timeout comes from
// config.MigrateSettings.PreflightTimeout (default 30s) — a cold NFS home
// or a huge legacy tree can exceed a fixed timeout with no operator
// recourse, so the caller resolves it from settings rather than a const.
// preflightKillWaitDelay bounds how long a timed-out preflight waits for
// stray chezmoi descendants (double-forked helpers that escaped the
// process-group kill) to release the output pipe before Wait force-closes
// it. Healthy calls never reach it — it only starts after the cancel.
const preflightKillWaitDelay = 2 * time.Second

func runMigratePreflight(ctx context.Context, chezmoiConfigPath string, timeout time.Duration) error {
	if _, err := os.Stat(chezmoiConfigPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	//nolint:gosec // G204: "chezmoi" is a constant; chezmoiConfigPath is program-resolved (config.DefaultPaths), not untrusted input
	cmd := exec.CommandContext(timeoutCtx, "chezmoi", "--config", chezmoiConfigPath, "diff")
	// chezmoi shells out to git and diff helpers. On timeout the default
	// cancel kills only chezmoi itself; a surviving helper inherits the
	// stdout pipe and Wait blocks until IT exits, so the configured
	// deadline would not actually bound this call. Give the child its own
	// process group and kill the whole group on cancel; WaitDelay caps the
	// pipe hold of any descendant that escaped the group (gitx applies the
	// same bound to git).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = preflightKillWaitDelay
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("migrate: pre-flight chezmoi diff failed (spec §10): %w — adjudicate every orphan first: restore keepers to their destination, `chezmoi forget` confirmed deletions, commit+push the legacy source, then re-run (or pass --skip-preflight only once you have already done this)", err)
	}
	if len(bytes.TrimSpace(output)) > 0 {
		return fmt.Errorf("migrate: pre-flight chezmoi diff is NOT empty (spec §10) — adjudicate every orphan before continuing: restore keepers to their destination, `chezmoi forget` confirmed deletions, commit+push the legacy source, then re-run:\n%s", output)
	}
	return nil
}

// printSkipPreflightWarning renders --skip-preflight's warning in red when
// the output is a real, color-capable terminal (NO_COLOR and non-TTY both
// degrade to plain text — spec §7 UX rules) — no lipgloss dependency
// needed for one ANSI-wrapped line.
func printSkipPreflightWarning(out io.Writer) error {
	const message = "WARNING: --skip-preflight bypasses the spec §10 chezmoi-diff gate. The history scrub (ADR 13) is the point of no return for anything left unadjudicated in the legacy source — only pass this if every orphan has ALREADY been reconciled by hand."
	if !colorAllowed(out) {
		_, err := fmt.Fprintln(out, message)
		return err
	}
	const ansiRed = "\x1b[31m"
	const ansiReset = "\x1b[0m"
	_, err := fmt.Fprintf(out, "%s%s%s\n", ansiRed, message, ansiReset)
	return err
}

func colorAllowed(out io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	file, ok := out.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(file.Fd()))
}

// legacyRoot is the bash-era runtime dir — always literally ~/.agent-brain
// regardless of AGENT_BRAIN_DATA_DIR/CONFIG_DIR (spec §10): migrate reads
// machine-local state from the OLD system's fixed location, never from a
// v2-managed path. home is threaded in (not os.UserHomeDir() called
// here directly) so tests can override it via trackDeps without touching
// $HOME.
func legacyRoot(home string) string {
	return filepath.Join(home, ".agent-brain")
}

// enumerateLegacySlugs lists every slug dir directly under root, skipping
// non-directories, in deterministic order.
func enumerateLegacySlugs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var slugs []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		slugs = append(slugs, entry.Name())
	}
	sort.Strings(slugs)
	return slugs, nil
}

// hasRealContent reports whether slugDir contains at least one file besides
// the bash-era droppings (.lock, *.sync-pending) SeedProject's own copy walk
// already filters (engine/admin.go) — belt and suspenders, so a slug that is
// nothing but droppings never even reaches the confirm/name prompts.
func hasRealContent(slugDir string) (bool, error) {
	var found bool
	err := filepath.WalkDir(slugDir, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if name == ".lock" || strings.HasSuffix(name, ".sync-pending") {
			return nil
		}
		found = true
		return fs.SkipAll
	})
	if err != nil {
		return false, err
	}
	return found, nil
}

// runMigrate is spec §10's per-machine importer: enumerate every legacy
// slug, confirm/name each project, seed it from the legacy tree while
// enrolling the LIVE claude memory dir (so the daemon's enrollment
// mirror-in overlays the seed as a second commit), then print the
// retirement pointer once everything has landed.
func runMigrate(ctx context.Context, deps trackDeps, client *api.Client, callbacks migrateCallbacks, out io.Writer) error {
	root := legacyRoot(deps.home)
	slugs, err := enumerateLegacySlugs(root)
	if err != nil {
		return err
	}
	if len(slugs) == 0 {
		_, err := fmt.Fprintln(out, "migrate: nothing to migrate")
		return err
	}

	report := &reportWriter{w: out}
	var migratedAny bool
	var cancelledAtSlug string
	for _, slug := range slugs {
		hasContent, err := hasRealContent(filepath.Join(root, slug))
		if err != nil {
			return err
		}
		if !hasContent {
			report.printf("migrate: %s has nothing but bash-era droppings — skipped\n", slug)
			continue
		}
		if err := migrateOne(ctx, deps, client, callbacks, report, slug); err != nil {
			// A cancelled confirm/name prompt stops the run here rather than
			// skipping just this slug and continuing: unlike track's discovery
			// picker (independent projects a user explicitly chose), migrate
			// works a fixed backlog of legacy slugs one at a time, and
			// response.Skipped already makes a later `agent-brain migrate`
			// resume idempotently — including retrying the cancelled slug.
			if formCancelled(err) {
				cancelledAtSlug = slug
				break
			}
			return explainDown(err)
		}
		migratedAny = true
	}
	if report.err != nil {
		return report.err
	}

	if cancelledAtSlug != "" {
		if _, err := fmt.Fprintf(out, "migrate: cancelled at %s — re-run `agent-brain migrate` to resume "+
			"(already-migrated projects are skipped automatically)\n", cancelledAtSlug); err != nil {
			return err
		}
		if !migratedAny {
			return nil
		}
		return syncAfterTrack(ctx, client, out)
	}

	if !migratedAny {
		_, err := fmt.Fprintln(out, "migrate: nothing to migrate (only bash-era droppings found)")
		return err
	}

	if err := syncAfterTrack(ctx, client, out); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "migrate: done — see spec §10's retirement checklist (remove the SessionStart healthcheck hook, ~/.local/bin/ab-claude, autoMemoryDirectory from settings.local.json, ~/.config/agent-brain/chezmoi.toml, and ~/.agent-brain/) once verified on every machine")
	return err
}

// migrateOne maps one legacy slug to a project (GuessPath → confirm/edit
// → Identify → remoteless name), then submits the MigrateRequest: SeedDir
// is the legacy tree, LocalDir is the LIVE claude memory dir the daemon
// enrolls — existing or not yet created — so enrollment's own mirror-in
// lands as the overlay layer over the seed (spec §10 step 4).
func migrateOne(ctx context.Context, deps trackDeps, client *api.Client, callbacks migrateCallbacks, report *reportWriter, slug string) error {
	statDir := func(p string) bool {
		info, err := os.Stat(p)
		return err == nil && info.IsDir()
	}
	guess := claude.GuessPath(slug, statDir)

	projectPath, err := callbacks.confirmProjectPath(guess)
	if err != nil {
		return err
	}

	claudeProvider, ok := deps.registry.Get("claude")
	if !ok {
		return errors.New("migrate: claude provider not registered")
	}
	identity, err := claudeProvider.Identify(ctx, provider.Discovered{}, projectPath)
	if err != nil {
		return err
	}
	projectID := identity.ProjectID
	preferredFolder := identity.PreferredFolder
	if projectID == "" {
		folderName, err := callbacks.nameRemotelessFolder(filepath.Base(projectPath))
		if err != nil {
			return err
		}
		projectID = "named/" + folderName
		preferredFolder = folderName
	}

	response, err := client.Migrate(ctx, api.MigrateRequest{
		Provider:        "claude",
		ProjectID:       projectID,
		PreferredFolder: preferredFolder,
		LocalDir:        claude.MemoryDirFor(deps.home, projectPath),
		Slug:            slug,
		SeedDir:         filepath.Join(legacyRoot(deps.home), slug),
	})
	if err != nil {
		return err
	}

	if response.Skipped {
		report.printf("migrate: %s -> %s (already imported — skipped)\n", slug, response.Folder)
	} else {
		report.printf("migrate: %s -> %s (seeded %d file(s))\n", slug, response.Folder, response.Files)
	}
	return report.err
}
