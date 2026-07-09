package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/ghx"
	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/keys"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// fakeGHRunner scripts gh for init's tests: canned responses for
// auth/login/repo-view/repo-create, and a REAL git clone of a
// test-local bare "remote" for repo-clone calls — init's own tests need
// the clone to actually populate a working tree (steps 4-6 exercise
// real git), which the shared ghxtest.Fake (pure canned responses, no
// side-effect hook) cannot do. Kept local to this file rather than
// added to ghxtest, which is outside this task's file scope.
type fakeGHRunner struct {
	t          *testing.T
	login      string
	repoName   string
	bareRemote string // local path standing in for the GitHub remote
	workDir    string // cwd for the real git clone invocation
	exists     bool   // RepoExists() answer; CreateRepo flips it true
	wantDesc   string // if non-empty, CreateRepo's description must match exactly

	createCalls int
	cloneCalls  int
}

func newFakeGHRunner(t *testing.T, login, repoName, bareRemote string) *fakeGHRunner {
	t.Helper()
	return &fakeGHRunner{
		t: t, login: login, repoName: repoName, bareRemote: bareRemote,
		workDir: t.TempDir(),
	}
}

func (f *fakeGHRunner) Run(ctx context.Context, args ...string) (ghx.Result, error) {
	f.t.Helper()
	switch {
	case slices.Equal(args, []string{"auth", "status"}):
		return ghx.Result{ExitCode: 0}, nil
	case slices.Equal(args, []string{"api", "user", "--jq", ".login"}):
		return ghx.Result{Stdout: f.login + "\n", ExitCode: 0}, nil
	case len(args) == 5 && args[0] == "repo" && args[1] == "view":
		if want := f.login + "/" + f.repoName; args[2] != want {
			f.t.Fatalf("fakeGHRunner: repo view target = %q, want %q", args[2], want)
		}
		if f.exists {
			return ghx.Result{ExitCode: 0}, nil
		}
		return ghx.Result{ExitCode: 1, Stderr: "GraphQL: Could not resolve to a Repository with the name 'x'. (repository)\n"}, nil
	case len(args) == 6 && args[0] == "repo" && args[1] == "create":
		f.createCalls++
		if args[2] != f.repoName {
			f.t.Fatalf("fakeGHRunner: repo create name = %q, want %q", args[2], f.repoName)
		}
		if f.wantDesc != "" && args[5] != f.wantDesc {
			f.t.Fatalf("fakeGHRunner: repo create description = %q, want %q", args[5], f.wantDesc)
		}
		f.exists = true
		return ghx.Result{Stdout: "https://github.com/" + f.login + "/" + f.repoName + "\n", ExitCode: 0}, nil
	case len(args) >= 4 && args[0] == "repo" && args[1] == "clone":
		f.cloneCalls++
		if want := f.login + "/" + f.repoName; args[2] != want {
			f.t.Fatalf("fakeGHRunner: repo clone target = %q, want %q", args[2], want)
		}
		dir := args[3]
		gitArgs := []string{"clone", f.bareRemote, dir}
		if i := slices.Index(args, "--"); i >= 0 {
			gitArgs = append(gitArgs, args[i+1:]...)
		}
		if _, err := gitx.Run(ctx, f.workDir, gitArgs...); err != nil {
			f.t.Fatalf("fakeGHRunner: real git clone backing gh repo clone failed: %v", err)
		}
		return ghx.Result{ExitCode: 0}, nil
	default:
		f.t.Fatalf("fakeGHRunner: unexpected gh call: %v", args)
		return ghx.Result{}, nil
	}
}

// provisionUninitializedMachine sets up a hermetic, from-scratch machine
// for init's tests: HOME + AGENT_BRAIN_*_DIR overrides pointing at a
// fresh temp tree (no keyset, no checkout — that is exactly what init
// must create), plus the test-binary seam so any filter wiring init
// performs never touches the test binary itself (see testBinaryPath's
// doc comment, testmain_test.go).
func provisionUninitializedMachine(t *testing.T) config.Paths {
	t.Helper()
	base := t.TempDir()

	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", filepath.Join(base, "cfg"))
	t.Setenv("AGENT_BRAIN_DATA_DIR", filepath.Join(base, "data"))
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", filepath.Join(base, "run"))
	t.Setenv(testBinaryPathEnv, testBinaryPath)

	paths, err := config.DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestStepIdentityResolvesPathsSettingsRegistryAndBinary(t *testing.T) {
	paths := provisionUninitializedMachine(t)
	var out bytes.Buffer
	state := &initState{out: &out}

	if err := stepIdentity(context.Background(), state); err != nil {
		t.Fatalf("stepIdentity: %v", err)
	}
	if state.paths != paths {
		t.Fatalf("state.paths = %+v, want %+v", state.paths, paths)
	}
	if state.binaryPath != testBinaryPath {
		t.Fatalf("state.binaryPath = %q, want the injected test binary %q (must never resolve os.Executable() — see fork-bomb incident)", state.binaryPath, testBinaryPath)
	}
	if state.registry == nil {
		t.Fatal("state.registry is nil")
	}
	if got := len(state.registry.All()); got == 0 {
		t.Fatal("state.registry has no providers")
	}
	if state.home == "" {
		t.Fatal("state.home is empty")
	}
	if out.Len() == 0 {
		t.Fatal("stepIdentity printed nothing")
	}
}

func TestStepGHAuthenticatesAndResolvesLogin(t *testing.T) {
	var out bytes.Buffer
	fake := newFakeGHRunner(t, "octocat", "agent-brain-memories", "")
	state := &initState{
		out: &out,
		gh:  ghx.NewClientWithRunner(fake, "/usr/bin/gh"),
	}

	if err := stepGH(context.Background(), state); err != nil {
		t.Fatalf("stepGH: %v", err)
	}
	if state.login != "octocat" {
		t.Fatalf("state.login = %q, want %q", state.login, "octocat")
	}
	if !strings.Contains(out.String(), "octocat") {
		t.Fatalf("stepGH output missing login:\n%s", out.String())
	}
}

func TestStepKeysetGenerateNonInteractivePrintsExportOnceWithPasswordManagerLine(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	state := &initState{
		out:         &out,
		paths:       config.Paths{ConfigDir: dir, DataDir: dir},
		generateKey: true,
	}

	if err := stepKeyset(context.Background(), state); err != nil {
		t.Fatalf("stepKeyset: %v", err)
	}

	// The keyset must actually exist and validate now.
	if _, err := keys.Primitive(state.paths.Keyset()); err != nil {
		t.Fatalf("keyset was not generated at %s: %v", state.paths.Keyset(), err)
	}

	printed := out.String()
	if got := strings.Count(printed, "password manager"); got != 1 {
		t.Fatalf("password-manager line printed %d times, want exactly 1:\n%s", got, printed)
	}
	armored, err := keys.Export(state.paths.Keyset())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(printed, armored) {
		t.Fatalf("stepKeyset did not print the armored export:\n%s", printed)
	}
}

// TestStepKeysetGenerateSetsKeysetGeneratedFlag proves stepKeyset reports
// back that it took the fresh-generate branch: init.go's interactive
// password-manager confirm gate must fire only then, never when an
// existing keyset was merely validated or imported.
func TestStepKeysetGenerateSetsKeysetGeneratedFlag(t *testing.T) {
	dir := t.TempDir()
	state := &initState{
		out:         &bytes.Buffer{},
		paths:       config.Paths{ConfigDir: dir, DataDir: dir},
		generateKey: true,
	}
	if err := stepKeyset(context.Background(), state); err != nil {
		t.Fatalf("stepKeyset: %v", err)
	}
	if !state.keysetGenerated {
		t.Fatal("state.keysetGenerated not set after the generate branch ran")
	}
}

func TestStepKeysetImportInstallsArmoredKeyset(t *testing.T) {
	// Build a source keyset elsewhere purely to get valid armored bytes.
	sourceDir := t.TempDir()
	if err := keys.Generate(filepath.Join(sourceDir, "keyset.json")); err != nil {
		t.Fatal(err)
	}
	armored, err := keys.Export(filepath.Join(sourceDir, "keyset.json"))
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	var out bytes.Buffer
	state := &initState{
		out:           &out,
		paths:         config.Paths{ConfigDir: dir, DataDir: dir},
		importKey:     true,
		importArmored: armored,
	}

	if err := stepKeyset(context.Background(), state); err != nil {
		t.Fatalf("stepKeyset: %v", err)
	}
	if _, err := keys.Primitive(state.paths.Keyset()); err != nil {
		t.Fatalf("imported keyset does not validate: %v", err)
	}
}

func TestStepKeysetAlreadyPresentValidatesAndSkips(t *testing.T) {
	dir := t.TempDir()
	keysetPath := filepath.Join(dir, "keyset.json")
	if err := keys.Generate(keysetPath); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	state := &initState{
		out:         &out,
		paths:       config.Paths{ConfigDir: dir, DataDir: dir},
		generateKey: true, // must be ignored: a keyset is already present
	}

	if err := stepKeyset(context.Background(), state); err != nil {
		t.Fatalf("stepKeyset: %v", err)
	}
	if strings.Contains(out.String(), "password manager") {
		t.Fatalf("stepKeyset re-printed the export/confirm gate for an already-present keyset:\n%s", out.String())
	}
}

func TestStepKeysetMissingAndUndeterminedErrors(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	state := &initState{
		out:   &out,
		paths: config.Paths{ConfigDir: dir, DataDir: dir},
		// neither generateKey nor importKey set
	}

	err := stepKeyset(context.Background(), state)
	if err == nil {
		t.Fatal("stepKeyset with no keyset and no decision succeeded; want an actionable error")
	}
	if !strings.Contains(err.Error(), "--generate-key") || !strings.Contains(err.Error(), "--import-key") {
		t.Fatalf("error does not name both flags: %v", err)
	}
}

func TestStepKeysetInvalidExistingKeysetErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keyset.json"), []byte("not a keyset"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	state := &initState{out: &out, paths: config.Paths{ConfigDir: dir, DataDir: dir}}

	if err := stepKeyset(context.Background(), state); err == nil {
		t.Fatal("stepKeyset accepted a corrupt existing keyset")
	}
}

// TestFakeGHRunnerCloneUsesRealGit is a sanity check on the test fixture
// itself: the fake's "repo clone" branch must perform a real git clone
// (steps 4-6 need a real working tree to operate on), not merely
// pretend to succeed.
func TestFakeGHRunnerCloneUsesRealGit(t *testing.T) {
	base := t.TempDir()
	bare := filepath.Join(base, "remote.git")
	if _, err := gitx.Run(context.Background(), base, "init", "--bare", "-b", "main", bare); err != nil {
		t.Fatal(err)
	}
	fake := newFakeGHRunner(t, "octocat", "agent-brain-memories", bare)
	client := ghx.NewClientWithRunner(fake, "/usr/bin/gh")

	dest := filepath.Join(base, "checkout")
	if err := client.Clone(context.Background(), "octocat/agent-brain-memories", dest, "--no-checkout"); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
		t.Fatalf("clone did not produce a real .git dir: %v", err)
	}
}

// --- step 4 (repo) ---

func TestStepRepoCreatesRepoWhenMissingThenClones(t *testing.T) {
	base := t.TempDir()
	bareRemote := filepath.Join(base, "remote.git")
	mustGitCLI(t, base, "init", "--bare", "-b", "main", bareRemote)

	fake := newFakeGHRunner(t, "alice", "agent-brain-memories", bareRemote)
	fake.wantDesc = "agent-brain encrypted memories (github.com/Sawmonabo/agent-brain)"
	var out bytes.Buffer
	state := &initState{
		out:      &out,
		paths:    config.Paths{ConfigDir: filepath.Join(base, "cfg"), DataDir: filepath.Join(base, "data")},
		login:    "alice",
		repoName: "agent-brain-memories",
		gh:       ghx.NewClientWithRunner(fake, "/usr/bin/gh"),
	}

	if err := stepRepo(context.Background(), state); err != nil {
		t.Fatalf("stepRepo: %v", err)
	}
	if fake.createCalls != 1 {
		t.Fatalf("createCalls = %d, want 1", fake.createCalls)
	}
	if _, err := os.Stat(filepath.Join(state.paths.MemoriesDir(), ".git")); err != nil {
		t.Fatalf("stepRepo did not leave a real checkout: %v", err)
	}
}

// TestStepRepoUsesCustomRepoName proves --repo-name threads all the way
// through gh repo view/create/clone rather than silently falling back to
// defaultRepoName — fakeGHRunner asserts the exact name/owner-repo
// argument on every gh call, so a stepRepo regression that ignored
// state.repoName would fail fakeGHRunner.Run's assertions, not just this
// test's own.
func TestStepRepoUsesCustomRepoName(t *testing.T) {
	base := t.TempDir()
	bareRemote := filepath.Join(base, "remote.git")
	mustGitCLI(t, base, "init", "--bare", "-b", "main", bareRemote)

	const customRepoName = "my-work-memories"
	fake := newFakeGHRunner(t, "alice", customRepoName, bareRemote)
	fake.wantDesc = "agent-brain encrypted memories (github.com/Sawmonabo/agent-brain)"
	var out bytes.Buffer
	state := &initState{
		out:      &out,
		paths:    config.Paths{ConfigDir: filepath.Join(base, "cfg"), DataDir: filepath.Join(base, "data")},
		login:    "alice",
		repoName: customRepoName,
		gh:       ghx.NewClientWithRunner(fake, "/usr/bin/gh"),
	}

	if err := stepRepo(context.Background(), state); err != nil {
		t.Fatalf("stepRepo: %v", err)
	}
	if fake.createCalls != 1 {
		t.Fatalf("createCalls = %d, want 1", fake.createCalls)
	}
	if _, err := os.Stat(filepath.Join(state.paths.MemoriesDir(), ".git")); err != nil {
		t.Fatalf("stepRepo did not leave a real checkout: %v", err)
	}
	if !strings.Contains(out.String(), customRepoName) {
		t.Fatalf("output does not mention the custom repo name %q: %s", customRepoName, out.String())
	}
}

func TestStepRepoClonesWithoutCreatingWhenRepoAlreadyExistsRemotely(t *testing.T) {
	base := t.TempDir()
	bareRemote := filepath.Join(base, "remote.git")
	mustGitCLI(t, base, "init", "--bare", "-b", "main", bareRemote)

	fake := newFakeGHRunner(t, "alice", "agent-brain-memories", bareRemote)
	fake.exists = true
	state := &initState{
		out:      &bytes.Buffer{},
		paths:    config.Paths{ConfigDir: filepath.Join(base, "cfg"), DataDir: filepath.Join(base, "data")},
		login:    "alice",
		repoName: "agent-brain-memories",
		gh:       ghx.NewClientWithRunner(fake, "/usr/bin/gh"),
	}

	if err := stepRepo(context.Background(), state); err != nil {
		t.Fatalf("stepRepo: %v", err)
	}
	if fake.createCalls != 0 {
		t.Fatalf("createCalls = %d, want 0 (repo already existed remotely)", fake.createCalls)
	}
	if fake.cloneCalls != 1 {
		t.Fatalf("cloneCalls = %d, want 1", fake.cloneCalls)
	}
}

func TestStepRepoNoOpWhenLocalCheckoutAlreadyMatches(t *testing.T) {
	base := t.TempDir()
	bareRemote := filepath.Join(base, "remote.git")
	mustGitCLI(t, base, "init", "--bare", "-b", "main", bareRemote)
	dataDir := filepath.Join(base, "data")
	memories := filepath.Join(dataDir, "memories")
	mustGitCLI(t, base, "clone", bareRemote, memories)
	mustGitCLI(t, memories, "remote", "set-url", "origin", "https://github.com/alice/agent-brain-memories")

	fake := newFakeGHRunner(t, "alice", "agent-brain-memories", bareRemote)
	state := &initState{
		out:      &bytes.Buffer{},
		paths:    config.Paths{ConfigDir: filepath.Join(base, "cfg"), DataDir: dataDir},
		login:    "alice",
		repoName: "agent-brain-memories",
		gh:       ghx.NewClientWithRunner(fake, "/usr/bin/gh"),
	}

	if err := stepRepo(context.Background(), state); err != nil {
		t.Fatalf("stepRepo: %v", err)
	}
	if fake.createCalls != 0 || fake.cloneCalls != 0 {
		t.Fatalf("stepRepo touched gh (create=%d clone=%d) for an already-matching local checkout", fake.createCalls, fake.cloneCalls)
	}
}

func TestStepRepoRefusesForeignCheckoutNamingBothURLs(t *testing.T) {
	base := t.TempDir()
	bareRemote := filepath.Join(base, "remote.git")
	mustGitCLI(t, base, "init", "--bare", "-b", "main", bareRemote)
	dataDir := filepath.Join(base, "data")
	memories := filepath.Join(dataDir, "memories")
	mustGitCLI(t, base, "clone", bareRemote, memories)
	foreignURL := "https://github.com/someone-else/other-repo"
	mustGitCLI(t, memories, "remote", "set-url", "origin", foreignURL)

	fake := newFakeGHRunner(t, "alice", "agent-brain-memories", bareRemote)
	state := &initState{
		out:      &bytes.Buffer{},
		paths:    config.Paths{ConfigDir: filepath.Join(base, "cfg"), DataDir: dataDir},
		login:    "alice",
		repoName: "agent-brain-memories",
		gh:       ghx.NewClientWithRunner(fake, "/usr/bin/gh"),
	}

	err := stepRepo(context.Background(), state)
	if err == nil {
		t.Fatal("stepRepo accepted a foreign checkout")
	}
	wantExpected := "https://github.com/alice/agent-brain-memories"
	if !strings.Contains(err.Error(), foreignURL) {
		t.Fatalf("error does not name the foreign URL %q: %v", foreignURL, err)
	}
	if !strings.Contains(err.Error(), wantExpected) {
		t.Fatalf("error does not name the expected URL %q: %v", wantExpected, err)
	}
}

// --- step 5 (wiring) ---

func TestStepWiringInstallsFiltersCredentialHelperAndIdentity(t *testing.T) {
	dataDir := t.TempDir()
	memories := filepath.Join(dataDir, "memories")
	mustGitCLI(t, dataDir, "init", "-b", "main", memories)

	fake := newFakeGHRunner(t, "alice", "agent-brain-memories", "")
	state := &initState{
		out:        &bytes.Buffer{},
		paths:      config.Paths{DataDir: dataDir},
		login:      "alice",
		binaryPath: testBinaryPath,
		gh:         ghx.NewClientWithRunner(fake, "/usr/bin/fake-gh"),
	}

	if err := stepWiring(context.Background(), state); err != nil {
		t.Fatalf("stepWiring: %v", err)
	}

	clean, err := gitx.Run(context.Background(), memories, "config", "--local", "filter.agentbrain.clean")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(clean.Stdout, testBinaryPath) {
		t.Fatalf("filter.agentbrain.clean = %q, want it to reference %q", clean.Stdout, testBinaryPath)
	}

	helper, err := gitx.Run(context.Background(), memories, "config", "--local", "--get-all", "credential.helper")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(helper.Stdout, "/usr/bin/fake-gh") {
		t.Fatalf("credential.helper = %q, want it to reference the gh binary path", helper.Stdout)
	}

	name, err := gitx.Run(context.Background(), memories, "config", "--local", "user.name")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(name.Stdout) != "agent-brain daemon" {
		t.Fatalf("user.name = %q, want %q", name.Stdout, "agent-brain daemon")
	}

	email, err := gitx.Run(context.Background(), memories, "config", "--local", "user.email")
	if err != nil {
		t.Fatal(err)
	}
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	wantEmail := "agent-brain@" + repo.SanitizeHostname(hostname)
	if strings.TrimSpace(email.Stdout) != wantEmail {
		t.Fatalf("user.email = %q, want %q", email.Stdout, wantEmail)
	}
}

// TestStepWiringNeverOverwritesACustomizedIdentity proves the "only if
// unset" rule: a user.name/user.email a prior init/doctor run (or the
// user themselves) already set locally must survive a re-run untouched —
// the same write-once treatment config.toml gets (ADR 17), applied at
// the git-config level.
func TestStepWiringNeverOverwritesACustomizedIdentity(t *testing.T) {
	dataDir := t.TempDir()
	memories := filepath.Join(dataDir, "memories")
	mustGitCLI(t, dataDir, "init", "-b", "main", memories)
	mustGitCLI(t, memories, "config", "--local", "user.name", "custom name")
	mustGitCLI(t, memories, "config", "--local", "user.email", "custom@example.invalid")

	fake := newFakeGHRunner(t, "alice", "agent-brain-memories", "")
	state := &initState{
		out:        &bytes.Buffer{},
		paths:      config.Paths{DataDir: dataDir},
		login:      "alice",
		binaryPath: testBinaryPath,
		gh:         ghx.NewClientWithRunner(fake, "/usr/bin/fake-gh"),
	}
	if err := stepWiring(context.Background(), state); err != nil {
		t.Fatalf("stepWiring: %v", err)
	}

	name, err := gitx.Run(context.Background(), memories, "config", "--local", "user.name")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(name.Stdout) != "custom name" {
		t.Fatalf("stepWiring overwrote a customized user.name: got %q", name.Stdout)
	}
	email, err := gitx.Run(context.Background(), memories, "config", "--local", "user.email")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(email.Stdout) != "custom@example.invalid" {
		t.Fatalf("stepWiring overwrote a customized user.email: got %q", email.Stdout)
	}
}

// --- step 6 (repo state) ---

func TestStepRepoStateFirstMachineWritesSkeletonCommitsAndPushes(t *testing.T) {
	base := t.TempDir()
	bareRemote := filepath.Join(base, "remote.git")
	mustGitCLI(t, base, "init", "--bare", "-b", "main", bareRemote)

	configDir := filepath.Join(base, "cfg")
	dataDir := filepath.Join(base, "data")
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", configDir)
	t.Setenv("AGENT_BRAIN_DATA_DIR", dataDir)
	// Isolate the runtime dir at an empty path: stepRepoState now probes for a
	// resident daemon to quiesce, and without this the probe could reach a
	// real per-user daemon socket. An empty dir means "no daemon" — this test
	// is the no-daemon path, which must behave exactly as before.
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", filepath.Join(base, "run"))
	if err := keys.Generate(filepath.Join(configDir, "keyset.json")); err != nil {
		t.Fatal(err)
	}
	registry, err := buildRegistry(config.DefaultSettings(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	state := &initState{
		out:        &out,
		paths:      config.Paths{ConfigDir: configDir, DataDir: dataDir},
		registry:   registry,
		binaryPath: testBinaryPath,
		login:      "alice",
		repoName:   "agent-brain-memories",
		gh:         ghx.NewClientWithRunner(newFakeGHRunner(t, "alice", "agent-brain-memories", bareRemote), "/usr/bin/gh"),
	}
	if err := stepRepo(context.Background(), state); err != nil {
		t.Fatalf("stepRepo: %v", err)
	}
	if err := stepWiring(context.Background(), state); err != nil {
		t.Fatalf("stepWiring: %v", err)
	}
	if err := stepRepoState(context.Background(), state); err != nil {
		t.Fatalf("stepRepoState: %v", err)
	}

	memories := state.paths.MemoriesDir()
	wantAttributes := repo.GenerateAttributes(registry)
	gotAttributes, err := os.ReadFile(repo.NewLayout(memories).AttributesFile())
	if err != nil {
		t.Fatal(err)
	}
	if string(gotAttributes) != wantAttributes {
		t.Fatalf("attributes mismatch:\ngot:\n%s\nwant:\n%s", gotAttributes, wantAttributes)
	}

	projectsPath := repo.NewLayout(memories).ProjectsFile()
	projects, err := repo.LoadProjects(projectsPath)
	if err != nil {
		t.Fatalf("projects.toml not written or unreadable: %v", err)
	}
	if len(projects.Entries) != 0 {
		t.Fatalf("fresh projects registry has entries: %+v", projects.Entries)
	}

	if _, err := os.Stat(repo.NewLayout(memories).ManifestDir()); err != nil {
		t.Fatalf("manifests dir not created: %v", err)
	}

	subject, err := gitx.Run(context.Background(), memories, "log", "-1", "--format=%s")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(subject.Stdout) != "meta: initialize memories repo" {
		t.Fatalf("skeleton commit subject = %q, want %q", subject.Stdout, "meta: initialize memories repo")
	}

	freshClone := filepath.Join(base, "verify-clone")
	mustGitCLI(t, base, "clone", bareRemote, freshClone)
	if _, err := os.Stat(filepath.Join(freshClone, ".gitattributes")); err != nil {
		t.Fatalf("bare remote did not receive the push: %v", err)
	}
}

// TestStepRepoStateQuiescesLiveDaemonDuringSurgery pins the Phase-3 F2 fix:
// when a daemon is already resident (a prior init installed the service), the
// repo-state step holds its cycles for the checkout surgery and releases them
// after. The recording fake daemon (which sets AGENT_BRAIN_RUNTIME_DIR) proves
// exactly one 120s hold and one resume bracket the step, and the skeleton is
// still committed under the hold.
func TestStepRepoStateQuiescesLiveDaemonDuringSurgery(t *testing.T) {
	base := t.TempDir()
	bareRemote := filepath.Join(base, "remote.git")
	mustGitCLI(t, base, "init", "--bare", "-b", "main", bareRemote)

	configDir := filepath.Join(base, "cfg")
	dataDir := filepath.Join(base, "data")
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", configDir)
	t.Setenv("AGENT_BRAIN_DATA_DIR", dataDir)
	if err := keys.Generate(filepath.Join(configDir, "keyset.json")); err != nil {
		t.Fatal(err)
	}
	registry, err := buildRegistry(config.DefaultSettings(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The recorder points AGENT_BRAIN_RUNTIME_DIR at its own socket, so
	// stepRepoState's probe finds this fake daemon.
	hits := startFakeDaemonRecordingQuiesce(t)

	var out bytes.Buffer
	state := &initState{
		out:        &out,
		paths:      config.Paths{ConfigDir: configDir, DataDir: dataDir},
		registry:   registry,
		binaryPath: testBinaryPath,
		login:      "alice",
		repoName:   "agent-brain-memories",
		gh:         ghx.NewClientWithRunner(newFakeGHRunner(t, "alice", "agent-brain-memories", bareRemote), "/usr/bin/gh"),
	}
	if err := stepRepo(context.Background(), state); err != nil {
		t.Fatalf("stepRepo: %v", err)
	}
	if err := stepWiring(context.Background(), state); err != nil {
		t.Fatalf("stepWiring: %v", err)
	}
	if err := stepRepoState(context.Background(), state); err != nil {
		t.Fatalf("stepRepoState: %v", err)
	}

	got := hits()
	if len(got.held) != 1 || got.held[0] != quiesceHoldForInit {
		t.Fatalf("quiesce holds = %v, want exactly one of %d seconds", got.held, quiesceHoldForInit)
	}
	if got.resumed != 1 {
		t.Fatalf("resume count = %d, want 1", got.resumed)
	}
	if !strings.Contains(out.String(), "initialized a fresh checkout") {
		t.Fatalf("repo-state surgery did not run under the hold:\n%s", out.String())
	}
}

// TestStepRepoStateJoiningMachineMaterializesAndDecryptsExistingMemory is
// the joining-machine scenario end to end: a first machine (alice-a)
// provisions the repo and seeds one real, filter-encrypted memory file;
// a second machine (alice-b), with a different local checkout/data dir
// but the SAME imported keyset, joins the same remote. Its checkout
// must materialize real files (not just an empty .git) and the smudge
// filter must decrypt the seeded memory file back to its original
// plaintext — proving the whole clone -> checkout -> smudge chain
// works, not merely that files of some kind appear.
//
// AGENT_BRAIN_CONFIG_DIR/DATA_DIR are re-pointed with t.Setenv between
// the "machine A" and "machine B" phases: the actual clean/smudge
// filter runs as a SEPARATE PROCESS (testBinaryPath) that resolves its
// own config.DefaultPaths() from the environment, not from this test's
// initState values — so the env vars, not just the state structs, must
// track which machine is "active" at the moment each git operation
// runs.
func TestStepRepoStateJoiningMachineMaterializesAndDecryptsExistingMemory(t *testing.T) {
	base := t.TempDir()
	bareRemote := filepath.Join(base, "remote.git")
	mustGitCLI(t, base, "init", "--bare", "-b", "main", bareRemote)

	registry, err := buildRegistry(config.DefaultSettings(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Isolate the runtime dir (empty — no daemon) so stepRepoState's quiesce
	// probe never reaches a real per-user daemon socket. Both machine phases
	// share it; neither runs a daemon.
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", filepath.Join(base, "run"))

	// --- machine A: first machine ---
	aConfigDir := filepath.Join(base, "a-config")
	aDataDir := filepath.Join(base, "a-data")
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", aConfigDir)
	t.Setenv("AGENT_BRAIN_DATA_DIR", aDataDir)
	if err := keys.Generate(filepath.Join(aConfigDir, "keyset.json")); err != nil {
		t.Fatal(err)
	}
	armored, err := keys.Export(filepath.Join(aConfigDir, "keyset.json"))
	if err != nil {
		t.Fatal(err)
	}

	aState := &initState{
		out:        &bytes.Buffer{},
		paths:      config.Paths{ConfigDir: aConfigDir, DataDir: aDataDir},
		registry:   registry,
		binaryPath: testBinaryPath,
		login:      "alice",
		repoName:   "agent-brain-memories",
		gh:         ghx.NewClientWithRunner(newFakeGHRunner(t, "alice", "agent-brain-memories", bareRemote), "/usr/bin/gh"),
	}
	if err := stepRepo(context.Background(), aState); err != nil {
		t.Fatalf("A stepRepo: %v", err)
	}
	if err := stepWiring(context.Background(), aState); err != nil {
		t.Fatalf("A stepWiring: %v", err)
	}
	if err := stepRepoState(context.Background(), aState); err != nil {
		t.Fatalf("A stepRepoState: %v", err)
	}

	memoryRelPath := filepath.Join("_global", "codex", "memories", "raw_memories.md")
	memoryAbsPath := filepath.Join(aState.paths.MemoriesDir(), memoryRelPath)
	if err := os.MkdirAll(filepath.Dir(memoryAbsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("# raw memories\nthe rocket launches at dawn\n")
	if err := os.WriteFile(memoryAbsPath, plaintext, 0o644); err != nil {
		t.Fatal(err)
	}
	mustGitCLI(t, aState.paths.MemoriesDir(), "add", "-A")
	mustGitCLI(t, aState.paths.MemoriesDir(), "commit", "-m", "memory: seed")
	mustGitCLI(t, aState.paths.MemoriesDir(), "push")

	// --- machine B: joining machine ---
	bConfigDir := filepath.Join(base, "b-config")
	bDataDir := filepath.Join(base, "b-data")
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", bConfigDir)
	t.Setenv("AGENT_BRAIN_DATA_DIR", bDataDir)
	if err := keys.Import(filepath.Join(bConfigDir, "keyset.json"), armored); err != nil {
		t.Fatal(err)
	}

	bFake := newFakeGHRunner(t, "alice", "agent-brain-memories", bareRemote)
	bFake.exists = true
	bState := &initState{
		out:        &bytes.Buffer{},
		paths:      config.Paths{ConfigDir: bConfigDir, DataDir: bDataDir},
		registry:   registry,
		binaryPath: testBinaryPath,
		login:      "alice",
		repoName:   "agent-brain-memories",
		gh:         ghx.NewClientWithRunner(bFake, "/usr/bin/gh"),
	}
	if err := stepRepo(context.Background(), bState); err != nil {
		t.Fatalf("B stepRepo: %v", err)
	}
	if err := stepWiring(context.Background(), bState); err != nil {
		t.Fatalf("B stepWiring: %v", err)
	}
	if err := stepRepoState(context.Background(), bState); err != nil {
		t.Fatalf("B stepRepoState: %v", err)
	}

	gotAttributes, err := os.ReadFile(repo.NewLayout(bState.paths.MemoriesDir()).AttributesFile())
	if err != nil {
		t.Fatalf("B's checkout did not materialize .gitattributes: %v", err)
	}
	if string(gotAttributes) != repo.GenerateAttributes(registry) {
		t.Fatalf("B's .gitattributes does not match the canonical content")
	}

	gotMemory, err := os.ReadFile(filepath.Join(bState.paths.MemoriesDir(), memoryRelPath))
	if err != nil {
		t.Fatalf("B's checkout did not materialize the seeded memory file: %v", err)
	}
	if !bytes.Equal(gotMemory, plaintext) {
		t.Fatalf("B's checkout did not decrypt the seeded memory file: got %q want %q", gotMemory, plaintext)
	}

	// The object in git must still be ciphertext — `git show <rev>:<path>`
	// prints the raw blob (no smudge/textconv applied), so this proves
	// encryption happened at rest, not just that the working tree looks
	// right.
	blob, err := gitx.Run(context.Background(), bState.paths.MemoriesDir(), "show", "HEAD:"+filepath.ToSlash(memoryRelPath))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(blob.Stdout, string(plaintext)) {
		t.Fatal("the committed git object contains plaintext memory content — filters are not actually encrypting")
	}
}

// --- step 7 (config.toml) ---

func TestStepConfigFileWritesTemplateWhenMissing(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	state := &initState{out: &out, paths: config.Paths{ConfigDir: dir}}

	if err := stepConfigFile(context.Background(), state); err != nil {
		t.Fatalf("stepConfigFile: %v", err)
	}
	got, err := os.ReadFile(state.paths.SettingsFile())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != configTemplate {
		t.Fatalf("config.toml does not match the template:\ngot:\n%s\nwant:\n%s", got, configTemplate)
	}
	info, err := os.Stat(state.paths.SettingsFile())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config.toml perm = %o, want 0600", info.Mode().Perm())
	}

	// The template's active (uncommented) settings must equal
	// config.DefaultSettings() — the whole point of a "documented
	// defaults" template is that it doesn't silently diverge from what
	// the binary actually defaults to when the file is absent.
	loaded, err := config.LoadSettings(state.paths.SettingsFile())
	if err != nil {
		t.Fatalf("template is not valid config.toml: %v", err)
	}
	if diff := cmp.Diff(config.DefaultSettings(), loaded); diff != "" {
		t.Fatalf("template's active settings do not match config.DefaultSettings() (-want +got):\n%s", diff)
	}
}

func TestStepConfigFileLeavesExistingFileByteForByteUntouched(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	custom := "# my own settings\n[sync]\nticker = \"10m\"\n"
	settingsPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(settingsPath, []byte(custom), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	state := &initState{out: &out, paths: config.Paths{ConfigDir: dir}}
	if err := stepConfigFile(context.Background(), state); err != nil {
		t.Fatalf("stepConfigFile: %v", err)
	}

	got, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != custom {
		t.Fatalf("stepConfigFile modified an existing config.toml:\ngot:\n%s\nwant (unchanged):\n%s", got, custom)
	}
}

// --- step 8 (service) ---

func TestStepServiceSkipsWhenFlagSet(t *testing.T) {
	var out bytes.Buffer
	state := &initState{out: &out, skipService: true}
	if err := stepService(context.Background(), state); err != nil {
		t.Fatalf("stepService: %v", err)
	}
	if !strings.Contains(out.String(), "skip") {
		t.Fatalf("stepService --skip-service did not report skipping:\n%s", out.String())
	}
}

// --- step 9 (enrollment) / step 10 (first sync) ---

// fakeProvider is a minimal provider.Provider test double so
// stepEnrollment's tests control exactly what is "discovered" without
// depending on the real claude/codex adapters' filesystem layouts.
type fakeProvider struct {
	name       string
	scope      provider.Scope
	discovered []provider.Discovered
	identity   provider.Identity
	identifyFn func(d provider.Discovered, projectPath string) (provider.Identity, error)
}

func (f *fakeProvider) Name() string                                 { return f.name }
func (f *fakeProvider) Scope() provider.Scope                        { return f.scope }
func (f *fakeProvider) Patterns() []provider.Pattern                 { return nil }
func (f *fakeProvider) ReconcileIndex(context.Context, string) error { return nil }

func (f *fakeProvider) Discover(context.Context) ([]provider.Discovered, error) {
	return f.discovered, nil
}

func (f *fakeProvider) Identify(_ context.Context, d provider.Discovered, projectPath string) (provider.Identity, error) {
	if f.identifyFn != nil {
		return f.identifyFn(d, projectPath)
	}
	return f.identity, nil
}

// startFakeDaemonForEnrollment is init_test.go's own fake daemon variant:
// client_commands_test.go's startFakeDaemon doesn't serve /v0/track. It
// serves /v0/status (always ready), /v0/track (records each request —
// guarded by a mutex since the HTTP handler runs on its own goroutine),
// and /v0/sync (a canned completed summary). Uses os.MkdirTemp rather
// than t.TempDir(): the unix socket's sun_path has a ~104-byte limit
// that a t.TempDir() path (which embeds the full subtest name) can
// overrun (see internal/daemon/server_test.go's shortSocketDir).
func startFakeDaemonForEnrollment(t *testing.T, folderFor func(api.TrackRequest) string) func() []api.TrackRequest {
	t.Helper()
	dir, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", dir)

	var mu sync.Mutex
	var requests []api.TrackRequest

	mux := http.NewServeMux()
	mux.HandleFunc("/v0/status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.StatusResponse{State: "ready"})
	})
	mux.HandleFunc("/v0/track", func(w http.ResponseWriter, r *http.Request) {
		var req api.TrackRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		requests = append(requests, req)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(api.TrackResponse{Folder: folderFor(req)})
	})
	mux.HandleFunc("/v0/sync", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.SyncResponse{Status: "completed", Summary: &api.SyncSummary{Pushed: true}})
	})
	listener, err := net.Listen("unix", filepath.Join(dir, "agent-brain.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	return func() []api.TrackRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]api.TrackRequest(nil), requests...)
	}
}

// quiesceHits records what a fake daemon's /v0/quiesce route observed: the
// requested Seconds of each POST (a hold) and the count of DELETEs (resumes).
type quiesceHits struct {
	held    []int
	resumed int
}

// startFakeDaemonRecordingQuiesce serves /v0/status (always ready) plus
// /v0/quiesce (POST records the requested Seconds and replies with a deadline;
// DELETE counts a resume) on a short-path socket, pointing the CLI at it via
// AGENT_BRAIN_RUNTIME_DIR. It returns an accessor for what init's repo-state
// step or doctor --fix hit. Modeled on startFakeDaemonForEnrollment (this
// file); os.MkdirTemp keeps the socket under the sun_path limit.
func startFakeDaemonRecordingQuiesce(t *testing.T) func() quiesceHits {
	t.Helper()
	dir, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", dir)

	var mu sync.Mutex
	var hits quiesceHits

	mux := http.NewServeMux()
	mux.HandleFunc("/v0/status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.StatusResponse{State: "ready"})
	})
	mux.HandleFunc("/v0/quiesce", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			var req api.QuiesceRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			hits.held = append(hits.held, req.Seconds)
			_ = json.NewEncoder(w).Encode(api.QuiesceResponse{Until: time.Now().Add(time.Duration(req.Seconds) * time.Second)})
		case http.MethodDelete:
			hits.resumed++
			_ = json.NewEncoder(w).Encode(api.QuiesceResponse{})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	listener, err := net.Listen("unix", filepath.Join(dir, "agent-brain.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	return func() quiesceHits {
		mu.Lock()
		defer mu.Unlock()
		return quiesceHits{held: append([]int(nil), hits.held...), resumed: hits.resumed}
	}
}

func TestStepEnrollmentNoCandidatesPrintsNothingDiscovered(t *testing.T) {
	registry, err := provider.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	var out bytes.Buffer
	state := &initState{
		out:      &out,
		paths:    config.Paths{DataDir: dataDir},
		registry: registry,
		pickEnrollUnits: func([]enrollCandidate) ([]int, error) {
			t.Fatal("pickEnrollUnits must not be called with zero candidates")
			return nil, nil
		},
	}
	if err := stepEnrollment(context.Background(), state); err != nil {
		t.Fatalf("stepEnrollment: %v", err)
	}
	if !strings.Contains(out.String(), "no new memory roots") {
		t.Fatalf("output: %s", out.String())
	}
	if state.enrolledAny {
		t.Fatal("enrolledAny set with no candidates")
	}
}

func TestStepEnrollmentNothingSelectedIsANoOp(t *testing.T) {
	fp := &fakeProvider{name: "fakeproj", scope: provider.ScopePerProject, discovered: []provider.Discovered{
		{LocalDir: "/tmp/project-a/.claude/memory", Label: "project-a", PathGuess: "/tmp/project-a"},
	}}
	registry, err := provider.NewRegistry(fp)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	var out bytes.Buffer
	state := &initState{
		out:             &out,
		paths:           config.Paths{DataDir: dataDir},
		registry:        registry,
		pickEnrollUnits: func([]enrollCandidate) ([]int, error) { return nil, nil },
	}
	if err := stepEnrollment(context.Background(), state); err != nil {
		t.Fatalf("stepEnrollment: %v", err)
	}
	if !strings.Contains(out.String(), "nothing selected") {
		t.Fatalf("output: %s", out.String())
	}
	if state.enrolledAny {
		t.Fatal("enrolledAny set when nothing was selected")
	}
}

func TestStepEnrollmentDaemonUnreachablePrintsGuidanceAndDoesNotFail(t *testing.T) {
	fp := &fakeProvider{name: "fakeproj", scope: provider.ScopePerProject, discovered: []provider.Discovered{
		{LocalDir: "/tmp/project-a/.claude/memory", Label: "project-a", PathGuess: "/tmp/project-a"},
	}}
	registry, err := provider.NewRegistry(fp)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", t.TempDir()) // no socket inside
	var out bytes.Buffer
	state := &initState{
		out:                &out,
		paths:              config.Paths{DataDir: dataDir},
		registry:           registry,
		daemonPollTimeout:  20 * time.Millisecond,
		daemonPollInterval: 5 * time.Millisecond,
		pickEnrollUnits:    func([]enrollCandidate) ([]int, error) { return []int{0}, nil },
	}
	if err := stepEnrollment(context.Background(), state); err != nil {
		t.Fatalf("stepEnrollment: %v", err)
	}
	if !strings.Contains(out.String(), "daemon not reachable") {
		t.Fatalf("output: %s", out.String())
	}
	if state.enrolledAny {
		t.Fatal("enrolledAny set when the daemon was unreachable")
	}
}

func TestStepEnrollmentTracksChosenPerProjectCandidate(t *testing.T) {
	fp := &fakeProvider{
		name: "fakeproj", scope: provider.ScopePerProject,
		discovered: []provider.Discovered{
			{LocalDir: "/tmp/project-a/.claude/memory", Label: "project-a", PathGuess: "/tmp/project-a"},
		},
		identity: provider.Identity{ProjectID: "github.com/alice/project-a", PreferredFolder: "project-a"},
	}
	registry, err := provider.NewRegistry(fp)
	if err != nil {
		t.Fatal(err)
	}

	getRequests := startFakeDaemonForEnrollment(t, func(api.TrackRequest) string { return "project-a" })

	var out bytes.Buffer
	state := &initState{
		out:                  &out,
		paths:                config.Paths{DataDir: t.TempDir()},
		registry:             registry,
		pickEnrollUnits:      func([]enrollCandidate) ([]int, error) { return []int{0}, nil },
		confirmProjectPath:   func(guess string) (string, error) { return guess, nil },
		nameRemotelessFolder: func(string) (string, error) { t.Fatal("must not be called: candidate has a remote"); return "", nil },
	}
	if err := stepEnrollment(context.Background(), state); err != nil {
		t.Fatalf("stepEnrollment: %v", err)
	}
	if !state.enrolledAny {
		t.Fatal("enrolledAny not set after a successful Track")
	}
	requests := getRequests()
	if len(requests) != 1 {
		t.Fatalf("Track called %d times, want 1", len(requests))
	}
	want := api.TrackRequest{Provider: "fakeproj", ProjectID: "github.com/alice/project-a", PreferredFolder: "project-a", LocalDir: "/tmp/project-a/.claude/memory"}
	if diff := cmp.Diff(want, requests[0]); diff != "" {
		t.Fatalf("TrackRequest (-want +got):\n%s", diff)
	}
}

// TestStepEnrollmentNamesRemotelessProjectAndTracksWithNamedPrefix pins the
// named/<folder> contract (spec's canonical remoteless id, provider.go's
// Identity.ProjectID doc comment) at the wire: a remoteless per-project
// candidate whose human-provided folder name is accepted must produce a
// TrackRequest.ProjectID of literally "named/" + that folder name — not
// just "some non-empty string" — so a future refactor of enrollOne cannot
// silently drift the prefix without failing a test, not just a comment.
func TestStepEnrollmentNamesRemotelessProjectAndTracksWithNamedPrefix(t *testing.T) {
	fp := &fakeProvider{
		name: "fakeproj", scope: provider.ScopePerProject,
		discovered: []provider.Discovered{
			{LocalDir: "/tmp/project-c/.claude/memory", Label: "project-c", PathGuess: "/tmp/project-c"},
		},
		identity: provider.Identity{}, // remoteless: empty ProjectID
	}
	registry, err := provider.NewRegistry(fp)
	if err != nil {
		t.Fatal(err)
	}

	getRequests := startFakeDaemonForEnrollment(t, func(api.TrackRequest) string { return "my-chosen-folder" })

	var out bytes.Buffer
	state := &initState{
		out:                  &out,
		paths:                config.Paths{DataDir: t.TempDir()},
		registry:             registry,
		pickEnrollUnits:      func([]enrollCandidate) ([]int, error) { return []int{0}, nil },
		confirmProjectPath:   func(guess string) (string, error) { return guess, nil },
		nameRemotelessFolder: func(string) (string, error) { return "my-chosen-folder", nil },
	}
	if err := stepEnrollment(context.Background(), state); err != nil {
		t.Fatalf("stepEnrollment: %v", err)
	}
	requests := getRequests()
	if len(requests) != 1 {
		t.Fatalf("Track called %d times, want 1", len(requests))
	}
	want := api.TrackRequest{
		Provider:        "fakeproj",
		ProjectID:       "named/my-chosen-folder",
		PreferredFolder: "my-chosen-folder",
		LocalDir:        "/tmp/project-c/.claude/memory",
	}
	if diff := cmp.Diff(want, requests[0]); diff != "" {
		t.Fatalf("TrackRequest (-want +got):\n%s", diff)
	}
}

func TestStepEnrollmentSkipsRemotelessUnderEnrollAll(t *testing.T) {
	fp := &fakeProvider{
		name: "fakeproj", scope: provider.ScopePerProject,
		discovered: []provider.Discovered{
			{LocalDir: "/tmp/project-b/.claude/memory", Label: "project-b", PathGuess: "/tmp/project-b"},
		},
		identity: provider.Identity{}, // remoteless: empty ProjectID
	}
	registry, err := provider.NewRegistry(fp)
	if err != nil {
		t.Fatal(err)
	}
	getRequests := startFakeDaemonForEnrollment(t, func(api.TrackRequest) string { return "project-b" })

	var out bytes.Buffer
	state := &initState{
		out:      &out,
		paths:    config.Paths{DataDir: t.TempDir()},
		registry: registry,
		pickEnrollUnits: func(candidates []enrollCandidate) ([]int, error) {
			indices := make([]int, len(candidates))
			for i := range candidates {
				indices[i] = i
			}
			return indices, nil
		},
		confirmProjectPath:   func(guess string) (string, error) { return guess, nil },
		nameRemotelessFolder: func(string) (string, error) { return "", errSkipRemoteless },
	}
	if err := stepEnrollment(context.Background(), state); err != nil {
		t.Fatalf("stepEnrollment: %v", err)
	}
	if state.enrolledAny {
		t.Fatal("enrolledAny set for a skipped remoteless project")
	}
	if len(getRequests()) != 0 {
		t.Fatal("Track was called for a remoteless project that should have been skipped")
	}
	if !strings.Contains(out.String(), "remoteless") {
		t.Fatalf("output does not explain the skip: %s", out.String())
	}
}

func TestStepEnrollmentGlobalScopeGroupsAllRootsIntoOneCandidateTrackedSeparately(t *testing.T) {
	fp := &fakeProvider{
		name: "fakeglobal", scope: provider.ScopeGlobal,
		discovered: []provider.Discovered{
			{LocalDir: "/home/x/.codex/memories", Label: "memories", RepoSubdir: ""},
			{LocalDir: "/home/x/.codex/chronicle", Label: "chronicle", RepoSubdir: "chronicle"},
		},
	}
	registry, err := provider.NewRegistry(fp)
	if err != nil {
		t.Fatal(err)
	}
	getRequests := startFakeDaemonForEnrollment(t, func(api.TrackRequest) string { return "_global" })

	var out bytes.Buffer
	state := &initState{
		out:      &out,
		paths:    config.Paths{DataDir: t.TempDir()},
		registry: registry,
		pickEnrollUnits: func(candidates []enrollCandidate) ([]int, error) {
			if len(candidates) != 1 {
				t.Fatalf("global-scope roots must collapse into ONE candidate, got %d", len(candidates))
			}
			return []int{0}, nil
		},
	}
	if err := stepEnrollment(context.Background(), state); err != nil {
		t.Fatalf("stepEnrollment: %v", err)
	}
	requests := getRequests()
	if len(requests) != 2 {
		t.Fatalf("Track called %d times, want 2 (one per global root)", len(requests))
	}
	var localDirs []string
	for _, r := range requests {
		localDirs = append(localDirs, r.LocalDir)
	}
	sort.Strings(localDirs)
	want := []string{"/home/x/.codex/chronicle", "/home/x/.codex/memories"}
	if diff := cmp.Diff(want, localDirs); diff != "" {
		t.Fatalf("tracked local dirs (-want +got):\n%s", diff)
	}
}

func TestStepEnrollmentFiltersAlreadyEnrolledUnits(t *testing.T) {
	fp := &fakeProvider{
		name: "fakeproj", scope: provider.ScopePerProject,
		discovered: []provider.Discovered{
			{LocalDir: "/tmp/already/.claude/memory", Label: "already", PathGuess: "/tmp/already"},
		},
	}
	registry, err := provider.NewRegistry(fp)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	local := repo.NewLocalRegistry()
	if err := local.Enroll(repo.Unit{Provider: "fakeproj", Folder: "already", LocalDir: "/tmp/already/.claude/memory"}); err != nil {
		t.Fatal(err)
	}
	paths := config.Paths{DataDir: dataDir}
	if err := local.Save(paths.LocalRegistryFile()); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	state := &initState{
		out:      &out,
		paths:    paths,
		registry: registry,
		pickEnrollUnits: func([]enrollCandidate) ([]int, error) {
			t.Fatal("pickEnrollUnits must not be called: everything is already enrolled")
			return nil, nil
		},
	}
	if err := stepEnrollment(context.Background(), state); err != nil {
		t.Fatalf("stepEnrollment: %v", err)
	}
	if !strings.Contains(out.String(), "no new memory roots") {
		t.Fatalf("output: %s", out.String())
	}
}

func TestStepFirstSyncSkipsWhenNothingWasEnrolled(t *testing.T) {
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", t.TempDir()) // hermetic: no daemon here regardless
	dir := t.TempDir()
	var out bytes.Buffer
	state := &initState{out: &out, home: dir, enrolledAny: false}

	start := time.Now()
	if err := stepFirstSync(context.Background(), state); err != nil {
		t.Fatalf("stepFirstSync: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("stepFirstSync took %s; it must never poll for the daemon when nothing was enrolled (the default poll timeout is 15s)", elapsed)
	}
	if !strings.Contains(out.String(), "next:") {
		t.Fatalf("output missing next-steps guidance: %s", out.String())
	}
}

func TestStepFirstSyncRunsWhenSomethingWasEnrolled(t *testing.T) {
	startFakeDaemonForEnrollment(t, func(api.TrackRequest) string { return "x" })
	dir := t.TempDir()
	var out bytes.Buffer
	state := &initState{out: &out, home: dir, enrolledAny: true}
	if err := stepFirstSync(context.Background(), state); err != nil {
		t.Fatalf("stepFirstSync: %v", err)
	}
	if !strings.Contains(out.String(), "sync completed") {
		t.Fatalf("output: %s", out.String())
	}
}

func TestStepFirstSyncSuggestsMigrateWhenLegacyTreeExists(t *testing.T) {
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", t.TempDir())
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".agent-brain"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	state := &initState{out: &out, home: home, enrolledAny: false}
	if err := stepFirstSync(context.Background(), state); err != nil {
		t.Fatalf("stepFirstSync: %v", err)
	}
	if !strings.Contains(out.String(), "agent-brain migrate") {
		t.Fatalf("output missing migrate tie-in: %s", out.String())
	}
}

func TestEnsureDaemonClientCachesUnreachableResultWithoutRepolling(t *testing.T) {
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", t.TempDir())
	state := &initState{daemonPollTimeout: 30 * time.Millisecond, daemonPollInterval: 5 * time.Millisecond}

	first := ensureDaemonClient(context.Background(), state)
	if first != nil {
		t.Fatal("expected nil: nothing is listening")
	}

	start := time.Now()
	second := ensureDaemonClient(context.Background(), state)
	if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
		t.Fatalf("second call re-polled (%s) instead of using the cached result", elapsed)
	}
	if second != nil {
		t.Fatal("expected the cached nil result")
	}
}

// --- whole-flow idempotency (init.go's runInit) ---

// TestInitNonInteractiveFullFlowIsIdempotentOnSecondRun drives runInit
// (init.go's orchestrator, the same function newInitCmd's RunE calls)
// twice against the same machine fixture — the second call simulates a
// user re-running `agent-brain init` on an already-provisioned machine
// (a common, supported scenario: init repairs forward, same philosophy
// as doctor). Every step must recognize its work is already done: zero
// new commits land in the memories repo, and each step reports a
// skip/verify outcome rather than a first-time action.
//
// Between the two runs the test fixes up the checkout's origin to a
// github.com-style URL, exactly as TestStepRepoNoOpWhenLocalCheckoutAlreadyMatches
// does: fakeGHRunner's "repo clone" case runs a REAL git clone from a
// local bare-repo stand-in (so steps 4-6 exercise real git), which
// naturally leaves origin pointing at that local path rather than the
// GitHub URL a real `gh repo clone` would have set — a fixture
// limitation, not something runInit itself needs to handle.
func TestInitNonInteractiveFullFlowIsIdempotentOnSecondRun(t *testing.T) {
	provisionUninitializedMachine(t)

	base := t.TempDir()
	bareRemote := filepath.Join(base, "remote.git")
	mustGitCLI(t, base, "init", "--bare", "-b", "main", bareRemote)
	runner := newFakeGHRunner(t, "alice", "agent-brain-memories", bareRemote)
	gh := ghx.NewClientWithRunner(runner, "/usr/bin/gh")

	buildState := func(out *bytes.Buffer) *initState {
		state := &initState{
			out:            out,
			nonInteractive: true,
			repoName:       "agent-brain-memories",
			skipService:    true,
			generateKey:    true,
			gh:             gh,
		}
		wireEnrollmentCallbacks(state, false)
		return state
	}

	ctx := context.Background()
	var firstOut bytes.Buffer
	first := buildState(&firstOut)
	if err := runInit(ctx, first, false); err != nil {
		t.Fatalf("first run: %v\noutput:\n%s", err, firstOut.String())
	}

	memories := first.paths.MemoriesDir()
	mustGitCLI(t, memories, "remote", "set-url", "origin", "https://github.com/alice/agent-brain-memories")

	firstLog, err := gitx.Run(ctx, memories, "log", "--oneline")
	if err != nil {
		t.Fatal(err)
	}

	var secondOut bytes.Buffer
	second := buildState(&secondOut)
	if err := runInit(ctx, second, false); err != nil {
		t.Fatalf("second run: %v\noutput:\n%s", err, secondOut.String())
	}

	secondLog, err := gitx.Run(ctx, memories, "log", "--oneline")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(firstLog.Stdout, secondLog.Stdout); diff != "" {
		t.Fatalf("second run changed the commit history (-first +second):\n%s", diff)
	}

	secondText := secondOut.String()
	for _, unwanted := range []string{
		"repo: created", "repo: cloned",
		"initialized a fresh checkout",
		"wrote defaults",
	} {
		if strings.Contains(secondText, unwanted) {
			t.Fatalf("second run repeated a first-run-only action %q:\noutput:\n%s", unwanted, secondText)
		}
	}
	for _, wanted := range []string{
		"validated",                // keyset
		"matches",                  // repo origin
		"already canonical",        // repo state
		"left untouched",           // config file
		"skipped (--skip-service)", // service
	} {
		if !strings.Contains(secondText, wanted) {
			t.Fatalf("second run missing expected skip/verify marker %q:\noutput:\n%s", wanted, secondText)
		}
	}
}
