package crypto

import (
	"bytes"
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
