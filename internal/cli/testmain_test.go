package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/gitx/gitxtest"
)

// testBinaryPath is a REAL, freshly built agent-brain binary (see TestMain).
// Every fixture in this package that wires filter.agentbrain.clean/smudge
// (gitx.InstallFilters) must point it at THIS, and every doctor-command test
// must route buildDoctorDeps at it via testBinaryPathEnv (doctor.go) —
// never at os.Executable(). Inside a test process, os.Executable() IS the
// compiled cli.test binary — wiring a git filter at it means git invokes
// cli.test as its own clean/smudge driver. A Go test binary given an
// unrecognized positional arg ("git-clean") does not error; it falls
// through to running the whole suite again, and with no -test.timeout
// (only `go test` injects that — a git-spawned subprocess bypasses it
// entirely), each nested run reinstalls filters pointing at itself and
// recurses without bound. That is what happened to internal/daemon on
// 2026-07-08: ~70GB of nested `go test` processes and a hard reboot
// (CLAUDE.md's fork-bomb rule, commit 8624631, and this package's own Q3
// gate finding I1). testBinaryPath removes the cause; TestMain's tripwire
// below is the backstop that turns any recurrence into one loud, immediate
// failure instead of a repeat.
var testBinaryPath string

// TestMain's FIRST action, before the testing package's own flag parsing or
// m.Run(), must be the tripwire above: a git filter invocation would arrive
// as a bare positional arg, which nothing else in this file inspects this
// early. See testBinaryPath's doc comment for the incident this prevents.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "git-clean", "git-smudge", "git-textconv", "git-merge":
			fmt.Fprintln(os.Stderr, "cli.test invoked as a git filter — a fixture wired filter config at the test binary; see the 2026-07-08 fork-bomb incident (testBinaryPath's doc comment, this file)")
			os.Exit(1)
		}
	}
	os.Exit(testMain(m))
}

// testMain builds the real binary testBinaryPath points at, isolates the
// suite's git config (gitxtest.Setenv — this package previously had none),
// then runs the suite. Building once per package-test-run (not per fixture)
// keeps every cli test's filter wiring pointed at the same real binary at
// near-zero added cost.
func testMain(m *testing.M) int {
	root, err := os.MkdirTemp("", "agent-brain-cli-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(root) }()

	testBinaryPath = filepath.Join(root, "agent-brain")
	build := exec.Command("go", "build", "-o", testBinaryPath, "../../cmd/agent-brain")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n%s", err, out)
		return 1
	}

	// This package previously ran with no git-config isolation at all: every
	// fixture inherited the developer's real ~/.gitconfig, with auto-gc/
	// auto-maintenance live. gitxtest.Setenv neutralizes both and disables
	// maintenance entirely, matching every other package's test posture.
	_, cleanup, err := gitxtest.Setenv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer cleanup()

	return m.Run()
}
