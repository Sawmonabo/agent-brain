package crypto

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func FuzzRoundtrip(f *testing.F) {
	codec := newTestCodec(f)
	f.Add([]byte(""))
	f.Add([]byte("# memory\nfact\n"))
	f.Add([]byte{0x00, 0xFF, 0x61, 0x67, 0x62, 0x31, 0x00})
	f.Add([]byte("agb1\x00looks like ciphertext but is plaintext"))
	f.Fuzz(func(t *testing.T, plaintext []byte) {
		first, err := codec.Encrypt(plaintext)
		if err != nil {
			t.Fatal(err)
		}
		second, err := codec.Encrypt(plaintext)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, second) {
			t.Fatal("determinism violated")
		}
		decrypted, err := codec.Decrypt(first)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(decrypted, plaintext) {
			t.Fatal("roundtrip mismatch")
		}
		// Clean must verify-decrypt genuine ciphertext and pass the exact
		// bytes through (Q2 verify-decrypt contract): whatever the plaintext,
		// its ciphertext decrypts under this keyset, so Clean neither rejects
		// nor alters it.
		cleaned, err := codec.Clean(first)
		if err != nil {
			t.Fatalf("Clean rejected genuine ciphertext: %v", err)
		}
		if !bytes.Equal(cleaned, first) {
			t.Fatal("Clean altered genuine ciphertext; want byte-identical passthrough")
		}
	})
}

func FuzzRewriteRetainBoth(f *testing.F) {
	f.Add([]byte("plain\n"))
	f.Add([]byte("<<<<<<< A\nx\n=======\ny\n>>>>>>> B\n"))
	f.Add([]byte("<<<<<<< A\nunterminated"))
	f.Fuzz(func(_ *testing.T, merged []byte) {
		out, _ := RewriteRetainBoth(merged, "A", "B", "2026-07-07T00:00:00Z")
		_ = out // must not panic; malformed hunks pass through unchanged
	})
}

// FuzzMergeFact drives the full driver path with arbitrary three-way inputs.
// Invariants: success ⇒ %A holds decryptable ciphertext with no leaked git
// markers; failure ⇒ %A is byte-identical to before (no data loss, spec §4).
func FuzzMergeFact(f *testing.F) {
	codec := newTestCodec(f)
	f.Add([]byte("base\n"), []byte("ours\n"), []byte("theirs\n"))
	f.Add([]byte(""), []byte("a\n"), []byte("b\n"))
	f.Add([]byte("x\ny\nz\n"), []byte("x\nY\nz\n"), []byte("x\ny\nZZ\n"))
	f.Fuzz(func(t *testing.T, base, ours, theirs []byte) {
		dir := t.TempDir()
		for name, plaintext := range map[string][]byte{"O": base, "A": ours, "B": theirs} {
			ciphertext, err := codec.Encrypt(plaintext)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), ciphertext, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		currentPath := filepath.Join(dir, "A")
		before, err := os.ReadFile(currentPath)
		if err != nil {
			t.Fatal(err)
		}
		_, mergeErr := MergeFact(context.Background(), codec,
			filepath.Join(dir, "O"), currentPath, filepath.Join(dir, "B"), "fuzz.md", "A", "B")
		after, err := os.ReadFile(currentPath)
		if err != nil {
			t.Fatal(err)
		}
		if mergeErr != nil {
			if !bytes.Equal(before, after) {
				t.Fatal("MergeFact errored AND modified %A — data-loss path")
			}
			return // e.g. merge-file rejects binary input; fallback ladder owns this
		}
		if !IsEncrypted(after) {
			t.Fatal("merge result is not ciphertext")
		}
		plaintext, err := codec.Decrypt(after)
		if err != nil {
			t.Fatal(err)
		}
		marker := []byte("<<<<<<<")
		inputsHaveMarkers := bytes.Contains(base, marker) ||
			bytes.Contains(ours, marker) || bytes.Contains(theirs, marker)
		if !inputsHaveMarkers && bytes.Contains(plaintext, marker) {
			t.Fatal("raw git conflict markers leaked from merge")
		}
	})
}
