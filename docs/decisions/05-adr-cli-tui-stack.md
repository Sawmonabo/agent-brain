# ADR 05: CLI + TUI stack — Cobra + Fang commands, Charm v2 TUI

- **Status:** Accepted
- **Date:** 2026-07-07
- **Deciders:** Sawmon (accepted within Approach A / buy-vs-build table)
- **Related:** ADR 01 (interactive enrollment CLI is a core product surface)

## Context

ADR 01 makes a pretty interactive CLI a first-class product surface: a dashboard
listing discovered projects with enroll/untrack toggles, sync status, and conflict
surfacing, plus first-run setup wizards. The mid-2026 Go ecosystem event: the Charm v2
stack went GA on 2026-02-24 (stable, `charm.land/...` import paths) and requires
Go 1.25+.

## Options considered

1. **Cobra + Fang for commands; Bubble Tea v2 + Lip Gloss v2 + Bubbles v2 + Huh v2 for
   TUI (chosen).** Verified via go.mod evidence: GitHub's `gh` ships cobra v1.10.2 +
   the full Charm v2 stack; Charm's own `crush` ships cobra + fang v2 + bubbletea v2.
   This is the paved path. Fang is a Cobra decorator (styled help/errors, `--version`,
   completions, man pages) — "pretty" for near-zero effort.
2. **Kong or urfave/cli v3 for commands** — cleaner APIs, but Fang is Cobra-only and
   Cobra's ubiquity (kubectl/gh/docker/helm) wins on contributor familiarity.
3. **Lip Gloss + Huh only, no Bubble Tea** — right if "pretty" meant styled output +
   prompts; rejected because the live dashboard (ADR 01) genuinely needs the
   full-screen event-loop model.

## Decision

Option 1. Versions as of 2026-07-07: cobra v1.10.2, fang v2.0.1, bubbletea v2.0.8,
lipgloss v2.0.5, bubbles v2.1.1, huh v2.0.3. Huh drives first-run setup, machine
pairing, and any guided flows. Consequence: `go.mod` floor is **Go 1.26** (current
stable; Charm v2 requires ≥1.25).

## Consequences

- Cobra's release cadence is deliberately slow (maintainer-bandwidth, documented by
  spf13 in "The Maintainer's Dilemma", 2026-05-20) — acceptable: it is stable
  infrastructure, not a moving target.
- The dashboard is a Bubble Tea program talking to the daemon over the ADR 09 control
  plane; CLI subcommands remain plain Cobra for scriptability.

## Buy vs build

**Buy everything.** All five libraries are actively maintained, GA-stable, and adopted
by exactly the class of tool we are building (`gh`, `crush`). No custom terminal
handling.

## Sources

Research delegated to a parallel research team (accessed 2026-07-07); links below are
the sources cited in its Topic B report.

- https://github.com/charmbracelet/bubbletea/releases
- https://byteiota.com/bubble-tea-v2-10x-faster-terminal-uis-for-go-developers/
- https://github.com/charmbracelet/fang
- https://spf13.com/p/the-maintainers-dilemma/
- https://github.com/cli/cli/blob/trunk/go.mod
- https://github.com/charmbracelet/crush/blob/main/go.mod
