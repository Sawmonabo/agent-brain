// Package crypto is the storage codec (spec §5): deterministic AEAD
// (Tink AES-SIV) behind a magic header that marks agent-brain ciphertext.
package crypto

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/tink-crypto/tink-go/v2/tink"
)

// magic prefixes every stored ciphertext. Version bump = new magic.
var magic = []byte("agb1\x00")

// ErrNotEncrypted reports input without the agent-brain magic header.
var ErrNotEncrypted = errors.New("data is not agent-brain ciphertext (missing magic header)")

// ErrCleanVerifyFailed reports that Clean received magic-prefixed input it
// could not decrypt: the bytes carry the agent-brain header but are not valid
// ciphertext under this keyset (keyset mismatch or corrupted content). Clean
// fails closed on it rather than commit lookalike plaintext or double-wrap.
var ErrCleanVerifyFailed = errors.New("clean: magic-prefixed input is not valid ciphertext under this keyset (keyset mismatch or corrupted content)")

// Codec encrypts/decrypts memory content. Associated data is always nil:
// the merge driver and textconv receive pathless temp blobs from git, and
// equal-plaintext ⇒ equal-ciphertext is the accepted determinism trade
// (spec §5) — so nothing can be bound into AD.
type Codec struct {
	daead tink.DeterministicAEAD
}

// NewCodec wraps a Deterministic AEAD primitive (from keys.Primitive).
func NewCodec(d tink.DeterministicAEAD) *Codec {
	return &Codec{daead: d}
}

// Encrypt seals plaintext and prefixes the magic header.
func (c *Codec) Encrypt(plaintext []byte) ([]byte, error) {
	ciphertext, err := c.daead.EncryptDeterministically(plaintext, nil)
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	return append(append(make([]byte, 0, len(magic)+len(ciphertext)), magic...), ciphertext...), nil
}

// Decrypt unseals Encrypt output; ErrNotEncrypted if the magic is absent.
func (c *Codec) Decrypt(data []byte) ([]byte, error) {
	if !IsEncrypted(data) {
		return nil, ErrNotEncrypted
	}
	plaintext, err := c.daead.DecryptDeterministically(data[len(magic):], nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt (wrong keyset or corrupted data): %w", err)
	}
	return plaintext, nil
}

// Clean is the clean-filter endpoint (spec §5, §8). It upholds the absolute
// invariant that plaintext memory content never reaches a git object.
//
// Non-magic input is encrypted. Magic-prefixed input is verify-decrypted: on
// success the ORIGINAL input passes through byte-identical (idempotent — git
// may re-clean already-stored ciphertext), on failure Clean fails closed with
// ErrCleanVerifyFailed. Verification is what makes passthrough safe: it proves
// the bytes are genuine ciphertext under this keyset, so lookalike plaintext
// (content that merely begins with the magic header) and foreign-keyset or
// corrupted ciphertext are rejected at commit time rather than stored or
// double-wrapped.
func (c *Codec) Clean(data []byte) ([]byte, error) {
	if IsEncrypted(data) {
		if _, err := c.Decrypt(data); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrCleanVerifyFailed, err)
		}
		return data, nil
	}
	return c.Encrypt(data)
}

// Smudge is the smudge/textconv endpoint (spec §5, §8): ciphertext is
// decrypted, never-encrypted content passes through.
func (c *Codec) Smudge(data []byte) ([]byte, error) {
	if !IsEncrypted(data) {
		return data, nil
	}
	return c.Decrypt(data)
}

// IsEncrypted reports whether data carries the agent-brain magic header.
func IsEncrypted(data []byte) bool {
	return bytes.HasPrefix(data, magic)
}
