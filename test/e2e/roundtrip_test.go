package e2e

import (
	"strings"
	"testing"
)

// magicPrefix is the on-the-wire marker of agent-brain ciphertext (crypto
// package's storage header). A stored blob that starts with it went through the
// clean filter; one that does not is unencrypted plaintext on the remote.
const magicPrefix = "agb1\x00"

func TestEncryptedRoundtripThroughRealGit(t *testing.T) {
	t.Parallel()
	// The plaintext memory and its embedded secret. Asserting the whole string
	// survives the roundtrip (not just the secret substring) proves the smudge
	// reproduces the original byte-for-byte.
	const (
		memory = "# memory\n\nthe launch code is swordfish\n"
		secret = "swordfish"
	)

	bare := newBareRepo(t)
	machineA := newMachine(t, "machine-a", bare)

	writeFile(t, machineA, ".gitattributes", gitAttributes)
	writeFile(t, machineA, "notes.md", memory)
	gitRun(t, machineA, "add", ".")
	gitRun(t, machineA, "commit", "--quiet", "-m", "memory: machine-a test 2026-07-07")
	gitRun(t, machineA, "push", "--quiet", "origin", "main")

	// On-the-wire proof: the stored blob is agent-brain ciphertext (magic
	// header) and the secret never appears anywhere in it.
	stored := remoteBlob(t, bare, "notes.md")
	if !strings.HasPrefix(stored, magicPrefix) {
		t.Fatalf("remote blob lacks agent-brain magic; stored plaintext? %q", stored[:min(16, len(stored))])
	}
	if strings.Contains(stored, secret) {
		t.Fatal("PLAINTEXT LEAKED TO REMOTE — filter chain broken")
	}

	// .gitattributes must itself land as plaintext (the `.gitattributes -filter`
	// exclusion in gitAttributes): if it were encrypted, a fresh clone could not
	// read the attributes that bootstrap the whole filter chain.
	attrs := remoteBlob(t, bare, ".gitattributes")
	if strings.HasPrefix(attrs, magicPrefix) {
		t.Fatal("ATTRIBUTES ENCRYPTED — a fresh clone cannot bootstrap the filter chain")
	}
	if !strings.Contains(attrs, "filter=agentbrain") {
		t.Fatalf(".gitattributes not stored verbatim on the wire: %q", attrs)
	}

	// A second machine sharing the keyset smudges the ciphertext back to the
	// exact original plaintext.
	machineB := newMachine(t, "machine-b", bare)
	if got := readFile(t, machineB, "notes.md"); got != memory {
		t.Fatalf("machine-b smudge mismatch:\n got: %q\nwant: %q", got, memory)
	}
}

func TestFailClosedWithoutKeyset(t *testing.T) {
	t.Parallel()
	bare := newBareRepo(t)
	machineA := newMachine(t, "machine-a", bare)
	writeFile(t, machineA, ".gitattributes", gitAttributes)
	writeFile(t, machineA, "notes.md", "secret\n")
	gitRun(t, machineA, "add", ".gitattributes")
	gitRun(t, machineA, "commit", "--quiet", "-m", "attributes only")

	// Point the filter at an empty config dir: no keyset.
	noKeyset := []string{"AGENT_BRAIN_CONFIG_DIR=" + t.TempDir()}

	// Clean must fail closed: required=true means git refuses the add.
	if out, err := gitRunEnv(t, machineA, noKeyset, "add", "notes.md"); err == nil {
		t.Fatalf("git add without keyset succeeded — plaintext could reach the repo:\n%s", out)
	}

	// Smudge must fail closed: checking out ciphertext without a keyset errors.
	gitRun(t, machineA, "add", "notes.md")
	gitRun(t, machineA, "commit", "--quiet", "-m", "memory: machine-a test 2026-07-07")
	if err := removeAndCheckout(t, machineA, "notes.md", noKeyset); err == nil {
		t.Fatal("checkout of ciphertext without keyset succeeded; want fail-closed error")
	}
}
