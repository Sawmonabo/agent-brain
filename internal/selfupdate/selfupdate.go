// Package selfupdate replaces the running agent-brain binary with a newer
// GitHub release (spec §7 `update`, ADR 18). Release resolution and asset
// download go through gh — the product's hard prerequisite, already
// authenticated, so a private distribution repo needs no separate token or
// HTTP client. The pipeline is: resolve target release → download archive +
// checksums → verify sha256 → extract → sanity-exec the new binary → then,
// and only then, atomically rename it over the current one. Every failure
// before that final rename leaves the installed binary untouched.
package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/mod/semver"

	"github.com/Sawmonabo/agent-brain/internal/ghx"
)

// Typed sentinels, identity-assertable with errors.Is through every %w wrap
// — mirrors internal/service's sentinel discipline (never string-match error
// text). runUpdate surfaces their self-remediating text verbatim rather than
// branching per sentinel; the test suite (and any scripted caller) asserts
// on them with errors.Is.
var (
	// ErrDevBuild refuses to update a non-release build: "dev" carries no
	// comparable version, and a from-source binary is the developer's own
	// to rebuild, not this command's to overwrite.
	ErrDevBuild = errors.New("this is a dev build, not an installed release — rebuild from source instead of updating")
	// ErrBrewManaged refuses to update a Homebrew-installed binary:
	// self-updating behind brew's back desyncs the Cellar's idea of what
	// is installed from what actually runs.
	ErrBrewManaged = errors.New("this binary is managed by Homebrew — run `brew upgrade agent-brain` instead")
	// ErrNoRelease means resolution found no installable release — the repo
	// publishes none, or the pinned version does not exist.
	ErrNoRelease = errors.New("no matching release found")
	// ErrChecksumMismatch means the downloaded archive did not hash to the
	// value the release's checksums file promises for it.
	ErrChecksumMismatch = errors.New("downloaded archive does not match its published checksum")
)

// ReleaseSource lists releases and downloads their assets — implemented by
// ghx.Client in production, scripted by a fake in tests.
type ReleaseSource interface {
	ListReleases(ctx context.Context, ownerRepo string, limit int) ([]ghx.ReleaseInfo, error)
	DownloadReleaseAssets(ctx context.Context, ownerRepo, tag, dir string, patterns ...string) error
}

// Options describe one update attempt.
type Options struct {
	// Repo is the distribution repository ("owner/name").
	Repo string
	// CurrentVersion is the running binary's ldflags-stamped version,
	// without a leading "v" ("2.0.0-rc.2"), or "dev" for source builds.
	CurrentVersion string
	// TargetPath is the resolved (symlink-free) path of the binary to
	// replace.
	TargetPath string
	// RequestedVersion pins the update to one exact release ("2.0.0-rc.1"
	// or "v2.0.0-rc.1") instead of resolving the newest. Unlike implicit
	// resolution, an explicitly requested OLDER release is honored
	// (deliberate rollback), reported through Decision.Downgrade so the CLI
	// can warn.
	RequestedVersion string
	// GOOS and GOARCH select the release asset (runtime values in
	// production; fixed values in tests).
	GOOS, GOARCH string
}

// Decision is Check's verdict.
type Decision struct {
	// Latest is the resolved target release tag ("v2.0.0-rc.2"): the
	// highest non-draft tag, or the pinned tag when Options.RequestedVersion
	// is set.
	Latest string
	// UpdateNeeded reports whether installing Latest would change the
	// running binary. Implicit resolution sets it only for a strictly
	// newer release (never downgrade); an explicitly requested version
	// sets it for any version other than the running one.
	UpdateNeeded bool
	// Downgrade reports that Latest is OLDER than the running version —
	// possible only for an explicitly requested version. The CLI warns:
	// state written by the newer version may not load under the older
	// binary (config parsing is strict, ADR 17).
	Downgrade bool
}

// Updater wires the seams. Getenv feeds the Homebrew-Cellar guard —
// os.Getenv in production, a scripted map in tests.
type Updater struct {
	Source ReleaseSource
	Getenv func(string) string
}

// ReleaseListLimit bounds the release-list fetch — Check's resolution
// window and the CLI picker's list share it. Far above any realistic
// distance between the running version and the newest release; the semver
// max over the window still picks correctly even if old rows fall off.
const ReleaseListLimit = 50

// maxBinaryBytes caps extraction as a decompression-bomb guard — the real
// binary is ~16 MB, so an archive claiming orders of magnitude more is
// corrupt or hostile, never legitimate.
const maxBinaryBytes = 256 << 20

// sanityExecTimeout bounds the freshly extracted binary's `--version`
// probe; sanityKillWaitDelay bounds cleanup after a kill, exactly like
// migrate's preflight (a descendant holding the output pipe must not block
// Wait past the deadline the timeout promised).
const (
	sanityExecTimeout   = 15 * time.Second
	sanityKillWaitDelay = 2 * time.Second
)

// Check resolves the target release — the newest non-draft release, or
// exactly Options.RequestedVersion when set — and reports whether
// installing it would change the running binary. The dev-build and
// Homebrew guards run here, before any network call, so `update --check`
// refuses exactly where `update` would.
func (u *Updater) Check(ctx context.Context, opts Options) (Decision, error) {
	if opts.CurrentVersion == "dev" {
		return Decision{}, ErrDevBuild
	}
	current := "v" + opts.CurrentVersion
	if !semver.IsValid(current) {
		return Decision{}, fmt.Errorf("running version %q is not valid semver", opts.CurrentVersion)
	}
	if brewManaged(opts.TargetPath, u.Getenv) {
		return Decision{}, ErrBrewManaged
	}

	releases, err := u.Source.ListReleases(ctx, opts.Repo, ReleaseListLimit)
	if err != nil {
		return Decision{}, err
	}
	if opts.RequestedVersion != "" {
		return resolveRequested(current, opts, releases)
	}
	latest := ""
	for _, release := range releases {
		if release.IsDraft {
			continue
		}
		if !semver.IsValid(release.TagName) {
			continue
		}
		if latest == "" || semver.Compare(release.TagName, latest) > 0 {
			latest = release.TagName
		}
	}
	if latest == "" {
		return Decision{}, fmt.Errorf("%w in %s", ErrNoRelease, opts.Repo)
	}
	return Decision{Latest: latest, UpdateNeeded: semver.Compare(latest, current) > 0}, nil
}

// resolveRequested pins the decision to an explicitly named release. An
// older release is honored with Downgrade set, because deliberate rollback
// is exactly why an operator names a version. Drafts stay invisible: they
// are unpublished by definition. Matching is by semver equality, so
// "2.1.0" and "v2.1.0" both resolve to the real tag.
func resolveRequested(current string, opts Options, releases []ghx.ReleaseInfo) (Decision, error) {
	requested := "v" + strings.TrimPrefix(opts.RequestedVersion, "v")
	if !semver.IsValid(requested) {
		return Decision{}, fmt.Errorf("requested version %q is not valid semver", opts.RequestedVersion)
	}
	for _, release := range releases {
		if release.IsDraft || !semver.IsValid(release.TagName) {
			continue
		}
		if semver.Compare(release.TagName, requested) != 0 {
			continue
		}
		comparison := semver.Compare(release.TagName, current)
		return Decision{
			Latest:       release.TagName,
			UpdateNeeded: comparison != 0,
			Downgrade:    comparison < 0,
		}, nil
	}
	return Decision{}, fmt.Errorf("%w: release %s does not exist in %s (`agent-brain update --list` shows what does)",
		ErrNoRelease, requested, opts.Repo)
}

// Apply downloads targetTag's archive for this platform, verifies it
// against the release's checksums file, extracts the binary, sanity-execs
// it, and atomically renames it over Options.TargetPath. The staging file
// lives in the target's own directory so the final rename never crosses a
// filesystem; the running daemon keeps its old inode and is unaffected
// until restarted.
func (u *Updater) Apply(ctx context.Context, opts Options, targetTag string) (retErr error) {
	version := strings.TrimPrefix(targetTag, "v")
	archiveName := fmt.Sprintf("agent-brain_%s_%s_%s.tar.gz", version, opts.GOOS, opts.GOARCH)
	checksumsName := fmt.Sprintf("agent-brain_%s_checksums.txt", version)

	downloadDir, err := os.MkdirTemp("", "agent-brain-update-")
	if err != nil {
		return err
	}
	defer func() {
		if removeErr := os.RemoveAll(downloadDir); retErr == nil && removeErr != nil {
			retErr = removeErr
		}
	}()

	if err := u.Source.DownloadReleaseAssets(ctx, opts.Repo, targetTag, downloadDir, archiveName, checksumsName); err != nil {
		return err
	}
	archivePath := filepath.Join(downloadDir, archiveName)
	if err := verifyChecksum(archivePath, filepath.Join(downloadDir, checksumsName), archiveName); err != nil {
		return err
	}
	extractedPath := filepath.Join(downloadDir, "agent-brain")
	if err := extractBinary(archivePath, extractedPath); err != nil {
		return err
	}
	if err := sanityExec(ctx, extractedPath, version); err != nil {
		return err
	}
	return replaceBinary(extractedPath, opts.TargetPath)
}

// brewManaged reports whether path lives inside a Homebrew Cellar —
// $HOMEBREW_CELLAR when set, plus the conventional /Cellar/ path segment
// that covers /opt/homebrew, /usr/local, and Linuxbrew prefixes alike.
func brewManaged(path string, getenv func(string) string) bool {
	if cellar := getenv("HOMEBREW_CELLAR"); cellar != "" && strings.HasPrefix(path, cellar+string(os.PathSeparator)) {
		return true
	}
	return strings.Contains(path, "/Cellar/")
}

// verifyChecksum hashes archivePath and compares it against archiveName's
// row in the release's checksums file (goreleaser format: "<sha256hex>
// <name>" per line).
func verifyChecksum(archivePath, checksumsPath, archiveName string) error {
	checksums, err := os.ReadFile(checksumsPath) //nolint:gosec // G304: path is built from our own MkdirTemp dir, not untrusted input
	if err != nil {
		return err
	}
	want := ""
	for line := range strings.SplitSeq(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == archiveName {
			want = fields[0]
			break
		}
	}
	if want == "" {
		return fmt.Errorf("checksums file has no entry for %s", archiveName)
	}

	archive, err := os.Open(archivePath) //nolint:gosec // G304: same MkdirTemp-derived path
	if err != nil {
		return err
	}
	defer archive.Close() //nolint:errcheck // read-only descriptor

	hash := sha256.New()
	if _, err := io.Copy(hash, archive); err != nil {
		return err
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("%w: %s hashed to %s, checksums file says %s", ErrChecksumMismatch, archiveName, got, want)
	}
	return nil
}

// extractBinary pulls exactly the top-level "agent-brain" entry out of the
// tar.gz archive into destPath (0755). Every other entry — including
// anything path-qualified — is ignored, which forecloses the zip-slip
// class outright; maxBinaryBytes caps the copy against decompression
// bombs.
func extractBinary(archivePath, destPath string) (retErr error) {
	archive, err := os.Open(archivePath) //nolint:gosec // G304: MkdirTemp-derived path
	if err != nil {
		return err
	}
	defer archive.Close() //nolint:errcheck // read-only descriptor

	gzipReader, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("open %s: %w", filepath.Base(archivePath), err)
	}
	defer gzipReader.Close() //nolint:errcheck // read-only descriptor

	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("archive %s has no agent-brain binary", filepath.Base(archivePath))
		}
		if err != nil {
			return err
		}
		if header.Name != "agent-brain" || header.Typeflag != tar.TypeReg {
			continue
		}
		destination, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755) //nolint:gosec // G302/G304: an executable needs 0755; path is MkdirTemp-derived
		if err != nil {
			return err
		}
		defer func() {
			if closeErr := destination.Close(); retErr == nil && closeErr != nil {
				retErr = closeErr
			}
		}()
		written, err := io.Copy(destination, io.LimitReader(tarReader, maxBinaryBytes+1))
		if err != nil {
			return err
		}
		if written > maxBinaryBytes {
			return fmt.Errorf("archive entry agent-brain exceeds %d bytes — refusing as corrupt", int64(maxBinaryBytes))
		}
		return nil
	}
}

// sanityExec runs the freshly extracted binary's `--version` and requires
// a clean exit that names the target version — the last gate before the
// rename, catching a wrong-arch, truncated, or otherwise unrunnable
// download while the installed binary is still untouched. The process
// group + WaitDelay treatment mirrors migrate's preflight: a kill must
// also take any descendant holding the output pipe.
func sanityExec(ctx context.Context, binaryPath, wantVersion string) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, sanityExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(timeoutCtx, binaryPath, "--version")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = sanityKillWaitDelay
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("new binary failed its --version probe (installed binary untouched): %w", err)
	}
	if !strings.Contains(string(output), wantVersion) {
		return fmt.Errorf("new binary reports %q, want it to name %s (installed binary untouched)", strings.TrimSpace(string(output)), wantVersion)
	}
	return nil
}

// replaceBinary stages sourcePath beside targetPath (same directory, so
// the rename below never crosses a filesystem) and renames it over the
// target. The rename is the single point of mutation: everything before
// it, and any failure in it, leaves the installed binary as it was.
func replaceBinary(sourcePath, targetPath string) (retErr error) {
	staged, err := os.CreateTemp(filepath.Dir(targetPath), ".agent-brain-update-*")
	if err != nil {
		return err
	}
	stagedPath := staged.Name()
	defer func() {
		if retErr != nil {
			os.Remove(stagedPath) //nolint:errcheck,gosec // best-effort cleanup on the error path
		}
	}()

	source, err := os.Open(sourcePath) //nolint:gosec // G304: MkdirTemp-derived path
	if err != nil {
		staged.Close() //nolint:errcheck,gosec // error path; the deferred Remove is the real cleanup
		return err
	}
	_, copyErr := io.Copy(staged, source)
	source.Close() //nolint:errcheck,gosec // read-only descriptor
	if copyErr != nil {
		staged.Close() //nolint:errcheck,gosec // error path; the deferred Remove is the real cleanup
		return copyErr
	}
	if err := staged.Chmod(0o755); err != nil {
		staged.Close() //nolint:errcheck,gosec // error path; the deferred Remove is the real cleanup
		return err
	}
	if err := staged.Close(); err != nil {
		return err
	}
	return os.Rename(stagedPath, targetPath)
}
