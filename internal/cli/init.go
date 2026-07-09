package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/ghx"
)

// defaultRepoName is the GitHub repo init provisions absent --repo-name.
const defaultRepoName = "agent-brain-memories"

// newInitCmd is the first-run onboarding wizard (spec §7; ADRs 04, 08):
// gh -> repo -> keyset -> wiring -> config -> service -> enrollment ->
// first sync. Every decision has a flag, so init is fully scriptable
// (Task 12's testscript drives it with --non-interactive); huh forms
// only ever appear when a decision is undetermined AND interactive —
// initsteps.go's step functions themselves never touch stdin or a TTY.
func newInitCmd() *cobra.Command {
	var (
		nonInteractive bool
		generateKey    bool
		importKey      bool
		skipService    bool
		enrollMode     string
		repoName       string
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "First-run onboarding: gh, repo, keyset, wiring, config, service, enrollment",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if enrollMode != "" && enrollMode != "all" && enrollMode != "none" {
				return fmt.Errorf(`--enroll must be "all" or "none", got %q`, enrollMode)
			}

			var importArmored string
			if importKey {
				input, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return err
				}
				importArmored = strings.TrimSpace(string(input))
			}

			gh, err := ghx.NewClient()
			if err != nil {
				return err
			}

			accessible := isAccessible()

			state := &initState{
				out:            cmd.OutOrStdout(),
				nonInteractive: nonInteractive,
				repoName:       repoName,
				skipService:    skipService,
				enrollMode:     enrollMode,
				generateKey:    generateKey,
				importKey:      importKey,
				importArmored:  importArmored,
				gh:             gh,
			}
			wireEnrollmentCallbacks(state, accessible)

			return runInit(cmd.Context(), state, accessible)
		},
	}
	cmd.Flags().BoolVar(&nonInteractive, "non-interactive", false,
		"never prompt; every undecided choice takes its safest default (enrollment: none) or fails loudly (keyset: pass --generate-key/--import-key)")
	cmd.Flags().BoolVar(&generateKey, "generate-key", false, "generate a fresh keyset without prompting")
	cmd.Flags().BoolVar(&importKey, "import-key", false, "import an armored keyset from stdin without prompting")
	cmd.MarkFlagsMutuallyExclusive("generate-key", "import-key")
	cmd.Flags().BoolVar(&skipService, "skip-service", false, "do not install/start the login service")
	cmd.Flags().StringVar(&enrollMode, "enroll", "", `enrollment mode: "all" or "none" (default: interactive picker)`)
	cmd.Flags().StringVar(&repoName, "repo-name", defaultRepoName, "GitHub repo name for the encrypted memories checkout")
	return cmd
}

// runInit runs every step in order, with two huh-only decision points
// interleaved: resolveKeysetDecision (before stepKeyset, only when the
// keyset is missing, undetermined by flags, and we're interactive) and
// confirmKeysetStored (after stepKeyset, only when it just generated a
// fresh key and we're interactive). Both are no-ops — and construct no
// form at all — whenever state.nonInteractive is set, which is what
// makes this function exactly as testable under --non-interactive as
// any single step function: init_test.go calls it directly, never
// through cobra or a real TTY.
func runInit(ctx context.Context, state *initState, accessible bool) error {
	steps := []func(context.Context, *initState) error{
		stepIdentity,
		stepGH,
		func(_ context.Context, state *initState) error { return resolveKeysetDecision(state, accessible) },
		stepKeyset,
		func(_ context.Context, state *initState) error { return confirmKeysetStored(state, accessible) },
		stepRepo,
		stepWiring,
		stepRepoState,
		stepConfigFile,
		stepService,
		stepEnrollment,
		stepFirstSync,
	}
	for _, step := range steps {
		if err := step(ctx, state); err != nil {
			return err
		}
	}
	return nil
}

// resolveKeysetDecision asks — interactively, only when needed — whether
// to generate a fresh keyset or import an existing one, setting
// state.generateKey/importKey/importArmored so stepKeyset itself never
// prompts. It is a no-op whenever the decision is already made: by a
// flag, by an already-present keyset (stepKeyset will just validate and
// skip), or by --non-interactive (stepKeyset's own "pass --generate-key
// or --import-key" error is the right outcome then, not a silent guess
// at which one the user meant).
func resolveKeysetDecision(state *initState, accessible bool) error {
	if state.generateKey || state.importKey {
		return nil
	}
	if _, err := os.Stat(state.paths.Keyset()); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if state.nonInteractive {
		return nil
	}

	var choice string
	if err := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("No keyset yet. Is this the first machine (generate a new keyset) or are you joining one that already has agent-brain set up (import its keyset)?").
			Options(
				huh.NewOption("Generate a new keyset (first machine)", "generate"),
				huh.NewOption("Import an existing keyset (joining a machine)", "import"),
			).
			Value(&choice),
	)).WithAccessible(accessible).Run(); err != nil {
		return err
	}
	if choice == "generate" {
		state.generateKey = true
		return nil
	}

	var armored string
	if err := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Paste the armored keyset (from `agent-brain key export` on the other machine)").
			Value(&armored),
	)).WithAccessible(accessible).Run(); err != nil {
		return err
	}
	state.importKey = true
	state.importArmored = strings.TrimSpace(armored)
	return nil
}

// confirmKeysetStored is the interactive half of spec §5's recovery
// gate: stepKeyset already printed the armored export and the
// password-manager instruction unconditionally when it generated a
// fresh key; here, unless --non-interactive, the user must affirm it
// before init continues. A "no" never deletes or regenerates the
// keyset — it just stops this run so the user can go save it (or
// retrieve it again via `agent-brain key export`) and re-run
// `agent-brain init`, where stepKeyset's validate-and-skip branch picks
// up right where this run left off.
func confirmKeysetStored(state *initState, accessible bool) error {
	if !state.keysetGenerated || state.nonInteractive {
		return nil
	}
	var confirmed bool
	if err := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title("I have stored the keyset in my password manager").
			Value(&confirmed),
	)).WithAccessible(accessible).Run(); err != nil {
		return err
	}
	if !confirmed {
		return errors.New("keyset generated but not confirmed stored — save it (`agent-brain key export` prints it again), then re-run `agent-brain init`")
	}
	return nil
}
