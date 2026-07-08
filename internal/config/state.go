package config

import "path/filepath"

// SettingsFile is the user-edited configuration (ADR 17).
func (p Paths) SettingsFile() string { return filepath.Join(p.ConfigDir, "config.toml") }

// MemoriesDir is the hidden agent-brain-memories checkout (spec §3).
func (p Paths) MemoriesDir() string { return filepath.Join(p.DataDir, "memories") }

// LocalRegistryFile is the machine-local enrollment registry (spec §3).
func (p Paths) LocalRegistryFile() string { return filepath.Join(p.DataDir, "registry-local.toml") }

// DaemonLogFile is the daemon's structured log (spec §3).
func (p Paths) DaemonLogFile() string { return filepath.Join(p.DataDir, "daemon.log") }

// ConflictLogFile is where the merge driver records retain-both events
// (spec §4: "records the event for the dashboard conflicts view") when
// the daemon exports AGENT_BRAIN_CONFLICT_LOG. Phase 3's conflicts view
// reads it.
func (p Paths) ConflictLogFile() string { return filepath.Join(p.DataDir, "conflicts.jsonl") }
