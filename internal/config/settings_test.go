package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/config"
)

func TestLoadSettingsMissingFileYieldsDefaults(t *testing.T) {
	t.Parallel()
	got, err := config.LoadSettings(filepath.Join(t.TempDir(), "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(config.DefaultSettings(), got); diff != "" {
		t.Fatalf("defaults mismatch (-want +got):\n%s", diff)
	}
	if time.Duration(got.Sync.Ticker) != 5*time.Minute {
		t.Fatalf("default ticker = %v, want 5m", got.Sync.Ticker)
	}
}

func TestLoadSettingsParsesAndValidates(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, content string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	good, err := config.LoadSettings(write(t, "[sync]\nticker = \"1m\"\ndebounce = \"500ms\"\npoll = \"30s\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(good.Sync.Ticker) != time.Minute || time.Duration(good.Sync.Debounce) != 500*time.Millisecond {
		t.Fatalf("parsed settings wrong: %+v", good)
	}

	cases := []struct{ name, content string }{
		{"unknown key", "[sync]\ntikcer = \"1m\"\n"},
		{"unknown table", "[sink]\nticker = \"1m\"\n"},
		{"bad duration", "[sync]\nticker = \"soon\"\n"},
		{"ticker under floor", "[sync]\nticker = \"5s\"\n"},
		{"debounce under floor", "[sync]\ndebounce = \"1ms\"\n"},
		{"poll under floor", "[sync]\npoll = \"1s\"\n"},
		{"corrupt", "[sync\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := config.LoadSettings(write(t, tc.content)); err == nil {
				t.Fatalf("LoadSettings accepted %s; want error", tc.name)
			}
		})
	}
}

// TestLoadSettingsMigratePreflightTimeoutDefaultsTo30s pins spec §10's
// pre-flight timeout default absent any [migrate] table — the same
// treatment TestLoadSettingsMissingFileYieldsDefaults gives [sync].
func TestLoadSettingsMigratePreflightTimeoutDefaultsTo30s(t *testing.T) {
	t.Parallel()
	got, err := config.LoadSettings(filepath.Join(t.TempDir(), "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(got.Migrate.PreflightTimeout) != 30*time.Second {
		t.Fatalf("default migrate.preflight_timeout = %v, want 30s", time.Duration(got.Migrate.PreflightTimeout))
	}
}

// TestLoadSettingsMigratePreflightTimeoutParses proves a [migrate] table
// overrides the default exactly like [sync]'s durations do.
func TestLoadSettingsMigratePreflightTimeoutParses(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte("[migrate]\npreflight_timeout = \"2m\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := config.LoadSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(got.Migrate.PreflightTimeout) != 2*time.Minute {
		t.Fatalf("migrate.preflight_timeout = %v, want 2m", time.Duration(got.Migrate.PreflightTimeout))
	}
}

// TestLoadSettingsMigratePreflightTimeoutAcceptsCeilingExactly proves the
// ≤10m bound is inclusive.
func TestLoadSettingsMigratePreflightTimeoutAcceptsCeilingExactly(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte("[migrate]\npreflight_timeout = \"10m\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadSettings(p); err != nil {
		t.Fatalf("LoadSettings() rejected preflight_timeout = 10m (bound is inclusive): %v", err)
	}
}

// TestLoadSettingsMigratePreflightTimeoutValidation proves out-of-bounds
// values are rejected with an error naming the violated bound — a typo'd
// "11m" or a "0s" must never silently apply (same strictness rationale
// as LoadSettings' doc comment: an ignored setting is worse than a loud
// refusal).
func TestLoadSettingsMigratePreflightTimeoutValidation(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, content string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	cases := []struct{ name, content, wantErrSubstr string }{
		{"zero", "[migrate]\npreflight_timeout = \"0s\"\n", "greater than 0"},
		{"negative", "[migrate]\npreflight_timeout = \"-1s\"\n", "greater than 0"},
		{"over ceiling", "[migrate]\npreflight_timeout = \"11m\"\n", "10m0s ceiling"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.LoadSettings(write(t, tc.content))
			if err == nil {
				t.Fatalf("LoadSettings() accepted %s; want error", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Fatalf("LoadSettings() error = %q, want it to contain %q (bounds must be named)", err, tc.wantErrSubstr)
			}
		})
	}
}

// TestLoadSettingsProviderOverridesRoundTrip pins the [providers.codex]
// override shape (spec §6: Codex's classification table is
// config-overridable so upstream format drift is absorbed without a
// release) — two classify rules round-trip through TOML unchanged.
func TestLoadSettingsProviderOverridesRoundTrip(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "config.toml")
	content := "[providers.codex]\n\n" +
		"[[providers.codex.classify]]\n" +
		"glob = \"extra/**\"\n" +
		"class = \"ignore\"\n\n" +
		"[[providers.codex.classify]]\n" +
		"glob = \"notes/*.md\"\n" +
		"class = \"fact\"\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := config.LoadSettings(p)
	if err != nil {
		t.Fatalf("LoadSettings() error = %v", err)
	}
	want := map[string]config.ProviderSettings{
		"codex": {
			Classify: []config.ClassifyRule{
				{Glob: "extra/**", Class: "ignore"},
				{Glob: "notes/*.md", Class: "fact"},
			},
		},
	}
	if diff := cmp.Diff(want, got.Providers); diff != "" {
		t.Fatalf("Providers mismatch (-want +got):\n%s", diff)
	}
}

// TestDefaultSettingsEditorDefaults pins editor.*'s documented default: no
// configured command (fall through to $VISUAL/$EDITOR) and in_terminal =
// true (suspend the TUI, like every other CLI editor integration surveyed
// for ADR 20 decision 2).
func TestDefaultSettingsEditorDefaults(t *testing.T) {
	t.Parallel()
	want := config.EditorSettings{Command: "", InTerminal: true}
	if diff := cmp.Diff(want, config.DefaultSettings().Editor); diff != "" {
		t.Fatalf("default editor settings mismatch (-want +got):\n%s", diff)
	}
}

// TestLoadSettingsEditorParses proves an [editor] table overrides the
// default exactly like [sync]/[migrate] do.
func TestLoadSettingsEditorParses(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "config.toml")
	content := "[editor]\ncommand = \"cursor --wait\"\nin_terminal = false\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := config.LoadSettings(p)
	if err != nil {
		t.Fatalf("LoadSettings() error = %v", err)
	}
	want := config.EditorSettings{Command: "cursor --wait", InTerminal: false}
	if diff := cmp.Diff(want, got.Editor); diff != "" {
		t.Fatalf("editor settings mismatch (-want +got):\n%s", diff)
	}
}

// TestLoadSettingsEditorRejectsUnknownKey proves [editor] gets the same
// strict-TOML treatment as every other section (ADR 17): a typo'd key is a
// load-time error, never a silently-ignored setting.
func TestLoadSettingsEditorRejectsUnknownKey(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte("[editor]\ncomand = \"vim\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadSettings(p); err == nil {
		t.Fatal("LoadSettings() accepted an unknown [editor] key; want error")
	}
}

// TestLoadSettingsProviderOverridesValidation pins that a bad classify
// rule is a strict load-time error (ADR 17) naming the offending rule —
// never a silently-ignored or silently-misclassifying override.
func TestLoadSettingsProviderOverridesValidation(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, content string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	cases := []struct{ name, content string }{
		{
			"bad class string",
			"[providers.codex]\n\n[[providers.codex.classify]]\nglob = \"memories/**\"\nclass = \"bogus\"\n",
		},
		{
			"bad glob",
			"[providers.codex]\n\n[[providers.codex.classify]]\nglob = \"bad glob\"\nclass = \"fact\"\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.LoadSettings(write(t, tc.content))
			if err == nil {
				t.Fatalf("LoadSettings() accepted %s; want error", tc.name)
			}
			if !strings.Contains(err.Error(), "providers.codex.classify[0]") {
				t.Fatalf("LoadSettings() error = %q, want it to name providers.codex.classify[0]", err)
			}
		})
	}
}
