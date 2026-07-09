package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/Sawmonabo/agent-brain/internal/provider"
)

// Duration is a time.Duration that unmarshals from TOML strings ("5m").
type Duration time.Duration

// UnmarshalText implements encoding.TextUnmarshaler (go-toml v2 honors it).
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// SyncSettings tunes the engine/watch cadence.
type SyncSettings struct {
	// Ticker is the idle fetch/integrate interval (spec §4: 5m default).
	Ticker Duration `toml:"ticker"`
	// Debounce is the watch trailing-quiet window (ADR 07: 2s default).
	Debounce Duration `toml:"debounce"`
	// Poll is the backstop rescan interval (ADR 07).
	Poll Duration `toml:"poll"`
}

// ClassifyRule overrides one classification pattern for a provider.
// Class is one of provider.Class.String()'s exact values ("fact",
// "derived-index", "regenerated", "ignore") — LoadSettings rejects
// anything else.
type ClassifyRule struct {
	Glob  string `toml:"glob"`
	Class string `toml:"class"`
}

// ProviderSettings is one provider's config.toml override section.
// Currently only classification tables are overridable (spec §6: Codex's
// on-disk layout is partly third-party-documented, so its table absorbs
// upstream format drift without a release; Claude's is deliberately not
// overridable and so has no entry here).
type ProviderSettings struct {
	Classify []ClassifyRule `toml:"classify"`
}

// Settings is ~/.config/agent-brain/config.toml — user-edited, read-only
// to the program (ADR 17: `agent-brain init` writes it once from a
// template; nothing ever rewrites it, so user comments survive).
type Settings struct {
	Sync SyncSettings `toml:"sync"`
	// Providers keys by provider name (e.g. "codex") — see ProviderSettings.
	Providers map[string]ProviderSettings `toml:"providers"`
}

// DefaultSettings returns the documented defaults.
func DefaultSettings() Settings {
	return Settings{Sync: SyncSettings{
		Ticker:   Duration(5 * time.Minute),
		Debounce: Duration(2 * time.Second),
		Poll:     Duration(45 * time.Second),
	}}
}

// LoadSettings reads path. A missing file is the default configuration; a
// present file must parse strictly — an unknown key is an error, because
// a typo'd setting silently ignored is a setting that silently doesn't
// apply. Floors keep pathological values from wedging the daemon.
func LoadSettings(path string) (Settings, error) {
	settings := DefaultSettings()
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the program-derived settings location, not untrusted input
	if errors.Is(err, fs.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("read settings: %w", err)
	}
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return Settings{}, fmt.Errorf("parse settings %s: %w", path, err)
	}
	if err := settings.validate(); err != nil {
		return Settings{}, fmt.Errorf("settings %s: %w", path, err)
	}
	for providerName, ps := range settings.Providers {
		for i, rule := range ps.Classify {
			if err := provider.ValidateGlob(rule.Glob); err != nil {
				return Settings{}, fmt.Errorf("providers.%s.classify[%d]: %w", providerName, i, err)
			}
			if _, err := provider.ClassFromString(rule.Class); err != nil {
				return Settings{}, fmt.Errorf("providers.%s.classify[%d]: %w", providerName, i, err)
			}
		}
	}
	return settings, nil
}

func (s Settings) validate() error {
	checks := []struct {
		name  string
		value time.Duration
		floor time.Duration
	}{
		{"sync.ticker", time.Duration(s.Sync.Ticker), 30 * time.Second},
		{"sync.debounce", time.Duration(s.Sync.Debounce), 100 * time.Millisecond},
		{"sync.poll", time.Duration(s.Sync.Poll), 5 * time.Second},
	}
	for _, c := range checks {
		if c.value < c.floor {
			return fmt.Errorf("%s = %s is below the %s floor", c.name, c.value, c.floor)
		}
	}
	return nil
}
