package config_test

import (
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/config"
)

func TestStatePathHelpers(t *testing.T) {
	t.Parallel()
	p := config.Paths{ConfigDir: "/cfg", DataDir: "/data"}
	tests := []struct{ name, got, want string }{
		{"settings", p.SettingsFile(), filepath.Join("/cfg", "config.toml")},
		{"memories", p.MemoriesDir(), filepath.Join("/data", "memories")},
		{"local registry", p.LocalRegistryFile(), filepath.Join("/data", "registry-local.toml")},
		{"daemon log", p.DaemonLogFile(), filepath.Join("/data", "daemon.log")},
		{"conflict log", p.ConflictLogFile(), filepath.Join("/data", "conflicts.jsonl")},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Fatalf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}
