package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// scan implements `agent-brain scan`: run the user's installed gitleaks
// binary over enrolled units' plaintext provider directories, looking for
// pasted secrets that ride encrypted on the wire today (spec §5's threat
// model protects GitHub at rest) but leak the moment the plaintext is
// exported or shared (ADR 10's claude-brain harvest; ADR 14's tool
// choice). Unlike sync/status/projects, scan never talks to the daemon —
// it reads the local registry directly (like doctor's checkRegistryLocal)
// and shells out to gitleaks itself, because the daemon deliberately never
// scans during sync cycles: a per-cycle gitleaks subprocess adds latency
// and false-positive fatigue to every save for zero wire-exposure
// reduction (the wire is ciphertext regardless of what gitleaks would
// find). On-demand `scan` plus doctor's advisory secrets-scan check
// (internal/doctor/checks.go) is the right cost/benefit for a single-user
// tool (design brief, Task 5's decided non-goal).

// errGitleaksMissing names the fix — mirrors ghx.ErrMissing's shape
// (internal/ghx/ghx.go) for the same reason: a missing external dependency
// must always tell the user exactly what to run.
var errGitleaksMissing = errors.New("gitleaks not found on PATH — install it (`brew install gitleaks` on macOS; see https://github.com/gitleaks/gitleaks#installing otherwise) and retry")

// gitleaksResult carries one finished gitleaks invocation (same shape idiom
// as ghx.Result / gitx's Result — see internal/ghx/ghx.go).
type gitleaksResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// gitleaksRunner executes gitleaks. gitleaksExecRunner is process-global
// reality; scan_test.go PATH-shims a fake `gitleaks` script and exercises
// the real exec path end-to-end instead of injecting a Go-level fake at
// the command level (a hand-written fake IS used directly against
// scanUnit/scanUnits for pure merge/sort/error-path coverage). This seam
// copies ghx's Runner PATTERN, not the ghx package itself — gitleaks is a
// different external tool with its own argv/exit-code semantics, so it
// gets its own narrow interface rather than a shared abstraction.
type gitleaksRunner interface {
	Run(ctx context.Context, args ...string) (gitleaksResult, error)
}

// gitleaksWaitDelay bounds cleanup after a gitleaks invocation's context is
// canceled or gitleaks exits while an I/O pipe is still held open — same
// rationale as ghx/gitx's identical constants: never let a straggling
// child hang this process indefinitely.
const gitleaksWaitDelay = 10 * time.Second

// gitleaksExecRunner shells the real gitleaks binary.
type gitleaksExecRunner struct {
	binaryPath string
}

// Run mirrors ghx's execRunner.Run contract exactly: a normal exit (0, or
// 1 — gitleaks' own "leaks found" signal) reports its code as data (nil
// error); a canceled/expired context or a signal-terminated child is
// surfaced as an error, never mapped to a bogus exit code a caller could
// misread as real data; a spawn failure (bad path, not executable) is also
// an error.
func (r *gitleaksExecRunner) Run(ctx context.Context, args ...string) (gitleaksResult, error) {
	cmd := exec.CommandContext(ctx, r.binaryPath, args...) //nolint:gosec // G204: binaryPath comes from exec.LookPath, args are internal to this package; no untrusted-input boundary.
	cmd.WaitDelay = gitleaksWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := gitleaksResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, fmt.Errorf("gitleaks %v: %w", args, ctxErr)
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return result, nil
	case errors.As(err, &exitErr):
		if !exitErr.Exited() {
			return result, fmt.Errorf("gitleaks %v terminated by signal: %w", args, err)
		}
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	default:
		return result, fmt.Errorf("spawn gitleaks %v: %w", args, err)
	}
}

// gitleaksFinding is one entry of gitleaks' `--report-format json` output in
// `dir` mode (verified against the real gitleaks 8.30.1 binary, 2026-07-09:
// `gitleaks dir <dir> --no-banner --report-format json --report-path -`).
// Field names match gitleaks' own JSON keys exactly; tags are stated
// explicitly anyway to pin the wire contract rather than rely on Go's
// default case-insensitive matching. Commit/Author/Email/Date/Message are
// `git`-mode-only fields, always empty for the `dir` scans this command
// runs.
type gitleaksFinding struct {
	RuleID      string   `json:"RuleID"`
	Description string   `json:"Description"`
	StartLine   int      `json:"StartLine"`
	EndLine     int      `json:"EndLine"`
	StartColumn int      `json:"StartColumn"`
	EndColumn   int      `json:"EndColumn"`
	Match       string   `json:"Match"`
	Secret      string   `json:"Secret"`
	File        string   `json:"File"`
	SymlinkFile string   `json:"SymlinkFile"`
	Commit      string   `json:"Commit"`
	Entropy     float32  `json:"Entropy"`
	Author      string   `json:"Author"`
	Email       string   `json:"Email"`
	Date        string   `json:"Date"`
	Message     string   `json:"Message"`
	Tags        []string `json:"Tags"`
	Fingerprint string   `json:"Fingerprint"`
}

// scanFinding is `agent-brain scan`'s per-finding table row / --json
// element: one gitleaks finding joined with which enrolled unit's
// plaintext directory produced it (scan invokes gitleaks once per unit and
// merges results). Folder/Provider/LocalDir use this project's own
// snake_case JSON convention (matching daemon/api.UnitInfo) instead of
// embedding repo.Unit directly, which carries fields (ProjectID,
// RepoSubdir) with toml tags only and isn't meant for wire exposure
// (ProjectID is consumed by doctor's project-identity check; RepoSubdir by
// the engine's path mapping).
// Finding mirrors gitleaks' OWN report schema verbatim (PascalCase) since
// that half of the payload is a pass-through of an external tool's format,
// not this project's to rename.
type scanFinding struct {
	Folder   string          `json:"folder"`
	Provider string          `json:"provider"`
	LocalDir string          `json:"local_dir"`
	Finding  gitleaksFinding `json:"finding"`
}

// scanGitleaksArgs is the exact invocation ADR 14 and the design brief
// specify: `gitleaks dir <dir> --no-banner --report-format json
// --report-path -`. The `dir` mode family (not the deprecated-since-v8.19.0
// `detect`/`protect`) is the ADR-14-verified surface.
//
// redact appends gitleaks' own `--redact` flag (bare, so gitleaks' default
// 100%), which replaces both the `Secret` and `Match` fields with the
// literal string "REDACTED" in gitleaks' OWN JSON report — verified
// directly against the real 8.30.1 binary, 2026-07-09 — before that report
// ever reaches this process. Every other field (RuleID, File, StartLine,
// Fingerprint, ...) survives untouched, which is all `agent-brain scan`
// needs to locate and rotate a finding. This is the Q2 review's binding
// adjudication on Task 5's flagged judgment call (p4-task-5-review.md): a
// scan command's own `--json` output is exactly the kind of persistent sink
// (redirected file, CI log, terminal scrollback) this feature exists to
// keep secrets out of, so redaction is the default, gated off only by the
// explicit `--reveal-secrets` flag (newScanCmd). Passing `--redact` at the
// gitleaks-invocation layer — rather than scrubbing Secret/Match in Go after
// the fact — means the raw secret never enters this process's stdout
// buffer at all in the default case.
func scanGitleaksArgs(dir string, redact bool) []string {
	args := []string{"dir", dir, "--no-banner", "--report-format", "json", "--report-path", "-"}
	if redact {
		args = append(args, "--redact")
	}
	return args
}

// scanUnit runs one gitleaks invocation over unit's plaintext LocalDir and
// decodes its JSON report. gitleaks itself exits 1 when it finds leaks and
// 0 when clean (both are DATA, not failure — mirrored by gitleaksRunner's
// contract above); any other exit code is a real gitleaks failure (bad
// flags, unreadable config) and is surfaced as an error rather than
// silently read as "no findings". redact is forwarded to scanGitleaksArgs
// verbatim — see its doc comment for why redaction defaults on.
func scanUnit(ctx context.Context, runner gitleaksRunner, unit repo.Unit, redact bool) ([]scanFinding, error) {
	result, err := runner.Run(ctx, scanGitleaksArgs(unit.LocalDir, redact)...)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 && result.ExitCode != 1 {
		return nil, fmt.Errorf("gitleaks dir %s: exit %d: %s", unit.LocalDir, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	var findings []gitleaksFinding
	if err := json.Unmarshal([]byte(result.Stdout), &findings); err != nil {
		return nil, fmt.Errorf("parse gitleaks report for %s: %w", unit.LocalDir, err)
	}
	scanFindings := make([]scanFinding, len(findings))
	for i, finding := range findings {
		scanFindings[i] = scanFinding{Folder: unit.Folder, Provider: unit.Provider, LocalDir: unit.LocalDir, Finding: finding}
	}
	return scanFindings, nil
}

// scanUnits runs scanUnit over every unit and merges the results in a
// deterministic order (folder, then provider, then file, then line) — a
// slice built by iterating several subprocess invocations must never leave
// the printed report's order to depend on anything but the data itself.
// redact is forwarded to every scanUnit call unchanged — one gitleaks
// invocation per enrolled unit, all scanned under the same redaction
// policy for a single `agent-brain scan` run.
func scanUnits(ctx context.Context, runner gitleaksRunner, units []repo.Unit, redact bool) ([]scanFinding, error) {
	findings := []scanFinding{}
	for _, unit := range units {
		unitFindings, err := scanUnit(ctx, runner, unit, redact)
		if err != nil {
			return nil, fmt.Errorf("scan %s/%s: %w", unit.Folder, unit.Provider, err)
		}
		findings = append(findings, unitFindings...)
	}
	sort.Slice(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.Folder != b.Folder {
			return a.Folder < b.Folder
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Finding.File != b.Finding.File {
			return a.Finding.File < b.Finding.File
		}
		return a.Finding.StartLine < b.Finding.StartLine
	})
	return findings, nil
}

// filterUnitsByFolder narrows units to one enrolled folder (--project). A
// folder can legitimately hold more than one unit (e.g. claude AND codex
// both enrolled under the same project), so this returns every match, not
// just the first.
func filterUnitsByFolder(units []repo.Unit, folder string) []repo.Unit {
	var filtered []repo.Unit
	for _, unit := range units {
		if unit.Folder == folder {
			filtered = append(filtered, unit)
		}
	}
	return filtered
}

// renderScanReport writes one table row per finding, or a clean "no
// findings" line. The raw secret/match text is deliberately NOT printed
// here — the rule, folder, provider, file, and line are enough to locate
// and rotate it, without echoing a live credential into a terminal,
// scrollback, or session log any more than necessary. --json is no more
// permissive by default: gitleaks itself redacts Secret/Match to
// "REDACTED" before the report ever reaches this process (scanGitleaksArgs)
// unless the user passes --reveal-secrets, since a --json stdout stream is
// itself a persistent sink (redirected file, CI log) this feature exists
// to keep secrets out of.
func renderScanReport(report *reportWriter, findings []scanFinding) {
	if len(findings) == 0 {
		report.println("no findings")
		return
	}
	for _, f := range findings {
		report.printf("%-20s %-16s %-8s %-50s %d\n", f.Finding.RuleID, f.Folder, f.Provider, f.Finding.File, f.Finding.StartLine)
	}
}

func newScanCmd() *cobra.Command {
	var project string
	var jsonOut bool
	var revealSecrets bool
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Scan enrolled memory for pasted secrets (gitleaks)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.DefaultPaths()
			if err != nil {
				return err
			}
			localRegistry, err := repo.LoadLocalRegistry(paths.LocalRegistryFile())
			if err != nil {
				return err
			}
			units := localRegistry.Units
			if project != "" {
				units = filterUnitsByFolder(units, project)
				if len(units) == 0 {
					return fmt.Errorf("scan: no enrolled unit for folder %q (see `agent-brain projects`)", project)
				}
			}
			if len(units) == 0 {
				// Honor --json even on this empty-state path (Q2 review,
				// Minor finding): a scripted consumer that unconditionally
				// decodes stdout as JSON must not hit a parse error just
				// because nothing happens to be enrolled yet.
				if jsonOut {
					return printJSON(cmd, []scanFinding{})
				}
				report := &reportWriter{w: cmd.OutOrStdout()}
				report.println("no projects enrolled — run `agent-brain track`")
				return report.err
			}

			binaryPath, err := exec.LookPath("gitleaks")
			if err != nil {
				return errGitleaksMissing
			}
			runner := &gitleaksExecRunner{binaryPath: binaryPath}

			// --reveal-secrets only affects --json. Table rendering never reads
			// Secret/Match, so dropping gitleaks' --redact outside --json would pull
			// raw secret material into the child report and this process's memory for
			// zero benefit — keep redaction ON there and note it on stderr (stdout
			// stays the parseable report). The 0-clean/1-findings exit contract that
			// wrappers depend on is unchanged: this is a note, not a usage error.
			redact := !revealSecrets || !jsonOut
			if revealSecrets && !jsonOut {
				if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "--reveal-secrets has no effect without --json; secrets stay redacted in the table"); err != nil {
					return err
				}
			}

			findings, err := scanUnits(cmd.Context(), runner, units, redact)
			if err != nil {
				return err
			}

			if jsonOut {
				if err := printJSON(cmd, findings); err != nil {
					return err
				}
			} else {
				report := &reportWriter{w: cmd.OutOrStdout()}
				renderScanReport(report, findings)
				if report.err != nil {
					return report.err
				}
			}

			if len(findings) > 0 {
				return fmt.Errorf("scan: %d finding(s) — see above", len(findings))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "limit the scan to one enrolled folder (see `agent-brain projects`); default is every enrolled unit")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print findings as JSON")
	cmd.Flags().BoolVar(&revealSecrets, "reveal-secrets", false, "DANGER: output will contain live, usable secret material — disables gitleaks' --redact (effective only together with --json; otherwise ignored with a stderr note) so findings carry the raw Secret/Match text instead of \"REDACTED\"; only for scripted remediation with a specific, considered reason")
	return cmd
}
