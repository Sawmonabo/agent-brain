package config

import (
	"path/filepath"
	"runtime"
	"testing"
)

// t.Setenv forbids t.Parallel — these tests stay serial.
func TestDefaultPathsEnvOverride(t *testing.T) {
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", "/tmp/cfg")
	t.Setenv("AGENT_BRAIN_DATA_DIR", "/tmp/data")
	paths, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if paths.ConfigDir != "/tmp/cfg" || paths.DataDir != "/tmp/data" {
		t.Fatalf("got %+v, want env-injected dirs", paths)
	}
	if got, want := paths.Keyset(), filepath.Join("/tmp/cfg", "keyset.json"); got != want {
		t.Fatalf("Keyset() = %q, want %q", got, want)
	}
}

func TestDefaultPathsPerOS(t *testing.T) {
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", "")
	t.Setenv("AGENT_BRAIN_DATA_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/home/u")
	paths, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/home/u", ".config", "agent-brain"); paths.ConfigDir != want {
		t.Fatalf("ConfigDir = %q, want %q", paths.ConfigDir, want)
	}
	wantData := filepath.Join("/home/u", ".local", "share", "agent-brain")
	if runtime.GOOS == "darwin" {
		wantData = filepath.Join("/home/u", "Library", "Application Support", "agent-brain")
	}
	if paths.DataDir != wantData {
		t.Fatalf("DataDir = %q, want %q", paths.DataDir, wantData)
	}
}

// TestDefaultPathsOverrideWithoutHome pins the filter-subprocess hardening:
// when both dirs are injected via env, DefaultPaths must not depend on $HOME.
// os.UserHomeDir returns "$HOME is not defined" for an empty HOME on
// darwin/linux (verified against the installed os.UserHomeDir source,
// go1.26.5), so an empty HOME models a git-spawned filter process that has no
// home in its environment.
func TestDefaultPathsOverrideWithoutHome(t *testing.T) {
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", "/tmp/cfg")
	t.Setenv("AGENT_BRAIN_DATA_DIR", "/tmp/data")
	t.Setenv("HOME", "")
	paths, err := DefaultPaths()
	if err != nil {
		t.Fatalf("DefaultPaths() with both overrides must not depend on $HOME, got err: %v", err)
	}
	if paths.ConfigDir != "/tmp/cfg" || paths.DataDir != "/tmp/data" {
		t.Fatalf("got %+v, want env-injected dirs", paths)
	}
}

// TestDefaultPathsPartialOverrideNeedsHome guards that the override-first
// reorder does not weaken the partial case: with only one dir injected, the
// other still needs its OS default, so an unresolvable $HOME must still error
// rather than silently yield an empty path.
func TestDefaultPathsPartialOverrideNeedsHome(t *testing.T) {
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", "/tmp/cfg")
	t.Setenv("AGENT_BRAIN_DATA_DIR", "")
	t.Setenv("HOME", "")
	if _, err := DefaultPaths(); err == nil {
		t.Fatal("DefaultPaths() with only ConfigDir overridden and no $HOME must error; DataDir needs the OS default")
	}
}
