package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/keys"
)

// TestKeyExportIsPipeClean pins the export contract: stdout carries EXACTLY
// the armored keyset plus a trailing newline (so a shell pipeline gets clean
// bytes), while the password-manager reminder — which must never end up
// piped into a file — goes to stderr.
func TestKeyExportIsPipeClean(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", dir)
	keysetPath := filepath.Join(dir, "keyset.json")
	if err := keys.Generate(keysetPath); err != nil {
		t.Fatal(err)
	}
	want, err := keys.Export(keysetPath)
	if err != nil {
		t.Fatal(err)
	}

	root := Root()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"key", "export"})
	if err := root.Execute(); err != nil {
		t.Fatalf("key export: %v\nstderr: %s", err, stderr.String())
	}
	if got := stdout.String(); got != want+"\n" {
		t.Fatalf("key export stdout = %q, want %q (armored + newline, nothing else)", got, want+"\n")
	}
	if !strings.Contains(stderr.String(), "password manager") {
		t.Fatalf("key export stderr missing the recovery reminder: %q", stderr.String())
	}
}

// TestKeyImportRoundtrip proves an exported keyset can be piped into import
// on a clean machine (empty config dir) and the result loads as a valid
// primitive.
func TestKeyImportRoundtrip(t *testing.T) {
	srcDir := t.TempDir()
	srcKeyset := filepath.Join(srcDir, "keyset.json")
	if err := keys.Generate(srcKeyset); err != nil {
		t.Fatal(err)
	}
	armored, err := keys.Export(srcKeyset)
	if err != nil {
		t.Fatal(err)
	}

	dstDir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", dstDir)

	root := Root()
	root.SetIn(strings.NewReader(armored + "\n")) // trailing newline must be trimmed, not treated as payload
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"key", "import"})
	if err := root.Execute(); err != nil {
		t.Fatalf("key import: %v\n%s", err, out.String())
	}

	if _, err := keys.Primitive(filepath.Join(dstDir, "keyset.json")); err != nil {
		t.Fatalf("imported keyset does not load as a primitive: %v", err)
	}
}

// TestKeyImportRefusesClobberWithoutForce proves import never silently
// destroys an existing keyset: it refuses, names --force in the error, and
// leaves the on-disk keyset byte-for-byte untouched.
func TestKeyImportRefusesClobberWithoutForce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", dir)
	keysetPath := filepath.Join(dir, "keyset.json")
	if err := keys.Generate(keysetPath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(keysetPath)
	if err != nil {
		t.Fatal(err)
	}

	armored, err := keys.Export(keysetPath)
	if err != nil {
		t.Fatal(err)
	}

	root := Root()
	root.SetIn(strings.NewReader(armored))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"key", "import"})
	err = root.Execute()
	if err == nil {
		t.Fatal("key import onto an existing keyset without --force succeeded; must refuse")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("refusal must name --force as the fix: %v", err)
	}
	if !strings.Contains(err.Error(), keysetPath) {
		t.Fatalf("refusal must name the existing keyset path: %v", err)
	}

	after, err := os.ReadFile(keysetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a refused import must not modify the existing keyset")
	}
}

// TestKeyImportForceBacksUpAndReplaces proves --force never destroys key
// material outright: it renames the old keyset to a .bak-<unixts> sibling,
// and the freshly imported keyset decrypts what the OLD keyset encrypted
// (i.e. it is really the replacement key, not a fresh/blank one).
func TestKeyImportForceBacksUpAndReplaces(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", dir)
	keysetPath := filepath.Join(dir, "keyset.json")
	if err := keys.Generate(keysetPath); err != nil {
		t.Fatal(err)
	}

	otherDir := t.TempDir()
	otherKeyset := filepath.Join(otherDir, "keyset.json")
	if err := keys.Generate(otherKeyset); err != nil {
		t.Fatal(err)
	}
	armored, err := keys.Export(otherKeyset)
	if err != nil {
		t.Fatal(err)
	}

	root := Root()
	root.SetIn(strings.NewReader(armored))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"key", "import", "--force"})
	if err := root.Execute(); err != nil {
		t.Fatalf("key import --force: %v\n%s", err, out.String())
	}

	matches, err := filepath.Glob(keysetPath + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("want exactly one keyset.json.bak-<unixts> file, got %v", matches)
	}

	// The replaced keyset must decrypt what the OLD (otherKeyset) key sealed —
	// proving --force actually installed the new key material, not a no-op.
	newPrimitive, err := keys.Primitive(keysetPath)
	if err != nil {
		t.Fatalf("post-force keyset does not load: %v", err)
	}
	oldPrimitive, err := keys.Primitive(otherKeyset)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := oldPrimitive.EncryptDeterministically([]byte("probe"), nil)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := newPrimitive.DecryptDeterministically(sealed, nil)
	if err != nil || string(opened) != "probe" {
		t.Fatalf("imported keyset cannot decrypt what the source keyset encrypted: %v", err)
	}
}

// TestKeyImportForceValidatesBeforeTouchingExistingKeyset proves --force
// validates the incoming armored text BEFORE it disturbs the existing
// keyset: garbage input must fail without renaming the existing keyset
// away, without leaving a .bak file, and without leaving a scratch file
// behind.
func TestKeyImportForceValidatesBeforeTouchingExistingKeyset(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", dir)
	keysetPath := filepath.Join(dir, "keyset.json")
	if err := keys.Generate(keysetPath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(keysetPath)
	if err != nil {
		t.Fatal(err)
	}

	root := Root()
	root.SetIn(strings.NewReader("not a valid armored keyset"))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"key", "import", "--force"})
	if err := root.Execute(); err == nil {
		t.Fatal("key import --force with garbage input must fail")
	}

	after, err := os.ReadFile(keysetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a failed --force import must not disturb the existing keyset")
	}
	if matches, _ := filepath.Glob(keysetPath + ".bak-*"); len(matches) != 0 {
		t.Fatalf("a failed --force import must not create a backup file either: %v", matches)
	}
	if leftovers, _ := filepath.Glob(keysetPath + ".importing-*"); len(leftovers) != 0 {
		t.Fatalf("a failed --force import must not leave a scratch file behind: %v", leftovers)
	}
}
