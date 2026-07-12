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
	return runInitSteps(ctx, state, steps)
}

// errInitCancelled signals that one of init's two huh-only decision points
// was cancelled, its explanatory message already printed. It never reaches
// a caller of runInit: runInitSteps stops at it but reports success,
// exactly like update.go's own cancel precedent — a form the user backed
// out of is a deliberate no-op, not a command failure.
var errInitCancelled = errors.New("init: cancelled")

// runInitSteps runs each step in order, stopping early — but reporting
// success — at errInitCancelled. Split from runInit so the stop-early
// behavior is testable against a fake step list, without driving a real
// huh form to produce the sentinel.
func runInitSteps(ctx context.Context, state *initState, steps []func(context.Context, *initState) error) error {
	for _, step := range steps {
		if err := step(ctx, state); err != nil {
			if errors.Is(err, errInitCancelled) {
				return nil
			}
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
//
// Both forms here run after only stepIdentity and stepGH — gh is
// authenticated, but nothing persistent (keyset, repo, service) exists yet
// — so a cancel at either one can honestly say exactly that much has
// happened, via the shared errInitCancelled sentinel that stops runInit's
// remaining steps without failing the command.
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
	err := buildKeysetSourceForm(accessible, &choice).Run()
	if formCancelled(err) {
		return reportInitCancelledBeforeKeyset(state)
	}
	if err != nil {
		return err
	}
	if choice == "generate" {
		state.generateKey = true
		return nil
	}

	var armored string
	err = buildKeysetImportForm(accessible, &armored).Run()
	if formCancelled(err) {
		return reportInitCancelledBeforeKeyset(state)
	}
	if err != nil {
		return err
	}
	state.importKey = true
	state.importArmored = strings.TrimSpace(armored)
	return nil
}

// buildKeysetSourceForm is resolveKeysetDecision's first form, split out so
// a test can render it (Init/View) without ever running it — the render is
// the only way to pin that the cancel hint actually appears in the real
// production form, not a hand-built replica of it.
func buildKeysetSourceForm(accessible bool, choice *string) *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title(titleWithCancelHint("No keyset yet. Is this the first machine (generate a new keyset) or are you joining one that already has agent-brain set up (import its keyset)?", accessible)).
			Options(
				huh.NewOption("Generate a new keyset (first machine)", "generate"),
				huh.NewOption("Import an existing keyset (joining a machine)", "import"),
			).
			Value(choice),
	)).WithAccessible(accessible).WithKeyMap(cancellableKeyMap())
}

// buildKeysetImportForm is resolveKeysetDecision's second form (only ever
// run when buildKeysetSourceForm resolved to "import"), split out for the
// same render-without-running reason as buildKeysetSourceForm above.
func buildKeysetImportForm(accessible bool, armored *string) *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title(titleWithCancelHint("Paste the armored keyset (from `agent-brain key export` on the other machine)", accessible)).
			Value(armored),
	)).WithAccessible(accessible).WithKeyMap(cancellableKeyMap())
}

// reportInitCancelledBeforeKeyset prints resolveKeysetDecision's cancel
// message and returns the sentinel that stops runInit cleanly. state.login
// is already set by stepGH, which always runs before either of this
// function's two forms.
func reportInitCancelledBeforeKeyset(state *initState) error {
	if _, err := fmt.Fprintf(state.out,
		"init: cancelled — gh is authenticated as %s but no keyset, repo, or service was set up yet; re-run `agent-brain init` to resume\n",
		state.login); err != nil {
		return err
	}
	return errInitCancelled
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
	err := buildKeysetStoredConfirmForm(accessible, &confirmed).Run()
	return keysetStoredFormResult(state, confirmed, err)
}

// buildKeysetStoredConfirmForm is confirmKeysetStored's form construction,
// split out for the same render-without-running reason as
// buildKeysetSourceForm above.
func buildKeysetStoredConfirmForm(accessible bool, confirmed *bool) *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title(titleWithCancelHint("I have stored the keyset in my password manager", accessible)).
			Value(confirmed),
	)).WithAccessible(accessible).WithKeyMap(cancellableKeyMap())
}

// keysetStoredFormResult turns confirmKeysetStored's form outcome into its
// return, split out so both branches are testable without driving a real
// huh form. formCancelled is checked before confirmed is trusted: Confirm's
// Update writes its bound value through on every toggle, not just at
// submission, so a cancelled form can still leave confirmed set to true.
// The cancel message differs from resolveKeysetDecision's — by this point
// the keyset itself already exists on disk — and a plain decline
// (confirmed == false, no error) is untouched: an existing, legitimately
// hard error unrelated to cancellation.
func keysetStoredFormResult(state *initState, confirmed bool, err error) error {
	if formCancelled(err) {
		if _, printErr := fmt.Fprintln(state.out,
			"init: cancelled — the keyset was generated and is already on disk; "+
				"save it (`agent-brain key export` prints it again), then re-run `agent-brain init` to resume"); printErr != nil {
			return printErr
		}
		return errInitCancelled
	}
	if err != nil {
		return err
	}
	if !confirmed {
		return errors.New("keyset generated but not confirmed stored — save it (`agent-brain key export` prints it again), then re-run `agent-brain init`")
	}
	return nil
}
