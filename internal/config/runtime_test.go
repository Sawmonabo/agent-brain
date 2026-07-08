package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/config"
)

// Runtime-dir resolution is env-driven; t.Setenv forbids t.Parallel.
func TestRuntimeDirOverrideWins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", dir)
	got, err := config.RuntimeDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Fatalf("RuntimeDir() = %q, want override %q", got, dir)
	}
}

func TestRuntimeDirPlatformDefaults(t *testing.T) {
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", "")
	switch runtime.GOOS {
	case "darwin":
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		got, err := config.RuntimeDir()
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(tmp, "agent-brain"); got != want {
			t.Fatalf("darwin RuntimeDir() = %q, want %q", got, want)
		}
	case "linux":
		xdg := t.TempDir()
		t.Setenv("XDG_RUNTIME_DIR", xdg)
		got, err := config.RuntimeDir()
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(xdg, "agent-brain"); got != want {
			t.Fatalf("linux RuntimeDir() = %q, want %q", got, want)
		}
	}
}

func TestRuntimeDirLinuxFallbackWithoutXDG(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only fallback chain")
	}
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	got, err := config.RuntimeDir()
	if err != nil {
		t.Fatal(err)
	}
	runUser := fmt.Sprintf("/run/user/%d", os.Getuid())
	if _, statErr := os.Stat(runUser); statErr == nil {
		if want := filepath.Join(runUser, "agent-brain"); got != want {
			t.Fatalf("RuntimeDir() = %q, want %q", got, want)
		}
	} else if !strings.Contains(got, "agent-brain-") {
		t.Fatalf("RuntimeDir() = %q, want temp-dir fallback containing agent-brain-<uid>", got)
	}
}

func TestValidateSocketPath(t *testing.T) {
	t.Parallel()
	if err := config.ValidateSocketPath("/tmp/agent-brain/agent-brain.sock"); err != nil {
		t.Fatalf("short path rejected: %v", err)
	}
	long := "/" + strings.Repeat("x", 120) + "/agent-brain.sock"
	err := config.ValidateSocketPath(long)
	if err == nil {
		t.Fatal("101+ byte socket path accepted; sun_path would truncate it")
	}
	if !strings.Contains(err.Error(), "AGENT_BRAIN_RUNTIME_DIR") {
		t.Fatalf("error must name the escape hatch, got: %v", err)
	}
}
