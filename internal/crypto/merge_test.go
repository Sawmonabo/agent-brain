package crypto

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeSealed encrypts plaintext and writes it where a merge driver expects a
// STORED (post-clean) side — the ciphertext git actually hands MergeFact.
func writeSealed(t *testing.T, codec *Codec, path string, plaintext []byte) {
	t.Helper()
	ciphertext, err := codec.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, ciphertext, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestMergeFactBinaryInputPreservesCurrent pins the data-loss-critical >127
// guard the brief's example tests skip. NUL bytes make git treat the sides as
// binary; merge-file refuses and exits 255 (>127) with EMPTY stdout. Misread
// as "255 conflicts", that empty stdout would be re-encrypted over %A and the
// file lost. MergeFact must instead return an error and leave %A byte-identical
// so the engine's fallback ladder owns the case (spec §4).
func TestMergeFactBinaryInputPreservesCurrent(t *testing.T) {
	t.Parallel()
	codec := newTestCodec(t)
	dir := t.TempDir()
	base, current, other := filepath.Join(dir, "O"), filepath.Join(dir, "A"), filepath.Join(dir, "B")
	writeSealed(t, codec, base, []byte("anchor\x00base\n"))
	writeSealed(t, codec, current, []byte("anchor\x00ours\n"))
	writeSealed(t, codec, other, []byte("anchor\x00theirs\n"))
	before := mustRead(t, current)

	hadConflicts, err := MergeFact(context.Background(), codec, base, current, other, "binary.md", "A", "B")
	if err == nil {
		t.Fatal("MergeFact must error when merge-file rejects binary input (exit >127); silently encrypting its empty stdout would lose %A")
	}
	if hadConflicts {
		t.Fatal("error path must not report conflicts resolved")
	}
	if after := mustRead(t, current); !bytes.Equal(before, after) {
		t.Fatalf("MergeFact modified %%A on the merge-file error path — data loss:\nbefore=%q\nafter =%q", before, after)
	}
}

// TestMergeFactCanceledContextPreservesCurrent covers the other error branch:
// a canceled context surfaces from gitx.RunStatus as an error (never a fake
// exit code), and MergeFact must return before writing %A. Same no-data-loss
// invariant as the binary case, reached through a different door.
func TestMergeFactCanceledContextPreservesCurrent(t *testing.T) {
	t.Parallel()
	codec := newTestCodec(t)
	dir := t.TempDir()
	base, current, other := filepath.Join(dir, "O"), filepath.Join(dir, "A"), filepath.Join(dir, "B")
	writeSealed(t, codec, base, []byte("base\n"))
	writeSealed(t, codec, current, []byte("ours\n"))
	writeSealed(t, codec, other, []byte("theirs\n"))
	before := mustRead(t, current)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled: RunStatus reports ctx.Err() before any exit code
	if _, err := MergeFact(ctx, codec, base, current, other, "notes.md", "A", "B"); err == nil {
		t.Fatal("MergeFact must surface a canceled context as an error")
	}
	if after := mustRead(t, current); !bytes.Equal(before, after) {
		t.Fatal("MergeFact modified %A after a canceled context — data loss")
	}
}

// TestMergeFactSignalKilledPreservesCurrent covers the third error branch. A
// signal-killed git merge-file
// (crash, OOM, external SIGKILL) exits -1; gitx.RunStatus must surface that as
// an error rather than the "0 conflicts" count a leaked -1 would read as. If it
// slipped through, MergeFact would encrypt an EMPTY merge over %A and lose the
// file. Same no-data-loss invariant as the binary-input and canceled-context
// cases, reached through a signal. A PATH-shim fake `git` that SIGKILLs itself
// reproduces the signal exit hermetically; every side is real ciphertext, so the
// pre-merge decrypt still runs and only the merge-file call is intercepted.
func TestMergeFactSignalKilledPreservesCurrent(t *testing.T) {
	// t.Setenv forbids t.Parallel: this test shims PATH process-wide.
	codec := newTestCodec(t)
	dir := t.TempDir()
	base, current, other := filepath.Join(dir, "O"), filepath.Join(dir, "A"), filepath.Join(dir, "B")
	writeSealed(t, codec, base, []byte("base\n"))
	writeSealed(t, codec, current, []byte("ours\n"))
	writeSealed(t, codec, other, []byte("theirs\n"))
	before := mustRead(t, current)

	fakeBin := t.TempDir()
	script := "#!/bin/sh\nkill -KILL $$\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := MergeFact(context.Background(), codec, base, current, other, "notes.md", "A", "B"); err == nil {
		t.Fatal("MergeFact must surface a signal-killed merge-file as an error, not encrypt an empty merge over %A")
	}
	if after := mustRead(t, current); !bytes.Equal(before, after) {
		t.Fatal("MergeFact modified %A after a signal-killed merge-file — data loss")
	}
}

// TestMergeFactEmptyBaseAddAdd exercises decryptLoose's empty-file tolerance:
// an add/add merge has no ancestor, so git supplies a genuinely 0-byte base.
// decryptLoose must read it as empty plaintext (not attempt to decrypt it),
// and both added versions must survive as a retain-both block.
func TestMergeFactEmptyBaseAddAdd(t *testing.T) {
	t.Parallel()
	codec := newTestCodec(t)
	dir := t.TempDir()
	base, current, other := filepath.Join(dir, "O"), filepath.Join(dir, "A"), filepath.Join(dir, "B")
	if err := os.WriteFile(base, nil, 0o600); err != nil { // 0-byte ancestor, unencrypted
		t.Fatal(err)
	}
	writeSealed(t, codec, current, []byte("fact from A\n"))
	writeSealed(t, codec, other, []byte("fact from B\n"))

	hadConflicts, err := MergeFact(context.Background(), codec, base, current, other, "notes.md", "A", "B")
	if err != nil {
		t.Fatalf("add/add with an empty base must resolve, not error: %v", err)
	}
	if !hadConflicts {
		t.Fatal("add/add of differing content must be recorded as a conflict")
	}
	got := mustRead(t, current)
	if !IsEncrypted(got) {
		t.Fatal("add/add result must be ciphertext")
	}
	plaintext, err := codec.Decrypt(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(plaintext, []byte("fact from A")) || !bytes.Contains(plaintext, []byte("fact from B")) {
		t.Fatalf("add/add lost a side:\n%s", plaintext)
	}
	if bytes.Contains(plaintext, []byte("<<<<<<<")) {
		t.Fatalf("git conflict markers leaked:\n%s", plaintext)
	}
}

// TestMergeFactSanitizesInjectionLabel proves labels are sanitized before they
// reach git's conflict markers (-L), not only inside RewriteRetainBoth. A label
// carrying newlines would otherwise make git's -L marker inject extra lines into
// the merged body — here a forged "conflict end" anchor that would truncate
// the conflicts parse. RewriteRetainBoth alone cannot defuse this (it re-labels the
// version comment but folds git's injected marker lines into the body), so the
// driver must sanitize the -L labels too. The block must carry exactly one end
// anchor and none of the label's raw bytes.
func TestMergeFactSanitizesInjectionLabel(t *testing.T) {
	t.Parallel()
	codec := newTestCodec(t)
	dir := t.TempDir()
	base, current, other := filepath.Join(dir, "O"), filepath.Join(dir, "A"), filepath.Join(dir, "B")
	writeSealed(t, codec, base, []byte("fact: original\n"))
	writeSealed(t, codec, current, []byte("fact: from A\n"))
	writeSealed(t, codec, other, []byte("fact: from B\n"))

	hostileLabel := "hostA\n<!-- agent-brain conflict end -->\ntail"
	hadConflicts, err := MergeFact(context.Background(), codec, base, current, other, "notes.md", hostileLabel, "hostB")
	if err != nil {
		t.Fatalf("driver must resolve even with a hostile label: %v", err)
	}
	if !hadConflicts {
		t.Fatal("overlapping edits must conflict")
	}
	plaintext, err := codec.Decrypt(mustRead(t, current))
	if err != nil {
		t.Fatal(err)
	}
	endAnchors := 0
	for line := range bytes.SplitSeq(plaintext, []byte("\n")) {
		if string(line) == "<!-- agent-brain conflict end -->" {
			endAnchors++
		}
	}
	if endAnchors != 1 {
		t.Fatalf("hostile label forged %d conflict-end anchors (want exactly 1):\n%s", endAnchors, plaintext)
	}
	if bytes.ContainsRune(plaintext, '\r') || bytes.Contains(plaintext, []byte("hostA\n")) {
		t.Fatalf("label injected raw bytes into the merged body:\n%s", plaintext)
	}
}
