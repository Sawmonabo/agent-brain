package doctor_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
)

// fakeGitleaksOnPath puts a trivial executable named "gitleaks" on PATH —
// checkSecretsScan only probes presence (exec.LookPath), never runs it, so
// the script's content is irrelevant (mirrors doctor_test.go's fakeGhOnPath
// for the analogous "gh" presence check).
func fakeGitleaksOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gitleaks"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// minimalDeps builds just enough Deps for checkSecretsScan, which reads
// nothing from Deps at all (a pure PATH probe) — mirrors
// TestRunCheckoutNotAGitRepo's minimal, fixture-free Deps{} style rather
// than the full newFixture machine, since no git/keyset/checkout is
// needed here.
func minimalDeps(t *testing.T) doctor.Deps {
	t.Helper()
	base := t.TempDir()
	return doctor.Deps{
		Paths:    config.Paths{ConfigDir: filepath.Join(base, "cfg"), DataDir: filepath.Join(base, "data")},
		Registry: testRegistry(t),
		Home:     filepath.Join(base, "home"),
	}
}

func TestRunSecretsScanNotInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir: no gitleaks anywhere on PATH
	got := result(t, doctor.Run(context.Background(), minimalDeps(t)), "secrets-scan")
	if got.Status != doctor.StatusWarn {
		t.Fatalf("secrets-scan check = %+v, want warn", got)
	}
	if got.Fix == "" {
		t.Fatal("secrets-scan warn result has no Fix guidance")
	}
}

func TestRunSecretsScanInstalled(t *testing.T) {
	fakeGitleaksOnPath(t)
	got := result(t, doctor.Run(context.Background(), minimalDeps(t)), "secrets-scan")
	if got.Status != doctor.StatusOK {
		t.Fatalf("secrets-scan check = %+v, want ok", got)
	}
}
