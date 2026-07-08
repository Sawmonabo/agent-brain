package ghx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestExecRunnerExitCode pins the exec-backed Runner's data contract: a
// normal, non-zero exit is DATA (nil error), mirroring gitx.RunStatus. This
// test needs direct access to the unexported execRunner type, so it lives in
// this internal (package ghx) file rather than alongside the Client tests,
// which import ghxtest — a package that itself imports ghx, so an internal
// test file here cannot import it too without an import cycle.
func TestExecRunnerExitCode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ghPath := filepath.Join(dir, "gh")
	const script = "#!/bin/sh\necho out\necho err >&2\nexit 3\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &execRunner{binaryPath: ghPath}

	result, err := runner.Run(context.Background(), "version")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	want := Result{Stdout: "out\n", Stderr: "err\n", ExitCode: 3}
	if diff := cmp.Diff(want, result); diff != "" {
		t.Errorf("Run() mismatch (-want +got):\n%s", diff)
	}
}

// TestExecRunnerUnstartableBinary pins the spawn-failure contract: a
// binaryPath that cannot even be exec'd (missing file) is an error, never
// data with a fabricated exit code.
func TestExecRunnerUnstartableBinary(t *testing.T) {
	t.Parallel()
	runner := &execRunner{binaryPath: filepath.Join(t.TempDir(), "no-such-gh")}

	if _, err := runner.Run(context.Background(), "version"); err == nil {
		t.Fatal("Run() with an unstartable binary path succeeded; want error")
	}
}

// TestExecRunnerSignalKilled mirrors gitx's TestRunStatusSignalKilledErrors:
// a signal-terminated child exits with a bogus code (-1) that must never
// leak as data — it is reported as an error instead.
func TestExecRunnerSignalKilled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ghPath := filepath.Join(dir, "gh")
	const script = "#!/bin/sh\nkill -KILL $$\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &execRunner{binaryPath: ghPath}

	result, err := runner.Run(context.Background(), "version")
	if err == nil {
		t.Fatal("Run() for a signal-killed process succeeded; want error")
	}
	if result.ExitCode == -1 {
		t.Error("signal kill leaked as ExitCode -1 alongside the error; want the exit code left unset")
	}
}

// TestExecRunnerContextCanceled mirrors gitx's TestRunContextCanceled: a
// canceled context must surface as an error, never as data.
func TestExecRunnerContextCanceled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ghPath := filepath.Join(dir, "gh")
	const script = "#!/bin/sh\nsleep 5\n"
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := &execRunner{binaryPath: ghPath}

	_, err := runner.Run(ctx, "version")
	if err == nil {
		t.Fatal("Run() with an already-canceled context succeeded; want error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error does not wrap context.Canceled: %v", err)
	}
}
