package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/doctor"
)

// agentEnvVars is the coding-agent fingerprint (spec §1 exact list, ADR 20
// D1): stripe-cli/vercel-style detection so a coding agent driving this
// terminal is routed away from an interactive wizard it cannot answer.
var agentEnvVars = []string{
	"CLAUDECODE", "CURSOR_AGENT", "CODEX_SANDBOX", "CODEX_THREAD_ID",
	"CODEX_CI", "GEMINI_CLI", "CLINE_ACTIVE", "OPENCODE",
	"OPENCLAW_SHELL", "ANTIGRAVITY_CLI_ALIAS",
}

// hubEntryDecision is what a bare invocation does. Pure — the unit-testable
// core of spec §1's matrix.
type hubEntryDecision int

const (
	hubOpen        hubEntryDecision = iota // initialized + TTY
	hubGuidedInit                          // uninitialized + human TTY
	hubPointerExit                         // everything else — print pointer, exit non-zero
)

// hubPointer is printed (via a returned error, spec §1) whenever a bare
// invocation cannot proceed: uninitialized and non-TTY, or uninitialized
// with a detected coding-agent environment even on a real TTY.
const hubPointer = "agent-brain is not initialized. To get started, run: agent-brain init"

// hubAnnounce is printed before guided init runs, so an uninitialized human
// TTY session sees an explicit heads-up rather than a wizard appearing with
// no explanation (spec §1; ADR 20 decision 1).
const hubAnnounce = "agent-brain is not set up on this machine — starting guided setup (esc cancels)"

// decideHubEntry is spec §1's bare-invocation matrix as a pure function —
// unit-testable with no TTY, no daemon, and no filesystem. It is total,
// including the tty=false rows: runHub's own TTY gate routes a non-TTY
// invocation to its wording before ever consulting this function (the
// initialized flag it already holds picks the refusal vs. pointer text), so
// in practice decideHubEntry is only ever called with tty=true. The tty=false
// rows still resolve to hubPointerExit here because the function's contract
// covers the full matrix, which is what the unit test pins.
func decideHubEntry(initialized, tty, agentEnv bool) hubEntryDecision {
	if !tty {
		return hubPointerExit
	}
	if initialized {
		return hubOpen
	}
	if agentEnv {
		return hubPointerExit
	}
	return hubGuidedInit
}

// agentEnvDetected reports whether any coding-agent fingerprint variable
// (agentEnvVars) is set to a non-empty value. A variable merely present but
// empty does not count as detection — only a genuine non-empty value is
// evidence of a coding agent's own shell.
func agentEnvDetected(getenv func(string) string) bool {
	for _, name := range agentEnvVars {
		if getenv(name) != "" {
			return true
		}
	}
	return false
}

// hubInitialized probes the machine the same way the daemon's readiness
// gate does — SafetyGate over the ambient deps. Cheap CLI-side call; the
// daemon does NOT need to be running (spec §1: daemon-unreachable still
// opens the hub with the degraded banner). buildDoctorDeps' own assembly
// failing (a broken home-directory lookup, an unreadable registry config)
// counts as "not initialized" too — there is nothing an uninitialized-only
// error path could add that init's own diagnostics do not already cover
// better.
func hubInitialized(ctx context.Context) bool {
	deps, err := buildDoctorDeps(true, os.Getenv(testBinaryPathEnv))
	if err != nil {
		return false
	}
	return doctor.SafetyGate(ctx, deps.Paths, deps.Registry, deps.BinaryPath) == nil
}

// runHub is root's RunE (spec §1; ADR 20 decision 1): bare `agent-brain`
// opens the hub on an initialized machine's interactive terminal, reuses
// the dashboard command's own non-TTY refusal when initialized but
// scripted, and on an uninitialized machine either runs the announced
// guided init (human TTY, no detected coding agent) or exits with the
// pointer everywhere else — non-TTY, or a coding-agent fingerprint even on
// a real TTY, since an agent cannot answer the wizard's forms.
func runHub(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	initialized := hubInitialized(ctx)
	tty := isInteractiveTTY(cmd)

	if !tty {
		if initialized {
			// launchHub's own TTY gate produces the identical refusal
			// dashboard's own invocation returns — calling it here (rather
			// than duplicating that error text) is what keeps the two
			// callers from ever drifting apart in wording.
			return launchHub(cmd)
		}
		return errors.New(hubPointer)
	}

	switch decideHubEntry(initialized, tty, agentEnvDetected(os.Getenv)) {
	case hubOpen:
		return launchHub(cmd)
	case hubGuidedInit:
		return runGuidedInit(cmd)
	default:
		// hubPointerExit: the only remaining possibility once tty is
		// true — an uninitialized machine with a detected coding-agent
		// fingerprint (spec §1's agent-gating clause).
		return errors.New(hubPointer)
	}
}

// runGuidedInit is runHub's uninitialized+human-TTY branch: announce, run
// the full interactive init flow, then RE-PROBE hubInitialized. init
// reports a user cancel as success (runInitSteps swallows errInitCancelled,
// init.go), so the re-probe is the ONLY honest completion signal — a
// cancelled form must still exit non-zero here even though
// runInteractiveInitFlow itself returned nil.
func runGuidedInit(cmd *cobra.Command) error {
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), hubAnnounce); err != nil {
		return err
	}
	if err := runInteractiveInitFlow(cmd); err != nil {
		return err
	}
	if hubInitialized(cmd.Context()) {
		return launchHub(cmd)
	}
	return errors.New("setup was not completed — run: agent-brain init")
}
