package views

import (
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
)

// sentinelRender wraps its markdown input in unmistakable markers so a View
// assertion can prove content flowed through the glamour seam rather than
// being printed raw or dropped.
func sentinelRender(markdown string, _ int) string {
	return "RENDERED<<" + markdown + ">>"
}

// newMappedDetail builds a detail over a single real claude memory named
// notes.md (repo path acme/claude/notes.md), returning the screen and the
// memory it should resolve to. render is the glamour seam under test (pass nil
// for the raw body).
func newMappedDetail(t *testing.T, content string, render func(string, int) string) (*ConflictDetail, memoryfs.Memory) {
	t.Helper()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	writeBrowserFile(t, dir, "notes.md", content, time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC))
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}

	memories, err := memoryfs.List(registry, units)
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 1 {
		t.Fatalf("fixture listed %d memories, want exactly 1", len(memories))
	}
	detail := NewConflictDetail(ConflictDetailDeps{
		Record:   config.ConflictRecord{Time: "2026-07-09T11:00:00Z", Path: "acme/" + memories[0].RepoPath, Mode: "fact"},
		Units:    units,
		Registry: registry,
		ReadBody: memoryfs.ReadBody,
		Render:   render,
		Styles:   theme.Default(true),
	})
	return detail, memories[0]
}

// TestConflictDetailMappedRendersMetadataAndContent pins the mapped view: the
// event metadata (time, project, repo path, mode) over the memory's current
// content, the content flowing through the glamour seam.
func TestConflictDetailMappedRendersMetadataAndContent(t *testing.T) {
	t.Parallel()
	detail, _ := newMappedDetail(t, "# Heading\n\nbody text\n", sentinelRender)

	body := plain(detail.View(80, 40))
	for _, want := range []string{"2026-07-09T11:00:00Z", "acme", "claude/notes.md", "fact"} {
		if !strings.Contains(body, want) {
			t.Errorf("view missing metadata %q; got:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "RENDERED<<") || !strings.Contains(body, "body text") {
		t.Errorf("view did not render the memory body through the glamour seam; got:\n%s", body)
	}
}

// TestConflictDetailResolvesRepoSubdirUnit pins that a unit with a RepoSubdir
// (the codex memories+chronicle shape, spec §3) resolves: the recorded path
// carries the subdir, but the file lives at LocalDir/<rel>, and matching on
// (LocalDir, RelPath) — the pair LocalTarget returns — finds it regardless of
// the subdir that only shapes the repo key.
func TestConflictDetailResolvesRepoSubdirUnit(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	writeBrowserFile(t, dir, "log.md", "chronicle body\n", time.Now())
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir, RepoSubdir: "chronicle"}}

	detail := NewConflictDetail(ConflictDetailDeps{
		Record:   config.ConflictRecord{Time: "t", Path: "acme/claude/chronicle/log.md", Mode: "fact"},
		Units:    units,
		Registry: registry,
		ReadBody: memoryfs.ReadBody,
		Render:   nil,
		Styles:   theme.Default(true),
	})
	memory, ok := detail.Memory()
	if !ok {
		t.Fatalf("RepoSubdir unit did not resolve; view:\n%s", plain(detail.View(80, 40)))
	}
	if memory.RepoPath != "claude/chronicle/log.md" {
		t.Errorf("resolved RepoPath = %q, want %q", memory.RepoPath, "claude/chronicle/log.md")
	}
}

// TestConflictDetailUntrackedShowsNotice pins the unmapped case the brief
// names: a record whose folder no longer names an enrolled unit renders the
// metadata over the honest "no longer tracked" notice, resolves to no memory
// (so read/edit availability strikes), and shows no content.
func TestConflictDetailUntrackedShowsNotice(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: t.TempDir()}}
	detail := NewConflictDetail(ConflictDetailDeps{
		Record:   config.ConflictRecord{Time: "2026-07-09T11:00:00Z", Path: "ghost/claude/gone.md", Mode: "fact"},
		Units:    units,
		Registry: registry,
		ReadBody: memoryfs.ReadBody,
		Styles:   theme.Default(true),
	})

	if _, ok := detail.Memory(); ok {
		t.Fatal("an untracked record resolved to a memory, want ok=false")
	}
	body := plain(detail.View(80, 40))
	if !strings.Contains(body, conflictUntrackedNotice) {
		t.Errorf("view missing the untracked notice; got:\n%s", body)
	}
	// The metadata still renders — the event happened even if the file is gone.
	if !strings.Contains(body, "gone.md") {
		t.Errorf("view missing the recorded path; got:\n%s", body)
	}
}

// TestConflictDetailEnrolledButFileMissing pins the second unmapped fact,
// distinct from untracked: the project+provider is still enrolled but the
// specific file was deleted since the conflict, so the notice says exactly
// that rather than mislabeling it "no longer tracked".
func TestConflictDetailEnrolledButFileMissing(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	writeBrowserFile(t, dir, "present.md", "still here\n", time.Now())
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	detail := NewConflictDetail(ConflictDetailDeps{
		Record:   config.ConflictRecord{Time: "t", Path: "acme/claude/deleted.md", Mode: "fact"},
		Units:    units,
		Registry: registry,
		ReadBody: memoryfs.ReadBody,
		Styles:   theme.Default(true),
	})

	if _, ok := detail.Memory(); ok {
		t.Fatal("a deleted file resolved to a memory, want ok=false")
	}
	body := plain(detail.View(80, 40))
	if !strings.Contains(body, conflictMissingNotice) {
		t.Errorf("view missing the file-missing notice; got:\n%s", body)
	}
}

// TestConflictDetailEditEmitsRequestWhenMapped pins e → EditRequestMsg for the
// resolved memory, emit-only: the detail requests the handoff and stops, the
// root's startEditFlow owning every gate (cleaning up a merge IS an edit).
func TestConflictDetailEditEmitsRequestWhenMapped(t *testing.T) {
	t.Parallel()
	detail, want := newMappedDetail(t, "body\n", nil)

	_, cmd := detail.Update(key("e"))
	msgs := drain(cmd)
	if len(msgs) != 1 {
		t.Fatalf("e produced %d messages, want exactly 1 (EditRequestMsg)", len(msgs))
	}
	edit, ok := msgs[0].(EditRequestMsg)
	if !ok {
		t.Fatalf("e emitted %T, want EditRequestMsg", msgs[0])
	}
	if edit.Memory.RepoPath != want.RepoPath || edit.Memory.Path() != want.Path() {
		t.Errorf("EditRequestMsg.Memory = %q (%s), want %q (%s)",
			edit.Memory.RepoPath, edit.Memory.Path(), want.RepoPath, want.Path())
	}
}

// TestConflictDetailUnmappedEditAndReadAreInert pins that neither e nor enter
// does anything when the path did not resolve — there is nothing to edit or
// read — while esc still pops.
func TestConflictDetailUnmappedEditAndReadAreInert(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: t.TempDir()}}
	detail := NewConflictDetail(ConflictDetailDeps{
		Record:   config.ConflictRecord{Time: "t", Path: "ghost/claude/gone.md", Mode: "fact"},
		Units:    units,
		Registry: registry,
		ReadBody: memoryfs.ReadBody,
		Styles:   theme.Default(true),
	})

	if _, cmd := detail.Update(key("e")); cmd != nil {
		t.Errorf("e on an unmapped detail produced %#v, want nil", drain(cmd))
	}
	if _, cmd := detail.Update(key("enter")); cmd != nil {
		t.Errorf("enter on an unmapped detail produced %#v, want nil", drain(cmd))
	}
	_, cmd := detail.Update(key("esc"))
	if msgs := drain(cmd); len(msgs) != 1 {
		t.Fatalf("esc produced %d messages, want exactly 1 (PopScreenMsg)", len(msgs))
	} else if _, ok := msgs[0].(PopScreenMsg); !ok {
		t.Fatalf("esc emitted %T, want PopScreenMsg", msgs[0])
	}
}

// TestConflictDetailEnterPushesReading pins enter → a Reading screen over the
// same resolved memory (spec §10's jump to the full reading view), delivered
// as a PushScreenMsg the root stacks.
func TestConflictDetailEnterPushesReading(t *testing.T) {
	t.Parallel()
	detail, want := newMappedDetail(t, "# Title\n\n[[other]] link\n", sentinelRender)

	_, cmd := detail.Update(key("enter"))
	msgs := drain(cmd)
	if len(msgs) != 1 {
		t.Fatalf("enter produced %d messages, want exactly 1 (PushScreenMsg)", len(msgs))
	}
	push, ok := msgs[0].(PushScreenMsg)
	if !ok {
		t.Fatalf("enter emitted %T, want PushScreenMsg", msgs[0])
	}
	reading, ok := push.Screen.(*Reading)
	if !ok {
		t.Fatalf("pushed screen = %T, want *Reading", push.Screen)
	}
	if reading.Memory().RepoPath != want.RepoPath {
		t.Errorf("pushed reading over %q, want the resolved %q", reading.Memory().RepoPath, want.RepoPath)
	}
}

// TestConflictDetailEscPopsWhenMapped pins that esc pops the mapped detail too
// (it has no internal open state to consume first).
func TestConflictDetailEscPopsWhenMapped(t *testing.T) {
	t.Parallel()
	detail, _ := newMappedDetail(t, "body\n", nil)
	_, cmd := detail.Update(key("esc"))
	msgs := drain(cmd)
	if len(msgs) != 1 {
		t.Fatalf("esc produced %d messages, want exactly 1 (PopScreenMsg)", len(msgs))
	}
	if _, ok := msgs[0].(PopScreenMsg); !ok {
		t.Fatalf("esc emitted %T, want PopScreenMsg", msgs[0])
	}
}

// TestConflictDetailStaysLiveAgainstEdits pins that a RefreshMsg re-reads the
// file, so cleaning up the merge through e (which writes this exact file) is
// reflected when the tick forwards the refresh, rather than freezing the
// content at construction.
func TestConflictDetailStaysLiveAgainstEdits(t *testing.T) {
	t.Parallel()
	detail, memory := newMappedDetail(t, "before edit\n", sentinelRender)
	if body := plain(detail.View(80, 40)); !strings.Contains(body, "before edit") {
		t.Fatalf("initial view missing the original body; got:\n%s", body)
	}

	writeBrowserFile(t, memory.LocalDir, memory.RelPath, "after cleanup\n", time.Now())
	detail.Update(RefreshMsg{Now: time.Now()})

	body := plain(detail.View(80, 40))
	if !strings.Contains(body, "after cleanup") {
		t.Errorf("view did not adopt the edited body after a refresh; got:\n%s", body)
	}
	if strings.Contains(body, "before edit") {
		t.Errorf("view still shows the pre-edit body after a refresh; got:\n%s", body)
	}
}

// TestConflictDetailViewFillsHeightExactly pins the honest-height contract in
// both directions for mapped and unmapped screens: at and above the chrome
// floor (metadata block + one blank = 5, so floor+1 = 6) View renders exactly
// height lines, and below it the frame clamps to the irreducible chrome+1.
func TestConflictDetailViewFillsHeightExactly(t *testing.T) {
	t.Parallel()
	mapped, _ := newMappedDetail(t, "# H\n\nsome body content\n", nil)

	registry := browserFixtureRegistry(t)
	unmapped := NewConflictDetail(ConflictDetailDeps{
		Record:   config.ConflictRecord{Time: "t", Path: "ghost/claude/gone.md", Mode: "fact"},
		Units:    []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: t.TempDir()}},
		Registry: registry,
		ReadBody: memoryfs.ReadBody,
		Styles:   theme.Default(true),
	})

	for _, detail := range []*ConflictDetail{mapped, unmapped} {
		for _, height := range []int{6, 8, 20} {
			got := detail.View(80, height)
			if lines := strings.Count(got, "\n") + 1; lines != height {
				t.Errorf("View(80, %d) rendered %d lines, want exactly %d; got:\n%s",
					height, lines, height, plain(got))
			}
		}
		// Below the floor the frame clamps to chrome + one viewport row = 6.
		for _, height := range []int{1, 5} {
			got := detail.View(80, height)
			if lines := strings.Count(got, "\n") + 1; lines != 6 {
				t.Errorf("View(80, %d) rendered %d lines, want the clamped floor 6; got:\n%s",
					height, lines, plain(got))
			}
		}
	}
}

// TestConflictDetailTitle pins the breadcrumb segment: the memory's name when
// mapped, the record's own filename when not.
func TestConflictDetailTitle(t *testing.T) {
	t.Parallel()
	mapped, _ := newMappedDetail(t, "---\nname: My Notes\n---\n", nil)
	if got := mapped.Title(); got != "My Notes" {
		t.Errorf("mapped Title() = %q, want the memory name %q", got, "My Notes")
	}

	registry := browserFixtureRegistry(t)
	unmapped := NewConflictDetail(ConflictDetailDeps{
		Record:   config.ConflictRecord{Time: "t", Path: "ghost/claude/gone.md", Mode: "fact"},
		Units:    []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: t.TempDir()}},
		Registry: registry,
		ReadBody: memoryfs.ReadBody,
		Styles:   theme.Default(true),
	})
	if got := unmapped.Title(); got != "gone.md" {
		t.Errorf("unmapped Title() = %q, want the record filename %q", got, "gone.md")
	}
}

// TestConflictDetailResolvesCollisionByLocalTargetUnit pins that when two
// same-provider units in one folder collide on RepoPath — a bare-root ("" repo
// subdir) unit carrying chronicle/log.md beside a chronicle-subdir unit
// carrying log.md, both listing as codex/chronicle/log.md — resolve picks the
// unit LocalTarget designates (the longest-prefix match, what a restore would
// write), not merely the first memory that happens to share that RepoPath.
// Matching on the (LocalDir, RelPath) pair is exactly what discriminates them;
// a RepoPath-only match would seize the wrong unit's file. No current adapter
// emits a bare-root unit beside a subdir one, but memoryfs.LocalTarget's doc
// names and defends this shape, so the detail's use of the pair is pinned here
// against a future provider that could.
func TestConflictDetailResolvesCollisionByLocalTargetUnit(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)

	// The bare-root unit's file lives at chronicle/log.md, so it lists as
	// RepoPath codex/chronicle/log.md — byte-identical to the subdir unit's key.
	bareDir := t.TempDir()
	writeBrowserFile(t, bareDir, "chronicle/log.md", "bare-root unit body — the WRONG target\n", time.Now())
	// The subdir unit's file is log.md under RepoSubdir chronicle → same key.
	subdirDir := t.TempDir()
	writeBrowserFile(t, subdirDir, "log.md", "chronicle-subdir unit body — the correct target\n", time.Now())

	// Bare-root unit first: on an equal (folder, RepoPath) sort key the listing
	// preserves input order, so a RepoPath-only match would seize this wrong
	// file. LocalTarget instead resolves the longest prefix (the chronicle unit).
	units := []api.UnitInfo{
		{Provider: "codex", Folder: "acme", LocalDir: bareDir},
		{Provider: "codex", Folder: "acme", LocalDir: subdirDir, RepoSubdir: "chronicle"},
	}
	detail := NewConflictDetail(ConflictDetailDeps{
		Record:   config.ConflictRecord{Time: "t", Path: "acme/codex/chronicle/log.md", Mode: "fact"},
		Units:    units,
		Registry: registry,
		ReadBody: memoryfs.ReadBody,
		Render:   nil,
		Styles:   theme.Default(true),
	})

	memory, ok := detail.Memory()
	if !ok {
		t.Fatalf("collision record did not resolve; view:\n%s", plain(detail.View(80, 40)))
	}
	if memory.LocalDir != subdirDir || memory.RelPath != "log.md" {
		t.Errorf("resolved (LocalDir, RelPath) = (%q, %q), want the LocalTarget unit (%q, %q)",
			memory.LocalDir, memory.RelPath, subdirDir, "log.md")
	}
	body := plain(detail.View(80, 40))
	if !strings.Contains(body, "correct target") {
		t.Errorf("view did not render the LocalTarget unit's body; got:\n%s", body)
	}
	if strings.Contains(body, "WRONG target") {
		t.Errorf("view rendered the bare-root unit's body — resolved the wrong colliding unit; got:\n%s", body)
	}
}

// TestConflictDetailListErrorShowsNotice pins resolve's third content state:
// when listing the record's folder errors — here a unit naming a provider the
// registry does not know, which LocalTarget still prefix-matches (it never
// consults the registry) but memoryfs.List rejects — the detail renders the
// exact "cannot read this project's memories" notice over the still-visible
// metadata, resolves to no memory (so read/edit strike), and offers no action.
// Honest degradation, distinct from both the untracked and file-missing notices.
func TestConflictDetailListErrorShowsNotice(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	units := []api.UnitInfo{{Provider: "ghost", Folder: "acme", LocalDir: t.TempDir()}}
	detail := NewConflictDetail(ConflictDetailDeps{
		Record:   config.ConflictRecord{Time: "2026-07-09T11:00:00Z", Path: "acme/ghost/notes.md", Mode: "fact"},
		Units:    units,
		Registry: registry,
		ReadBody: memoryfs.ReadBody,
		Styles:   theme.Default(true),
	})

	if _, ok := detail.Memory(); ok {
		t.Fatal("a list-error record resolved to a memory, want ok=false")
	}
	body := plain(detail.View(80, 40))
	if !strings.Contains(body, "cannot read this project's memories") {
		t.Errorf("view missing the list-error notice; got:\n%s", body)
	}
	// The metadata still renders — the event happened even if the listing failed.
	for _, want := range []string{"acme", "ghost/notes.md"} {
		if !strings.Contains(body, want) {
			t.Errorf("view missing metadata %q; got:\n%s", want, body)
		}
	}
	// Nothing to read or edit: e and enter are inert, esc still pops.
	if _, cmd := detail.Update(key("e")); cmd != nil {
		t.Errorf("e on a list-error detail produced %#v, want nil", drain(cmd))
	}
	if _, cmd := detail.Update(key("enter")); cmd != nil {
		t.Errorf("enter on a list-error detail produced %#v, want nil", drain(cmd))
	}
	_, cmd := detail.Update(key("esc"))
	if msgs := drain(cmd); len(msgs) != 1 {
		t.Fatalf("esc produced %d messages, want exactly 1 (PopScreenMsg)", len(msgs))
	} else if _, ok := msgs[0].(PopScreenMsg); !ok {
		t.Fatalf("esc emitted %T, want PopScreenMsg", msgs[0])
	}
}
