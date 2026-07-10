// Package keys manages the shared Tink keyset (spec §5): one AES256_SIV
// Deterministic-AEAD keyset across all hosts, stored plaintext at 0600 —
// the documented no-KMS posture for a local dev tool (ADR 06).
package keys

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/google/renameio/v2"
	"github.com/tink-crypto/tink-go/v2/daead"
	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"
	"github.com/tink-crypto/tink-go/v2/tink"
)

// ErrKeysetExists is the sentinel behind both refuse-to-overwrite paths
// (Generate and Import): losing a keyset loses every memory encrypted under it,
// so neither ever clobbers an existing one. Phase 2's init / key-import UX
// branches on this with errors.Is rather than matching the message string.
var ErrKeysetExists = errors.New("keyset already exists")

// Generate creates a fresh AES256_SIV keyset at path (0600). It refuses to
// overwrite: losing a keyset means losing every memory encrypted under it.
func Generate(path string) error {
	// The Stat check is a best-effort guard, not a lock: the check-then-write
	// window is a benign TOCTOU. The write path uses renameio's atomic replace,
	// chosen for crash-atomicity (a partially written keyset is never visible)
	// over an O_CREATE|O_EXCL no-clobber open. Keyset bootstrap is a single-user,
	// one-shot CLI action and the sync engine is the only concurrent writer, so
	// the race is accepted.
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%w at %s (use key import/export to move keys)", ErrKeysetExists, path)
	}
	// AESSIVKeyTemplate is daead's only template; it generates the
	// AES256_SIV key type (64-byte key, RFC 5297) the spec pins.
	handle, err := keyset.NewHandle(daead.AESSIVKeyTemplate())
	if err != nil {
		return fmt.Errorf("generate keyset: %w", err)
	}
	return write(path, handle)
}

// Rotate adds a fresh AES256_SIV key to the keyset at path, promotes it to
// primary, and atomically rewrites the file (renameio, same as Generate).
// New content encrypts under the new primary; a full re-encrypt of existing
// blobs is the engine's job (ReencryptAll), so Rotate touches only the keyset.
//
// The previous keys are RETAINED, never disabled or destroyed: history blobs
// (old commits) and peers that have not imported the new keyset yet still smudge
// through them (spec §5). A destroy/disable lifecycle is deliberately post-v2.
//
// It refuses a missing keyset, wrapping fs.ErrNotExist: installing a first
// keyset is `key import`/init's job, not rotation's.
func Rotate(path string) error {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the program-derived keyset location (config.Paths.Keyset), not untrusted input
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("no keyset at %s to rotate — run `agent-brain key import` (or `agent-brain init`) to install one first: %w", path, err)
		}
		return fmt.Errorf("read keyset: %w", err)
	}
	handle, err := insecurecleartextkeyset.Read(keyset.NewJSONReader(bytes.NewReader(data)))
	if err != nil {
		return fmt.Errorf("parse keyset: %w", err)
	}
	// Tink keysets are natively multi-key: Add appends a new AES256_SIV key,
	// SetPrimary makes it the one Encrypt uses, and every prior key stays a
	// valid Decrypt target — exactly the multi-key rotation spec §5 designed for.
	manager := keyset.NewManagerFromHandle(handle)
	newID, err := manager.Add(daead.AESSIVKeyTemplate())
	if err != nil {
		return fmt.Errorf("add rotation key: %w", err)
	}
	if err := manager.SetPrimary(newID); err != nil {
		return fmt.Errorf("promote rotation key to primary: %w", err)
	}
	rotated, err := manager.Handle()
	if err != nil {
		return fmt.Errorf("read back rotated keyset: %w", err)
	}
	return write(path, rotated)
}

// Primitive loads the keyset and returns the Deterministic AEAD primitive.
func Primitive(path string) (tink.DeterministicAEAD, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the program-derived keyset location (config.Paths.Keyset), not untrusted input
	if err != nil {
		return nil, fmt.Errorf("read keyset: %w", err)
	}
	handle, err := insecurecleartextkeyset.Read(keyset.NewJSONReader(bytes.NewReader(data)))
	if err != nil {
		return nil, fmt.Errorf("parse keyset: %w", err)
	}
	primitive, err := daead.New(handle)
	if err != nil {
		return nil, fmt.Errorf("keyset is not a Deterministic AEAD keyset: %w", err)
	}
	return primitive, nil
}

// Export validates the on-disk keyset and returns it as std-base64 for transfer
// over a user-chosen channel; the export IS the recovery artifact (spec §5), so
// a corrupt or wrong-type keyset must fail here rather than at restore time.
func Export(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the program-derived keyset location (config.Paths.Keyset), not untrusted input
	if err != nil {
		return "", fmt.Errorf("read keyset: %w", err)
	}
	handle, err := insecurecleartextkeyset.Read(keyset.NewJSONReader(bytes.NewReader(data)))
	if err != nil {
		return "", fmt.Errorf("parse keyset: %w", err)
	}
	if _, err := daead.New(handle); err != nil {
		return "", fmt.Errorf("keyset is not a Deterministic AEAD keyset: %w", err)
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// Import validates an armored keyset and installs it at path (0600).
func Import(path, armored string) error {
	// Best-effort no-clobber guard with the same accepted check-then-write TOCTOU
	// as Generate; refusing to overwrite protects an existing keyset from an
	// errant import.
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%w at %s; refusing to overwrite", ErrKeysetExists, path)
	}
	data, err := base64.StdEncoding.DecodeString(armored)
	if err != nil {
		return fmt.Errorf("decode armored keyset: %w", err)
	}
	handle, err := insecurecleartextkeyset.Read(keyset.NewJSONReader(bytes.NewReader(data)))
	if err != nil {
		return fmt.Errorf("parse keyset: %w", err)
	}
	if _, err := daead.New(handle); err != nil {
		return fmt.Errorf("keyset is not a Deterministic AEAD keyset: %w", err)
	}
	return write(path, handle)
}

func write(path string, handle *keyset.Handle) error {
	var buf bytes.Buffer
	if err := insecurecleartextkeyset.Write(handle, keyset.NewJSONWriter(&buf)); err != nil {
		return fmt.Errorf("serialize keyset: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	if err := renameio.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write keyset: %w", err)
	}
	return nil
}
