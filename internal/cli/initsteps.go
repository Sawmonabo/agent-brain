package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/renameio/v2"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/ghx"
	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/keys"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/repo"
	"github.com/Sawmonabo/agent-brain/internal/service"
)

// createRepoDescription is the literal description gh repo create attaches
// to a freshly created agent-brain-memories repo — it points back at the
// tool that created it, not at the memories repo itself.
const createRepoDescription = "agent-brain encrypted memories (github.com/Sawmonabo/agent-brain)"

// initState threads every value one init step resolves to the steps
// that follow it. Steps never touch stdin/a TTY or construct huh forms
// directly (init.go owns all of that) — everything a step needs to make
// a decision is either a plain field set by an earlier step/by init.go
// from flags, or one of the three enrollment callback fields init.go
// wires to either a huh-backed closure (interactive) or a trivial
// flag-driven one (--enroll all/none). That split is what keeps every
// step directly unit-testable with no TTY and no huh import here.
type initState struct {
	out io.Writer

	nonInteractive bool
	repoName       string
	skipService    bool
	enrollMode     string // "all" | "none" | "" (interactive picker)

	generateKey   bool
	importKey     bool
	importArmored string

	daemonPollTimeout  time.Duration
	daemonPollInterval time.Duration

	// step 1 (identity)
	paths      config.Paths
	settings   config.Settings
	registry   *provider.Registry
	binaryPath string
	home       string

	// step 2 (gh)
	gh    *ghx.Client
	login string

	// steps 9/10 (enrollment, first sync): resolved lazily by
	// ensureDaemonClient (which also sets daemonResolved so a step that
	// already timed out waiting never polls again later in the same
	// run), or pre-set directly by a test/orchestrator — apiClient alone
	// (leaving daemonResolved false) is the test seam for "use this fake
	// client, never poll".
	apiClient      *api.Client
	daemonResolved bool

	// step 9 (enrollment) human-interaction seams. init.go always wires
	// all three, whether interactively (huh) or from flags/const
	// answers (--non-interactive, --enroll all/none); tests supply
	// their own stubs directly, bypassing huh and cobra entirely.
	pickEnrollUnits      func(candidates []enrollCandidate) ([]int, error)
	confirmProjectPath   func(guess string) (string, error)
	nameRemotelessFolder func(hint string) (string, error)

	// enrolledAny is set by stepEnrollment the first time a Track call
	// actually succeeds — stepFirstSync reads it to decide whether
	// there's anything worth syncing at all (an idle daemon has nothing
	// to report).
	enrolledAny bool

	// keysetGenerated is set by stepKeyset only when it took the
	// fresh-generate branch this run — init.go's interactive
	// password-manager confirm gate fires only then, never for an
	// already-present (validated) or freshly-imported keyset.
	keysetGenerated bool
}

// stepIdentity resolves this machine's paths, settings, provider
// registry, and the absolute binary path everything else in init wires
// into git config. binaryPath prefers AGENT_BRAIN_TEST_BINARY_PATH
// (testBinaryPathEnv, doctor.go) over resolveBinary()'s real
// os.Executable() — the same seam buildDoctorDeps uses, and for the
// identical reason: inside a test process os.Executable() is the
// compiled cli.test binary, and wiring a git filter at it re-invokes
// the whole suite as a "clean"/"smudge" driver, recursing without bound
// (see testBinaryPath's doc comment, testmain_test.go).
func stepIdentity(_ context.Context, state *initState) error {
	binaryPath := os.Getenv(testBinaryPathEnv)
	if binaryPath == "" {
		resolved, err := resolveBinary()
		if err != nil {
			return err
		}
		binaryPath = resolved
	}
	state.binaryPath = binaryPath

	paths, err := config.DefaultPaths()
	if err != nil {
		return err
	}
	state.paths = paths

	settings, err := config.LoadSettings(paths.SettingsFile())
	if err != nil {
		return err
	}
	state.settings = settings

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	state.home = home

	registry, err := buildRegistry(settings, home)
	if err != nil {
		return err
	}
	state.registry = registry

	_, err = fmt.Fprintf(state.out, "identity: binary %s, config %s, data %s\n", state.binaryPath, state.paths.ConfigDir, state.paths.DataDir)
	return err
}

// stepGH confirms gh is installed and authenticated, then resolves the
// logged-in user's login — every later step that talks to GitHub
// (repo exists/create/clone) needs it. state.gh is pre-resolved by
// init.go (production: ghx.NewClient(); tests: a fake Runner) so this
// step never touches exec.LookPath itself.
func stepGH(ctx context.Context, state *initState) error {
	if err := state.gh.AuthOK(ctx); err != nil {
		return err
	}
	login, err := state.gh.Login(ctx)
	if err != nil {
		return err
	}
	state.login = login
	_, err = fmt.Fprintf(state.out, "gh: authenticated as %s\n", login)
	return err
}

// stepKeyset validates an existing keyset, or generates/imports a fresh
// one per state.generateKey/state.importKey/state.importArmored — all
// pre-resolved by init.go (flags, or a huh decision made before this
// step runs) so this step never prompts. The confirm-gate loop
// ("did you save it?") is intentionally NOT here: it is an interactive
// huh.Confirm and lives in init.go, run only when this step actually
// generated a fresh keyset — the export text and password-manager
// reminder this step prints are what that gate re-displays/confirms
// against.
func stepKeyset(_ context.Context, state *initState) error {
	path := state.paths.Keyset()
	if _, err := os.Stat(path); err == nil {
		if _, err := keys.Primitive(path); err != nil {
			return fmt.Errorf("keyset at %s is invalid: %w", path, err)
		}
		_, err = fmt.Fprintf(state.out, "keyset: found at %s, validated\n", path)
		return err
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	switch {
	case state.generateKey:
		if err := keys.Generate(path); err != nil {
			return err
		}
		armored, err := keys.Export(path)
		if err != nil {
			return err
		}
		report := &reportWriter{w: state.out}
		report.println(armored)
		report.println("This armored keyset IS the recovery artifact — store a copy in your password manager now.")
		state.keysetGenerated = true
		return report.err
	case state.importKey:
		if err := keys.Import(path, state.importArmored); err != nil {
			return err
		}
		_, err := fmt.Fprintf(state.out, "keyset: imported to %s\n", path)
		return err
	default:
		return fmt.Errorf("keyset missing at %s: pass --generate-key or --import-key (or run `agent-brain init` interactively)", path)
	}
}

// stepRepo ensures the memories checkout exists locally, either verifying
// an already-present one or provisioning it from scratch. An existing
// checkout at state.paths.MemoriesDir() is trusted only if its origin
// canonicalizes (provider.NormalizeRemoteURL) to the SAME project as
// login/repoName — anything else is refused rather than silently
// overwritten or (worse) mixed with foreign history, and the error names
// both URLs so the fix is obvious. Absent a local checkout, it creates
// the GitHub repo if missing (gh repo create, with the literal
// createRepoDescription) and clones it via a temp-sibling-then-rename so
// a crash mid-clone never leaves a half-populated memories dir where a
// real one is expected.
func stepRepo(ctx context.Context, state *initState) error {
	if err := os.MkdirAll(state.paths.DataDir, 0o700); err != nil {
		return err
	}
	memories := state.paths.MemoriesDir()

	if _, err := os.Stat(filepath.Join(memories, ".git")); err == nil {
		return verifyExistingCheckoutOrigin(ctx, state, memories)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	exists, err := state.gh.RepoExists(ctx, state.login, state.repoName)
	if err != nil {
		return err
	}
	if !exists {
		if _, err := state.gh.CreateRepo(ctx, state.repoName, createRepoDescription); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(state.out, "repo: created %s/%s\n", state.login, state.repoName); err != nil {
			return err
		}
	}

	partial := memories + ".partial"
	if err := os.RemoveAll(partial); err != nil {
		return err
	}
	if err := state.gh.Clone(ctx, state.login+"/"+state.repoName, partial, "--no-checkout"); err != nil {
		return err
	}
	// Deterministic branch regardless of the user's init.defaultBranch:
	if _, err := gitx.Run(ctx, partial, "symbolic-ref", "HEAD", "refs/heads/main"); err != nil {
		return err
	}
	if err := os.Rename(partial, memories); err != nil {
		return err
	}
	// A daemon cycle racing between this rename and step 5 (stepWiring)
	// fails CLOSED, not silently: filter.agentbrain.required=true isn't
	// wired yet, so git refuses to write plaintext into a filtered path —
	// the cycle degrades and heals itself on the next tick once wiring
	// lands. This is the one place a non-daemon process is allowed to
	// touch the checkout directly (single-writer invariant, spec §2 /
	// ADR 03):
	// there is no enrolled unit yet for any daemon to race against until
	// step 9.
	_, err = fmt.Fprintf(state.out, "repo: cloned to %s\n", memories)
	return err
}

// verifyExistingCheckoutOrigin refuses a local checkout whose origin does
// not canonicalize to state.login/state.repoName — a foreign or stale
// checkout must never be adopted silently.
func verifyExistingCheckoutOrigin(ctx context.Context, state *initState, memories string) error {
	result, err := gitx.Run(ctx, memories, "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("existing checkout at %s has no readable origin: %w", memories, err)
	}
	actual := strings.TrimSpace(result.Stdout)
	normalizedActual, err := provider.NormalizeRemoteURL(actual)
	if err != nil {
		return fmt.Errorf("existing checkout at %s has an unparseable origin %q: %w", memories, actual, err)
	}
	expected := "https://github.com/" + state.login + "/" + state.repoName
	normalizedExpected, err := provider.NormalizeRemoteURL(expected)
	if err != nil {
		return err
	}
	if normalizedActual != normalizedExpected {
		return fmt.Errorf("existing checkout at %s has origin %s, not the expected %s — refusing to touch a foreign checkout", memories, actual, expected)
	}
	_, err = fmt.Fprintf(state.out, "repo: existing checkout at %s matches %s/%s\n", memories, state.login, state.repoName)
	return err
}

// stepWiring installs the git filter/merge/textconv chain, the repo-local
// gh credential helper, the foreground auto-maintenance posture (ADR 19),
// and a repo-local git identity — all idempotent, re-run-safe writes to
// memories/.git/config (spec §5; ADR 08).
func stepWiring(ctx context.Context, state *initState) error {
	memories := state.paths.MemoriesDir()
	if err := gitx.InstallFilters(ctx, memories, state.binaryPath); err != nil {
		return err
	}
	if err := gitx.InstallCredentialHelper(ctx, memories, state.gh.BinaryPath()); err != nil {
		return err
	}
	if err := gitx.InstallMaintenancePosture(ctx, memories); err != nil {
		return err
	}
	if err := ensureRepoIdentity(ctx, memories); err != nil {
		return err
	}
	_, err := fmt.Fprintln(state.out, "wiring: filters, credential helper, maintenance posture, and repo-local identity installed")
	return err
}

// ensureRepoIdentity sets a repo-local synthetic git identity for commits
// this checkout makes: neither engine/commit.go nor admin.go sets
// GIT_AUTHOR_*/GIT_COMMITTER_* env or user.name/user.email per commit, so
// without this every commit here would fall through to the user's global
// identity (or fail outright if none is configured). It sets ONLY
// whatever is currently unset: a user who has deliberately customized the
// repo-local identity on a prior init/doctor run is never silently
// overwritten on a re-run — the same write-once treatment config.toml
// gets (ADR 17), applied at the git-config level instead of a file.
func ensureRepoIdentity(ctx context.Context, memories string) error {
	if result, err := gitx.RunStatus(ctx, memories, "config", "--local", "user.name"); err != nil {
		return err
	} else if result.ExitCode != 0 {
		if _, err := gitx.Run(ctx, memories, "config", "--local", "user.name", "agent-brain daemon"); err != nil {
			return err
		}
	}
	if result, err := gitx.RunStatus(ctx, memories, "config", "--local", "user.email"); err != nil {
		return err
	} else if result.ExitCode != 0 {
		hostname, err := os.Hostname()
		if err != nil {
			hostname = "unknown-host"
		}
		email := "agent-brain@" + repo.SanitizeHostname(hostname)
		if _, err := gitx.Run(ctx, memories, "config", "--local", "user.email", email); err != nil {
			return err
		}
	}
	return nil
}

// stepRepoState brings the checkout's tracked content to the state this
// machine expects: an unborn HEAD means this is the first machine ever to
// provision the repo (provisionFirstMachine writes the skeleton); a real
// HEAD means a prior machine already did, and this one is joining
// (healJoiningMachine materializes the working tree and heals a
// non-canonical root .gitattributes if needed).
func stepRepoState(ctx context.Context, state *initState) error {
	// A daemon already resident on this machine (a prior init installed the
	// service) runs cycles that would race this step's commit/push on git
	// locks. Hold its automatic cycles for the surgery, best
	// effort: a daemon that is down, mid-shutdown, or refuses is the
	// pre-quiesce status quo — the transient-error fallback — never a reason
	// to fail init. The daemon's TTL auto-releases even if this process dies.
	if client := tryAPIClient(ctx); client != nil {
		if _, err := client.Quiesce(ctx, quiesceHoldForInit); err != nil {
			if _, werr := fmt.Fprintf(state.out, "repo state: could not quiesce the daemon (%v) — proceeding\n", err); werr != nil {
				return werr
			}
		} else {
			defer resumeQuietly(client)
		}
	}

	memories := state.paths.MemoriesDir()
	layout := repo.NewLayout(memories)

	result, err := gitx.RunStatus(ctx, memories, "rev-parse", "--verify", "-q", "HEAD")
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return provisionFirstMachine(ctx, state, layout)
	}
	return healJoiningMachine(ctx, state, layout)
}

// provisionFirstMachine writes the canonical .gitattributes and an empty
// projects registry, creates the (git-untracked — git does not track
// empty directories) manifests dir so later per-host manifest writes
// never need their own mkdir, then commits and pushes — the one-time act
// of bringing a brand-new agent-brain-memories repo into existence.
func provisionFirstMachine(ctx context.Context, state *initState, layout repo.Layout) error {
	memories := layout.Root()
	if err := repo.WriteAttributes(layout, state.registry); err != nil {
		return err
	}
	if err := os.MkdirAll(layout.ManifestDir(), 0o750); err != nil {
		return err
	}
	if err := repo.NewProjects().Save(layout.ProjectsFile()); err != nil {
		return err
	}
	if _, err := gitx.Run(ctx, memories, "add", "-A"); err != nil {
		return err
	}
	if _, err := gitx.Run(ctx, memories, "commit", "-m", "meta: initialize memories repo"); err != nil {
		return err
	}
	if _, err := gitx.Run(ctx, memories, "push", "-u", "origin", "main"); err != nil {
		return err
	}
	_, err := fmt.Fprintln(state.out, "repo state: initialized a fresh checkout (skeleton committed and pushed)")
	return err
}

// healJoiningMachine materializes the working tree from the history a
// prior machine already pushed (the --no-checkout clone in stepRepo left
// the working tree empty — HEAD's symref already names this branch, per
// stepRepo's symbolic-ref, so a plain checkout populates the index and
// working tree without needing -f), then heals the root .gitattributes
// if it does not match what this registry generates — a machine running
// a newer binary than whichever one wrote the file last.
func healJoiningMachine(ctx context.Context, state *initState, layout repo.Layout) error {
	memories := layout.Root()
	if _, err := gitx.Run(ctx, memories, "checkout", "main"); err != nil {
		return err
	}

	want := repo.GenerateAttributes(state.registry)
	current, err := os.ReadFile(layout.AttributesFile())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if string(current) == want {
		_, err = fmt.Fprintln(state.out, "repo state: joined existing checkout (attributes already canonical)")
		return err
	}

	if err := repo.WriteAttributes(layout, state.registry); err != nil {
		return err
	}
	if _, err := gitx.Run(ctx, memories, "add", "--", ".gitattributes"); err != nil {
		return err
	}
	if _, err := gitx.Run(ctx, memories, "commit", "-m", "meta: heal root .gitattributes"); err != nil {
		return err
	}
	if _, err := gitx.Run(ctx, memories, "push"); err != nil {
		return err
	}
	_, err = fmt.Fprintln(state.out, "repo state: joined existing checkout (healed non-canonical .gitattributes)")
	return err
}

// configTemplate is config.toml's exact first-write content (ADR 17):
// written once by init and never rewritten again — a present file, even
// one a prior init wrote byte-for-byte identically, is always left
// untouched, so user edits/comments always survive. Split around the two
// literal backticks (Go raw strings cannot contain one) rather than
// escaping the whole thing as an interpreted string.
const configTemplate = `# agent-brain configuration (TOML). Deleting this file restores defaults.
# The daemon reads it at startup: ` + "`" + `agent-brain service restart` + "`" + ` to apply.

[sync]
# ticker: idle fetch/integrate interval.
ticker = "5m"
# debounce: trailing quiet window after a file event before syncing.
debounce = "2s"
# poll: backstop rescan interval for filesystems fsnotify misses.
poll = "45s"

[migrate]
# preflight_timeout: bounds the ` + "`" + `chezmoi diff` + "`" + ` subprocess the one-time
# ` + "`" + `agent-brain migrate` + "`" + ` command runs before importing the bash-era
# memory tree (spec §10). Must be >0 and <=10m.
preflight_timeout = "30s"

# Per-provider classification overrides (advanced; spec §6). Overriding
# a provider REPLACES its whole table. Classes: fact | derived-index |
# regenerated | ignore.
# [providers.codex]
# classify = [
#   { glob = "memories/raw_memories.md", class = "fact" },
# ]
`

// stepConfigFile writes config.toml exactly once (ADR 17): a file already
// present — from a prior init, doctor run, or a user's own edits — is
// reported and left completely untouched, never merged or reformatted.
func stepConfigFile(_ context.Context, state *initState) error {
	path := state.paths.SettingsFile()
	if _, err := os.Stat(path); err == nil {
		_, err = fmt.Fprintf(state.out, "config: found at %s, left untouched\n", path)
		return err
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := renameio.WriteFile(path, []byte(configTemplate), 0o600); err != nil {
		return fmt.Errorf("write config template: %w", err)
	}
	_, err := fmt.Fprintf(state.out, "config: wrote defaults to %s\n", path)
	return err
}

// stepService installs and starts the login service unless --skip-service
// was given. Install/report, start/report, and status/linger delegate to
// installServiceAndReport, startServiceAndReport, and printServiceStatus
// (service.go) — the same helpers the standalone `service install`/
// `service start`/`service status` commands use, so the idempotency
// branches (service.ErrAlreadyInstalled and
// service.ErrAlreadyRunning, both errors.Is), the WSL2 linger warning,
// and the linger advisory line live in exactly one place
// rather than being duplicated here. The start branch is
// what makes re-running init against a healthy daemon a no-op instead of
// dying on launchd's already-loaded EIO.
func stepService(_ context.Context, state *initState) error {
	if state.skipService {
		_, err := fmt.Fprintln(state.out, "service: skipped (--skip-service)")
		return err
	}

	controller, err := service.NewController(state.binaryPath)
	if err != nil {
		return err
	}
	if err := installServiceAndReport(controller, state.out); err != nil && !errors.Is(err, service.ErrAlreadyInstalled) {
		return err
	}
	if err := startServiceAndReport(controller, state.out); err != nil && !errors.Is(err, service.ErrAlreadyRunning) {
		return err
	}
	return printServiceStatus(state.out, controller)
}

// stepEnrollment offers every discovered-but-unenrolled memory root for
// enrollment and submits each accepted one to the daemon via
// client.Track — the CLI process itself never writes units into
// the local registry or the checkout directly (single-writer invariant,
// spec §2 / ADR 03). Global-scope providers (codex) group ALL their
// still-unenrolled roots into ONE picker entry (buildEnrollCandidates):
// picking it enrolls every root together, since they are one
// pseudo-project (_global) on this machine, not independent choices.
func stepEnrollment(ctx context.Context, state *initState) error {
	local, err := repo.LoadLocalRegistry(state.paths.LocalRegistryFile())
	if err != nil {
		return err
	}
	enrolled := enrolledSet(local.Units)

	candidates, err := buildEnrollCandidates(ctx, state.registry, enrolled)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		_, err = fmt.Fprintln(state.out, "enroll: no new memory roots discovered")
		return err
	}

	chosen, err := state.pickEnrollUnits(candidates)
	if err != nil {
		return err
	}
	if len(chosen) == 0 {
		_, err = fmt.Fprintln(state.out, "enroll: nothing selected")
		return err
	}

	client := ensureDaemonClient(ctx, state)
	if client == nil {
		_, err = fmt.Fprintln(state.out, "enroll: daemon not reachable — start it, then run `agent-brain track`")
		return err
	}

	target := enrollTarget{out: state.out, confirmProjectPath: state.confirmProjectPath, nameRemotelessFolder: state.nameRemotelessFolder}
	for _, index := range chosen {
		candidate := candidates[index]
		for _, discovered := range candidate.discovered {
			err := enrollOne(ctx, target, client, candidate.provider, discovered)
			if errors.Is(err, errSkipRemoteless) {
				if _, err := fmt.Fprintf(state.out, "enroll: skipped %s (remoteless; needs a folder name — re-run interactively or `agent-brain track`)\n", discovered.LocalDir); err != nil {
					return err
				}
				continue
			}
			if formCancelled(err) {
				if _, err := fmt.Fprintf(state.out, "enroll: cancelled — nothing enrolled for %s\n", discovered.LocalDir); err != nil {
					return err
				}
				continue
			}
			if err != nil {
				return err
			}
			state.enrolledAny = true
		}
	}
	return nil
}

// ensureDaemonClient returns state.apiClient if a caller (orchestrator or
// test) already resolved one, or if a PRIOR call already tried and
// cached the (possibly nil) result — daemonResolved makes "we already
// waited and it never came up" sticky, so a second step later in the
// same run never waits out the same timeout again. Otherwise it polls
// for the daemon socket (bounded: step 8's service start is
// asynchronous) using state.daemonPollTimeout/Interval, defaulting to
// 15s/500ms when unset (zero value) — production's values; tests set
// both fields directly to something small.
func ensureDaemonClient(ctx context.Context, state *initState) *api.Client {
	if state.apiClient != nil || state.daemonResolved {
		return state.apiClient
	}
	timeout := state.daemonPollTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	interval := state.daemonPollInterval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	state.apiClient = pollForDaemonClient(ctx, timeout, interval)
	state.daemonResolved = true
	return state.apiClient
}

// pollForDaemonClient repeatedly tries to reach the daemon until timeout
// elapses; a nil return means it never came up — --skip-service and a
// user who defers `service install`/`daemon run` are both legitimate,
// non-fatal reasons, not errors.
func pollForDaemonClient(ctx context.Context, timeout, interval time.Duration) *api.Client {
	deadline := time.Now().Add(timeout)
	for {
		if client, err := newAPIClient(); err == nil {
			if _, err := client.Status(ctx); err == nil {
				return client
			}
		}
		if time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// stepFirstSync triggers one sync cycle ONLY when step 9 actually
// enrolled something (an idle daemon has nothing new to report), prints
// the outcome via the existing printSummary, then always prints standing
// next-steps guidance — including a one-time nudge toward
// `agent-brain migrate` when a legacy ~/.agent-brain tree exists
// (spec §10).
func stepFirstSync(ctx context.Context, state *initState) error {
	if state.enrolledAny {
		client := ensureDaemonClient(ctx, state)
		if client == nil {
			if _, err := fmt.Fprintln(state.out, "sync: daemon not reachable — run `agent-brain sync` once it's up"); err != nil {
				return err
			}
		} else {
			response, err := client.Sync(ctx, "")
			if err != nil {
				return explainDown(err)
			}
			report := &reportWriter{w: state.out}
			if response.Status == "running" {
				report.println("sync still running — check `agent-brain status`")
			} else {
				report.println("sync completed")
				printSummary(report, response.Summary)
			}
			if report.err != nil {
				return report.err
			}
		}
	}

	if _, err := fmt.Fprintln(state.out, "next: `agent-brain status` to check in, `agent-brain track` to enroll more projects"); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(state.home, ".agent-brain")); err == nil {
		_, err = fmt.Fprintln(state.out, "found a legacy ~/.agent-brain tree — run `agent-brain migrate` to bring it in")
		return err
	}
	return nil
}
