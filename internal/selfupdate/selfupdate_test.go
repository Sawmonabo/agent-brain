package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/ghx"
)

// fakeSource scripts ReleaseSource: releases feed ListReleases, and
// download (when set) materializes asset files into the destination dir
// exactly the way `gh release download` would.
type fakeSource struct {
	releases    []ghx.ReleaseInfo
	listErr     error
	download    func(dir string) error
	downloadErr error

	listCalls     int
	downloadCalls int
}

func (f *fakeSource) ListReleases(_ context.Context, _ string, _ int) ([]ghx.ReleaseInfo, error) {
	f.listCalls++
	return f.releases, f.listErr
}

func (f *fakeSource) DownloadReleaseAssets(_ context.Context, _, _, dir string, _ ...string) error {
	f.downloadCalls++
	if f.downloadErr != nil {
		return f.downloadErr
	}
	if f.download != nil {
		return f.download(dir)
	}
	return nil
}

// noEnv is the Getenv seam for tests that want no Homebrew variables set.
func noEnv(string) string { return "" }

func TestCheckRefusesDevBuild(t *testing.T) {
	t.Parallel()
	updater := &Updater{Source: &fakeSource{}, Getenv: noEnv}
	_, err := updater.Check(t.Context(), Options{CurrentVersion: "dev", TargetPath: "/usr/local/bin/agent-brain"})
	if !errors.Is(err, ErrDevBuild) {
		t.Fatalf("Check error = %v, want errors.Is(_, ErrDevBuild)", err)
	}
}

func TestCheckRefusesInvalidCurrentVersion(t *testing.T) {
	t.Parallel()
	updater := &Updater{Source: &fakeSource{}, Getenv: noEnv}
	_, err := updater.Check(t.Context(), Options{CurrentVersion: "not-a-version", TargetPath: "/usr/local/bin/agent-brain"})
	if err == nil || !strings.Contains(err.Error(), "not valid semver") {
		t.Fatalf("Check error = %v, want an invalid-semver refusal", err)
	}
}

// TestCheckRefusesBrewManagedInstall proves both detection paths: the
// conventional /Cellar/ segment (any prefix — /opt/homebrew, /usr/local,
// Linuxbrew) and an explicit $HOMEBREW_CELLAR ancestry, and that the guard
// runs BEFORE any network call — a refused update must not hit gh at all.
func TestCheckRefusesBrewManagedInstall(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		path   string
		getenv func(string) string
	}{
		{
			name:   "conventional Cellar segment",
			path:   "/opt/homebrew/Cellar/agent-brain/2.0.0/bin/agent-brain",
			getenv: noEnv,
		},
		{
			name: "explicit HOMEBREW_CELLAR",
			path: "/custom/kegs/agent-brain/2.0.0/bin/agent-brain",
			getenv: func(key string) string {
				if key == "HOMEBREW_CELLAR" {
					return "/custom/kegs"
				}
				return ""
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			source := &fakeSource{}
			updater := &Updater{Source: source, Getenv: test.getenv}
			_, err := updater.Check(t.Context(), Options{CurrentVersion: "2.0.0", TargetPath: test.path})
			if !errors.Is(err, ErrBrewManaged) {
				t.Fatalf("Check error = %v, want errors.Is(_, ErrBrewManaged)", err)
			}
			if source.listCalls != 0 {
				t.Fatalf("ListReleases called %d times, want 0 — guards must answer before any network call", source.listCalls)
			}
		})
	}
}

// TestCheckResolutionAndOrdering proves implicit resolution in one table:
// the newest non-draft semver tag wins regardless of channel — an rc above
// every stable IS taken with no flag — while drafts and non-semver tags are
// skipped, the maximum is by semver (not list order), an equal-or-older
// maximum means no update (never downgrade, even when the only candidate is
// an rc below the running version), and a corpus with no installable tag
// yields ErrNoRelease.
func TestCheckResolutionAndOrdering(t *testing.T) {
	t.Parallel()
	releases := []ghx.ReleaseInfo{
		{TagName: "v2.0.0-rc.2", IsPrerelease: true},
		{TagName: "v3.0.0", IsDraft: true},
		{TagName: "nightly-build"},
		{TagName: "v2.0.1"},
		{TagName: "v2.1.0-rc.1", IsPrerelease: true},
		{TagName: "v2.1.0"},
		{TagName: "v2.0.0"},
	}
	tests := []struct {
		name       string
		current    string
		releases   []ghx.ReleaseInfo
		wantLatest string
		wantNeeded bool
		wantErr    error
	}{
		{
			name:       "newest non-draft semver wins, drafts and non-semver skipped",
			current:    "2.0.0",
			releases:   releases,
			wantLatest: "v2.1.0",
			wantNeeded: true,
		},
		{
			name:       "rc above every stable is taken with no flag",
			current:    "2.1.0",
			releases:   []ghx.ReleaseInfo{{TagName: "v2.1.0"}, {TagName: "v2.2.0-rc.1", IsPrerelease: true}},
			wantLatest: "v2.2.0-rc.1",
			wantNeeded: true,
		},
		{
			name:       "already up to date",
			current:    "2.1.0",
			releases:   releases,
			wantLatest: "v2.1.0",
			wantNeeded: false,
		},
		{
			name:       "never downgrade to an older stable",
			current:    "9.9.9",
			releases:   releases,
			wantLatest: "v2.1.0",
			wantNeeded: false,
		},
		{
			name:       "rc below the running version never downgrades",
			current:    "2.1.0",
			releases:   []ghx.ReleaseInfo{{TagName: "v2.0.0-rc.2", IsPrerelease: true}},
			wantLatest: "v2.0.0-rc.2",
			wantNeeded: false,
		},
		{
			name:     "no releases at all is ErrNoRelease",
			current:  "2.0.0",
			releases: nil,
			wantErr:  ErrNoRelease,
		},
		{
			name:     "only draft and non-semver tags is ErrNoRelease",
			current:  "2.0.0",
			releases: []ghx.ReleaseInfo{{TagName: "v9.0.0", IsDraft: true}, {TagName: "nightly-build"}},
			wantErr:  ErrNoRelease,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			updater := &Updater{Source: &fakeSource{releases: test.releases}, Getenv: noEnv}
			decision, err := updater.Check(t.Context(), Options{
				CurrentVersion: test.current,
				TargetPath:     "/home/user/.local/bin/agent-brain",
			})
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("Check error = %v, want errors.Is(_, %v)", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if decision.Latest != test.wantLatest || decision.UpdateNeeded != test.wantNeeded {
				t.Fatalf("Check = {Latest: %s, UpdateNeeded: %t}, want {%s, %t}",
					decision.Latest, decision.UpdateNeeded, test.wantLatest, test.wantNeeded)
			}
		})
	}
}

// TestCheckPreservesAuthInvalidSentinel pins the update-check detection chain
// end to end: when the release listing fails because the gh token is dead, the
// error Check returns must still satisfy errors.Is(_, ghx.ErrAuthInvalid) — the
// hub's update-check detector rides Check and arms its sticky attention on that
// sentinel. The wrap survives only because Check forwards the ReleaseSource
// error unchanged; a future edit that reformats it with %v would sever the
// feature's primary detection path while every suite stayed green, and this is
// the guard that turns that red.
func TestCheckPreservesAuthInvalidSentinel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		listErr      error
		wantSentinel bool
	}{
		{
			// The exact shape ghx.classifyFailure produces for an auth-invalid gh
			// call — "op: stderr: %w" carrying a real 401 body and the sentinel. ghx
			// exposes no exported constructor for it (classifyFailure is unexported,
			// reachable only through a real Client.ListReleases), so the fixture is
			// hand-wrapped with that same shape and a real corpus line rather than a
			// placeholder, so its text cannot drift away from what production emits.
			name:         "dead-token wrapped sentinel survives Check",
			listErr:      fmt.Errorf("gh release list --repo owner/agent-brain: %s: %w", "HTTP 401: Bad credentials", ghx.ErrAuthInvalid),
			wantSentinel: true,
		},
		{
			// A 5xx / other failure carries no sentinel — the fail-closed direction:
			// the attention must arm ONLY on a real dead token, never on any list error.
			name:         "unrelated failure carries no sentinel",
			listErr:      errors.New("gh release list --repo owner/agent-brain: HTTP 500: Internal Server Error"),
			wantSentinel: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			updater := &Updater{Source: &fakeSource{listErr: test.listErr}, Getenv: noEnv}
			_, err := updater.Check(t.Context(), Options{
				CurrentVersion: "2.0.0",
				TargetPath:     "/home/user/.local/bin/agent-brain",
			})
			if got := errors.Is(err, ghx.ErrAuthInvalid); got != test.wantSentinel {
				t.Fatalf("errors.Is(Check err, ErrAuthInvalid) = %v, want %v (err: %v)", got, test.wantSentinel, err)
			}
		})
	}
}

// TestCheckRequestedVersion proves explicit-version pinning in one table:
// an rc pins the same as a stable release, an older release is honored with
// Downgrade set (deliberate rollback), equal is a no-op, drafts stay
// invisible, and both "X" and "vX" spellings resolve to the real tag.
func TestCheckRequestedVersion(t *testing.T) {
	t.Parallel()
	releases := []ghx.ReleaseInfo{
		{TagName: "v2.1.0"},
		{TagName: "v2.0.0"},
		{TagName: "v2.0.0-rc.2", IsPrerelease: true},
		{TagName: "v3.0.0", IsDraft: true},
	}
	tests := []struct {
		name      string
		current   string
		requested string
		want      Decision
		wantErr   string
	}{
		{
			name:      "pins a newer stable",
			current:   "2.0.0",
			requested: "2.1.0",
			want:      Decision{Latest: "v2.1.0", UpdateNeeded: true},
		},
		{
			name:      "pins a prerelease",
			current:   "1.9.0",
			requested: "v2.0.0-rc.2",
			want:      Decision{Latest: "v2.0.0-rc.2", UpdateNeeded: true},
		},
		{
			name:      "explicit older release downgrades",
			current:   "2.1.0",
			requested: "2.0.0",
			want:      Decision{Latest: "v2.0.0", UpdateNeeded: true, Downgrade: true},
		},
		{
			name:      "explicit equal is a no-op",
			current:   "2.1.0",
			requested: "v2.1.0",
			want:      Decision{Latest: "v2.1.0"},
		},
		{
			name:      "nonexistent release is refused with the tag named",
			current:   "2.0.0",
			requested: "9.9.9",
			wantErr:   "v9.9.9 does not exist",
		},
		{
			name:      "draft releases are invisible",
			current:   "2.0.0",
			requested: "3.0.0",
			wantErr:   "v3.0.0 does not exist",
		},
		{
			name:      "invalid semver is refused before any matching",
			current:   "2.0.0",
			requested: "not-a-version",
			wantErr:   "not valid semver",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			updater := &Updater{Source: &fakeSource{releases: releases}, Getenv: noEnv}
			decision, err := updater.Check(t.Context(), Options{
				CurrentVersion:   test.current,
				TargetPath:       "/home/user/.local/bin/agent-brain",
				RequestedVersion: test.requested,
			})
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Check error = %v, want it to contain %q", err, test.wantErr)
				}
				if test.wantErr != "not valid semver" && !errors.Is(err, ErrNoRelease) {
					t.Fatalf("Check error = %v, want errors.Is(_, ErrNoRelease)", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if decision != test.want {
				t.Fatalf("Check = %+v, want %+v", decision, test.want)
			}
		})
	}
}

// TestCheckRequestedVersionGuardsStillFirst proves pinning a version does
// not sidestep the dev-build and Homebrew refusals — and that both still
// answer before any network call.
func TestCheckRequestedVersionGuardsStillFirst(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		opts    Options
		wantErr error
	}{
		{
			name:    "dev build",
			opts:    Options{CurrentVersion: "dev", TargetPath: "/home/user/.local/bin/agent-brain", RequestedVersion: "2.1.0"},
			wantErr: ErrDevBuild,
		},
		{
			name:    "brew managed",
			opts:    Options{CurrentVersion: "2.0.0", TargetPath: "/opt/homebrew/Cellar/agent-brain/2.0.0/bin/agent-brain", RequestedVersion: "2.1.0"},
			wantErr: ErrBrewManaged,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			source := &fakeSource{}
			updater := &Updater{Source: source, Getenv: noEnv}
			_, err := updater.Check(t.Context(), test.opts)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Check error = %v, want errors.Is(_, %v)", err, test.wantErr)
			}
			if source.listCalls != 0 {
				t.Fatalf("ListReleases called %d times, want 0 — guards must answer before any network call", source.listCalls)
			}
		})
	}
}

// buildArchive assembles a goreleaser-shaped tar.gz: the binary at the
// archive root as "agent-brain" plus a README the extractor must ignore.
func buildArchive(t *testing.T, binaryContent []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range []struct {
		name    string
		mode    int64
		content []byte
	}{
		{name: "README.md", mode: 0o644, content: []byte("# agent-brain\n")},
		{name: "agent-brain", mode: 0o755, content: binaryContent},
	} {
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: entry.name, Mode: entry.mode, Size: int64(len(entry.content)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(entry.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// fixtureVersion is the release version every Apply fixture publishes; the
// binary scripts and tag literals in the Apply tests assert against it.
const fixtureVersion = "2.1.0"

// fixture wires a complete downloadable release for Apply tests: a fake
// binary (shell script) whose --version output is scriptable, archived and
// checksummed exactly like a real release.
func fixture(t *testing.T, binaryScript string, corruptChecksum bool) (*fakeSource, Options) {
	t.Helper()
	archive := buildArchive(t, []byte(binaryScript))
	archiveName := fmt.Sprintf("agent-brain_%s_testos_testarch.tar.gz", fixtureVersion)
	checksumsName := fmt.Sprintf("agent-brain_%s_checksums.txt", fixtureVersion)

	hash := sha256.Sum256(archive)
	digest := hex.EncodeToString(hash[:])
	if corruptChecksum {
		digest = strings.Repeat("0", 64)
	}
	checksums := fmt.Sprintf("%s  %s\n", digest, archiveName)

	source := &fakeSource{
		download: func(dir string) error {
			if err := os.WriteFile(filepath.Join(dir, archiveName), archive, 0o600); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, checksumsName), []byte(checksums), 0o600)
		},
	}

	targetDir := t.TempDir()
	targetPath := filepath.Join(targetDir, "agent-brain")
	if err := os.WriteFile(targetPath, []byte("#!/bin/sh\necho agent-brain version OLD\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return source, Options{
		Repo:           "owner/agent-brain",
		CurrentVersion: "2.0.0",
		TargetPath:     targetPath,
		GOOS:           "testos",
		GOARCH:         "testarch",
	}
}

func TestApplyReplacesBinaryAtomically(t *testing.T) {
	t.Parallel()
	source, opts := fixture(t, "#!/bin/sh\necho agent-brain version 2.1.0\n", false)
	updater := &Updater{Source: source, Getenv: noEnv}
	if err := updater.Apply(t.Context(), opts, "v2.1.0"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	replaced, err := os.ReadFile(opts.TargetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(replaced), "2.1.0") {
		t.Fatalf("target content = %q, want the new binary", string(replaced))
	}
	info, err := os.Stat(opts.TargetPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("target mode = %v, want 0755", info.Mode().Perm())
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(opts.TargetPath), ".agent-brain-update-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("staging leftovers %v, want none", leftovers)
	}
}

// TestApplyChecksumMismatchLeavesTargetUntouched proves the integrity gate:
// a corrupt (or tampered) archive is refused with ErrChecksumMismatch and
// the installed binary is byte-identical to before.
func TestApplyChecksumMismatchLeavesTargetUntouched(t *testing.T) {
	t.Parallel()
	source, opts := fixture(t, "#!/bin/sh\necho agent-brain version 2.1.0\n", true)
	before, err := os.ReadFile(opts.TargetPath)
	if err != nil {
		t.Fatal(err)
	}

	updater := &Updater{Source: source, Getenv: noEnv}
	applyErr := updater.Apply(t.Context(), opts, "v2.1.0")
	if !errors.Is(applyErr, ErrChecksumMismatch) {
		t.Fatalf("Apply error = %v, want errors.Is(_, ErrChecksumMismatch)", applyErr)
	}

	after, err := os.ReadFile(opts.TargetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("target binary changed despite a checksum mismatch")
	}
}

// TestApplySanityExecFailureLeavesTargetUntouched proves the last gate: a
// new binary that exits non-zero on --version never replaces the target.
func TestApplySanityExecFailureLeavesTargetUntouched(t *testing.T) {
	t.Parallel()
	source, opts := fixture(t, "#!/bin/sh\nexit 1\n", false)
	before, err := os.ReadFile(opts.TargetPath)
	if err != nil {
		t.Fatal(err)
	}

	updater := &Updater{Source: source, Getenv: noEnv}
	applyErr := updater.Apply(t.Context(), opts, "v2.1.0")
	if applyErr == nil || !strings.Contains(applyErr.Error(), "--version probe") {
		t.Fatalf("Apply error = %v, want the sanity-probe failure", applyErr)
	}

	after, err := os.ReadFile(opts.TargetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("target binary changed despite a failed sanity exec")
	}
}

// TestApplySanityExecVersionMismatchRefuses proves a runnable binary that
// reports the WRONG version (mislabeled asset, stale cache) is refused.
func TestApplySanityExecVersionMismatchRefuses(t *testing.T) {
	t.Parallel()
	source, opts := fixture(t, "#!/bin/sh\necho agent-brain version 9.9.9\n", false)
	updater := &Updater{Source: source, Getenv: noEnv}
	applyErr := updater.Apply(t.Context(), opts, "v2.1.0")
	if applyErr == nil || !strings.Contains(applyErr.Error(), "want it to name 2.1.0") {
		t.Fatalf("Apply error = %v, want the version-mismatch refusal", applyErr)
	}
}

// TestApplyArchiveWithoutBinaryRefuses proves an archive missing the
// top-level agent-brain entry is rejected rather than silently succeeding.
func TestApplyArchiveWithoutBinaryRefuses(t *testing.T) {
	t.Parallel()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	content := []byte("not the binary")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "docs/agent-brain", Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	archive := buffer.Bytes()

	archiveName := "agent-brain_2.1.0_testos_testarch.tar.gz"
	hash := sha256.Sum256(archive)
	checksums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(hash[:]), archiveName)
	source := &fakeSource{download: func(dir string) error {
		if err := os.WriteFile(filepath.Join(dir, archiveName), archive, 0o600); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "agent-brain_2.1.0_checksums.txt"), []byte(checksums), 0o600)
	}}

	targetPath := filepath.Join(t.TempDir(), "agent-brain")
	if err := os.WriteFile(targetPath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	updater := &Updater{Source: source, Getenv: noEnv}
	err := updater.Apply(t.Context(), Options{Repo: "owner/agent-brain", TargetPath: targetPath, GOOS: "testos", GOARCH: "testarch"}, "v2.1.0")
	if err == nil || !strings.Contains(err.Error(), "no agent-brain binary") {
		t.Fatalf("Apply error = %v, want the missing-binary refusal", err)
	}
}

// TestApplyDownloadFailureSurfaces proves a failed download (network,
// auth, missing asset) reaches the caller and nothing is touched.
func TestApplyDownloadFailureSurfaces(t *testing.T) {
	t.Parallel()
	want := errors.New("gh release download: HTTP 404")
	source := &fakeSource{downloadErr: want}
	targetPath := filepath.Join(t.TempDir(), "agent-brain")
	if err := os.WriteFile(targetPath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	updater := &Updater{Source: source, Getenv: noEnv}
	err := updater.Apply(t.Context(), Options{Repo: "owner/agent-brain", TargetPath: targetPath, GOOS: "testos", GOARCH: "testarch"}, "v2.1.0")
	if !errors.Is(err, want) {
		t.Fatalf("Apply error = %v, want it to surface %v", err, want)
	}
}
