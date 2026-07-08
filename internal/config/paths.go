// Package config resolves agent-brain's on-disk locations (spec §3, §5).
// It imports provider (ValidateGlob, ClassFromString) to validate
// per-provider classify overrides at load time; provider imports
// nothing internal, so config → provider is not a cycle.
package config

import (
	"os"
	"path/filepath"
	"runtime"
)

// Paths locates agent-brain's local state. ConfigDir holds keyset.json and
// config.toml on every OS; DataDir holds the memories checkout, local
// registry, and logs.
type Paths struct {
	ConfigDir string
	DataDir   string
}

// DefaultPaths resolves per-OS defaults. AGENT_BRAIN_CONFIG_DIR and
// AGENT_BRAIN_DATA_DIR override — required by tests and by filter processes
// git spawns without our process environment conventions. The overrides are
// consulted first: when both are set, $HOME is never read, so a fully-injected
// filter subprocess resolves paths even with no home in its environment.
func DefaultPaths() (Paths, error) {
	configDir := os.Getenv("AGENT_BRAIN_CONFIG_DIR")
	dataDir := os.Getenv("AGENT_BRAIN_DATA_DIR")
	if configDir != "" && dataDir != "" {
		return Paths{ConfigDir: configDir, DataDir: dataDir}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	if configDir == "" {
		configDir = filepath.Join(xdgDir("XDG_CONFIG_HOME", filepath.Join(home, ".config")), "agent-brain")
	}
	if dataDir == "" {
		if runtime.GOOS == "darwin" {
			dataDir = filepath.Join(home, "Library", "Application Support", "agent-brain")
		} else {
			dataDir = filepath.Join(xdgDir("XDG_DATA_HOME", filepath.Join(home, ".local", "share")), "agent-brain")
		}
	}
	return Paths{ConfigDir: configDir, DataDir: dataDir}, nil
}

// Keyset returns the Tink keyset location (spec §5: beside config.toml).
func (p Paths) Keyset() string {
	return filepath.Join(p.ConfigDir, "keyset.json")
}

func xdgDir(env, fallback string) string {
	if dir := os.Getenv(env); dir != "" {
		return dir
	}
	return fallback
}
