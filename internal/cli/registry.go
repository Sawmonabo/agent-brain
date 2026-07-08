package cli

import (
	"os"
	"path/filepath"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/claude"
	"github.com/Sawmonabo/agent-brain/internal/provider/codex"
)

// buildRegistry is THE composition point for provider adapters — daemon,
// doctor, init, and track must all see the identical registry, or
// generated attributes and classification drift apart.
func buildRegistry(settings config.Settings, home string) (*provider.Registry, error) {
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	var codexOverrides []provider.Pattern
	if ps, ok := settings.Providers["codex"]; ok {
		for _, rule := range ps.Classify {
			class, err := provider.ClassFromString(rule.Class) // validated at load; defensive here
			if err != nil {
				return nil, err
			}
			codexOverrides = append(codexOverrides, provider.Pattern{Glob: rule.Glob, Class: class})
		}
	}
	return provider.NewRegistry(claude.New(home), codex.New(codexHome, codexOverrides))
}
