package e2e

import (
	"fmt"
	"os"
	"path/filepath"
)

// fakeGHScript is the fake `gh` the testscript flows run against, in place of
// the real GitHub CLI (spec §12: no network, ever). Its whole behavior is
// driven by $GH_FAKE_REMOTE — a local `git init --bare` path standing in for
// the remote agent-brain-memories repo — so every gh call resolves against a
// real local repo, never the network. The flag surface it answers is exactly
// the one internal/ghx drives (verified against gh v2.96.0, global
// constraints):
//
//   - `gh --version`                          → a fixed banner
//   - `gh auth status`                        → exit 0 (always authenticated)
//   - `gh api user --jq .login`               → fakeuser
//   - `gh repo view OWNER/NAME --json name`   → exit 0 iff $GH_FAKE_REMOTE
//     exists, else exit 1 + gh's own not-found signature on stderr
//   - `gh repo create NAME --private ...`      → `git init --bare` the remote,
//     print a fake URL
//   - `gh repo clone OWNER/NAME DIR -- ARGS`   → `git clone ARGS $GH_FAKE_REMOTE DIR`
//   - `gh auth git-credential`                 → exit 0 draining stdin
//
// The credential branch is deliberately a no-op: every remote here is a local
// file path, and git never invokes a credential helper for a file-path remote.
// The credential WIRING (ADR 08) is asserted by internal/ghx and internal/gitx
// unit tests, not here — do NOT "fix" this branch to do more than exit 0.
const fakeGHScript = `#!/bin/sh
set -eu
case "${1:-}" in
--version)
	echo "gh version 2.96.0 (fake)"
	;;
auth)
	case "${2:-}" in
	status) exit 0 ;;
	git-credential) cat >/dev/null; exit 0 ;;
	*) echo "fakegh: unhandled auth subcommand: $*" >&2; exit 2 ;;
	esac
	;;
api)
	# gh api user --jq .login
	if [ "${2:-}" = "user" ]; then echo "fakeuser"; exit 0; fi
	echo "fakegh: unhandled api call: $*" >&2; exit 2
	;;
repo)
	case "${2:-}" in
	view)
		# gh repo view OWNER/NAME --json name
		if [ -d "$GH_FAKE_REMOTE" ]; then exit 0; fi
		echo "GraphQL: Could not resolve to a Repository with the name '${3:-}'. (repository)" >&2
		exit 1
		;;
	create)
		# gh repo create NAME --private --description DESC
		# --initial-branch pinned: the real GitHub creates repos with HEAD at
		# main, but a bare git init inherits the host git binary's compiled
		# default (Apple git patches it to main; upstream git still says
		# master), which would leave the remote HEAD dangling once init
		# pushes main — an environment difference, not a product behavior.
		git init --bare --initial-branch=main "$GH_FAKE_REMOTE" >/dev/null
		echo "https://github.com/fakeuser/${3:-}"
		;;
	clone)
		# gh repo clone OWNER/NAME DIR -- GITARGS...
		dir="$4"
		shift 4
		if [ "${1:-}" = "--" ]; then shift; fi
		git clone "$@" "$GH_FAKE_REMOTE" "$dir" >/dev/null 2>&1
		;;
	*) echo "fakegh: unhandled repo subcommand: $*" >&2; exit 2 ;;
	esac
	;;
*)
	echo "fakegh: unhandled invocation: $*" >&2; exit 2
	;;
esac
`

// writeFakeGH installs the fake `gh` executable into dir (which every script's
// Setup puts first on PATH, ahead of any real gh). 0o755 so git and our own
// binary can exec it.
func writeFakeGH(dir string) error {
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte(fakeGHScript), 0o755); err != nil {
		return fmt.Errorf("write fake gh: %w", err)
	}
	return nil
}
