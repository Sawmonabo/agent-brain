package gitx

import (
	"context"
	"errors"
	"strings"
)

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
	// git runs filter/diff/merge command lines through `sh -c`, so binPath needs
	// POSIX-sh quoting, not Go %q: the two diverge on $, backtick, and backslash
	// (inside sh double quotes a command substitution would expand and escapes
	// would mangle). Single-quoting, escaping any embedded quote as '\'', makes
	// sh treat every byte literally — closing the injection surface entirely.
	quoted := "'" + strings.ReplaceAll(binPath, "'", `'\''`) + "'"
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
