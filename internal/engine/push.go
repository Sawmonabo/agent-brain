package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
)

// pushRaceRetries bounds the reject → re-integrate → retry loop
// (spec §4 step 6); after that the commits wait for the next cycle.
const pushRaceRetries = 3

// pushOutcome reports spec §4 step 6. Queued means unpushed commits
// remain — the git-native queue; never an error state.
type pushOutcome struct {
	Pushed      bool
	Queued      bool
	DegradedAll bool
	Degraded    []string
}

// push delivers local commits. Only a non-fast-forward REJECTION enters
// the race-retry loop; network failures queue immediately for the
// backoff/ticker to retry (spec §11).
func (e *Engine) push(ctx context.Context) (pushOutcome, error) {
	for attempt := 0; ; attempt++ {
		ahead, err := gitx.Run(ctx, e.checkout, "rev-list", "--count", upstreamRef+"..HEAD")
		if err != nil {
			return pushOutcome{}, fmt.Errorf("push: ahead count: %w", err)
		}
		if strings.TrimSpace(ahead.Stdout) == "0" {
			return pushOutcome{}, nil // nothing to push
		}

		res, err := gitx.RunStatus(ctx, e.checkout, "push", "--quiet", remoteName, defaultBranch)
		if err != nil {
			return pushOutcome{}, fmt.Errorf("push: %w", err)
		}
		if res.ExitCode == 0 {
			return pushOutcome{Pushed: true}, nil
		}
		if !isRejection(res.Stderr) || attempt >= pushRaceRetries {
			return pushOutcome{Queued: true}, nil
		}

		// Race lost: someone pushed since our fetch. Re-integrate and retry.
		integ, err := e.integrate(ctx)
		if err != nil {
			return pushOutcome{Queued: true}, err
		}
		if !integ.Integrated {
			return pushOutcome{
				Queued:      true,
				DegradedAll: integ.DegradedAll,
				Degraded:    integ.Degraded,
			}, nil
		}
	}
}

// isRejection detects a non-fast-forward push rejection (as opposed to
// a transport failure). Git's phrasing is stable across versions:
// "[rejected]" plus "fetch first" / "non-fast-forward" hints.
func isRejection(stderr string) bool {
	return strings.Contains(stderr, "[rejected]") ||
		strings.Contains(stderr, "non-fast-forward") ||
		strings.Contains(stderr, "fetch first")
}
