package crypto

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/renameio/v2"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
)

// MergeFact three-way merges STORED (post-clean, ciphertext) base/current/
// other files on their plaintext, rewrites true overlaps as retain-both
// blocks, and re-encrypts the result into currentPath (%A). On merge-able
// input it always succeeds — the driver must exit resolved so a rebase can
// never strand (spec §4). It returns an error ONLY for unexpected failure
// (unreadable input, wrong keyset, merge-file error), where the engine's
// fallback ladder takes over (spec §4 step 3) — and it never writes
// currentPath on that path, so no data is lost.
func MergeFact(ctx context.Context, codec *Codec, basePath, currentPath, otherPath, pathname, labelA, labelB string) (bool, error) {
	tempDir, err := os.MkdirTemp("", "agent-brain-merge-*")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.RemoveAll(tempDir) }() // best-effort temp cleanup

	plaintextPaths := make([]string, 3)
	for i, storedPath := range []string{currentPath, basePath, otherPath} {
		plaintext, err := decryptLoose(codec, storedPath)
		if err != nil {
			return false, fmt.Errorf("decrypt %s side of %s: %w", []string{"current", "base", "other"}[i], pathname, err)
		}
		plaintextPaths[i] = filepath.Join(tempDir, fmt.Sprintf("side-%d", i))
		if err := os.WriteFile(plaintextPaths[i], plaintext, 0o600); err != nil {
			return false, err
		}
	}

	result, err := gitx.RunStatus(ctx, tempDir, "merge-file", "-p",
		"-L", labelA, "-L", "base", "-L", labelB,
		plaintextPaths[0], plaintextPaths[1], plaintextPaths[2])
	if err != nil {
		return false, err
	}
	// merge-file's exit value is the conflict count (bounded to 127) — but
	// negative-on-error surfaces through exec as >127 (e.g. 255 for binary
	// input). Treating that as "conflicts" would encrypt an EMPTY stdout
	// over %A and lose the file, so it must be an error instead.
	if result.ExitCode > 127 {
		return false, fmt.Errorf("git merge-file failed on %s: %s", pathname, result.Stderr)
	}

	merged := []byte(result.Stdout)
	hadConflicts := false
	if result.ExitCode > 0 {
		merged, hadConflicts = RewriteRetainBoth(merged, labelA, labelB,
			time.Now().UTC().Format(time.RFC3339))
	}
	ciphertext, err := codec.Encrypt(merged)
	if err != nil {
		return false, err
	}
	if err := renameio.WriteFile(currentPath, ciphertext, 0o644); err != nil {
		return false, err
	}
	return hadConflicts, nil
}

// decryptLoose tolerates what git actually hands a merge driver: empty temp
// files (add/add) and post-clean ciphertext; plain content passes through.
func decryptLoose(codec *Codec, path string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is a git-supplied merge-driver argument (the %O/%A/%B placeholders), a trusted invocation path, not untrusted user input
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || !IsEncrypted(data) {
		return data, nil
	}
	return codec.Decrypt(data)
}
