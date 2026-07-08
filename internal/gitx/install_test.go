package gitx

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestInstallCredentialHelper pins the repo-local credential wiring (ADR 08):
// a first empty credential.helper entry clears every helper this repo would
// otherwise inherit from global/system config (a stale global osxkeychain
// PAT for github.com must not shadow gh — the reason gh's own `setup-git` is
// never used to reach this state instead), then gh's own helper is appended
// by absolute path, POSIX-sh quoted (git runs a credential.helper value
// through /bin/sh, exactly like the filter commands InstallFilters wires —
// see its quoting comment). The embedded space in ghPath proves the quoting
// actually runs. Re-running must converge on the same two entries, never
// accumulate a duplicate gh line.
func TestInstallCredentialHelper(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := Run(ctx, dir, "init", "--quiet"); err != nil {
		t.Fatal(err)
	}

	const ghPath = "/fake path/gh"
	want := []string{"", `!'/fake path/gh' auth git-credential`}

	for i := range 2 {
		if err := InstallCredentialHelper(ctx, dir, ghPath); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		result, err := Run(ctx, dir, "config", "--local", "--get-all", "credential.helper")
		if err != nil {
			t.Fatalf("run %d: get-all credential.helper: %v", i, err)
		}
		got := strings.Split(strings.TrimRight(result.Stdout, "\n"), "\n")
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("run %d: credential.helper entries (-want +got):\n%s", i, diff)
		}
	}
}

// TestInstallCredentialHelperEmptyGhPath pins the fail-closed guard: wiring
// an empty command as gh's credential helper would silently make every HTTPS
// credential lookup fail, so InstallCredentialHelper must reject it up front
// rather than write an unusable helper entry.
func TestInstallCredentialHelperEmptyGhPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := Run(ctx, dir, "init", "--quiet"); err != nil {
		t.Fatal(err)
	}
	if err := InstallCredentialHelper(ctx, dir, ""); err == nil {
		t.Error("InstallCredentialHelper with empty ghPath succeeded; want error")
	}
}
