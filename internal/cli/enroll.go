package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/huh/v2"
	"golang.org/x/term"

	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// This file is the shared discovery -> confirm -> Track flow: init's step 9
// (initsteps.go's stepEnrollment) and track's own discovery/path-argument
// resolution (track.go) are its two callers. Neither owns it — a change
// here must keep both working.

// errSkipRemoteless signals "this unit needs a human-chosen folder name
// and none is available right now" — the --enroll all / track --all
// closure for nameRemotelessFolder returns it for every remoteless
// per-project discovery (a project with no git remote has no canonical id
// to derive one from): remoteless units are skipped with a printed
// warning — they need a human name. Callers treat it as "skip this one
// unit", never as a fatal error for the whole run. The interactive closure
// (huh Input, below) never returns it — a human is present to type the
// name.
var errSkipRemoteless = errors.New("remoteless project needs a folder name")

// enrollCandidate is one row the enrollment picker offers. Normally it is
// exactly one discovered-but-unenrolled memory root; a global-scope
// provider (codex) instead collapses ALL of its still-unenrolled roots
// into a single candidate, since picking it enrolls them together as one
// pseudo-project (_global) rather than as independent choices. label is
// precomputed so callers never need to format huh option text themselves.
type enrollCandidate struct {
	provider   provider.Provider
	discovered []provider.Discovered
	label      string
}

// buildEnrollCandidates runs Discover on every registered provider, drops
// roots the local registry already tracks, and groups the rest into
// picker rows: one row per root for per-project providers, one row for
// ALL of a global-scope provider's remaining roots together.
func buildEnrollCandidates(ctx context.Context, registry *provider.Registry, enrolled map[[2]string]bool) ([]enrollCandidate, error) {
	var candidates []enrollCandidate
	for _, p := range registry.All() {
		discovered, err := p.Discover(ctx)
		if err != nil {
			return nil, fmt.Errorf("discover %s: %w", p.Name(), err)
		}
		var unenrolled []provider.Discovered
		for _, d := range discovered {
			if !enrolled[[2]string{p.Name(), d.LocalDir}] {
				unenrolled = append(unenrolled, d)
			}
		}
		if len(unenrolled) == 0 {
			continue
		}

		if p.Scope() == provider.ScopeGlobal {
			labels := make([]string, len(unenrolled))
			for i, d := range unenrolled {
				labels[i] = d.Label
			}
			candidates = append(candidates, enrollCandidate{
				provider:   p,
				discovered: unenrolled,
				label:      fmt.Sprintf("%s  %s", p.Name(), strings.Join(labels, ", ")),
			})
			continue
		}
		for _, d := range unenrolled {
			candidates = append(candidates, enrollCandidate{
				provider:   p,
				discovered: []provider.Discovered{d},
				label:      fmt.Sprintf("%s  %s  → %s", p.Name(), d.Label, d.PathGuess),
			})
		}
	}
	return candidates, nil
}

// enrolledSet indexes units by (provider, local dir) — the dedup key
// buildEnrollCandidates filters discovery against, and the same key
// track's path-argument resolution consults to tell "already tracked"
// apart from "not found".
func enrolledSet(units []repo.Unit) map[[2]string]bool {
	enrolled := make(map[[2]string]bool, len(units))
	for _, u := range units {
		enrolled[[2]string{u.Provider, u.LocalDir}] = true
	}
	return enrolled
}

// enrollTarget is enrollOne's minimal write/interaction surface: the exact
// slice of a caller's larger state (initState's step 9, track's discovery
// flow) that enrollOne actually needs, so this file depends on neither
// caller's type.
type enrollTarget struct {
	out                  io.Writer
	confirmProjectPath   func(guess string) (string, error)
	nameRemotelessFolder func(hint string) (string, error)
}

// enrollOne resolves d's cross-machine identity and submits it to the
// daemon. Global-scope units (codex) skip identity resolution entirely —
// TrackRequest's ProjectID/PreferredFolder are documented as ignored for
// global scope; the daemon maps them to repo.GlobalFolder itself.
// Per-project units with no git remote (Identify returns "" ProjectID)
// need a human-chosen folder name: target.nameRemotelessFolder either
// prompts for one (interactive) or returns errSkipRemoteless (--enroll
// all / track --all), which the caller turns into a skipped-with-a-warning
// unit rather than failing the whole run. A nil error return is exactly
// "this one enrolled" — every non-error path prints the enroll line right
// after a successful Track, so callers needing an enrolled-anything flag
// (stepFirstSync's trigger) can set it straight from err == nil.
func enrollOne(ctx context.Context, target enrollTarget, client *api.Client, p provider.Provider, d provider.Discovered) error {
	var projectID, preferredFolder string

	if p.Scope() == provider.ScopePerProject {
		projectPath, err := target.confirmProjectPath(d.PathGuess)
		if err != nil {
			return err
		}
		identity, err := p.Identify(ctx, d, projectPath)
		if err != nil {
			return err
		}
		projectID = identity.ProjectID
		preferredFolder = identity.PreferredFolder
		if projectID == "" {
			// The hint (the prompt's prefilled default) is Identify's
			// PreferredFolder — Base(projectPath) for a remoteless project.
			// NOT Base(d.LocalDir): for claude that basename is always
			// "memory" (…/projects/<slug>/memory), and the prefill is what
			// an empty answer accepts — both an interactive Enter and an
			// EOF'd accessible run (a headless track once
			// enrolled a project under the folder "memory").
			folderName, err := target.nameRemotelessFolder(identity.PreferredFolder)
			if err != nil {
				return err // includes errSkipRemoteless — caller checks errors.Is
			}
			// The named/ shape and its collision-safety argument live in
			// provider.NamedIdentity — one contract for every enrollment
			// surface (this flow and the dashboard's add flow).
			named := provider.NamedIdentity(folderName)
			projectID, preferredFolder = named.ProjectID, named.PreferredFolder
		}
	}

	response, err := client.Track(ctx, api.TrackRequest{
		Provider:        p.Name(),
		ProjectID:       projectID,
		PreferredFolder: preferredFolder,
		LocalDir:        d.LocalDir,
		RepoSubdir:      d.RepoSubdir,
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(target.out, "enroll: %s -> %s\n", d.LocalDir, response.Folder)
	return err
}

// enrollCallbacks bundles the three human-interaction seams enrollment
// needs, built once per invocation by resolveEnrollCallbacks.
type enrollCallbacks struct {
	pickEnrollUnits      func(candidates []enrollCandidate) ([]int, error)
	confirmProjectPath   func(guess string) (string, error)
	nameRemotelessFolder func(hint string) (string, error)
}

// resolveEnrollCallbacks is the one decision table behind both init's step
// 9 and track's discovery flow: "none" (or a non-interactive run with no
// mode given) never prompts and skips every remoteless unit with a
// warning; "all" (`--enroll all` / `track --all` — the same semantics by
// design) accepts every candidate and still skips remoteless, since
// naming one needs a human; anything else is the interactive huh picker.
func resolveEnrollCallbacks(mode string, nonInteractive, accessible bool) enrollCallbacks {
	switch {
	case mode == "none", nonInteractive && mode == "":
		return enrollCallbacks{
			pickEnrollUnits:      func([]enrollCandidate) ([]int, error) { return nil, nil },
			confirmProjectPath:   func(guess string) (string, error) { return guess, nil },
			nameRemotelessFolder: func(string) (string, error) { return "", errSkipRemoteless },
		}
	case mode == "all":
		return enrollCallbacks{
			pickEnrollUnits: func(candidates []enrollCandidate) ([]int, error) {
				indices := make([]int, len(candidates))
				for i := range candidates {
					indices[i] = i
				}
				return indices, nil
			},
			confirmProjectPath:   func(guess string) (string, error) { return guess, nil },
			nameRemotelessFolder: func(string) (string, error) { return "", errSkipRemoteless },
		}
	default:
		return enrollCallbacks{
			pickEnrollUnits: func(candidates []enrollCandidate) ([]int, error) {
				return pickEnrollUnitsInteractive(candidates, accessible)
			},
			confirmProjectPath: func(guess string) (string, error) {
				return confirmProjectPathInteractive(guess, accessible)
			},
			nameRemotelessFolder: func(hint string) (string, error) {
				return nameRemotelessFolderInteractive(hint, accessible)
			},
		}
	}
}

// wireEnrollmentCallbacks sets stepEnrollment's three human-interaction
// seams on state, from the shared resolveEnrollCallbacks table.
func wireEnrollmentCallbacks(state *initState, accessible bool) {
	callbacks := resolveEnrollCallbacks(state.enrollMode, state.nonInteractive, accessible)
	state.pickEnrollUnits = callbacks.pickEnrollUnits
	state.confirmProjectPath = callbacks.confirmProjectPath
	state.nameRemotelessFolder = callbacks.nameRemotelessFolder
}

// buildEnrollPickerForm renders the huh MultiSelect[int] picker over
// candidate INDICES, not enrollCandidate values directly — huh's type
// parameter must be comparable, and enrollCandidate embeds a
// provider.Provider interface plus a []provider.Discovered slice, so it
// isn't. Options are pre-labeled by buildEnrollCandidates (<provider>
// <Label>  -> <PathGuess>, or the grouped global-scope form).
//
// Filterable(false): huh's MultiSelect defaults to filterable, which binds
// "/" to enter a live filter and esc to leave it — the same esc
// cancellableKeyMap now also binds to abort the whole form. Form-level Quit
// is matched before the field ever sees the keypress, so a filtering user's
// esc would silently cancel the picker (and any selections already made)
// instead of just clearing the filter. Turning filtering off removes the
// ambiguity outright rather than leaving a trap for a list that is
// typically a handful of discovered roots anyway.
//
// Split from pickEnrollUnitsInteractive so a test can render this form
// (Init/View) without ever running it — the render is the only way to pin
// that the cancel hint actually appears in the real production form, not a
// hand-built replica of it.
func buildEnrollPickerForm(candidates []enrollCandidate, accessible bool, chosen *[]int) *huh.Form {
	options := make([]huh.Option[int], len(candidates))
	for i, candidate := range candidates {
		options[i] = huh.NewOption(candidate.label, i)
	}

	title := titleWithCancelHint("Select memory roots to enroll (space to toggle, enter to confirm)", accessible)
	titleLines := strings.Count(title, "\n") + 1
	return huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[int]().
			Title(title).
			Filterable(false).
			Options(options...).
			Value(chosen).
			// Explicit Height, sized as TOTAL FIELD height (options + title), not
			// just the options count. huh v2.0.3's MultiSelect.updateViewportSize
			// (field_multiselect.go) has no unset-height branch the way Select's
			// does (field_select.go: "If no height is set size the viewport to the
			// number of options"): left unset, it seeds height from an
			// options-ONLY measurement and then unconditionally subtracts the
			// rendered title+description height from it, so every title line eats
			// one option row — with this field's two-line cancel-hint title, a
			// 2-option list rendered 0 options. Passing the total field height
			// here restores one visible row per option regardless of title length.
			// Accepted degradation: at very narrow widths where the title itself
			// wraps onto more lines than titleLines counted, the viewport loses
			// that many rows and scrolls instead (huh's ensureCursorVisible keeps
			// the cursor on-screen) — correct, just not perfect, and simpler than
			// tracking the title's actual rendered wrap width.
			Height(len(candidates) + titleLines),
	)).WithAccessible(accessible).WithKeyMap(cancellableKeyMap())
}

// pickEnrollUnitsInteractive runs buildEnrollPickerForm and routes its
// outcome through enrollPickerResult.
func pickEnrollUnitsInteractive(candidates []enrollCandidate, accessible bool) ([]int, error) {
	var chosen []int
	err := buildEnrollPickerForm(candidates, accessible, &chosen).Run()
	return enrollPickerResult(chosen, err)
}

// enrollPickerResult decides pickEnrollUnitsInteractive's return from the
// form's outcome, split out so the cancel branch is testable without
// driving a real huh form. A cancelled picker enrolls nothing — exactly
// the outcome of an explicit empty selection, which stepEnrollment and
// runTrackDiscover already report correctly ("nothing selected") — so
// routing it through the same nil/nil path needs no cancel-specific
// handling at either caller.
func enrollPickerResult(chosen []int, err error) ([]int, error) {
	if formCancelled(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return chosen, nil
}

// buildConfirmProjectPathForm prefills the field with the adapter's
// PathGuess (a slug reversal, which is lossy — hence the confirmation) or,
// for track's path-argument flow, the exact path already given on the
// command line, so the common case is just pressing enter, but lets the
// user correct it before Identify reads the project's git remote. path is
// the caller's prefill — the caller seeds it into *path before passing the
// pointer in.
//
// Split from confirmProjectPathInteractive so a test can render this form
// (Init/View) without ever running it — the render is the only way to pin
// that the cancel hint actually appears in the real production form, not a
// hand-built replica of it.
func buildConfirmProjectPathForm(accessible bool, path *string) *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title(titleWithCancelHint("Confirm this project's path", accessible)).
			Value(path),
	)).WithAccessible(accessible).WithKeyMap(cancellableKeyMap())
}

// confirmProjectPathInteractive runs buildConfirmProjectPathForm and
// returns the confirmed (or corrected) path.
func confirmProjectPathInteractive(guess string, accessible bool) (string, error) {
	path := guess
	if err := buildConfirmProjectPathForm(accessible, &path).Run(); err != nil {
		return "", err
	}
	return path, nil
}

// buildNameRemotelessFolderForm asks for a folder name for a project
// Identify could not derive a canonical id for (no git remote). Validated
// live against repo.ValidateFolderName so a bad name is caught here, not
// as a wire error surfaced much later from the daemon's own identical
// check.
//
// Split from nameRemotelessFolderInteractive so a test can render this
// form (Init/View) without ever running it — the render is the only way
// to pin that the cancel hint actually appears in the real production
// form, not a hand-built replica of it.
func buildNameRemotelessFolderForm(accessible bool, name *string) *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title(titleWithCancelHint("This project has no git remote — choose a folder name for it", accessible)).
			Value(name).
			Validate(func(s string) error {
				return repo.ValidateFolderName(strings.TrimSpace(s))
			}),
	)).WithAccessible(accessible).WithKeyMap(cancellableKeyMap())
}

// nameRemotelessFolderInteractive runs buildNameRemotelessFolderForm and
// returns the trimmed folder name.
func nameRemotelessFolderInteractive(hint string, accessible bool) (string, error) {
	name := hint
	if err := buildNameRemotelessFolderForm(accessible, &name).Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(name), nil
}

// isAccessible reports whether prompts should degrade to huh's
// screen-reader-friendly plain-text mode: an explicit ACCESSIBLE
// environment variable, or stdin not being a terminal at all (piped
// input, CI, a test harness).
//
// CONTRACT for headless runs (huh v2.0.3, verified live 2026-07-09): in
// accessible mode an exhausted stdin (EOF) does not error — each form
// silently keeps its prefilled value, exactly as if the user pressed
// Enter. Every prefill must therefore be a value we are willing to
// accept unattended: confirmProjectPath prefills the explicitly-given
// path or the discovered guess, and nameRemotelessFolder prefills
// Identify's PreferredFolder (Base of the project path). init's two
// keyset forms stay fail-closed on EOF by construction — the select
// resolves to the import branch whose empty keyset is rejected, and the
// unconfirmed store gate aborts with the recovery instruction.
func isAccessible() bool {
	return os.Getenv("ACCESSIBLE") != "" || !term.IsTerminal(int(os.Stdin.Fd()))
}
