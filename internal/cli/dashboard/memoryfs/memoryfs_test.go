package memoryfs_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/providertest"
)

// testRegistry builds a registry with the same two provider identities and
// pattern shapes the engine's own tests fake (internal/engine/helpers_test.go):
// a per-project "claude" and a global "codex" with a RepoSubdir root, close
// enough to the real adapters' tables (claude.Patterns/codex.builtinPatterns)
// to exercise classification without pulling in their Discover/Identify
// filesystem logic, which this package never calls.
func testRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	claudeFake := providertest.New("claude", provider.ScopePerProject, []provider.Pattern{
		{Glob: "MEMORY.md", Class: provider.ClassDerivedIndex},
		{Glob: ".DS_Store", Class: provider.ClassIgnore},
		{Glob: "**/.DS_Store", Class: provider.ClassIgnore},
	})
	codexFake := providertest.New("codex", provider.ScopeGlobal, []provider.Pattern{
		{Glob: "memories/MEMORY.md", Class: provider.ClassRegenerated},
		{Glob: ".DS_Store", Class: provider.ClassIgnore},
		{Glob: "**/.DS_Store", Class: provider.ClassIgnore},
	})
	registry, err := provider.NewRegistry(claudeFake, codexFake)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// projected is the subset of Memory fields this test can pin deterministically:
// ModTime is filesystem-assigned and varies per run, so it is checked
// separately rather than folded into the cmp.Diff below.
type projected struct {
	Provider    string
	Folder      string
	RelPath     string
	RepoPath    string
	Name        string
	Description string
	Class       provider.Class
	Size        int64
}

func project(memories []memoryfs.Memory) []projected {
	out := make([]projected, len(memories))
	for i, m := range memories {
		out[i] = projected{
			Provider:    m.Provider,
			Folder:      m.Folder,
			RelPath:     m.RelPath,
			RepoPath:    m.RepoPath,
			Name:        m.Name,
			Description: m.Description,
			Class:       m.Class,
			Size:        m.Size,
		}
	}
	return out
}

// TestListClassifiesAndOrders exercises List across a claude unit (a fact
// file, the derived-index MEMORY.md, an ignored .DS_Store, and a skipped
// symlink) and a codex unit with a RepoSubdir, pinning: ignore-class
// exclusion, RepoPath's subdir inclusion, frontmatter-vs-stem naming, and a
// deterministic (Folder, RepoPath) sort order.
func TestListClassifiesAndOrders(t *testing.T) {
	t.Parallel()
	registry := testRegistry(t)

	claudeDir := t.TempDir()
	writeFile(t, claudeDir, "notes.md", "plain fact, no frontmatter\n")
	writeFile(t, claudeDir, "MEMORY.md", "# Memory index\n")
	writeFile(t, claudeDir, ".DS_Store", "finder junk\n")
	if err := os.Symlink(filepath.Join(claudeDir, "notes.md"), filepath.Join(claudeDir, "shortcut.md")); err != nil {
		t.Fatalf("seed symlink: %v", err)
	}

	codexDir := t.TempDir()
	writeFile(t, codexDir, "topic.md", "---\nname: Topic Name\ndescription: Topic hook\n---\nbody\n")

	units := []api.UnitInfo{
		{Provider: "claude", Folder: "acme", LocalDir: claudeDir},
		{Provider: "codex", Folder: "acme", LocalDir: codexDir, RepoSubdir: "memories"},
	}

	got, err := memoryfs.List(registry, units)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	want := []projected{
		{Provider: "claude", Folder: "acme", RelPath: "MEMORY.md", RepoPath: "claude/MEMORY.md", Name: "MEMORY", Class: provider.ClassDerivedIndex, Size: int64(len("# Memory index\n"))},
		{Provider: "claude", Folder: "acme", RelPath: "notes.md", RepoPath: "claude/notes.md", Name: "notes", Class: provider.ClassFact, Size: int64(len("plain fact, no frontmatter\n"))},
		{Provider: "codex", Folder: "acme", RelPath: "topic.md", RepoPath: "codex/memories/topic.md", Name: "Topic Name", Description: "Topic hook", Class: provider.ClassFact, Size: int64(len("---\nname: Topic Name\ndescription: Topic hook\n---\nbody\n"))},
	}
	if diff := cmp.Diff(want, project(got)); diff != "" {
		t.Errorf("List() mismatch (-want +got):\n%s", diff)
	}
	for _, m := range got {
		if m.ModTime.IsZero() {
			t.Errorf("Memory %s: ModTime is zero", m.RepoPath)
		}
		if m.LocalDir == "" {
			t.Errorf("Memory %s: LocalDir is empty", m.RepoPath)
		}
	}
}

// TestListMarksPrimaryIndex pins IsIndex enumeration: exactly the file whose
// RepoSubdir-prefixed classify path equals its provider's PrimaryIndexPath is
// marked, every other file stays false, and the match is computed in the same
// classifyRel(RepoSubdir, rel) space Classify uses — so a codex-shaped unit
// under RepoSubdir "memories" matches on "memories/MEMORY.md", not the bare
// "MEMORY.md". The second case's index also sits at ClassRegenerated (not a
// derived-index class), proving IsIndex is a display fact independent of Class.
func TestListMarksPrimaryIndex(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		provider   string
		scope      provider.Scope
		indexPath  string
		patterns   []provider.Pattern
		repoSubdir string
		files      []string
		wantIndex  string // the RelPath expected to carry IsIndex==true
	}{
		{
			name:      "per-project index at provider-dir root",
			provider:  "claude",
			scope:     provider.ScopePerProject,
			indexPath: "MEMORY.md",
			files:     []string{"MEMORY.md", "alpha.md", "zulu.md"},
			wantIndex: "MEMORY.md",
		},
		{
			name:       "global index under a RepoSubdir prefix",
			provider:   "codex",
			scope:      provider.ScopeGlobal,
			indexPath:  "memories/MEMORY.md",
			patterns:   []provider.Pattern{{Glob: "memories/MEMORY.md", Class: provider.ClassRegenerated}},
			repoSubdir: "memories",
			files:      []string{"MEMORY.md", "notes.md"},
			wantIndex:  "MEMORY.md",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := providertest.New(tt.provider, tt.scope, tt.patterns).WithPrimaryIndex(tt.indexPath)
			registry, err := provider.NewRegistry(fake)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			for _, rel := range tt.files {
				writeFile(t, dir, rel, "# "+rel+"\n")
			}
			units := []api.UnitInfo{{Provider: tt.provider, Folder: "acme", LocalDir: dir, RepoSubdir: tt.repoSubdir}}

			got, err := memoryfs.List(registry, units)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}

			var markedIndex bool
			for _, memory := range got {
				wantIsIndex := memory.RelPath == tt.wantIndex
				if memory.IsIndex != wantIsIndex {
					t.Errorf("RelPath %q: IsIndex = %v, want %v", memory.RelPath, memory.IsIndex, wantIsIndex)
				}
				if memory.IsIndex {
					markedIndex = true
				}
			}
			if !markedIndex {
				t.Errorf("no memory marked IsIndex; expected %q to be the index", tt.wantIndex)
			}
		})
	}
}

// TestListMissingUnitDirIsNotError pins the "enrolled-but-empty unit is
// normal" contract: a LocalDir that does not exist yet yields no entries and
// no error, distinguishing it from a genuine walk failure.
func TestListMissingUnitDirIsNotError(t *testing.T) {
	t.Parallel()
	registry := testRegistry(t)
	units := []api.UnitInfo{
		{Provider: "claude", Folder: "acme", LocalDir: filepath.Join(t.TempDir(), "never-created")},
	}
	got, err := memoryfs.List(registry, units)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List() = %v, want empty", got)
	}
}

// TestListUnregisteredProviderErrors pins the fail-loud contract mirror_in.go
// itself uses: a unit naming a provider absent from the registry is a
// configuration error, never a silently-skipped unit.
func TestListUnregisteredProviderErrors(t *testing.T) {
	t.Parallel()
	registry := testRegistry(t)
	units := []api.UnitInfo{{Provider: "ghost", Folder: "acme", LocalDir: t.TempDir()}}
	if _, err := memoryfs.List(registry, units); err == nil {
		t.Fatal("List() with an unregistered provider = nil error, want an error")
	}
}

// TestReadBodyCapsOversize pins the 1 MiB read cap: a file at the cap reads
// clean, one byte over is refused with ErrTooLarge rather than silently
// truncated.
func TestReadBodyCapsOversize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "well under cap", size: 1024, wantErr: false},
		{name: "exactly at cap", size: 1 << 20, wantErr: false},
		{name: "one byte over cap", size: 1<<20 + 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			content := bytes.Repeat([]byte("a"), tt.size)
			writeFile(t, dir, "big.md", string(content))
			m := memoryfs.Memory{LocalDir: dir, RelPath: "big.md"}
			got, err := memoryfs.ReadBody(m)
			if tt.wantErr {
				if !errors.Is(err, memoryfs.ErrTooLarge) {
					t.Fatalf("ReadBody() error = %v, want ErrTooLarge", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadBody() error = %v, want nil", err)
			}
			if got != string(content) {
				t.Fatalf("ReadBody() returned %d bytes, want %d", len(got), len(content))
			}
		})
	}
}

// TestWriteFileAtomicReplacesInOneRename proves the no-partial-content
// acceptance row (spec §17): a concurrent reader racing 50 replacements
// always observes exactly one whole generation of content, never a byte
// mix of both — the property that only holds if WriteFileAtomic never
// truncates-in-place and always lands content via a same-volume rename.
func TestWriteFileAtomicReplacesInOneRename(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const rel = "notes.md"
	fullPath := filepath.Join(dir, rel)

	contentA := bytes.Repeat([]byte("A"), 64*1024)
	contentB := bytes.Repeat([]byte("B"), 64*1024)
	if err := memoryfs.WriteFileAtomic(dir, rel, contentA); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	var stop atomic.Bool
	var mismatch error
	var readers sync.WaitGroup
	readers.Go(func() {
		for !stop.Load() {
			data, err := os.ReadFile(fullPath)
			if err != nil {
				continue // rename(2) is atomic; a transient error here is not the property under test
			}
			if !bytes.Equal(data, contentA) && !bytes.Equal(data, contentB) {
				mismatch = fmt.Errorf("observed %d bytes matching neither seeded generation (partial content)", len(data))
				return
			}
		}
	})

	for i := range 50 {
		want := contentA
		if i%2 == 0 {
			want = contentB
		}
		if err := memoryfs.WriteFileAtomic(dir, rel, want); err != nil {
			t.Fatalf("replace %d: %v", i, err)
		}
	}
	stop.Store(true)
	readers.Wait()

	if mismatch != nil {
		t.Fatal(mismatch)
	}
}

// TestWriteFileAtomicCreatesParentDirs pins the documented 0o700 parent-dir
// creation for a target whose subdirectory does not exist yet (a fresh
// codex RepoSubdir root, or a nested rename target).
func TestWriteFileAtomicCreatesParentDirs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := memoryfs.WriteFileAtomic(dir, "sub/deep/notes.md", []byte("content")); err != nil {
		t.Fatalf("WriteFileAtomic() error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "sub", "deep", "notes.md"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(got) != "content" {
		t.Fatalf("content = %q, want %q", got, "content")
	}
}

// TestWriteFileAtomicValidatesTarget pins the symmetric guard Rename already
// had: a traversal, absolute, or "."-segment rel is refused before any
// filesystem effect, the same repo.ValidateRelPath contract Rename applies
// to its own target. Each rejected row asserts the parent directory (one
// level above the unit dir passed as dir) gains no file at all — not just
// that the write lands somewhere unexpected, but that nothing happens
// outside dir.
func TestWriteFileAtomicValidatesTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		rel     string
		wantErr bool
	}{
		{name: "path traversal rejected", rel: "../escape.md", wantErr: true},
		{name: "absolute path rejected", rel: "/escape.md", wantErr: true},
		{name: "dot segment rejected", rel: ".", wantErr: true},
		{name: "clean relative path succeeds", rel: "notes.md", wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parent := t.TempDir()
			dir := filepath.Join(parent, "unit")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}

			err := memoryfs.WriteFileAtomic(dir, tt.rel, []byte("content"))

			if tt.wantErr {
				if err == nil {
					t.Fatalf("WriteFileAtomic(%q) = nil, want error", tt.rel)
				}
				parentEntries, readErr := os.ReadDir(parent)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if len(parentEntries) != 1 || parentEntries[0].Name() != "unit" {
					t.Fatalf("parent dir contents = %v, want only the untouched unit dir", parentEntries)
				}
				unitEntries, readErr := os.ReadDir(dir)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if len(unitEntries) != 0 {
					t.Fatalf("unit dir contents = %v, want empty (rejected write had no effect)", unitEntries)
				}
				return
			}
			if err != nil {
				t.Fatalf("WriteFileAtomic(%q) error = %v, want nil", tt.rel, err)
			}
			got, readErr := os.ReadFile(filepath.Join(dir, tt.rel))
			if readErr != nil {
				t.Fatalf("read written file: %v", readErr)
			}
			if string(got) != "content" {
				t.Fatalf("content = %q, want %q", got, "content")
			}
		})
	}
}

// TestDeleteRemovesFile pins Delete's plain-remove contract.
func TestDeleteRemovesFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "notes.md", "content")
	m := memoryfs.Memory{LocalDir: dir, RelPath: "notes.md"}
	if err := memoryfs.Delete(m); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.md")); !os.IsNotExist(err) {
		t.Fatalf("file still exists after Delete(), stat err = %v", err)
	}
}

// TestRenameValidatesTarget pins the three named acceptance rows: a
// traversal target and an extension-changing target are both rejected
// without disturbing the original file, a same-extension flat rename
// succeeds, and a rename into a not-yet-existing subdirectory succeeds too
// (WriteFileAtomic's own parent-dir handling, mirrored here for Rename).
func TestRenameValidatesTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		originalRel string
		newRel      string
		wantErr     bool
	}{
		{name: "path traversal rejected", originalRel: "notes.md", newRel: "../escape.md", wantErr: true},
		{name: "extension change rejected", originalRel: "notes.txt", newRel: "notes.md", wantErr: true},
		{name: "same-extension rename succeeds", originalRel: "notes.md", newRel: "renamed.md", wantErr: false},
		{name: "rename into new subdir succeeds", originalRel: "notes.md", newRel: "sub/renamed.md", wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeFile(t, dir, tt.originalRel, "content")
			m := memoryfs.Memory{LocalDir: dir, RelPath: tt.originalRel}

			err := memoryfs.Rename(m, tt.newRel)

			originalPath := filepath.Join(dir, filepath.FromSlash(tt.originalRel))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Rename(%q) = nil, want error", tt.newRel)
				}
				if _, statErr := os.Stat(originalPath); statErr != nil {
					t.Fatalf("original file disturbed despite rejected rename: %v", statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Rename(%q) error = %v, want nil", tt.newRel, err)
			}
			if _, statErr := os.Stat(originalPath); !os.IsNotExist(statErr) {
				t.Fatalf("old path %s still exists after rename", originalPath)
			}
			newPath := filepath.Join(dir, filepath.FromSlash(tt.newRel))
			data, statErr := os.ReadFile(newPath)
			if statErr != nil {
				t.Fatalf("new path missing after rename: %v", statErr)
			}
			if string(data) != "content" {
				t.Fatalf("renamed content = %q, want %q", data, "content")
			}
		})
	}
}

// TestRenameRefusesClobber pins Rename's no-clobber contract: renaming onto
// an existing target is refused with ErrTargetExists, and both the source
// and the pre-existing target's content are left completely intact — silent
// data loss for a user's memory file is never acceptable.
func TestRenameRefusesClobber(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, dir, "source.md", "source content")
	writeFile(t, dir, "target.md", "target content")
	m := memoryfs.Memory{LocalDir: dir, RelPath: "source.md"}

	err := memoryfs.Rename(m, "target.md")
	if !errors.Is(err, memoryfs.ErrTargetExists) {
		t.Fatalf("Rename() error = %v, want ErrTargetExists", err)
	}

	gotSource, readErr := os.ReadFile(filepath.Join(dir, "source.md"))
	if readErr != nil {
		t.Fatalf("source file missing after refused rename: %v", readErr)
	}
	if string(gotSource) != "source content" {
		t.Fatalf("source content = %q, want unchanged %q", gotSource, "source content")
	}
	gotTarget, readErr := os.ReadFile(filepath.Join(dir, "target.md"))
	if readErr != nil {
		t.Fatalf("target file missing after refused rename: %v", readErr)
	}
	if string(gotTarget) != "target content" {
		t.Fatalf("target content = %q, want unchanged %q", gotTarget, "target content")
	}
}

// TestRenameMissingSourceErrors pins Rename's behavior when the source no
// longer exists: an ordinary error, distinct from ErrTargetExists, the same
// way os.Rename behaved before the link-then-remove rework.
func TestRenameMissingSourceErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m := memoryfs.Memory{LocalDir: dir, RelPath: "missing.md"}

	err := memoryfs.Rename(m, "renamed.md")
	if err == nil {
		t.Fatal("Rename() with a missing source = nil error, want error")
	}
	if errors.Is(err, memoryfs.ErrTargetExists) {
		t.Fatalf("Rename() with a missing source = ErrTargetExists, want a plain not-exist error")
	}
}

// TestLocalTargetRoundTrips proves List → (Folder, RepoPath) → LocalTarget
// recovers each Memory's own (LocalDir, RelPath) — the mapping restore
// depends on to know where a /v0/history path key writes back to.
func TestLocalTargetRoundTrips(t *testing.T) {
	t.Parallel()
	registry := testRegistry(t)
	claudeDir := t.TempDir()
	writeFile(t, claudeDir, "notes.md", "fact\n")
	codexDir := t.TempDir()
	writeFile(t, codexDir, "topic.md", "fact\n")

	units := []api.UnitInfo{
		{Provider: "claude", Folder: "acme", LocalDir: claudeDir},
		{Provider: "codex", Folder: "acme", LocalDir: codexDir, RepoSubdir: "memories"},
	}
	memories, err := memoryfs.List(registry, units)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(memories) != 2 {
		t.Fatalf("List() returned %d memories, want 2", len(memories))
	}
	for _, m := range memories {
		dir, rel, ok := memoryfs.LocalTarget(units, m.Folder, m.RepoPath)
		if !ok {
			t.Fatalf("LocalTarget(%q, %q) ok = false, want true", m.Folder, m.RepoPath)
		}
		if dir != m.LocalDir {
			t.Errorf("LocalTarget(%q, %q) dir = %q, want %q", m.Folder, m.RepoPath, dir, m.LocalDir)
		}
		if rel != m.RelPath {
			t.Errorf("LocalTarget(%q, %q) rel = %q, want %q", m.Folder, m.RepoPath, rel, m.RelPath)
		}
	}
}

// TestLocalTargetUnknownUnitFails pins ok=false for a repo path whose unit
// is no longer enrolled (e.g. untracked since the path was captured).
func TestLocalTargetUnknownUnitFails(t *testing.T) {
	t.Parallel()
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: t.TempDir()}}
	if _, _, ok := memoryfs.LocalTarget(units, "acme", "codex/notes.md"); ok {
		t.Fatal("LocalTarget() ok = true for an unenrolled provider, want false")
	}
	if _, _, ok := memoryfs.LocalTarget(units, "other-folder", "claude/notes.md"); ok {
		t.Fatal("LocalTarget() ok = true for a foreign folder, want false")
	}
}

// TestLocalTargetPrefersLongestSubdirMatch resolves an ambiguity a raw
// prefix match would get wrong: a provider enrolled both at its own root
// (RepoSubdir "") and under a named RepoSubdir that starts with the same
// provider name shares "codex/" as a candidate prefix for both units, so
// the longest (most specific) matching prefix must win, or a "memories/"
// path would incorrectly resolve to the bare-root unit.
func TestLocalTargetPrefersLongestSubdirMatch(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	subDir := t.TempDir()
	units := []api.UnitInfo{
		{Provider: "codex", Folder: "_global", LocalDir: rootDir},
		{Provider: "codex", Folder: "_global", LocalDir: subDir, RepoSubdir: "memories"},
	}
	dir, rel, ok := memoryfs.LocalTarget(units, "_global", "codex/memories/x.md")
	if !ok {
		t.Fatal("LocalTarget() ok = false, want true")
	}
	if dir != subDir {
		t.Errorf("LocalTarget() dir = %q, want the RepoSubdir unit's dir %q", dir, subDir)
	}
	if rel != "x.md" {
		t.Errorf("LocalTarget() rel = %q, want %q", rel, "x.md")
	}
}

// TestSkeletonClaudeFrontmatter pins the claude new-memory stub: the
// frontmatter block names/describes/types the memory, and a body stub
// follows the closing fence.
func TestSkeletonClaudeFrontmatter(t *testing.T) {
	t.Parallel()
	got := memoryfs.Skeleton("claude", "my-topic")
	for _, want := range []string{"name: my-topic", "description:", "metadata:", "type:"} {
		if !strings.Contains(got, want) {
			t.Errorf("Skeleton(claude, ...) missing %q; got:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "# my-topic") {
		t.Errorf("Skeleton(claude, ...) missing a body stub heading; got:\n%s", got)
	}
}

// TestSkeletonOtherProviderIsPlainHeading pins every non-claude provider's
// stub to exactly "# <name>\n\n" — no frontmatter block.
func TestSkeletonOtherProviderIsPlainHeading(t *testing.T) {
	t.Parallel()
	got := memoryfs.Skeleton("codex", "my-topic")
	want := "# my-topic\n\n"
	if got != want {
		t.Errorf("Skeleton(codex, ...) = %q, want %q", got, want)
	}
}
