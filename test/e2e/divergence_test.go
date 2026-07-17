package e2e

import (
	"strings"
	"testing"
)

// seedTwoMachines pushes an initial memory file from machine A and clones
// machine B at that state. Returns bare, machineA, machineB.
func seedTwoMachines(t *testing.T, seedContent string) (string, string, string) {
	t.Helper()
	bare := newBareRepo(t)
	machineA := newMachine(t, "machine-a", bare)
	writeFile(t, machineA, ".gitattributes", gitAttributes)
	writeFile(t, machineA, "notes.md", seedContent)
	gitRun(t, machineA, "add", ".")
	gitRun(t, machineA, "commit", "--quiet", "-m", "memory: machine-a seed 2026-07-07")
	gitRun(t, machineA, "push", "--quiet", "origin", "main")
	machineB := newMachine(t, "machine-b", bare)
	return bare, machineA, machineB
}

func TestDivergentOverlapRetainsBoth(t *testing.T) {
	t.Parallel()
	bare, machineA, machineB := seedTwoMachines(t, "fact: original\n")

	writeFile(t, machineA, "notes.md", "fact: edited on machine A\n")
	gitRun(t, machineA, "add", "notes.md")
	gitRun(t, machineA, "commit", "--quiet", "-m", "memory: machine-a edit 2026-07-07")
	gitRun(t, machineA, "push", "--quiet", "origin", "main")

	writeFile(t, machineB, "notes.md", "fact: edited on machine B\n")
	gitRun(t, machineB, "add", "notes.md")
	gitRun(t, machineB, "commit", "--quiet", "-m", "memory: machine-b edit 2026-07-07")

	// The moment of truth: rebase must complete WITHOUT stranding — the
	// driver always resolves (spec §4).
	gitRun(t, machineB, "pull", "--rebase", "--quiet", "origin", "main")

	merged := readFile(t, machineB, "notes.md")
	for _, must := range []string{
		"agent-brain conflict",
		"fact: edited on machine A",
		"fact: edited on machine B",
		"agent-brain conflict end",
	} {
		if !strings.Contains(merged, must) {
			t.Fatalf("retain-both missing %q; merged file:\n%s", must, merged)
		}
	}
	if strings.Contains(merged, "<<<<<<<") {
		t.Fatalf("raw git conflict markers leaked:\n%s", merged)
	}

	// Converge machine A onto the retained result and verify equality.
	gitRun(t, machineB, "push", "--quiet", "origin", "main")
	gitRun(t, machineA, "pull", "--rebase", "--quiet", "origin", "main")
	if got := readFile(t, machineA, "notes.md"); got != merged {
		t.Fatalf("machines diverge after sync:\nA: %q\nB: %q", got, merged)
	}

	// And the wire never saw plaintext, even through the driver's re-encrypt:
	// the converged blob is agent-brain ciphertext (magic header), not merely
	// free of the known plaintext — a HasPrefix guard that also rules out a
	// vacuous pass on an empty blob.
	stored := remoteBlob(t, bare, "notes.md")
	if !strings.HasPrefix(stored, magicPrefix) {
		t.Fatalf("converged blob lacks agent-brain magic; plaintext on the wire? %q", stored[:min(16, len(stored))])
	}
	if strings.Contains(stored, "edited on machine") {
		t.Fatal("PLAINTEXT LEAKED TO REMOTE after merge-driver rewrite")
	}
}

func TestDivergentNonOverlapMergesClean(t *testing.T) {
	t.Parallel()
	_, machineA, machineB := seedTwoMachines(t, "top\n\nmiddle\n\nbottom\n")

	writeFile(t, machineA, "notes.md", "top EDITED A\n\nmiddle\n\nbottom\n")
	gitRun(t, machineA, "add", "notes.md")
	gitRun(t, machineA, "commit", "--quiet", "-m", "memory: machine-a top 2026-07-07")
	gitRun(t, machineA, "push", "--quiet", "origin", "main")

	writeFile(t, machineB, "notes.md", "top\n\nmiddle\n\nbottom EDITED B\n")
	gitRun(t, machineB, "add", "notes.md")
	gitRun(t, machineB, "commit", "--quiet", "-m", "memory: machine-b bottom 2026-07-07")
	gitRun(t, machineB, "pull", "--rebase", "--quiet", "origin", "main")

	merged := readFile(t, machineB, "notes.md")
	want := "top EDITED A\n\nmiddle\n\nbottom EDITED B\n"
	if merged != want {
		t.Fatalf("clean 3-way through real git wrong:\n%q\nwant:\n%q", merged, want)
	}
	if strings.Contains(merged, "agent-brain conflict") {
		t.Fatal("non-overlapping edits produced a retain-both block")
	}
}

// TestDivergentLwwKeepsUpstream drives the newest-wins class through a real
// rebase (spec §12): `*.lww.md` maps to merge.agentbrain-lww, whose driver
// keeps %A — the upstream side under pull --rebase. Machine B's replayed
// edit dissolves (its commit becomes empty and the rebase drops it), so B
// converges to A's regenerated copy with no conflict block.
func TestDivergentLwwKeepsUpstream(t *testing.T) {
	t.Parallel()
	bare := newBareRepo(t)
	machineA := newMachine(t, "machine-a", bare)
	writeFile(t, machineA, ".gitattributes", gitAttributes)
	writeFile(t, machineA, "summary.lww.md", "regenerated v1\n")
	gitRun(t, machineA, "add", ".")
	gitRun(t, machineA, "commit", "--quiet", "-m", "memory: machine-a seed 2026-07-07")
	gitRun(t, machineA, "push", "--quiet", "origin", "main")
	machineB := newMachine(t, "machine-b", bare)

	writeFile(t, machineA, "summary.lww.md", "regenerated on machine A\n")
	gitRun(t, machineA, "add", "summary.lww.md")
	gitRun(t, machineA, "commit", "--quiet", "-m", "memory: machine-a regen 2026-07-07")
	gitRun(t, machineA, "push", "--quiet", "origin", "main")

	writeFile(t, machineB, "summary.lww.md", "regenerated on machine B\n")
	gitRun(t, machineB, "add", "summary.lww.md")
	gitRun(t, machineB, "commit", "--quiet", "-m", "memory: machine-b regen 2026-07-07")
	gitRun(t, machineB, "pull", "--rebase", "--quiet", "origin", "main")

	merged := readFile(t, machineB, "summary.lww.md")
	if merged != "regenerated on machine A\n" {
		t.Fatalf("lww through a real rebase must keep the upstream side, got %q", merged)
	}
	if strings.Contains(merged, "agent-brain conflict") || strings.Contains(merged, "<<<<<<<") {
		t.Fatalf("lww must never produce conflict blocks:\n%s", merged)
	}
	stored := remoteBlob(t, bare, "summary.lww.md")
	if !strings.HasPrefix(stored, magicPrefix) {
		t.Fatalf("lww blob lacks agent-brain magic; plaintext on the wire? %q", stored[:min(16, len(stored))])
	}
	if strings.Contains(stored, "regenerated") {
		t.Fatal("PLAINTEXT LEAKED TO REMOTE on the lww path")
	}
}

// TestRetainBothReencryptsThroughResolution hardens the overlap path beyond
// the brief. The brief's overlap test exercises the merge driver's own
// re-encrypt; this proves the retain-both block — plaintext markdown carrying
// HTML-comment anchors and multi-byte UTF-8 (the em-dash and § of spec §4) —
// makes a full round trip through a SUBSEQUENT human commit. The resolved
// block re-enters the repo through the clean filter (git add/commit), a
// different code path than the driver's Encrypt; it must reach the remote as
// agent-brain ciphertext (magic header, no plaintext) and smudge back
// byte-identical on a fresh clone. That is the path a real conflict
// resolution takes, and the engine tests build on it.
func TestRetainBothReencryptsThroughResolution(t *testing.T) {
	t.Parallel()
	bare, machineA, machineB := seedTwoMachines(t, "fact: original\n")

	// Drive both machines into a genuine retain-both on the same file.
	writeFile(t, machineA, "notes.md", "fact: edited on machine A\n")
	gitRun(t, machineA, "add", "notes.md")
	gitRun(t, machineA, "commit", "--quiet", "-m", "memory: machine-a edit 2026-07-07")
	gitRun(t, machineA, "push", "--quiet", "origin", "main")

	writeFile(t, machineB, "notes.md", "fact: edited on machine B\n")
	gitRun(t, machineB, "add", "notes.md")
	gitRun(t, machineB, "commit", "--quiet", "-m", "memory: machine-b edit 2026-07-07")
	gitRun(t, machineB, "pull", "--rebase", "--quiet", "origin", "main")

	retained := readFile(t, machineB, "notes.md")
	if !strings.Contains(retained, "agent-brain conflict") {
		t.Fatalf("setup did not produce a retain-both block:\n%s", retained)
	}

	// A human keeps the retained block and appends their reconciliation, then
	// commits — routing the em-dash and comment anchors through the clean
	// filter rather than the driver's re-encrypt.
	resolved := retained + "fact: reconciled by a human\n"
	writeFile(t, machineB, "notes.md", resolved)
	gitRun(t, machineB, "add", "notes.md")
	gitRun(t, machineB, "commit", "--quiet", "-m", "memory: machine-b resolve 2026-07-07")
	gitRun(t, machineB, "push", "--quiet", "origin", "main")

	// On the wire the resolved block is agent-brain ciphertext, and none of
	// its plaintext markers survive to the remote.
	stored := remoteBlob(t, bare, "notes.md")
	if !strings.HasPrefix(stored, magicPrefix) {
		t.Fatalf("resolved blob lacks agent-brain magic; plaintext on the wire? %q", stored[:min(16, len(stored))])
	}
	for _, leaked := range []string{"agent-brain conflict", "edited on machine", "reconciled by a human"} {
		if strings.Contains(stored, leaked) {
			t.Fatalf("PLAINTEXT LEAKED TO REMOTE — %q visible in stored blob", leaked)
		}
	}

	// A fresh clone smudges the ciphertext back to the exact resolved text,
	// em-dash and anchors intact: the retain-both block round-trips.
	machineC := newMachine(t, "machine-c", bare)
	if got := readFile(t, machineC, "notes.md"); got != resolved {
		t.Fatalf("retain-both block lost bytes across a re-encrypt round trip:\n got: %q\nwant: %q", got, resolved)
	}
}
