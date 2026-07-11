// Package doctor evaluates whether this machine's agent-brain wiring is
// sound (spec §5, §7): checkout present, keyset loads, filter/merge/
// attribute wiring intact, gh available and authenticated, the daemon
// reachable, provider prerequisites satisfied, and no bash-era leftovers.
//
// It is a PACKAGE, not just a command, because the daemon consumes its
// sync-blocking subset (SafetyGate, gate.go) as the readiness gate
// evaluated before every cycle (spec §5: "the daemon refuses to sync
// until doctor passes"). The full check battery (checks.go) — gh,
// service, provider prerequisites, legacy leftovers — is CLI/dashboard
// surface only; the daemon never runs it, and this package never imports
// daemon or cli (the daemon imports doctor, never the reverse).
package doctor

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/ghx"
	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// Status is a check's outcome.
type Status int

// Status values, in increasing severity. StatusInfo is APPENDED after
// StatusFail rather than inserted by severity (which would read OK < Info <
// Warn < Fail) because no code compares Status ordinally — every existing
// site switches on named values or compares for equality (Report.Failed,
// printDoctorReport, dashboard's doctorview.go) — so renumbering buys
// nothing and would only cost every persisted/logged ordinal a silent
// meaning change. Info sits below Warn in the reporting the same way
// "unknown" does: named, never compared as a number.
const (
	StatusOK Status = iota
	StatusWarn
	StatusFail
	StatusInfo
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusWarn:
		return "warn"
	case StatusFail:
		return "fail"
	case StatusInfo:
		return "info"
	default:
		return "unknown"
	}
}

// MarshalJSON renders Status as its String() text: the wire/dashboard
// form (spec §7) is meant to be read directly, not decoded against the
// unexported int values behind it.
func (s Status) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// UnmarshalJSON is String()'s inverse.
func (s *Status) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	switch text {
	case StatusOK.String():
		*s = StatusOK
	case StatusWarn.String():
		*s = StatusWarn
	case StatusFail.String():
		*s = StatusFail
	case StatusInfo.String():
		*s = StatusInfo
	default:
		return fmt.Errorf("doctor: status %q is not one of %q, %q, %q, %q", text, StatusOK, StatusWarn, StatusFail, StatusInfo)
	}
	return nil
}

// CheckResult is one check's outcome.
type CheckResult struct {
	Name   string `json:"name"` // stable machine key, e.g. "keyset", "filters", "attributes"
	Status Status `json:"status"`
	Detail string `json:"detail"` // human sentence: what was found
	Fix    string `json:"fix,omitempty"`
	Fixed  bool   `json:"fixed,omitempty"`
}

// Report is the full check battery's outcome, in deterministic order.
type Report struct {
	Results []CheckResult `json:"results"`
}

// Failed reports whether any check is a hard failure. Warnings never
// fail a Report — they are advisory (spec §7).
func (r Report) Failed() bool {
	for _, res := range r.Results {
		if res.Status == StatusFail {
			return true
		}
	}
	return false
}

// Deps carries everything the check battery probes. Nil-able fields
// degrade to a named fail/warn, never a panic — a half-initialized
// machine is doctor's PRIMARY audience, so every check must survive a
// zero-value or missing dependency and simply report it.
type Deps struct {
	Paths       config.Paths
	Settings    config.Settings
	SettingsErr error // LoadSettings' error, surfaced as its own check
	Registry    *provider.Registry
	GH          *ghx.Client                     // nil → "gh" check fails with install guidance
	BinaryPath  string                          // what filter/helper wiring must point at
	DaemonPing  func(ctx context.Context) error // nil → "daemon" check skipped (CLI-only use)
	Enrolled    []repo.Unit                     // provider-prerequisite checks scope to what's in use
	Home        string                          // provider-prerequisite file locations
	Offline     bool                            // skip the network check (ls-remote)
}

// checkFunc is one battery entry. The bool return reports whether the
// check applies to these Deps at all — false omits it from Report.Results
// entirely (e.g. "daemon" with no DaemonPing, "remote" when Offline,
// "codex-prereqs" with no codex unit enrolled).
type checkFunc func(context.Context, Deps) (CheckResult, bool)

// battery is the full check list, in the deterministic order the CLI/
// dashboard renders it (spec: settings · keyset · checkout · filters ·
// attributes · git-meta · credential-helper · remote · gh · daemon ·
// service · registry-local · project-identity · conflict-log ·
// claude-prereqs · codex-prereqs · legacy-leftovers · secrets-scan ·
// keyset-decrypt).
// SafetyGate (gate.go) reuses these same functions in its own narrower,
// checkout-first order — but deliberately NOT checkGitMeta,
// checkSecretsScan, or checkKeysetDecrypt: checkGitMeta's doc comment
// explains why gating on it would deadlock the heal; checkSecretsScan's
// explains why gitleaks (an opt-in, on-demand external tool) has no
// bearing on whether a sync cycle is safe to run; checkKeysetDecrypt's
// explains why a stale-but-loadable keyset degrades a cycle gracefully
// (fails closed per-file, never corrupts) rather than making it unsafe to
// attempt, so it stays a human-facing advisory, last in the list since it
// samples repo content rather than merely wiring.
var battery = []checkFunc{
	checkSettings,
	checkKeyset,
	checkCheckout,
	checkFilters,
	checkAttributes,
	checkGitMeta,
	checkCredentialHelper,
	checkRemote,
	checkGH,
	checkDaemon,
	checkService,
	checkRegistryLocal,
	checkProjectIdentity,
	checkConflictLog,
	checkClaudePrereqs,
	checkCodexPrereqs,
	checkLegacyLeftovers,
	checkSecretsScan,
	checkKeysetDecrypt,
}

// Run evaluates the full check battery and returns every applicable
// result in deterministic order.
func Run(ctx context.Context, deps Deps) Report {
	var report Report
	for _, check := range battery {
		if res, applicable := check(ctx, deps); applicable {
			report.Results = append(report.Results, res)
		}
	}
	return report
}

// Fix applies the three sanctioned, idempotent repairs — filter/merge
// wiring, canonical .gitattributes, and (when gh is available) the
// repo-local credential helper — then re-runs the full battery and marks
// the repaired checks Fixed. Nothing else is auto-fixed: a wrong keyset
// or a dead gh login is a human decision, never a repair doctor applies
// on its own.
func Fix(ctx context.Context, deps Deps) (Report, error) {
	if err := gitx.InstallFilters(ctx, deps.Paths.MemoriesDir(), deps.BinaryPath); err != nil {
		return Report{}, fmt.Errorf("doctor fix filters: %w", err)
	}
	if deps.Registry != nil {
		if err := repo.WriteAttributes(repo.NewLayout(deps.Paths.MemoriesDir()), deps.Registry); err != nil {
			return Report{}, fmt.Errorf("doctor fix attributes: %w", err)
		}
	}
	if deps.GH != nil {
		if err := gitx.InstallCredentialHelper(ctx, deps.Paths.MemoriesDir(), deps.GH.BinaryPath()); err != nil {
			return Report{}, fmt.Errorf("doctor fix credential helper: %w", err)
		}
	}

	report := Run(ctx, deps)
	for i := range report.Results {
		switch report.Results[i].Name {
		case "filters":
			report.Results[i].Fixed = true
		case "attributes":
			// WriteAttributes only ran above when deps.Registry != nil —
			// Fixed must follow that same condition (Q3 gate finding M2),
			// exactly as credential-helper's Fixed already follows
			// deps.GH != nil below.
			report.Results[i].Fixed = deps.Registry != nil
		case "credential-helper":
			report.Results[i].Fixed = deps.GH != nil
		}
	}
	return report, nil
}
