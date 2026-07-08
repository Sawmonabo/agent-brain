package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/crypto"
	"github.com/Sawmonabo/agent-brain/internal/keys"
)

// runCmd executes the root tree in-process. t.Setenv (keyset injection)
// forbids t.Parallel in callers.
func runCmd(t *testing.T, stdin []byte, args ...string) (stdout []byte, err error) {
	t.Helper()
	root := Root()
	root.SetIn(bytes.NewReader(stdin))
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	err = root.Execute()
	if err != nil {
		t.Logf("stderr: %s (err: %v)", errBuf.String(), err)
	}
	return out.Bytes(), err
}

func setupKeyset(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", dir)
	if err := keys.Generate(filepath.Join(dir, "keyset.json")); err != nil {
		t.Fatal(err)
	}
}

// ciphertextUnderForeignKeyset seals plaintext under a brand-new keyset in a
// throwaway dir WITHOUT touching AGENT_BRAIN_CONFIG_DIR, so the bytes are valid
// agent-brain ciphertext that the currently-active keyset (if any) cannot
// decrypt. It fabricates the "wrong machine's keyset" and "no keyset present"
// fixtures without duplicating crypto's unexported magic constant.
func ciphertextUnderForeignKeyset(t *testing.T, plaintext []byte) []byte {
	t.Helper()
	keysetPath := filepath.Join(t.TempDir(), "keyset.json")
	if err := keys.Generate(keysetPath); err != nil {
		t.Fatal(err)
	}
	primitive, err := keys.Primitive(keysetPath)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := crypto.NewCodec(primitive).Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	return ciphertext
}

// assertBlocked verifies a filter fail-closed: it returned a non-nil error AND
// wrote nothing. The empty-output half is the load-bearing safety assertion —
// git-clean must never emit content on an error path (spec §5).
func assertBlocked(t *testing.T, name string, stdout []byte, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s succeeded on input it must reject; want a fail-closed error", name)
	}
	if len(stdout) != 0 {
		t.Fatalf("%s emitted %d bytes on an error path; must emit nothing", name, len(stdout))
	}
}

func TestCleanSmudgeRoundtrip(t *testing.T) {
	setupKeyset(t)
	plaintext := []byte("# memory\nfact one\n")

	ciphertext, err := runCmd(t, plaintext, "git-clean")
	if err != nil {
		t.Fatal(err)
	}
	if !crypto.IsEncrypted(ciphertext) || bytes.Contains(ciphertext, []byte("fact one")) {
		t.Fatal("git-clean did not encrypt")
	}

	again, err := runCmd(t, ciphertext, "git-clean")
	if err != nil || !bytes.Equal(again, ciphertext) {
		t.Fatalf("git-clean not idempotent on ciphertext: %v", err)
	}

	back, err := runCmd(t, ciphertext, "git-smudge")
	if err != nil || !bytes.Equal(back, plaintext) {
		t.Fatalf("git-smudge roundtrip failed: %v %q", err, back)
	}

	passthrough, err := runCmd(t, plaintext, "git-smudge")
	if err != nil || !bytes.Equal(passthrough, plaintext) {
		t.Fatalf("git-smudge should pass plaintext through: %v", err)
	}
}

func TestCleanFailsClosedWithoutKeyset(t *testing.T) {
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir()) // empty dir: no keyset
	if _, err := runCmd(t, []byte("secret"), "git-clean"); err == nil {
		t.Fatal("git-clean without keyset succeeded; must fail closed")
	}
}

func TestTextconv(t *testing.T) {
	setupKeyset(t)
	plaintext := []byte("readable\n")
	ciphertext, err := runCmd(t, plaintext, "git-clean")
	if err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(blob, ciphertext, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, nil, "git-textconv", blob)
	if err != nil || !bytes.Equal(out, plaintext) {
		t.Fatalf("git-textconv failed: %v %q", err, out)
	}
}

// TestCleanFailsClosedOnUndecryptableMagic proves the behavior-table cell the
// brief's own tests skip: git-clean on magic-prefixed input it cannot decrypt
// must fail closed (never store lookalike/corrupt/foreign bytes, never leak the
// plaintext it verify-decrypts). This is the plumbing counterpart to the
// codec's TestCleanFailsClosed unit test — here we assert the command exits
// non-zero AND writes nothing across the realistic undecryptable inputs.
func TestCleanFailsClosedOnUndecryptableMagic(t *testing.T) {
	setupKeyset(t) // active keyset "A"
	plaintext := []byte("# memory\ntop secret\n")
	ciphertext, err := runCmd(t, plaintext, "git-clean")
	if err != nil {
		t.Fatal(err)
	}

	corrupted := append([]byte(nil), ciphertext...)
	corrupted[len(corrupted)-1] ^= 0xFF // magic intact, AES-SIV auth fails

	truncated := ciphertext[:len(ciphertext)-1] // magic intact, body incomplete

	foreign := ciphertextUnderForeignKeyset(t, plaintext) // valid only under keyset "B"

	cases := []struct {
		name  string
		input []byte
	}{
		{"corrupted ciphertext", corrupted},
		{"truncated ciphertext", truncated},
		{"foreign-keyset ciphertext", foreign},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if !crypto.IsEncrypted(testCase.input) {
				t.Fatalf("fixture %q lost its magic header; the test would not exercise the verify-decrypt branch", testCase.name)
			}
			out, err := runCmd(t, testCase.input, "git-clean")
			assertBlocked(t, "git-clean", out, err)
		})
	}
}

// TestCleanNeverEmitsWithoutKeyset strengthens TestCleanFailsClosedWithoutKeyset
// with the safety half: git-clean without a keyset must emit nothing, not just
// exit non-zero — a missing keyset can never become an excuse to write
// plaintext toward the git object (spec §5).
func TestCleanNeverEmitsWithoutKeyset(t *testing.T) {
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir()) // empty dir: no keyset
	out, err := runCmd(t, []byte("top secret plaintext"), "git-clean")
	assertBlocked(t, "git-clean", out, err)
}

// TestSmudgeFailsClosedOnMagicWithoutKeyset covers the smudge keyset-missing
// cell: a clone that lacks the keyset cannot decrypt an encrypted file, so
// smudge must fail closed rather than hand git the raw ciphertext.
func TestSmudgeFailsClosedOnMagicWithoutKeyset(t *testing.T) {
	ciphertext := ciphertextUnderForeignKeyset(t, []byte("secret\n"))
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir()) // empty dir: no keyset to decrypt with
	out, err := runCmd(t, ciphertext, "git-smudge")
	assertBlocked(t, "git-smudge", out, err)
}

// TestSmudgePlainPassthroughWithoutKeyset covers the other smudge keyset-missing
// cell: never-encrypted content still passes through so a keyset-less clone can
// read files that were never filtered.
func TestSmudgePlainPassthroughWithoutKeyset(t *testing.T) {
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir()) // empty dir: no keyset required for plaintext
	plaintext := []byte("never encrypted\n")
	out, err := runCmd(t, plaintext, "git-smudge")
	if err != nil {
		t.Fatalf("git-smudge on plaintext without a keyset errored: %v", err)
	}
	if !bytes.Equal(out, plaintext) {
		t.Fatalf("git-smudge altered plaintext passthrough: got %q want %q", out, plaintext)
	}
}

// TestTextconvPlainPassthrough covers the textconv plaintext cell: a
// never-encrypted file is printed verbatim and needs no keyset.
func TestTextconvPlainPassthrough(t *testing.T) {
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir()) // empty dir: plain files need no keyset
	plaintext := []byte("plain diff content\n")
	blob := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(blob, plaintext, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, nil, "git-textconv", blob)
	if err != nil {
		t.Fatalf("git-textconv on a plain file errored: %v", err)
	}
	if !bytes.Equal(out, plaintext) {
		t.Fatalf("git-textconv altered a plain file: got %q want %q", out, plaintext)
	}
}

// TestTextconvFailsClosedOnMagicWithoutKeyset covers the textconv keyset-missing
// cell: an encrypted blob with no keyset available must error, not print
// ciphertext into a diff.
func TestTextconvFailsClosedOnMagicWithoutKeyset(t *testing.T) {
	ciphertext := ciphertextUnderForeignKeyset(t, []byte("secret\n"))
	blob := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(blob, ciphertext, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir()) // empty dir: cannot decrypt
	out, err := runCmd(t, nil, "git-textconv", blob)
	assertBlocked(t, "git-textconv", out, err)
}

// TestTextconvMissingFile exercises the read-error path: an unreadable path
// argument must surface as a non-zero exit, not a silent empty diff.
func TestTextconvMissingFile(t *testing.T) {
	setupKeyset(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	out, err := runCmd(t, nil, "git-textconv", missing)
	if err == nil {
		t.Fatal("git-textconv on a missing file succeeded; want a read error")
	}
	if len(out) != 0 {
		t.Fatalf("git-textconv emitted %d bytes for a missing file", len(out))
	}
}

// TestFilterCommandsHidden guards the contract that these are git-only plumbing
// commands: registered on the root tree but hidden from user-facing help.
func TestFilterCommandsHidden(t *testing.T) {
	t.Parallel()
	root := Root()
	for _, name := range []string{"git-clean", "git-smudge", "git-textconv"} {
		cmd, _, err := root.Find([]string{name})
		if err != nil {
			t.Fatalf("plumbing command %q is not registered: %v", name, err)
		}
		if cmd.Name() != name {
			t.Fatalf("Find(%q) resolved to %q", name, cmd.Name())
		}
		if !cmd.Hidden {
			t.Errorf("plumbing command %q must be Hidden (git-only, not user-facing)", name)
		}
	}
}
