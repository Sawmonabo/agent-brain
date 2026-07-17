package crypto

import (
	"bytes"
	"errors"
	"testing"
)

// flip returns a copy of data with the byte at index i inverted, so the
// original ciphertext used to build several cases is never mutated in place.
func flip(data []byte, i int) []byte {
	out := append([]byte{}, data...)
	out[i] ^= 0xFF
	return out
}

// TestIsEncryptedDiscriminator pins the magic header as an EXACT prefix
// discriminator (spec §5 safety invariant): the check is prefix, not
// containment, and every length/content edge around the 5-byte magic
// (agb1\x00) resolves deterministically. The expected magic is hard-coded here
// rather than read from the package var so a version bump breaks this test
// loudly instead of silently tracking the change. A false positive would let
// Clean pass genuine plaintext through unencrypted; a false negative would make
// Decrypt reject real ciphertext.
func TestIsEncryptedDiscriminator(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{"nil", nil, false},
		{"empty", []byte{}, false},
		{"shorter than magic", []byte("agb1"), false},
		{"exact magic, no ciphertext", []byte("agb1\x00"), true},
		{"magic plus payload", []byte("agb1\x00payload"), true},
		{"wrong version digit", []byte("agb0\x00rest"), false},
		{"missing nul terminator", []byte("agb1Xrest"), false},
		{"uppercase mismatch", []byte("AGB1\x00rest"), false},
		{"magic not at start (containment, not prefix)", []byte("x\x61gb1\x00rest"), false},
		{"leading nul then magic", []byte("\x00agb1\x00"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsEncrypted(tc.data); got != tc.want {
				t.Fatalf("IsEncrypted(%q) = %v, want %v", tc.data, got, tc.want)
			}
		})
	}
}

// TestDecryptNonMagicReturnsSentinel pins the exported sentinel so downstream
// callers (the smudge/textconv command) can branch on ErrNotEncrypted
// via errors.Is rather than string-matching.
func TestDecryptNonMagicReturnsSentinel(t *testing.T) {
	t.Parallel()
	codec := newTestCodec(t)
	plaintext, err := codec.Decrypt([]byte("plain, never filtered"))
	if !errors.Is(err, ErrNotEncrypted) {
		t.Fatalf("Decrypt of non-magic data: err = %v, want ErrNotEncrypted", err)
	}
	if plaintext != nil {
		t.Fatalf("Decrypt returned %q alongside ErrNotEncrypted; want nil", plaintext)
	}
}

// TestDecryptFailsClosed proves Decrypt never yields a plaintext for input that
// carries the magic but is not a valid AES-SIV ciphertext (empty, truncated, or
// tampered). Fail-closed is the safety invariant: the value returned on error
// must be nil, so no partial/garbage plaintext can reach a working tree.
func TestDecryptFailsClosed(t *testing.T) {
	t.Parallel()
	codec := newTestCodec(t)
	valid, err := codec.Encrypt([]byte("real memory content"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		data []byte
	}{
		{"magic only (empty ciphertext)", append([]byte{}, magic...)},
		{"magic plus short garbage", append(append([]byte{}, magic...), 'x')},
		{"first ciphertext byte flipped", flip(valid, len(magic))},
		{"last byte flipped", flip(valid, len(valid)-1)},
		{"truncated ciphertext", valid[:len(valid)-1]},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plaintext, err := codec.Decrypt(tc.data)
			if err == nil {
				t.Fatalf("Decrypt(%q) succeeded; want fail-closed error", tc.data)
			}
			if plaintext != nil {
				t.Fatalf("Decrypt returned %q alongside its error; want nil plaintext", plaintext)
			}
		})
	}
}

// TestSmudgeFailsClosedOnCorruptCiphertext proves the checkout/textconv path
// errors on magic-prefixed content it cannot decrypt rather than writing
// undecryptable bytes into the working tree. Contrast the brief's TestCodec,
// which covers only Smudge of valid ciphertext and of never-encrypted plaintext.
func TestSmudgeFailsClosedOnCorruptCiphertext(t *testing.T) {
	t.Parallel()
	codec := newTestCodec(t)
	valid, err := codec.Encrypt([]byte("content"))
	if err != nil {
		t.Fatal(err)
	}
	corrupt := flip(valid, len(valid)-1)
	plaintext, err := codec.Smudge(corrupt)
	if err == nil {
		t.Fatal("Smudge of corrupt ciphertext succeeded; want fail-closed error")
	}
	if plaintext != nil {
		t.Fatalf("Smudge returned %q alongside its error; want nil", plaintext)
	}
}

// TestCrossKeysetFailsClosed models the core multi-machine hazard: content
// sealed under one keyset must not decrypt under another. AES-SIV
// authentication makes both Decrypt and the Smudge checkout path fail closed
// rather than surface garbage plaintext.
func TestCrossKeysetFailsClosed(t *testing.T) {
	t.Parallel()
	alice := newTestCodec(t)
	bob := newTestCodec(t) // independent keyset
	ciphertext, err := alice.Encrypt([]byte("alice's private memory"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bob.Decrypt(ciphertext); err == nil {
		t.Fatal("Decrypt under a foreign keyset succeeded; want auth failure")
	}
	if _, err := bob.Smudge(ciphertext); err == nil {
		t.Fatal("Smudge under a foreign keyset succeeded; want auth failure")
	}
}

// TestCleanIdempotentAndPassthrough locks the clean-filter happy path at a
// boundary the brief's test skips: empty input is encrypted, and re-cleaning
// that genuine ciphertext verify-decrypts it and passes the original bytes
// through byte-identical (idempotent — git may re-clean already-stored
// content). Under the Q2-ratified verify-decrypt contract (spec §5),
// passthrough is reserved for bytes that prove decryptable; magic-prefixed
// lookalikes and foreign-keyset ciphertext fail closed instead, pinned by
// TestCleanFailsClosed.
func TestCleanIdempotentAndPassthrough(t *testing.T) {
	t.Parallel()
	codec := newTestCodec(t)

	sealed, err := codec.Clean(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !IsEncrypted(sealed) {
		t.Fatal("Clean(nil) did not produce ciphertext")
	}
	recleaned, err := codec.Clean(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sealed, recleaned) {
		t.Fatal("Clean not idempotent on its own output")
	}
}
