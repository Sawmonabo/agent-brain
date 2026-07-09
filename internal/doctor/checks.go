package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/ghx"
	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/keys"
	"github.com/Sawmonabo/agent-brain/internal/repo"
	"github.com/Sawmonabo/agent-brain/internal/service"
)

// fixDoctorFix is the canned remediation text for anything --fix repairs.
const fixDoctorFix = "run `agent-brain doctor --fix`"

func checkSettings(_ context.Context, deps Deps) (CheckResult, bool) {
	const name = "settings"
	if deps.SettingsErr != nil {
		return CheckResult{
			Name: name, Status: StatusFail,
			Detail: deps.SettingsErr.Error(),
			Fix:    fmt.Sprintf("fix %s and retry", deps.Paths.SettingsFile()),
		}, true
	}
	// Report the EFFECTIVE cadence, not merely that the file parsed: a
	// config.toml that loads can still carry a floor-clamped or defaulted
	// value the user did not expect, and "it loads" would hide that. These
	// are the values the daemon actually runs on (config.LoadSettings has
	// already applied defaults and floors).
	sync := deps.Settings.Sync
	detail := fmt.Sprintf("config.toml loads (ticker %s, debounce %s, poll %s)",
		time.Duration(sync.Ticker), time.Duration(sync.Debounce), time.Duration(sync.Poll))
	if overridden := overriddenProviders(deps.Settings); len(overridden) > 0 {
		detail += fmt.Sprintf("; classification overridden for %s", strings.Join(overridden, ", "))
	}
	return CheckResult{Name: name, Status: StatusOK, Detail: detail}, true
}

// overriddenProviders names, in deterministic order, the providers whose
// classification tables config.toml overrides (spec §6). An override that
// silently misclassifies a fact as ignore stops that file syncing, so
// doctor states plainly which tables are no longer stock.
func overriddenProviders(settings config.Settings) []string {
	var names []string
	for provider, providerSettings := range settings.Providers {
		if len(providerSettings.Classify) > 0 {
			names = append(names, provider)
		}
	}
	slices.Sort(names)
	return names
}

// checkGitMeta reports git-meta poison RESIDENT in the checkout below its
// root — a `<folder>/.gitattributes` carrying `* -filter` unselects the
// encryption clean filter for that subtree (see repo.IsGitMetaPath).
//
// ADVISORY BY CONTRACT, and it must NEVER join SafetyGate. The engine heals
// this automatically: every commit-creating entry point scrubs the checkout
// first (engine.prepareCheckout). SafetyGate is what the daemon evaluates
// BEFORE each cycle, so a failing git-meta gate would refuse the very sync
// whose scrub removes the poison — a deadlock that would strand the repo in
// exactly the state the check exists to flag. Warn, name the paths, and let
// the next cycle fix it.
//
// The root .gitattributes is legitimate (generated); checkAttributes already
// verifies its content byte-canonically. `.git` is skipped: it is the
// checkout's own git dir, not repo content.
func checkGitMeta(_ context.Context, deps Deps) (CheckResult, bool) {
	const name = "git-meta"
	root := deps.Paths.MemoriesDir()
	if _, err := os.Stat(root); err != nil {
		return CheckResult{}, false // no checkout yet — checkCheckout owns that
	}
	var found []string
	err := filepath.WalkDir(root, func(abs string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(root, abs)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		switch {
		case rel == ".":
			return nil
		case rel == ".git":
			return filepath.SkipDir
		case rel == ".gitattributes":
			return nil
		case !repo.IsGitMetaPath(rel):
			return nil
		}
		found = append(found, rel)
		if entry.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return CheckResult{
			Name: name, Status: StatusWarn,
			Detail: fmt.Sprintf("cannot scan %s for git-meta: %s", root, err),
		}, true
	}
	if len(found) > 0 {
		return CheckResult{
			Name: name, Status: StatusWarn,
			Detail: fmt.Sprintf("git-meta resident in the checkout (encryption filter can be unscoped for its subtree): %s", strings.Join(found, ", ")),
			Fix:    "the next sync cycle removes it; force one with `agent-brain sync`",
		}, true
	}
	return CheckResult{Name: name, Status: StatusOK, Detail: "no git-meta below the checkout root"}, true
}

func checkKeyset(_ context.Context, deps Deps) (CheckResult, bool) {
	const name = "keyset"
	path := deps.Paths.Keyset()
	if _, err := os.Stat(path); err != nil {
		return CheckResult{
			Name: name, Status: StatusFail,
			Detail: fmt.Sprintf("keyset missing at %s", path),
			Fix:    "run `agent-brain init` (first machine) or `agent-brain key import` (joining an existing keyset)",
		}, true
	}
	if _, err := keys.Primitive(path); err != nil {
		return CheckResult{
			Name: name, Status: StatusFail,
			Detail: fmt.Sprintf("keyset at %s does not load: %s", path, err),
			Fix:    "restore the keyset from a `key export` backup, or `key import` a working one",
		}, true
	}
	return CheckResult{Name: name, Status: StatusOK, Detail: "keyset loads"}, true
}

func checkCheckout(ctx context.Context, deps Deps) (CheckResult, bool) {
	const name = "checkout"
	dir := deps.Paths.MemoriesDir()
	result, err := gitx.RunStatus(ctx, dir, "rev-parse", "--is-inside-work-tree")
	if err != nil || result.ExitCode != 0 || strings.TrimSpace(result.Stdout) != "true" {
		return CheckResult{
			Name: name, Status: StatusFail,
			Detail: fmt.Sprintf("%s is not a git work tree", dir),
			Fix:    "run `agent-brain init`",
		}, true
	}
	return CheckResult{Name: name, Status: StatusOK, Detail: dir + " is a git work tree"}, true
}

// filterConfigKeys are exactly the local git-config keys gitx.InstallFilters
// (internal/gitx/install.go) writes, besides clean/required which get their
// own value assertions below. Kept as this hand-matched literal rather than
// an exported symbol so a future edit to either site forces a look at the
// other. Presence-only: InstallFilters writes all nine atomically in one
// pass, so a present-but-wrong entry among these seven never happens
// without clean/required ALSO drifting.
var filterConfigKeys = []string{
	"filter.agentbrain.smudge",
	"diff.agentbrain.textconv",
	"merge.agentbrain.name",
	"merge.agentbrain.driver",
	"merge.agentbrain-lww.name",
	"merge.agentbrain-lww.driver",
	"merge.renormalize",
}

// gitConfigLocalGet reads one --local git-config key, reporting ok=false
// for both "not set" and any harder failure (e.g. dir is not a git repo)
// — callers treat every such case identically as "not wired".
func gitConfigLocalGet(ctx context.Context, dir, key string) (string, bool) {
	result, err := gitx.RunStatus(ctx, dir, "config", "--local", "--get", key)
	if err != nil || result.ExitCode != 0 {
		return "", false
	}
	return strings.TrimSpace(result.Stdout), true
}

// localConfig reads this repo's entire --local git config in ONE
// subprocess (-z: NUL-delimited "key\nvalue" records, safe against a value
// containing '=' or a newline) instead of one spawn per key. checkFilters
// is part of SafetyGate, which runs before every daemon cycle (spec §5) —
// a per-key subprocess spawn there is the difference between a gate that's
// genuinely cheap and one that measurably slows every cycle. Absent keys
// simply miss the map, matching gitConfigLocalGet's ok=false-on-unset
// contract; ok=false here means dir has no local config at all (not a git
// repo, or git could not run).
func localConfig(ctx context.Context, dir string) (map[string]string, bool) {
	result, err := gitx.RunStatus(ctx, dir, "config", "--local", "--list", "-z")
	if err != nil || result.ExitCode != 0 {
		return nil, false
	}
	entries := strings.Split(strings.TrimSuffix(result.Stdout, "\x00"), "\x00")
	config := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry == "" {
			continue
		}
		key, value, _ := strings.Cut(entry, "\n")
		config[key] = value
	}
	return config, true
}

func checkFilters(ctx context.Context, deps Deps) (CheckResult, bool) {
	const name = "filters"
	// strings.Contains(x, "") is always true — without this guard an empty
	// BinaryPath would make the clean-filter comparison below vacuously
	// pass regardless of what filter.agentbrain.clean actually holds.
	// Never empty via daemon/CLI (both resolve a real path before building
	// Deps), but Deps/SafetyGate are exported, so a caller that forgets to
	// set BinaryPath must get a named failure, not a silent pass (Q3 gate
	// finding M4).
	if deps.BinaryPath == "" {
		return CheckResult{Name: name, Status: StatusFail, Detail: "BinaryPath is empty — cannot verify filter.agentbrain.clean points at a real binary"}, true
	}
	dir := deps.Paths.MemoriesDir()

	config, ok := localConfig(ctx, dir)
	if !ok || !strings.Contains(config["filter.agentbrain.clean"], deps.BinaryPath) {
		return CheckResult{Name: name, Status: StatusFail, Detail: "filter.agentbrain.clean is missing or does not point at this binary", Fix: fixDoctorFix}, true
	}
	if config["filter.agentbrain.required"] != "true" {
		return CheckResult{Name: name, Status: StatusFail, Detail: "filter.agentbrain.required is not true — encryption is not fail-closed", Fix: fixDoctorFix}, true
	}
	for _, key := range filterConfigKeys {
		if config[key] == "" {
			return CheckResult{Name: name, Status: StatusFail, Detail: fmt.Sprintf("%s is not wired", key), Fix: fixDoctorFix}, true
		}
	}
	return CheckResult{Name: name, Status: StatusOK, Detail: "filter/merge/textconv wiring intact"}, true
}

func checkAttributes(_ context.Context, deps Deps) (CheckResult, bool) {
	const name = "attributes"
	if deps.Registry == nil {
		return CheckResult{Name: name, Status: StatusFail, Detail: "no provider registry configured"}, true
	}
	path := repo.NewLayout(deps.Paths.MemoriesDir()).AttributesFile()
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the program-derived checkout attributes location (config.Paths -> repo.Layout), not untrusted input
	if err != nil {
		return CheckResult{Name: name, Status: StatusFail, Detail: fmt.Sprintf("cannot read %s: %s", path, err), Fix: fixDoctorFix}, true
	}
	if string(data) != repo.GenerateAttributes(deps.Registry) {
		return CheckResult{Name: name, Status: StatusFail, Detail: path + " does not match the canonical generated content", Fix: fixDoctorFix}, true
	}
	return CheckResult{Name: name, Status: StatusOK, Detail: ".gitattributes matches canonical content"}, true
}

// checkCredentialHelper only applies over an https remote (spec §5/ADR
// 08): ssh remotes never invoke a credential helper, so there is nothing
// to check when the remote is ssh or unset.
func checkCredentialHelper(ctx context.Context, deps Deps) (CheckResult, bool) {
	const name = "credential-helper"
	dir := deps.Paths.MemoriesDir()
	remoteURL, ok := gitConfigLocalGet(ctx, dir, "remote.origin.url")
	if !ok || !strings.HasPrefix(remoteURL, "https://") {
		return CheckResult{}, false
	}
	result, err := gitx.RunStatus(ctx, dir, "config", "--local", "--get-all", "credential.helper")
	if err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, "auth git-credential") {
		return CheckResult{Name: name, Status: StatusFail, Detail: "credential.helper is not wired to gh — HTTPS pushes will fail to authenticate", Fix: fixDoctorFix}, true
	}
	return CheckResult{Name: name, Status: StatusOK, Detail: "credential.helper wired to gh"}, true
}

// remoteCheckTimeout bounds the ls-remote probe so a hung or slow origin
// cannot stall the whole battery.
const remoteCheckTimeout = 5 * time.Second

func checkRemote(ctx context.Context, deps Deps) (CheckResult, bool) {
	if deps.Offline {
		return CheckResult{}, false
	}
	const name = "remote"
	timeoutCtx, cancel := context.WithTimeout(ctx, remoteCheckTimeout)
	defer cancel()
	result, err := gitx.RunStatus(timeoutCtx, deps.Paths.MemoriesDir(), "ls-remote", "--exit-code", "origin", "HEAD")
	if err != nil || result.ExitCode != 0 {
		return CheckResult{Name: name, Status: StatusFail, Detail: "origin is unreachable — commits will queue locally", Fix: "check network connectivity and `gh auth status`"}, true
	}
	return CheckResult{Name: name, Status: StatusOK, Detail: "origin is reachable"}, true
}

func checkGH(ctx context.Context, deps Deps) (CheckResult, bool) {
	const name = "gh"
	if deps.GH == nil {
		return CheckResult{Name: name, Status: StatusFail, Detail: ghx.ErrMissing.Error(), Fix: "install gh (https://cli.github.com) and run `gh auth login`"}, true
	}
	if err := deps.GH.AuthOK(ctx); err != nil {
		return CheckResult{Name: name, Status: StatusFail, Detail: err.Error(), Fix: "run `gh auth login`"}, true
	}
	return CheckResult{Name: name, Status: StatusOK, Detail: "gh installed and authenticated"}, true
}

func checkDaemon(ctx context.Context, deps Deps) (CheckResult, bool) {
	if deps.DaemonPing == nil {
		return CheckResult{}, false
	}
	const name = "daemon"
	if err := deps.DaemonPing(ctx); err != nil {
		return CheckResult{Name: name, Status: StatusFail, Detail: err.Error(), Fix: "run `agent-brain service start` or `agent-brain daemon run`"}, true
	}
	return CheckResult{Name: name, Status: StatusOK, Detail: "daemon responds"}, true
}

// checkService never fails: a stopped or not-installed login service is
// only a warning because a foreground `agent-brain daemon run` is an
// equally legitimate way to run this program (spec §7).
func checkService(_ context.Context, deps Deps) (CheckResult, bool) {
	const name = "service"
	controller, err := service.NewController(deps.BinaryPath)
	if err != nil {
		return CheckResult{Name: name, Status: StatusWarn, Detail: err.Error()}, true
	}
	status, err := controller.Status()
	if err != nil {
		return CheckResult{Name: name, Status: StatusWarn, Detail: err.Error()}, true
	}
	switch status {
	case service.StatusRunning:
		return CheckResult{Name: name, Status: StatusOK, Detail: "service running"}, true
	case service.StatusNotInstalled:
		return CheckResult{Name: name, Status: StatusWarn, Detail: "login service not installed — foreground `agent-brain daemon run` also works", Fix: "run `agent-brain service install`"}, true
	default:
		return CheckResult{Name: name, Status: StatusWarn, Detail: "service " + status.String(), Fix: "run `agent-brain service start`"}, true
	}
}

func checkRegistryLocal(_ context.Context, deps Deps) (CheckResult, bool) {
	const name = "registry-local"
	if _, err := repo.LoadLocalRegistry(deps.Paths.LocalRegistryFile()); err != nil {
		return CheckResult{
			Name: name, Status: StatusFail, Detail: err.Error(),
			Fix: "hand-fix or remove " + deps.Paths.LocalRegistryFile() + " (loses local enrollment; re-track afterward)",
		}, true
	}
	return CheckResult{Name: name, Status: StatusOK, Detail: "local registry loads"}, true
}

// conflictLogWarnBytes is doctor's own early-warning threshold — smaller
// than the daemon's 5 MiB rotation bound (internal/daemon/logging.go's
// maxConflictLogSize, unreachable from here by the doctor->daemon import
// boundary), deliberately, so `doctor` flags growth before rotation ever
// triggers.
const conflictLogWarnBytes = 4 << 20

func checkConflictLog(_ context.Context, deps Deps) (CheckResult, bool) {
	const name = "conflict-log"
	info, err := os.Stat(deps.Paths.ConflictLogFile())
	if err != nil {
		return CheckResult{Name: name, Status: StatusOK, Detail: "no conflict log yet"}, true
	}
	if info.Size() > conflictLogWarnBytes {
		return CheckResult{Name: name, Status: StatusWarn, Detail: fmt.Sprintf("conflict log is %d bytes, approaching rotation bound", info.Size())}, true
	}
	return CheckResult{Name: name, Status: StatusOK, Detail: "conflict log size nominal"}, true
}

// claudeSettingsFile is the narrow slice of ~/.claude/settings.json this
// check reads. AutoMemoryEnabled is a pointer so "absent" (default true,
// per spec §6) is distinguishable from an explicit false.
type claudeSettingsFile struct {
	AutoMemoryEnabled *bool `json:"autoMemoryEnabled"`
}

func checkClaudePrereqs(_ context.Context, deps Deps) (CheckResult, bool) {
	const name = "claude-prereqs"
	var warnings []string
	if os.Getenv("CLAUDE_CODE_DISABLE_AUTO_MEMORY") != "" {
		warnings = append(warnings, "CLAUDE_CODE_DISABLE_AUTO_MEMORY is set")
	}
	settingsPath := filepath.Join(deps.Home, ".claude", "settings.json")
	if data, err := os.ReadFile(settingsPath); err == nil { //nolint:gosec // G304: settingsPath is derived from deps.Home (program-resolved), not untrusted input
		var settings claudeSettingsFile
		if json.Unmarshal(data, &settings) == nil && settings.AutoMemoryEnabled != nil && !*settings.AutoMemoryEnabled {
			warnings = append(warnings, settingsPath+` sets "autoMemoryEnabled": false`)
		}
	}
	if len(warnings) > 0 {
		return CheckResult{
			Name: name, Status: StatusWarn, Detail: strings.Join(warnings, "; "),
			Fix: "unset CLAUDE_CODE_DISABLE_AUTO_MEMORY and/or remove autoMemoryEnabled: false",
		}, true
	}
	return CheckResult{Name: name, Status: StatusOK, Detail: "Claude Code auto-memory prerequisites satisfied"}, true
}

// checkCodexPrereqs only applies once a codex unit is actually enrolled
// (spec: Codex ships experimental, ADR 02) — an unused adapter's
// prerequisites are not this machine's problem.
func checkCodexPrereqs(_ context.Context, deps Deps) (CheckResult, bool) {
	enrolled := false
	for _, unit := range deps.Enrolled {
		if unit.Provider == "codex" {
			enrolled = true
			break
		}
	}
	if !enrolled {
		return CheckResult{}, false
	}
	const name = "codex-prereqs"
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(deps.Home, ".codex")
	}
	configPath := filepath.Join(codexHome, "config.toml")
	fix := `set "features.memories = true" in ` + configPath

	data, err := os.ReadFile(configPath) //nolint:gosec // G304: configPath is derived from $CODEX_HOME/deps.Home (program-resolved), not untrusted input
	if err != nil {
		return CheckResult{Name: name, Status: StatusWarn, Detail: fmt.Sprintf("cannot read %s: %s", configPath, err), Fix: fix}, true
	}
	var parsed struct {
		Features struct {
			Memories bool `toml:"memories"`
		} `toml:"features"`
	}
	if err := toml.Unmarshal(data, &parsed); err != nil || !parsed.Features.Memories {
		return CheckResult{Name: name, Status: StatusWarn, Detail: configPath + " does not set features.memories = true", Fix: fix}, true
	}
	return CheckResult{Name: name, Status: StatusOK, Detail: "codex memories feature enabled"}, true
}

// legacyCandidates are the exact bash-era leftovers spec §10's retirement
// checklist names.
func legacyCandidates(home string) []string {
	return []string{
		filepath.Join(home, ".local", "bin", "ab-claude"),
		filepath.Join(home, ".agent-brain"),
		filepath.Join(home, ".config", "agent-brain", "chezmoi.toml"),
	}
}

func checkLegacyLeftovers(_ context.Context, deps Deps) (CheckResult, bool) {
	const name = "legacy-leftovers"
	var found []string
	for _, path := range legacyCandidates(deps.Home) {
		if _, err := os.Lstat(path); err == nil {
			found = append(found, path)
		}
	}
	if len(found) > 0 {
		return CheckResult{
			Name: name, Status: StatusWarn,
			Detail: "bash-era system still present — see spec §10 retirement checklist: " + strings.Join(found, ", "),
		}, true
	}
	return CheckResult{Name: name, Status: StatusOK, Detail: "no bash-era leftovers found"}, true
}
