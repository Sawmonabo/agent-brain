package keys

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateLoadRoundtrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "keyset.json")
	if err := Generate(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("keyset perm = %o, want 600", perm)
	}
	if err := Generate(path); err == nil {
		t.Fatal("second Generate succeeded; want refuse-to-overwrite error")
	}
	primitive, err := Primitive(path)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := primitive.EncryptDeterministically([]byte("hello"), nil)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := primitive.DecryptDeterministically(ciphertext, nil)
	if err != nil || string(plaintext) != "hello" {
		t.Fatalf("roundtrip failed: %v %q", err, plaintext)
	}
}

func TestExportImport(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	original := filepath.Join(dir, "keyset.json")
	if err := Generate(original); err != nil {
		t.Fatal(err)
	}
	armored, err := Export(original)
	if err != nil {
		t.Fatal(err)
	}
	imported := filepath.Join(dir, "imported.json")
	if err := Import(imported, armored); err != nil {
		t.Fatal(err)
	}
	// The imported keyset must decrypt what the original encrypted.
	primitiveA, err := Primitive(original)
	if err != nil {
		t.Fatal(err)
	}
	primitiveB, err := Primitive(imported)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := primitiveA.EncryptDeterministically([]byte("shared identity"), nil)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := primitiveB.DecryptDeterministically(ciphertext, nil)
	if err != nil || string(plaintext) != "shared identity" {
		t.Fatalf("cross-keyset decrypt failed: %v %q", err, plaintext)
	}
	if err := Import(imported, armored); err == nil {
		t.Fatal("Import over existing file succeeded; want error")
	}
	if err := Import(filepath.Join(dir, "bad.json"), "!!!not-base64!!!"); err == nil {
		t.Fatal("Import of garbage succeeded; want error")
	}
}
