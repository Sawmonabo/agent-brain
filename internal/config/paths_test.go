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
