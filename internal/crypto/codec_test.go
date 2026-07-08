package crypto

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/keys"
)

// testing.TB so both tests (*testing.T) and fuzz targets (*testing.F) can use it.
func newTestCodec(tb testing.TB) *Codec {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "keyset.json")
	if err := keys.Generate(path); err != nil {
		tb.Fatal(err)
	}
	primitive, err := keys.Primitive(path)
	if err != nil {
		tb.Fatal(err)
	}
	return NewCodec(primitive)
}

func TestCodec(t *testing.T) {
	t.Parallel()
	codec := newTestCodec(t)
	plaintext := []byte("# memory\n\nsecret fact\n")

	ciphertext, err := codec.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if !IsEncrypted(ciphertext) {
		t.Fatal("Encrypt output not recognized by IsEncrypted")
	}
	if bytes.Contains(ciphertext, []byte("secret fact")) {
		t.Fatal("ciphertext contains plaintext")
	}

	again, err := codec.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ciphertext, again) {
		t.Fatal("determinism violated: equal plaintext produced different ciphertext")
	}

	decrypted, err := codec.Decrypt(ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("roundtrip mismatch: %q", decrypted)
	}

	if _, err := codec.Decrypt(plaintext); err == nil {
		t.Fatal("Decrypt of plaintext succeeded; want no-magic error")
	}

	cleaned, err := codec.Clean(ciphertext)
	if err != nil || !bytes.Equal(cleaned, ciphertext) {
		t.Fatalf("Clean not idempotent on ciphertext: %v", err)
	}
	cleaned, err = codec.Clean(plaintext)
	if err != nil || !bytes.Equal(cleaned, ciphertext) {
		t.Fatalf("Clean(plaintext) != Encrypt(plaintext): %v", err)
	}
	smudged, err := codec.Smudge(ciphertext)
	if err != nil || !bytes.Equal(smudged, plaintext) {
		t.Fatalf("Smudge(ciphertext) failed: %v", err)
	}
	smudged, err = codec.Smudge(plaintext)
	if err != nil || !bytes.Equal(smudged, plaintext) {
		t.Fatalf("Smudge must pass plaintext through: %v", err)
	}
	tampered := append([]byte{}, ciphertext...)
	tampered[len(tampered)-1] ^= 0xFF
	if _, err := codec.Decrypt(tampered); err == nil {
		t.Fatal("Decrypt of tampered ciphertext succeeded; want auth error")
	}
	if IsEncrypted([]byte("agb")) || IsEncrypted(nil) {
		t.Fatal("IsEncrypted false positives on short input")
	}
}

// TestCleanFailsClosed pins the Q2-ratified verify-decrypt contract: Clean
// must reject magic-prefixed input it cannot decrypt rather than pass it
// through, so plaintext that merely mimics the header never reaches a git
// object and ciphertext from a foreign keyset is not silently committed.
func TestCleanFailsClosed(t *testing.T) {
	t.Parallel()
	codec := newTestCodec(t)

	// An independent keyset stands in for another machine's ciphertext.
	foreign := newTestCodec(t)
	foreignCiphertext, err := foreign.Encrypt([]byte("secret from another machine"))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		input []byte
	}{
		{
			name:  "lookalike plaintext carrying the magic header",
			input: append(append([]byte{}, magic...), "not valid ciphertext"...),
		},
		{
			name:  "genuine ciphertext under a different keyset",
			input: foreignCiphertext,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := codec.Clean(testCase.input)
			if !errors.Is(err, ErrCleanVerifyFailed) {
				t.Fatalf("Clean error = %v; want ErrCleanVerifyFailed", err)
			}
			if got != nil {
				t.Fatalf("Clean returned %q on failure; want nil output", got)
			}
		})
	}
}
