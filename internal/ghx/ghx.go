// Package ghx wraps the gh CLI — the only auth agent-brain borrows (ADR 08).
// v1 requires gh: init and doctor verify it is installed and logged in, and
// every daemon push/pull rides gh's own credential storage thereafter — this
// package persists no token of its own.
//
// NEVER call `gh auth setup-git`: it writes gh's absolute path into the
// user's GLOBAL gitconfig, which breaks a gitconfig synced across machines
// with different install paths (cli/cli#9438) — a direct hazard for a
// chezmoi-managed dotfiles user. NEVER call `gh auth token` either: it would
// put a live token in this process's memory and argv for no benefit over the
// credential-helper indirection gitx.InstallCredentialHelper wires up.
//
// Every gh invocation goes through Runner, so the whole package is fakeable
// in tests (see ghxtest.Fake) without a network connection or a real gh
// binary.
package ghx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ErrMissing means no gh binary is on PATH. Every message names the fix.
var ErrMissing = errors.New("gh CLI not found — install it (https://cli.github.com) and run `gh auth login`")

// errRepoNotFound is gh's own stderr signature for "no such repository" —
// case-sensitive, as gh emits it. Matching it precisely is what lets
// RepoExists disambiguate "confirmed absent" from any other failure: a
// network outage must never read as "repo missing", or a caller (init's
// provisioning pre-flight) would race to create a repo that may already
// exist for an unrelated reason.
const errRepoNotFound = "Could not resolve to a Repository"

// Result carries a finished gh invocation (same shape idiom as gitx.Result).
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner executes gh. execRunner is process-global reality; ghxtest.Fake
// scripts it for tests.
type Runner interface {
	Run(ctx context.Context, args ...string) (Result, error)
}

// Client wraps the gh operations agent-brain needs. Zero persistence: no
// token ever leaves gh's own storage (ADR 08).
type Client struct {
	runner     Runner
	binaryPath string
}

// NewClient locates gh on PATH and wires the real, exec-backed Runner.
func NewClient() (*Client, error) {
	path, err := exec.LookPath("gh")
	if err != nil {
		return nil, ErrMissing
	}
	return NewClientWithRunner(&execRunner{binaryPath: path}, path), nil
}

// NewClientWithRunner wires an explicit Runner and binary path — the seam
// tests (ghxtest.Fake) and init/doctor (a path already resolved once) use.
func NewClientWithRunner(runner Runner, binaryPath string) *Client {
	return &Client{runner: runner, binaryPath: binaryPath}
}

// BinaryPath returns the absolute gh path this client runs — the same value
// gitx.InstallCredentialHelper wires into the repo-local credential helper.
func (c *Client) BinaryPath() string {
	return c.binaryPath
}

// AuthOK reports whether gh has a usable login. A non-zero exit surfaces
// gh's own stderr alongside the fix.
func (c *Client) AuthOK(ctx context.Context) error {
	result, err := c.runner.Run(ctx, "auth", "status")
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("gh auth status: %s (run `gh auth login`)", strings.TrimSpace(result.Stderr))
	}
	return nil
}

// Login returns the authenticated gh user's login name.
func (c *Client) Login(ctx context.Context) (string, error) {
	result, err := c.runner.Run(ctx, "api", "user", "--jq", ".login")
	if err != nil {
		return "", err
	}
	login := strings.TrimSpace(result.Stdout)
	if result.ExitCode != 0 || login == "" {
		return "", fmt.Errorf("gh api user --jq .login: %s", strings.TrimSpace(result.Stderr))
	}
	return login, nil
}

// RepoExists reports whether owner/name exists. A confirmed-absent repo
// (gh's own not-found signature) returns (false, nil); any other failure —
// notably a network error — is surfaced as an error rather than silently
// read as "does not exist" (see errRepoNotFound).
func (c *Client) RepoExists(ctx context.Context, owner, name string) (bool, error) {
	result, err := c.runner.Run(ctx, "repo", "view", owner+"/"+name, "--json", "name")
	if err != nil {
		return false, err
	}
	if result.ExitCode == 0 {
		return true, nil
	}
	if strings.Contains(result.Stderr, errRepoNotFound) {
		return false, nil
	}
	return false, fmt.Errorf("gh repo view %s/%s: %s", owner, name, strings.TrimSpace(result.Stderr))
}

// CreateRepo creates a private repository and returns its URL (gh prints it
// to stdout). Always private: this repo holds nothing but encrypted memory
// content, but it is never public by construction (ADR 08).
func (c *Client) CreateRepo(ctx context.Context, name, description string) (string, error) {
	result, err := c.runner.Run(ctx, "repo", "create", name, "--private", "--description", description)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("gh repo create %s: %s", name, strings.TrimSpace(result.Stderr))
	}
	return strings.TrimSpace(result.Stdout), nil
}

// Clone clones ownerRepo into dir, passing gitArgs through to the underlying
// git invocation after a `--` separator. With no gitArgs, no separator is
// emitted.
func (c *Client) Clone(ctx context.Context, ownerRepo, dir string, gitArgs ...string) error {
	args := []string{"repo", "clone", ownerRepo, dir}
	if len(gitArgs) > 0 {
		args = append(args, "--")
		args = append(args, gitArgs...)
	}
	result, err := c.runner.Run(ctx, args...)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("gh repo clone %s: %s", ownerRepo, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// ReleaseInfo is one release row from `gh release list` (spec §7 `update`).
// Drafts are included as gh reports them — callers filter; this package
// only translates.
type ReleaseInfo struct {
	TagName      string `json:"tagName"`
	IsPrerelease bool   `json:"isPrerelease"`
	IsDraft      bool   `json:"isDraft"`
}

// ListReleases returns up to limit of ownerRepo's releases, in gh's own
// (publication-date) order. Callers wanting "the latest version" must pick
// by semver themselves — publication order and version order can disagree
// (a patch release for an older minor publishes later).
func (c *Client) ListReleases(ctx context.Context, ownerRepo string, limit int) ([]ReleaseInfo, error) {
	result, err := c.runner.Run(ctx, "release", "list", "--repo", ownerRepo,
		"--limit", strconv.Itoa(limit), "--json", "tagName,isPrerelease,isDraft")
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("gh release list --repo %s: %s", ownerRepo, strings.TrimSpace(result.Stderr))
	}
	var releases []ReleaseInfo
	if err := json.Unmarshal([]byte(result.Stdout), &releases); err != nil {
		return nil, fmt.Errorf("gh release list --repo %s: parse json: %w", ownerRepo, err)
	}
	return releases, nil
}

// DownloadReleaseAssets downloads tag's release assets matching the given
// glob patterns into dir. gh performs the download with the user's existing
// authentication, so this works against a private repo with no separate
// token plumbing — the reason `update` shells gh instead of carrying its
// own HTTP client (ADR 18).
func (c *Client) DownloadReleaseAssets(ctx context.Context, ownerRepo, tag, dir string, patterns ...string) error {
	args := []string{"release", "download", tag, "--repo", ownerRepo, "--dir", dir}
	for _, pattern := range patterns {
		args = append(args, "--pattern", pattern)
	}
	result, err := c.runner.Run(ctx, args...)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("gh release download %s --repo %s: %s", tag, ownerRepo, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// waitDelay bounds cleanup after a gh invocation's context is canceled or gh
// exits while an I/O pipe is still held open — same rationale as gitx's
// identical constant (internal/gitx/gitx.go): never let a straggling child
// hang this daemon indefinitely.
const waitDelay = 10 * time.Second

// execRunner shells the real gh binary. args come only from this package's
// own call sites, never unsanitized user input — exec-ing gh with caller
// args is this runner's entire purpose (mirrors gitx's ADR 06 rationale).
type execRunner struct {
	binaryPath string
}

// Run implements Runner by mirroring gitx.RunStatus's contract exactly: a
// normal exit reports its code as data (nil error); a canceled/expired
// context or a signal-terminated child is surfaced as an error, never mapped
// to a bogus exit code a caller could misread as real data; a spawn failure
// (bad path, not executable) is also an error.
func (r *execRunner) Run(ctx context.Context, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, r.binaryPath, args...) //nolint:gosec // G204: binaryPath comes from exec.LookPath or a caller-supplied gh path, args are internal to this package; no untrusted-input boundary.
	cmd.WaitDelay = waitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	// A canceled/expired context kills gh; report that rather than mapping a
	// signal-kill (ExitCode -1) to data a caller would misread as real exit
	// status. Checked before the exit code so a genuine non-zero exit under a
	// live context stays data.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, fmt.Errorf("gh %v: %w", args, ctxErr)
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return result, nil
	case errors.As(err, &exitErr):
		if !exitErr.Exited() {
			return result, fmt.Errorf("gh %v terminated by signal: %w", args, err)
		}
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	default:
		return result, fmt.Errorf("spawn gh %v: %w", args, err)
	}
}
