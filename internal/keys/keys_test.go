package keys

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"
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
	if err := Generate(path); !errors.Is(err, ErrKeysetExists) {
		t.Fatalf("second Generate must refuse to overwrite with ErrKeysetExists, got %v", err)
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
	if err := Import(imported, armored); !errors.Is(err, ErrKeysetExists) {
		t.Fatalf("Import over an existing keyset must refuse with ErrKeysetExists, got %v", err)
	}
	if err := Import(filepath.Join(dir, "bad.json"), "!!!not-base64!!!"); err == nil {
		t.Fatal("Import of garbage succeeded; want error")
	}
}

// aeadKeysetJSON returns a cleartext-serialized AES256_GCM AEAD keyset: a
// structurally valid Tink keyset whose primitive is NOT Deterministic AEAD.
// It models the realistic mistake of pointing agent-brain at the wrong keyset,
// which every load path must reject.
func aeadKeysetJSON(t *testing.T) []byte {
	t.Helper()
	handle, err := keyset.NewHandle(aead.AES256GCMKeyTemplate())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := insecurecleartextkeyset.Write(handle, keyset.NewJSONWriter(&buf)); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestLoadPathsRejectWrongKeysetType pins the fail-closed guard shared by
// Primitive, Export, and Import: a valid keyset of the wrong primitive type is
// rejected, and a rejected Import leaves no file behind.
func TestLoadPathsRejectWrongKeysetType(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	wrongType := aeadKeysetJSON(t)
	file := filepath.Join(dir, "aead.json")
	if err := os.WriteFile(file, wrongType, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Primitive(file); err == nil {
		t.Fatal("Primitive accepted a non-DAEAD (AEAD) keyset; want rejection")
	}
	if _, err := Export(file); err == nil {
		t.Fatal("Export accepted a non-DAEAD (AEAD) keyset; want rejection")
	}
	dst := filepath.Join(dir, "imported.json")
	if err := Import(dst, base64.StdEncoding.EncodeToString(wrongType)); err == nil {
		t.Fatal("Import accepted a non-DAEAD (AEAD) keyset; want rejection")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("Import left a file after rejecting a wrong-type keyset; want no file, stat err = %v", err)
	}
}

// TestExportRejectsCorruptKeyset proves the export is validated before it is
// emitted: a corrupt on-disk keyset fails at Export, not at restore time, and
// no partial armored artifact is returned.
func TestExportRejectsCorruptKeyset(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "keyset.json")
	if err := os.WriteFile(file, []byte("{not a keyset}"), 0o600); err != nil {
		t.Fatal(err)
	}
	armored, err := Export(file)
	if err == nil {
		t.Fatal("Export accepted a corrupt keyset file; want error")
	}
	if armored != "" {
		t.Fatalf("Export returned %q alongside its error; want empty string", armored)
	}
}

// TestImportRejectsValidBase64NonKeyset exercises the parse branch distinct from
// the non-base64 branch: valid std-base64 whose bytes are not a keyset must be
// rejected before any file is written.
func TestImportRejectsValidBase64NonKeyset(t *testing.T) {
	t.Parallel()
	dst := filepath.Join(t.TempDir(), "imported.json")
	if err := Import(dst, base64.StdEncoding.EncodeToString([]byte("hello, not a keyset"))); err == nil {
		t.Fatal("Import accepted valid base64 that is not a keyset; want error")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("Import left a file after rejecting a non-keyset payload; want no file, stat err = %v", err)
	}
}

// loadHandle reads the on-disk keyset as a Tink handle so a test can inspect
// key count and primary id — the keyset-level facts Rotate must establish,
// beyond the ciphertext behaviour the public primitive already exposes.
func loadHandle(t *testing.T, path string) *keyset.Handle {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := insecurecleartextkeyset.Read(keyset.NewJSONReader(bytes.NewReader(data)))
	if err != nil {
		t.Fatal(err)
	}
	return handle
}

// TestRotateAddsPrimaryKeepsOldKeys pins the spec §5 rotation contract: Rotate
// adds a fresh AES256_SIV key, promotes it to primary, and RETAINS the old key
// (history blobs and not-yet-re-encrypted peers still need it to smudge). The
// primary switch is observable through the deterministic-AEAD property — the
// same plaintext seals to different bytes — while the old key's retention is
// proven by decrypting ciphertext sealed before the rotation.
func TestRotateAddsPrimaryKeepsOldKeys(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "keyset.json")
	if err := Generate(path); err != nil {
		t.Fatal(err)
	}

	before := loadHandle(t, path)
	if before.Len() != 1 {
		t.Fatalf("fresh keyset has %d keys, want 1", before.Len())
	}
	beforePrimary := before.KeysetInfo().GetPrimaryKeyId()

	// Seal a sample under the OLD primary so, after rotating, we can prove both
	// that the old key survives (this decrypts) and that the primary switched
	// (an identical plaintext now seals differently).
	oldPrimitive, err := Primitive(path)
	if err != nil {
		t.Fatal(err)
	}
	const plaintext = "the codebase pins go-cmp only for assertions"
	oldSealed, err := oldPrimitive.EncryptDeterministically([]byte(plaintext), nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := Rotate(path); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	after := loadHandle(t, path)
	if after.Len() != 2 {
		t.Fatalf("rotated keyset has %d keys, want 2 (new primary added, old key retained)", after.Len())
	}
	if after.KeysetInfo().GetPrimaryKeyId() == beforePrimary {
		t.Fatalf("primary key id %d unchanged after Rotate", beforePrimary)
	}

	newPrimitive, err := Primitive(path)
	if err != nil {
		t.Fatal(err)
	}
	// Old key retained: ciphertext sealed under the old primary still opens.
	opened, err := newPrimitive.DecryptDeterministically(oldSealed, nil)
	if err != nil || string(opened) != plaintext {
		t.Fatalf("old ciphertext no longer decrypts after rotate: err=%v opened=%q", err, opened)
	}
	// Primary switched: the same plaintext now seals to different bytes.
	newSealed, err := newPrimitive.EncryptDeterministically([]byte(plaintext), nil)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(oldSealed, newSealed) {
		t.Fatal("identical plaintext sealed to identical bytes after rotate — the primary did not switch")
	}
}

// TestRotateRefusesMissingKeyset pins that rotation is not a bootstrap path:
// with no keyset present it must fail, wrapping fs.ErrNotExist and naming
// `key import` as the fix (installing a first keyset is import/init's job).
func TestRotateRefusesMissingKeyset(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "keyset.json")
	err := Rotate(path)
	if err == nil {
		t.Fatal("Rotate on a missing keyset succeeded; it must refuse")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error does not wrap fs.ErrNotExist: %v", err)
	}
	if !strings.Contains(err.Error(), "key import") {
		t.Fatalf("error must name `key import` as the fix: %v", err)
	}
}
