package dashboard

import (
	"context"
	"errors"
	"os/exec"

	tea "charm.land/bubbletea/v2"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/views"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
	"github.com/Sawmonabo/agent-brain/internal/ghx"
)

// gh-auth staleness detection, surfacing, and one-key re-auth handoff.
//
// The memories checkout's remote is SSH, so sync never touches the gh OAuth
// token — an invalid token silently breaks only the gh-dependent features (the
// update banner, init, doctor's gh row). GitHub's device/browser flow makes a
// silent re-mint impossible, so this file gives the product the two things it
// can build around that constraint: loud sticky detection (authInvalid, fed by
// the ONE ghx classifier at every gh call site) and a one-keypress interactive
// remedy (the Doctor tab's f, reusing the $EDITOR flow's suspend/resume seam).

// ghDoctorCheckName is the Name doctor.checkGH gives its row (internal/doctor/
// checks.go). Keyed on here to read the gh row's live status out of a fetched
// report — a passing row clears the attention, an auth-invalid one arms it.
const ghDoctorCheckName = "gh"

// authAttentionText is the loud status-header segment shown while gh auth is
// invalid (spec §2's status bar). It names the exact remedy and where to reach
// it, so the sticky segment is self-explaining rather than a bare alarm.
const authAttentionText = "gh auth invalid — Doctor tab: f re-authenticates"

type (
	// ghAuthFinishedMsg reports the interactive `gh auth login` child's exit —
	// the tea.ExecProcess callback message, the gh-handoff twin of
	// editorFinishedMsg. err is the exec error (a launch failure), NOT gh's own
	// exit code, which is not authoritative about whether auth recovered.
	ghAuthFinishedMsg struct{ err error }
	// ghAuthProbedMsg carries the post-handoff `gh auth status` re-probe verdict:
	// nil clears the attention with an ok toast, non-nil keeps it and names the
	// manual command. The probe, not the login child's exit, is the sole truth
	// about whether the token is live again.
	ghAuthProbedMsg struct{ err error }
)

// ghReauthCmd suspends the TUI and hands the terminal to the interactive
// `gh auth login -h github.com` command — the SAME tea.ExecProcess suspend/
// resume seam launchEditorCmd uses for an in-terminal $EDITOR, because GitHub's
// device/browser flow needs the real terminal and a human at it. On the child's
// exit the program resumes and delivers ghAuthFinishedMsg, whose handler
// re-asserts DECSET 1007 (ADR 21, exactly like the editor return) and re-probes.
func ghReauthCmd(command *exec.Cmd) tea.Cmd {
	return tea.ExecProcess(command, func(err error) tea.Msg { return ghAuthFinishedMsg{err: err} })
}

// probeGHAuthCmd re-runs the injected gh-auth probe off the UI thread after the
// handoff returns. A nil probe (a build that wired no remedy) reports an honest
// unavailability rather than falsely clearing the attention. The 10s
// RequestTimeout bounds the `gh auth status` round trip.
func (m Model) probeGHAuthCmd() tea.Cmd {
	probe := m.probeGHAuth
	return func() tea.Msg {
		if probe == nil {
			return ghAuthProbedMsg{err: errors.New("gh auth probe is unavailable in this build")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), views.RequestTimeout)
		defer cancel()
		return ghAuthProbedMsg{err: probe(ctx)}
	}
}

// reassertAlternateScrollCmd is the 1007 re-set an in-terminal handoff (the
// $EDITOR flow, the gh re-auth) emits on return: the child may have reset
// private modes, so the mode is armed again (ADR 21). Set only, deliberately no
// paired XTSAVE — by now 1007 is our own armed state, not the user's pre-hub
// preference, so there is nothing of theirs left to capture. Returns nil when
// alternate-scroll is disabled, so a caller can batch it unconditionally.
// Factored out of the editorFinishedMsg handler so both return paths re-assert
// through one seam; the editor path stays behaviorally identical (same RawMsg or
// nil), which TestEditorFinishReassertsAlternateScroll pins.
func (m Model) reassertAlternateScrollCmd() tea.Cmd {
	if m.settings.Dashboard.AlternateScroll {
		return tea.Raw(setAlternateScroll)
	}
	return nil
}

// authAttentionSegment renders the loud gh-auth attention for the status header
// (spec §2). Fail-styled (the red the Doctor tab's ✗ uses) so it reads as
// urgent; plain text is asserted by the surfacing tests, which strip styling.
func (m Model) authAttentionSegment() string {
	return m.styles.Fail.Render(authAttentionText)
}

// applyGHAuthSignal folds a freshly-fetched doctor report into the sticky
// attention flag: a passing gh row is a successful probe that clears it, an
// auth-invalid gh row arms it, and anything else (gh missing, offline, a
// non-gh-only battery) leaves it untouched — the same fail-closed reading the
// update-check detector applies, through the one ghx.Classify seam. A report
// with no gh row (an errored fetch) changes nothing.
func (m *Model) applyGHAuthSignal(report doctor.Report) {
	for _, result := range report.Results {
		if result.Name != ghDoctorCheckName {
			continue
		}
		switch {
		case result.Status == doctor.StatusOK:
			m.authInvalid = false
		case result.Status == doctor.StatusFail && ghx.Classify(result.Detail) == ghx.FailureAuthInvalid:
			m.authInvalid = true
		}
		return
	}
}
