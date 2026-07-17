package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// TestAdversarialContainment is the STANDING adversarial corpus (spec §11).
// Each row delivers a hostile input through a RAW git push — an attacker with
// write access to the shared repo who never runs agent-brain and so bypasses
// its filters, scrub, and validation entirely — then pins the engine's
// containment invariant for that attack. Every row ends on
// assertNoPlaintextOnWire, the universal invariant: no hostile action ever
// lands memory plaintext in any git object an attacker could read.
//
// STANDING CONTRACT: later phases APPEND rows here, never delete. A row that
// stops failing when its defense is reverted is not a pin — the git-meta scrub
// rows are proven to fail if their defense is removed: rows 1–5 if the
// post-integrate scrub is removed, and rows 10–11 if
// engine.prepareCheckout's top-of-entry scrub is removed (both leak their
// plaintext sentinel onto the wire).
//
// The two axes are independent, and a row on one axis does NOT cover the other:
// rows 1–5 deliver poison by INTEGRATE into a checkout an earlier cycle already
// scrubbed, so their pre-integrate commit is a no-op; rows 10–11 find poison
// already RESIDENT when the checkout is first cloned or seeded, and commit new
// memory beside it before any scrub of that machine's own has ever run.
func TestAdversarialContainment(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"nested_unit_gitattributes", advNestedUnitGitattributes},
		{"folder_level_gitattributes", advFolderLevelGitattributes},
		{"root_gitattributes_mutation", advRootGitattributesMutation},
		{"gitignore_and_meta_tree", advGitignoreAndMetaTree},
		{"filter_subject_gitignore_file", advFilterSubjectGitignoreFile},
		{"hostile_projects_toml", advHostileProjectsToml},
		{"hostile_manifest_paths", advHostileManifestPaths},
		{"magic_prefix_memory", advMagicPrefixMemory},
		{"file_burst_single_cycle", advFileBurstSingleCycle},
		{"fresh_join_resident_folder_poison", advFreshJoinResidentFolderPoison},
		{"seed_beside_resident_poison", advSeedBesideResidentPoison},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			row.run(t)
		})
	}
}

// --- Rows 1, 2, 4: git-meta scrub (whole-checkout walk) ----------------------

func advNestedUnitGitattributes(t *testing.T) {
	// A .gitattributes INSIDE the unit dir: its `* -filter` would unscope the
	// encryption clean filter for every sibling memory file.
	gitMetaScrubCase(
		t,
		func(t *testing.T, dir string) {
			writeFileRaw(t, dir, "alpha/claude/.gitattributes", "* -filter -diff -merge\n")
		},
		[]string{"alpha/claude/.gitattributes"},
	)
}

func advFolderLevelGitattributes(t *testing.T) {
	// A .gitattributes one level ABOVE the unit dir — the folder-level hole
	// mirror-in's unit-scoped scrub cannot see; only the whole-checkout
	// walk catches it.
	gitMetaScrubCase(
		t,
		func(t *testing.T, dir string) {
			writeFileRaw(t, dir, "alpha/.gitattributes", "* -filter -diff -merge\n")
		},
		[]string{"alpha/.gitattributes"},
	)
}

func advGitignoreAndMetaTree(t *testing.T) {
	// git-meta delivered as DIRECTORIES (trees named .gitignore and
	// .gitattributes) — the scrub's directory branch (RemoveAll + `git rm
	// --cached`). Two constraints shape this row:
	//   - A literal `.git` directory cannot be delivered by a raw push: git's
	//     own verify_path refuses to materialize any `.git` path in a working
	//     tree, so it never reaches a checkout to scrub. A meta-named tree is
	//     the same dir-branch code path the `.git` case would hit.
	//   - A .gitignore FILE (not tree) exercises the scrub's FILE branch and
	//     its force-removal semantics — that case is pinned by the
	//     filter_subject_gitignore_file row below.
	gitMetaScrubCase(
		t,
		func(t *testing.T, dir string) {
			writeFileRaw(t, dir, "alpha/claude/.gitignore/decoy.md", "tree-shaped .gitignore\n")
			writeFileRaw(t, dir, "alpha/claude/.gitattributes/decoy.md", "tree-shaped .gitattributes\n")
		},
		[]string{"alpha/claude/.gitignore", "alpha/claude/.gitattributes"},
	)
}

func advFilterSubjectGitignoreFile(t *testing.T) {
	// A .gitignore FILE inside the unit dir. Unlike .gitattributes (which the
	// root attributes exclude from filtering), .gitignore IS filter-subject:
	// machine B's worktree copy re-cleans (encrypts) to bytes that can never
	// match the attacker's plaintext index blob, so an up-to-date-checking
	// `git rm` would refuse ("local modifications") and abort B's cycle —
	// an availability wedge on attacker-supplied input, though never a
	// confidentiality break. Pins the scrub's force-removal path: git-meta
	// is poison, never user data, so its removal must not depend on any
	// content comparison.
	gitMetaScrubCase(
		t,
		func(t *testing.T, dir string) {
			// `memories/` (not `*`, which would ignore the .gitignore itself
			// and never commit): if this survived in a checkout, `git add -A`
			// sweeps would skip the memories subtree — silently freezing sync.
			writeFileRaw(t, dir, "alpha/claude/.gitignore", "memories/\n")
		},
		[]string{"alpha/claude/.gitignore"},
	)
}

// gitMetaScrubCase is the shared shape of rows 1, 2, and 4: machine A syncs a
// sentinel memory fact, the attacker raw-pushes git-meta at `mutate`'s paths,
// machine B integrates the poison, and every planted path is scrubbed from B's
// checkout AND healed off the remote main, while the fact stays ciphertext and
// its sentinel never leaks.
func gitMetaScrubCase(t *testing.T, mutate func(t *testing.T, dir string), absentPaths []string) {
	t.Helper()
	a, b, bare := newTwoMachines(t)

	const sentinel = "gitmeta-do-not-leak"
	a.write(t, "memories/fact.md", "the secret is "+sentinel+"\n")
	a.sync(t)
	b.sync(t) // B holds the fact

	attackerPush(t, bare, "poison: git-meta", mutate)

	b.sync(t) // B integrates the poison, scrubs it, heals, and pushes

	for _, path := range absentPaths {
		if _, err := os.Stat(filepath.Join(b.checkout, filepath.FromSlash(path))); !os.IsNotExist(err) {
			t.Fatalf("git-meta %q still in B's checkout after the scrub cycle", path)
		}
		// cat-file -e exits non-zero when the path is absent from main's tree:
		// the heal commit that removed it reached the remote.
		if _, err := gitRunEnv(t, bare, nil, "cat-file", "-e", "main:"+path); err == nil {
			t.Fatalf("git-meta %q still on the remote main — B's heal did not propagate", path)
		}
	}
	// The poison never reached B's provider dir.
	if _, err := os.Stat(filepath.Join(b.unit.LocalDir, ".gitattributes")); !os.IsNotExist(err) {
		t.Fatal("hostile .gitattributes reached B's provider dir")
	}
	if _, err := os.Stat(filepath.Join(b.unit.LocalDir, ".gitignore")); !os.IsNotExist(err) {
		t.Fatal("hostile .gitignore reached B's provider dir")
	}
	// The fact is still ciphertext on the wire.
	if blob := remoteBlob(t, bare, "alpha/claude/memories/fact.md"); !strings.HasPrefix(blob, magicPrefix) {
		t.Fatal("memory fact is not ciphertext after the scrub cycle — encryption was unscoped")
	}
	assertNoPlaintextOnWire(t, bare, sentinel)
}

// --- Row 3: root .gitattributes mutation (byte-canonical heal) --------------

func advRootGitattributesMutation(t *testing.T) {
	a, b, bare := newTwoMachines(t)

	const sentinel = "root-heal-sentinel"
	a.write(t, "memories/fact.md", "secret "+sentinel+"\n")
	a.sync(t)
	b.sync(t)

	// Strip the filter lines from the ROOT .gitattributes: a fleet-wide unscope
	// of the encryption filter for every later add.
	attackerPush(t, bare, "poison: unscope root attributes", func(t *testing.T, dir string) {
		writeFileRaw(t, dir, ".gitattributes", "* text\n")
	})

	b.sync(t) // integrate + heal the root file byte-canonical

	// The root .gitattributes on the wire carries the filter wiring again.
	if attrs := remoteBlob(t, bare, ".gitattributes"); !strings.Contains(attrs, "filter=agentbrain") {
		t.Fatalf("root .gitattributes not healed on the wire:\n%s", attrs)
	}

	// Subsequent adds still encrypt: a fresh memory file lands as ciphertext.
	const sentinel2 = "post-heal-sentinel"
	b.write(t, "memories/after.md", "still secret "+sentinel2+"\n")
	b.sync(t)
	if blob := remoteBlob(t, bare, "alpha/claude/memories/after.md"); !strings.HasPrefix(blob, magicPrefix) {
		t.Fatal("post-heal memory add is not ciphertext — the root filter is still unscoped")
	}
	assertNoPlaintextOnWire(t, bare, sentinel, sentinel2)
}

// --- Row 5: hostile projects.toml (repo.LoadProjects rejects it) -------------

func advHostileProjectsToml(t *testing.T) {
	a, b, bare := newTwoMachines(t)

	const sentinel = "projects-sentinel"
	a.write(t, "memories/fact.md", "secret "+sentinel+"\n")
	a.sync(t)
	b.sync(t)

	// A shared registry naming a traversal folder and delivered raw.
	attackerPush(t, bare, "poison: projects.toml", func(t *testing.T, dir string) {
		writeFileRaw(t, dir, ".agent-brain/projects.toml",
			"version = 1\n\n[projects.\"../escape\"]\nid = \"evil/owner/repo\"\n")
	})
	materialize(t, b) // B pulls the poisoned registry onto disk

	// An admin op (what a `track` drives) loads the shared registry; loudly
	// rejected, never silently skipped.
	if _, err := b.engine.RegisterProject(suiteCtx, "claude", "some/new/id", "newproj"); err == nil {
		t.Fatal("RegisterProject accepted a projects.toml with folder \"../escape\"")
	}
	// No traversal: nothing named "escape" was created beside the checkout.
	if _, err := os.Stat(filepath.Join(filepath.Dir(b.checkout), "escape")); !os.IsNotExist(err) {
		t.Fatal("hostile folder \"../escape\" escaped the checkout")
	}
	assertNoPlaintextOnWire(t, bare, sentinel)
}

// --- Row 6: hostile manifest path keys (repo.LoadManifest rejects them) ------

func advHostileManifestPaths(t *testing.T) {
	a, b, bare := newTwoMachines(t)

	const sentinel = "manifest-sentinel"
	a.write(t, "memories/fact.md", "secret "+sentinel+"\n")
	a.sync(t)
	b.sync(t)

	// Overwrite B's OWN manifest (the one its cycle loads first) with a
	// traversal path key.
	attackerPush(t, bare, "poison: manifest paths", func(t *testing.T, dir string) {
		writeFileRaw(t, dir, ".agent-brain/manifests/host-b.json",
			`{"version":1,"files":{"../../etc/evil":{"size":1,"mtime_unix_nano":0,"sha256":"deadbeef"}}}`+"\n")
	})
	materialize(t, b) // B pulls the poisoned manifest onto disk

	// B's next cycle loads its manifest at the top and must reject it before
	// doing any work.
	if _, err := b.engine.Sync(suiteCtx, []repo.Unit{b.unit}); err == nil {
		t.Fatal("Sync accepted a manifest carrying a '../../etc/evil' path key")
	}
	if _, err := os.Stat("/etc/evil"); !os.IsNotExist(err) {
		t.Fatal("manifest traversal wrote a file outside the checkout")
	}
	assertNoPlaintextOnWire(t, bare, sentinel)
}

// --- Row 7: memory content that mimics the codec magic prefix ----------------

func advMagicPrefixMemory(t *testing.T) {
	a, _, bare := newTwoMachines(t)

	// A memory file whose PLAINTEXT begins with the ciphertext magic prefix.
	// The clean filter must fail closed rather than store bytes it could later
	// mistake for already-encrypted content (a Phase-1 crypto pin, re-asserted
	// here at the engine level).
	a.write(t, "memories/spoof.md", magicPrefix+"pretend-ciphertext-sentinel\n")
	if _, err := a.engine.Sync(suiteCtx, []repo.Unit{a.unit}); err == nil {
		t.Fatal("Sync accepted a memory file starting with the codec magic prefix — the clean filter did not fail closed")
	}
	assertNoPlaintextOnWire(t, bare, "pretend-ciphertext-sentinel")
}

// --- Row 8: a large file burst coalesces into one cycle ----------------------

func advFileBurstSingleCycle(t *testing.T) {
	a, _, bare := newTwoMachines(t)

	const (
		sentinel = "burst-leak-sentinel"
		count    = 5000
	)
	for i := range count {
		a.write(t, fmt.Sprintf("memories/burst-%04d.md", i), fmt.Sprintf("fact %d %s\n", i, sentinel))
	}

	report := a.sync(t) // ONE coalesced cycle handles the whole burst
	if report.MirrorIn.Copied < count {
		t.Fatalf("burst was not coalesced into one cycle: mirrored %d of %d files", report.MirrorIn.Copied, count)
	}

	// Spot-check the extremes are ciphertext...
	for _, i := range []int{0, count / 2, count - 1} {
		path := fmt.Sprintf("alpha/claude/memories/burst-%04d.md", i)
		if blob := remoteBlob(t, bare, path); !strings.HasPrefix(blob, magicPrefix) {
			t.Fatalf("burst blob %s is not ciphertext", path)
		}
	}
	// ...and the universal invariant proves NONE of the 5k blobs leaked.
	assertNoPlaintextOnWire(t, bare, sentinel)
}

// --- Rows 10, 11: resident poison at the commit boundary (F1, final review) --

func advFreshJoinResidentFolderPoison(t *testing.T) {
	// Rows 1–5 all poison a machine whose checkout an EARLIER cycle already
	// scrubbed: the victim integrates the hostile push and the post-integrate
	// scrub heals before any commit lands beside the poison. This row pins
	// the window those rows structurally miss (F1, Phase-3 final review):
	// the poison is ALREADY RESIDENT when the victim's checkout comes into
	// existence — a fresh machine joins by cloning a poisoned main — and the
	// victim's FIRST cycle commits new memory beside it. A folder-level
	// `* -filter` unselects the encryption clean filter for the subtree
	// (deepest-.gitattributes-wins), `filter.required` never fires for an
	// unselected filter, and without a scrub at the cycle TOP the first
	// `git add` stores plaintext.
	bare := newBareRepo(t)
	a := newSyncMachine(t, "host-a", bare, true)
	a.write(t, "memories/steady.md", "benign steady fact\n")
	a.sync(t)

	// The attacker poisons the existing folder BEFORE the new machine exists.
	attackerPush(t, bare, "poison: folder attributes before join", func(t *testing.T, dir string) {
		writeFileRaw(t, dir, "alpha/.gitattributes", "* -filter -diff -merge\n")
	})

	// A fresh machine joins AFTER the poison landed: its clone materializes
	// alpha/.gitattributes, and no cycle of its own has ever scrubbed it.
	const sentinel = "freshjoin-do-not-leak"
	c := newSyncMachine(t, "host-c", bare, false)
	c.write(t, "memories/joined.md", "the secret is "+sentinel+"\n")
	report := c.sync(t) // FIRST cycle: mirror-in + commit with poison resident

	// The scrub ran at the boundary and the cycle log names the heal.
	if !slices.Contains(report.Scrubbed, "alpha/.gitattributes") {
		t.Fatalf("first cycle did not report scrubbing the resident poison: Scrubbed = %v", report.Scrubbed)
	}
	// The first-cycle commit is ciphertext…
	if blob := remoteBlob(t, bare, "alpha/claude/memories/joined.md"); !strings.HasPrefix(blob, magicPrefix) {
		t.Fatal("fresh machine's first commit stored plaintext beside resident folder poison")
	}
	// …the poison is gone from the fresh checkout and healed off the remote…
	if _, err := os.Stat(filepath.Join(c.checkout, "alpha", ".gitattributes")); !os.IsNotExist(err) {
		t.Fatal("resident folder poison still in the fresh checkout after its first cycle")
	}
	if _, err := gitRunEnv(t, bare, nil, "cat-file", "-e", "main:alpha/.gitattributes"); err == nil {
		t.Fatal("folder poison still on the remote main after the fresh machine's first cycle")
	}
	assertNoPlaintextOnWire(t, bare, sentinel)
}

func advSeedBesideResidentPoison(t *testing.T) {
	// migrate's seed (SeedProject) is a standalone admin commit OUTSIDE the
	// sync cycle. Same resident-poison window as the fresh-join row, reached
	// through the other committing entry point: a fresh machine joins a
	// poisoned main, then seeds a legacy memory tree into the poisoned
	// folder. The seed's own source-tree git-meta refusal cannot help — the
	// poison is already in the CHECKOUT, not the source tree. Without a
	// scrub before the seed's `git add`, the seed layer commits as plaintext
	// locally and the daemon's post-migrate cycle pushes those blobs.
	bare := newBareRepo(t)
	a := newSyncMachine(t, "host-a", bare, true)
	a.write(t, "memories/steady.md", "benign steady fact\n")
	a.sync(t)

	attackerPush(t, bare, "poison: folder attributes before migrate", func(t *testing.T, dir string) {
		writeFileRaw(t, dir, "alpha/.gitattributes", "* -filter -diff -merge\n")
	})

	const sentinel = "seedlayer-do-not-leak"
	c := newSyncMachine(t, "host-c", bare, false)
	legacy := t.TempDir()
	writeFileRaw(t, legacy, "imported.md", "legacy secret "+sentinel+"\n")
	if _, err := c.engine.SeedProject(suiteCtx, "alpha", "claude", "legacy-slug", legacy); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c.sync(t) // the migrate flow runs a cycle right after the seed — it pushes

	if blob := remoteBlob(t, bare, "alpha/claude/imported.md"); !strings.HasPrefix(blob, magicPrefix) {
		t.Fatal("seed layer stored plaintext beside resident folder poison")
	}
	assertNoPlaintextOnWire(t, bare, sentinel)
}

// --- Shared attacker + assertion helpers -------------------------------------

// attackerPush clones bare with NO agent-brain filters wired (an attacker with
// repo write access who never ran agent-brain), lets mutate plant hostile
// content, then commits and pushes it to main. Because no clean filter is
// wired in this clone, whatever mutate writes is stored RAW — the whole point:
// bypassing both encryption and the engine's own git-meta scrub.
func attackerPush(t *testing.T, bare, message string, mutate func(t *testing.T, dir string)) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "attacker")
	gitRun(t, filepath.Dir(dir), "clone", "--quiet", bare, dir)
	gitRun(t, dir, "config", "user.name", "attacker")
	gitRun(t, dir, "config", "user.email", "attacker@evil.invalid")
	mutate(t, dir)
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "--quiet", "-m", message)
	gitRun(t, dir, "push", "--quiet", "origin", "main")
}

// writeFileRaw writes content at the slash-separated rel under dir, creating
// parents — the attacker planting a file straight onto disk.
func writeFileRaw(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// materialize brings whatever was last pushed to the remote onto machine's
// working checkout (fetch + hard reset) — standing in for a prior cycle having
// integrated the attacker's commit, so the NEXT load reads the poison from
// disk. The engine self-heals its own manifest across a normal integrate, so
// forcing the poison onto disk is what exposes it to the load-time validators.
func materialize(t *testing.T, machine *syncMachine) {
	t.Helper()
	gitRun(t, machine.checkout, "fetch", "--quiet", "origin")
	gitRun(t, machine.checkout, "reset", "--hard", "origin/main")
}

// assertNoPlaintextOnWire streams EVERY object in the bare repo (reachable or
// not — every pushed object lands in the store) and fails if any sentinel
// appears in any of them. Sentinels must be content-only words that never
// occur in a path or commit message, since trees and commits are in the stream
// as plaintext by design.
func assertNoPlaintextOnWire(t *testing.T, bare string, sentinels ...string) {
	t.Helper()
	objects := gitRun(t, bare, "cat-file", "--batch-all-objects", "--batch", "--buffer")
	for _, sentinel := range sentinels {
		if strings.Contains(objects, sentinel) {
			t.Fatalf("SAFETY VIOLATION: plaintext sentinel %q found in a git object on the wire", sentinel)
		}
	}
}
