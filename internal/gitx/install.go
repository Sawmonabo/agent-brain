package gitx

import (
	"context"
	"errors"
	"strings"
)

// shellQuote POSIX-sh single-quotes s, escaping any embedded quote as:
//
//	close quote, backslash-quote, reopen quote
//
// Every git config value this file writes as a command line (filter/diff/
// merge drivers, the credential helper) is run through `sh -c` by git, so it
// needs this quoting, not Go %q: the two diverge on $, backtick, and
// backslash (inside sh double quotes a command substitution would expand and
// escapes would mangle). Single-quoting makes sh treat every byte literally —
// closing the injection surface entirely.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// InstallFilters writes the local .git/config wiring (spec §5). It runs on
// init/doctor on every machine and after every clone — .git/config is not
// versioned, so this is the only place the filter chain comes into being.
// Idempotent: every entry is a single-valued replace, so re-runs converge.
func InstallFilters(ctx context.Context, repoDir, binPath string) error {
	if binPath == "" {
		// filter.agentbrain.required=true wired to an empty command would
		// brick the repo — git could neither clean nor smudge memory files.
		// Fail closed here rather than write an unrunnable required filter.
		return errors.New("gitx: empty binPath for filter wiring")
	}
	quoted := shellQuote(binPath)
	settings := [][2]string{
		{"filter.agentbrain.clean", quoted + " git-clean"},
		{"filter.agentbrain.smudge", quoted + " git-smudge"},
		{"filter.agentbrain.required", "true"},
		{"diff.agentbrain.textconv", quoted + " git-textconv"},
		{"merge.agentbrain.name", "agent-brain fact merge (3-way + retain-both)"},
		{"merge.agentbrain.driver", quoted + " git-merge --mode fact -- %O %A %B %P"},
		{"merge.agentbrain-lww.name", "agent-brain newest-wins merge"},
		{"merge.agentbrain-lww.driver", quoted + " git-merge --mode lww -- %O %A %B %P"},
		{"merge.renormalize", "true"},
	}
	for _, setting := range settings {
		// --local pins each write to this repo's .git/config and fails closed
		// when repoDir is not a git repository, instead of silently falling
		// back to the user's global config.
		if _, err := Run(ctx, repoDir, "config", "--local", setting[0], setting[1]); err != nil {
			return err
		}
	}
	return nil
}

// WiredBinaryPath recovers the binary path out of a git-config value this
// file WROTE as a shell command line: InstallFilters' filter/diff/merge
// drivers (shellQuote(binPath) + " <subcommand>") and InstallCredentialHelper's
// helper ("!" + shellQuote(ghPath) + " auth git-credential"). It is the READER
// half of shellQuote; keep the two adjacent, because an edit to shellQuote's
// escaping is only correct if this decoder honors the same scheme.
// doctor.checkFilters / doctor.checkCredentialHelper call this to compare the
// recorded binary against the running one by FILE IDENTITY, so a symlink
// spelling and its target's real spelling equate — a raw substring test cannot.
//
// Decoding: one leading "!" (git's shell-command helper prefix) is stripped
// first. If the remainder begins with a single quote it is decoded exactly as
// shellQuote wrote it — literal characters inside '...', an embedded quote
// encoded as shellQuote's four-byte seam (close-quote, backslash-escaped
// quote, reopen-quote — spelled out in shellQuote's own doc, since gofmt's
// doc-comment quote conversion mangles the literal here), stopping at the
// first UNQUOTED space (the delimiter before the subcommand). Otherwise the
// value is treated as hand-edited, unquoted config:
// the path is its first space-delimited field. An empty value, a value that
// decodes to the empty string, or a single-quoted value with no closing quote,
// is undecodable (ok=false).
func WiredBinaryPath(value string) (string, bool) {
	value = strings.TrimPrefix(value, "!")
	if value == "" {
		return "", false
	}
	if value[0] != '\'' {
		field, _, _ := strings.Cut(value, " ")
		if field == "" {
			return "", false
		}
		return field, true
	}
	var decoded strings.Builder
	for index := 0; index < len(value); {
		character := value[index]
		if character == ' ' {
			break // unquoted delimiter: end of the path token
		}
		if character == '\'' {
			index++
			start := index
			for index < len(value) && value[index] != '\'' {
				index++
			}
			if index >= len(value) {
				return "", false // unterminated single quote
			}
			decoded.WriteString(value[start:index])
			index++ // consume the closing quote
			continue
		}
		if character == '\\' {
			// Backslash escape between quoted spans (shellQuote's '\'' seam):
			// the next byte is literal.
			index++
			if index >= len(value) {
				return "", false
			}
			decoded.WriteByte(value[index])
			index++
			continue
		}
		decoded.WriteByte(character)
		index++
	}
	result := decoded.String()
	if result == "" {
		return "", false
	}
	return result, true
}

// InstallMaintenancePosture pins git's automatic maintenance to the
// FOREGROUND in this repo's LOCAL .git/config (ADR 19). Git's auto
// maintenance defaults to DETACHING: after a repository-writing command it
// may spawn `git maintenance run --auto` (older gits: `git gc --auto`) as a
// background process that outlives the foreground command. In the memories
// checkout that detached process races the sync engine's single writer
// (ADR 03) — a later cycle, a quiesced init/doctor mutation, or teardown.
//
// Auto maintenance stays ENABLED: gc.auto / maintenance.auto are left at
// their defaults, so loose objects still get packed once their thresholds
// trip. Only the DETACH is disabled, so that work runs inline inside the
// engine's own git children — serialized by the single writer instead of
// racing it.
//
// Both keys are written explicitly even though maintenance.autoDetach alone
// would inherit gc.autoDetach through git's fallback chain today: that chain
// is an upstream implementation detail, and the explicit pair is
// self-documenting and robust to a future change in it. Values are literals,
// not command lines, so no shell quoting is involved (unlike InstallFilters).
//
// Idempotent: every entry is a single-valued replace, so re-runs converge.
// --local pins each write to this repo and fails closed outside a git repo.
// It runs on init, on doctor --fix, and at the top of every engine cycle
// (engine.prepareCheckout) — .git/config is not versioned, so this is the
// only place the posture comes into being, and the per-cycle re-pin heals
// any drift on every fleet member.
func InstallMaintenancePosture(ctx context.Context, repoDir string) error {
	settings := [][2]string{
		{"gc.autoDetach", "false"},
		{"maintenance.autoDetach", "false"},
	}
	for _, setting := range settings {
		// --local pins each write to this repo's .git/config and fails closed
		// when repoDir is not a git repository, instead of silently falling
		// back to the user's global config.
		if _, err := Run(ctx, repoDir, "config", "--local", setting[0], setting[1]); err != nil {
			return err
		}
	}
	return nil
}

// InstallCredentialHelper wires the hidden checkout's HTTPS credential
// lookup to gh's own helper (ADR 08), repo-LOCAL only — never the user's
// global gitconfig, which cli/cli#9438 documents as a synced-dotfiles hazard
// for `gh auth setup-git`'s absolute-path write; that command is never
// called, and this is the alternative that reaches the same end state
// without leaving the machine. SSH remotes never invoke a credential helper,
// so this wiring is inert for them.
//
// It runs on init and after every clone — .git/config is not versioned, so
// this is the only place the wiring comes into being — and doctor --fix
// re-wires it if gh moves.
//
// Idempotent in two steps:
//  1. credential.helper is reset to a single empty entry. Git treats an
//     empty credential.helper value as a reset sentinel: it clears every
//     helper accumulated from lower-priority config (global, system) so a
//     stale global osxkeychain PAT for github.com can't shadow gh, AND
//     --replace-all here (with no value pattern) collapses any local entries
//     from a PRIOR run of this function — including a previously-added gh
//     line — back down to just this one line.
//  2. gh's own helper is appended with --add. Because step 1 always
//     resets the local config to exactly one line first, --add here can
//     never accumulate a duplicate across re-runs: the result is always
//     exactly two local entries, the reset plus the current gh path.
func InstallCredentialHelper(ctx context.Context, repoDir, ghPath string) error {
	if ghPath == "" {
		// A helper wired to an empty command would silently fail every HTTPS
		// credential lookup. Fail closed here rather than write it.
		return errors.New("gitx: empty ghPath for credential helper wiring")
	}
	if _, err := Run(ctx, repoDir, "config", "--local", "--replace-all", "credential.helper", ""); err != nil {
		return err
	}
	quoted := shellQuote(ghPath)
	if _, err := Run(ctx, repoDir, "config", "--local", "--add", "credential.helper", "!"+quoted+" auth git-credential"); err != nil {
		return err
	}
	return nil
}
