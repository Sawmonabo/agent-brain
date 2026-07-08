// Package config resolves agent-brain's on-disk locations (spec §3, §5).
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
// git spawns without our process environment conventions.
func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	paths := Paths{
		ConfigDir: filepath.Join(xdgDir("XDG_CONFIG_HOME", filepath.Join(home, ".config")), "agent-brain"),
	}
	if runtime.GOOS == "darwin" {
		paths.DataDir = filepath.Join(home, "Library", "Application Support", "agent-brain")
	} else {
		paths.DataDir = filepath.Join(xdgDir("XDG_DATA_HOME", filepath.Join(home, ".local", "share")), "agent-brain")
	}
	if dir := os.Getenv("AGENT_BRAIN_CONFIG_DIR"); dir != "" {
		paths.ConfigDir = dir
	}
	if dir := os.Getenv("AGENT_BRAIN_DATA_DIR"); dir != "" {
		paths.DataDir = dir
	}
	return paths, nil
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
