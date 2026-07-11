package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/crypto"
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
	// Reachability only — deliberately NOT `--exit-code HEAD`: a remote whose
	// HEAD symref is unborn or dangling (a brand-new empty repo, or a bare
	// whose default branch never matched the pushed `main`) advertises no
	// HEAD and would exit 2 under --exit-code, indistinguishable from a dead
	// network. Reaching the remote at all is what this row reports; whether
	// our branch exists there is the sync engine's business.
	result, err := gitx.RunStatus(timeoutCtx, deps.Paths.MemoriesDir(), "ls-remote", "origin", "HEAD")
	if err != nil || result.ExitCode != 0 {
		detail := "origin is unreachable — commits will queue locally"
		// Carry git's own first stderr line (or the spawn/timeout error):
		// "unreachable" alone has already cost a debugging session that a
		// `Could not read from remote repository` suffix would have ended.
		if err != nil {
			detail += ": " + firstLine(err.Error())
		} else if s := strings.TrimSpace(result.Stderr); s != "" {
			detail += ": " + firstLine(s)
		}
		return CheckResult{Name: name, Status: StatusFail, Detail: detail, Fix: "check network connectivity and `gh auth status`"}, true
	}
	return CheckResult{Name: name, Status: StatusOK, Detail: "origin is reachable"}, true
}

// firstLine trims a potentially multi-line message to its first line, so a
// check Detail stays a single table row.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
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

// checkProjectIdentity verifies the cross-machine linchpin (spec §3): every
// per-project unit this machine enrolled must still be the project the
// SHARED registry (.agent-brain/projects.toml in the memories checkout) maps
// its folder to. The drift is real: machine A `untrack --purge`s folder F
// (the last tracker deletes the registry row), machine C later tracks a
// DIFFERENT project whose preferred folder lands back on F — a stale machine
// still enrolled under F would from then on mirror its memories into a
// folder the fleet has reassigned. Unit.ProjectID is recorded at Track
// exactly so this comparison needs no lossy re-derivation (slug reversal)
// and no network: local enrollment vs the checkout's registry file.
//
// Advisory (StatusWarn), deliberately NOT in SafetyGate: the gate's
// membership rule (gate.go) blocks the WHOLE fleet's cycles, and one
// drifted folder does not make the other units' cycles unsafe. Per-unit
// engine withholding on drift is recorded follow-up work in
// docs/plans/backlog-project-identity-engine-guard.md, not silently skipped.
func checkProjectIdentity(_ context.Context, deps Deps) (CheckResult, bool) {
	const name = "project-identity"
	perProject := make([]repo.Unit, 0, len(deps.Enrolled))
	for _, unit := range deps.Enrolled {
		if unit.ProjectID != "" { // global-scope units carry no project identity
			perProject = append(perProject, unit)
		}
	}
	if len(perProject) == 0 {
		return CheckResult{}, false
	}
	projects, err := repo.LoadProjects(repo.NewLayout(deps.Paths.MemoriesDir()).ProjectsFile())
	if err != nil {
		return CheckResult{
			Name: name, Status: StatusWarn,
			Detail: "cannot read the shared project registry: " + err.Error(),
			Fix:    "run `agent-brain sync`, then `agent-brain doctor` again",
		}, true
	}
	var drifted []string
	for _, unit := range perProject {
		entry, ok := projects.Entries[unit.Folder]
		switch {
		case !ok:
			drifted = append(drifted, fmt.Sprintf("%s (%s): folder missing from the shared registry", unit.Folder, unit.Provider))
		case entry.ID != unit.ProjectID:
			drifted = append(drifted, fmt.Sprintf("%s (%s): registry maps it to %q, this machine enrolled %q — mirroring crosses projects until re-tracked",
				unit.Folder, unit.Provider, entry.ID, unit.ProjectID))
		}
	}
	if len(drifted) > 0 {
		slices.Sort(drifted)
		return CheckResult{
			Name: name, Status: StatusWarn,
			Detail: "project identity drift: " + strings.Join(drifted, "; "),
			Fix:    "untrack the listed folders and re-track their local dirs (`agent-brain untrack <folder>`, then `agent-brain track <path>`)",
		}, true
	}
	return CheckResult{
		Name: name, Status: StatusOK,
		Detail: fmt.Sprintf("%d enrolled folder(s) match the shared registry", len(perProject)),
	}, true
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

// checkSecretsScan is ADVISORY ONLY — ok or warn, NEVER fail — and it must
// NEVER join SafetyGate (gate.go's membership rule: a check belongs there
// only if a cycle cannot safely run while it fails AND running a cycle
// cannot repair it; neither holds here, since gitleaks is an opt-in,
// user-installed external tool that only `agent-brain scan` (internal/cli/
// scan.go) shells out to, on demand — its absence has no bearing on
// whether a sync cycle is safe to run). This just reports presence, the
// same shape as checkGH but without checkGH's hard-fail severity: gh is a
// v1 hard requirement (ADR 08), gitleaks is not.
func checkSecretsScan(_ context.Context, _ Deps) (CheckResult, bool) {
	const name = "secrets-scan"
	path, err := exec.LookPath("gitleaks")
	if err != nil {
		return CheckResult{
			Name: name, Status: StatusWarn,
			Detail: "gitleaks not installed — `agent-brain scan` cannot run",
			Fix:    "install gitleaks (`brew install gitleaks` on macOS; see https://github.com/gitleaks/gitleaks#installing otherwise)",
		}, true
	}
	return CheckResult{Name: name, Status: StatusOK, Detail: "gitleaks installed at " + path}, true
}

// probeUpstreamRef is the fetched remote-tracking branch checkKeysetDecrypt
// prefers as its sample source, ahead of HEAD.
//
// The obvious design reads "reachable from HEAD" literally and samples
// HEAD alone. That fails the exact scenario this check exists for:
// engine/integrate.go's Integrate is all-or-nothing (its own doc comment:
// "Integrated means HEAD now contains origin/main; otherwise the checkout
// is back at its pre-integrate state") — when a stale keyset makes a
// smudge fail mid-cycle, the engine aborts the rebase/merge and restores
// HEAD to its last-good commit. But `git fetch` runs BEFORE that
// rebase/merge attempt and always completes, so origin/main keeps
// advancing every cycle while HEAD stays frozen at the last commit this
// machine could still read. A HEAD-only probe would therefore keep
// re-decrypting content it already proved it could decrypt, and this
// check would report OK forever on the one machine it exists to warn.
// Sampling the fetched tracking ref first — falling back to HEAD only
// when there is no remote-tracking ref yet (e.g. immediately after
// `init`, before any fetch) — is what makes the check actually see the
// content the stale keyset cannot open. Confirmed empirically: see
// test/e2e/rotate_test.go's Task 4.5 assertion, which reproduces this
// exact freeze/advance split with a real second keyset and a real fetch.
const probeUpstreamRef = "refs/remotes/origin/main"

// checkKeysetDecrypt is an ADVISORY probe closing the operator-guidance
// gap key rotation (Task 4) exposed: checkKeyset only loads the keyset
// file, which still succeeds for a keyset that is stale relative to this
// repo (rotated elsewhere, never imported here) — so without this check,
// every sync on that machine keeps degrading (per-file fail-closed,
// spec §5) while doctor reports all-OK. This samples exactly one
// ciphertext blob — the newest one reachable from probeUpstreamRef (or
// HEAD, see its doc comment) — and attempts to decrypt it.
//
// StatusInfo, never StatusWarn, on an empty state (no checkout, unborn
// HEAD, or a checkout with zero encrypted blobs so far): those are
// ordinary, healthy points in a machine's lifecycle (freshly initialized,
// or enrolled but nothing written yet), not evidence of anything wrong.
//
// Must NEVER join SafetyGate (gate.go): unlike a broken filter or missing
// keyset file, a stale keyset does not make a cycle unsafe to attempt —
// the clean/smudge filters already fail closed per file (spec §5), and
// the fix is a human decision (`key export` / `key import --force`), not
// something a cycle can repair by running. Gating on it would only block
// syncs that are already degrading gracefully, without helping them heal.
func checkKeysetDecrypt(ctx context.Context, deps Deps) (CheckResult, bool) {
	const name = "keyset-decrypt"
	primitive, err := keys.Primitive(deps.Paths.Keyset())
	if err != nil {
		return CheckResult{}, false // checkKeyset already reports this loudly
	}
	dir := deps.Paths.MemoriesDir()
	rev, ok := resolveProbeRev(ctx, dir)
	if !ok {
		return CheckResult{Name: name, Status: StatusInfo, Detail: "no checkout to probe yet"}, true
	}
	path, blob, ok := newestEncryptedBlob(ctx, dir, rev)
	if !ok {
		return CheckResult{Name: name, Status: StatusInfo, Detail: "no encrypted content in the checkout yet — nothing to probe"}, true
	}
	if _, err := crypto.NewCodec(primitive).Decrypt(blob); err != nil {
		return CheckResult{
			Name: name, Status: StatusWarn,
			Detail: fmt.Sprintf("keyset cannot decrypt %s — it is stale relative to this repo (a fleet key rotation this machine has not imported)", path),
			Fix:    "on the machine holding the current key, run `agent-brain key export`; on this machine, run `agent-brain key import --force` with that output",
		}, true
	}
	return CheckResult{Name: name, Status: StatusOK, Detail: "keyset decrypts the newest sampled memory blob"}, true
}

// resolveProbeRev picks probeUpstreamRef when it exists, falling back to
// HEAD (e.g. right after `init`, before any fetch has ever run) — see
// probeUpstreamRef's doc comment for why the tracking ref is tried first.
// ok=false means neither resolves: no checkout, or an unborn HEAD.
func resolveProbeRev(ctx context.Context, dir string) (rev string, ok bool) {
	if result, err := gitx.RunStatus(ctx, dir, "rev-parse", "--verify", "-q", probeUpstreamRef); err == nil && result.ExitCode == 0 {
		return probeUpstreamRef, true
	}
	if result, err := gitx.RunStatus(ctx, dir, "rev-parse", "--verify", "-q", "HEAD"); err == nil && result.ExitCode == 0 {
		return "HEAD", true
	}
	return "", false
}

// newestEncryptedBlob returns the path and raw stored bytes (i.e. read via
// plumbing, bypassing the smudge filter) of one encrypted blob at rev —
// the check needs exactly one sample, not a repo walk. It tries the tip
// commit's own changed paths first (cheap, and the most likely to reflect
// whatever this fleet last touched); a tip commit that changes nothing
// encrypted — a manifest-only sync commit, or a merge commit, where
// diff-tree without -m shows nothing — falls back to a full tree listing
// at rev. ok=false means rev has no encrypted content at all yet.
func newestEncryptedBlob(ctx context.Context, dir, rev string) (path string, blob []byte, ok bool) {
	if path, blob, ok := firstEncrypted(ctx, dir, rev, tipCommitPaths(ctx, dir, rev)); ok {
		return path, blob, true
	}
	return firstEncrypted(ctx, dir, rev, trackedPaths(ctx, dir, rev))
}

// tipCommitPaths lists the paths rev's own commit changed, relative to its
// parent(s) (--root handles rev being the repo's first commit). Empty
// (rather than an error) for a merge commit, since diff-tree without -m
// shows no diff for one — newestEncryptedBlob's tree-listing fallback
// covers that case.
func tipCommitPaths(ctx context.Context, dir, rev string) []string {
	result, err := gitx.RunStatus(ctx, dir, "diff-tree", "--no-commit-id", "--name-only", "-r", "--root", "-z", rev)
	if err != nil || result.ExitCode != 0 {
		return nil
	}
	return splitNulPaths(result.Stdout)
}

// trackedPaths lists every path in rev's full tree — the fallback source
// when the tip commit alone yields no encrypted blob.
func trackedPaths(ctx context.Context, dir, rev string) []string {
	result, err := gitx.RunStatus(ctx, dir, "ls-tree", "-r", "--name-only", "-z", rev)
	if err != nil || result.ExitCode != 0 {
		return nil
	}
	return splitNulPaths(result.Stdout)
}

// splitNulPaths splits a -z-delimited git plumbing path list.
func splitNulPaths(output string) []string {
	var paths []string
	for path := range strings.SplitSeq(strings.TrimSuffix(output, "\x00"), "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

// firstEncrypted reads each candidate path's raw stored bytes AT rev via
// `cat-file blob` — plumbing, so this bypasses the smudge filter entirely
// and sees exactly what git has stored, encrypted or not — returning the
// first one whose content carries the encryption magic.
func firstEncrypted(ctx context.Context, dir, rev string, candidates []string) (path string, blob []byte, ok bool) {
	for _, candidate := range candidates {
		result, err := gitx.RunStatus(ctx, dir, "cat-file", "blob", rev+":"+candidate)
		if err != nil || result.ExitCode != 0 {
			continue
		}
		data := []byte(result.Stdout)
		if crypto.IsEncrypted(data) {
			return candidate, data, true
		}
	}
	return "", nil, false
}
