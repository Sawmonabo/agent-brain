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
	})
}
