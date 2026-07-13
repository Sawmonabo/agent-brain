package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/keys"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// TestHistoryPathModeListsNewestFirstWithLiveFlag pins path-mode History
// against three real capture commits: newest-first ordering, Host/Stamp
// parsed off the engine's own capture-subject convention, Paths empty (a
// path-mode contract), and Live tracking content identity rather than
// "is this the newest rev" — after a fourth capture byte-restores an
// earlier version's exact content, both the new head and that earlier
// version read Live.
func TestHistoryPathModeListsNewestFirstWithLiveFlag(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	ctx := context.Background()

	writeCheckout(t, checkout, "projA/claude/notes.md", "first content\n")
	if _, err := engine.commitProjects(ctx, fixedStamp); err != nil {
		t.Fatal(err)
	}
	writeCheckout(t, checkout, "projA/claude/notes.md", "second content\n")
	if _, err := engine.commitProjects(ctx, fixedStamp); err != nil {
		t.Fatal(err)
	}
	writeCheckout(t, checkout, "projA/claude/notes.md", "third content\n")
	if _, err := engine.commitProjects(ctx, fixedStamp); err != nil {
		t.Fatal(err)
	}

	versions, err := engine.History(ctx, "projA", "claude/notes.md", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 3 {
		t.Fatalf("len(versions) = %d, want 3", len(versions))
	}
	wantSubject := "memory: host-a projA " + fixedStamp
	for i, version := range versions {
		if version.Subject != wantSubject {
			t.Errorf("versions[%d].Subject = %q, want %q", i, version.Subject, wantSubject)
		}
		if version.Host != "host-a" {
			t.Errorf("versions[%d].Host = %q, want host-a", i, version.Host)
		}
		if diff := cmp.Diff(fixedNow(), version.Stamp); diff != "" {
			t.Errorf("versions[%d].Stamp mismatch (-want +got):\n%s", i, diff)
		}
		if version.Paths != nil {
			t.Errorf("versions[%d].Paths = %v, want nil in path mode", i, version.Paths)
		}
	}
	if !versions[0].Live {
		t.Error("newest version is not Live")
	}
	for i := 1; i < len(versions); i++ {
		if versions[i].Live {
			t.Errorf("versions[%d] (not newest) is unexpectedly Live before any restore", i)
		}
	}

	// A fourth capture commit byte-restores the second version's exact
	// content: content identity means BOTH the new head and the original
	// second-version entry read Live, not just the new head.
	writeCheckout(t, checkout, "projA/claude/notes.md", "second content\n")
	if _, err := engine.commitProjects(ctx, fixedStamp); err != nil {
		t.Fatal(err)
	}
	afterRestore, err := engine.History(ctx, "projA", "claude/notes.md", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterRestore) != 4 {
		t.Fatalf("len(afterRestore) = %d, want 4", len(afterRestore))
	}
	// Newest-first: 0 = restore head, 1 = third content, 2 = second content
	// (original), 3 = first content.
	wantLive := []bool{true, false, true, false}
	for i, version := range afterRestore {
		if version.Live != wantLive[i] {
			t.Errorf("afterRestore[%d].Live = %v, want %v", i, version.Live, wantLive[i])
		}
	}
}

// TestHistoryFolderWideCarriesChangedPaths pins folder-wide History:
// changed Paths are folder-relative and populated, and Live is always
// false (it is a path-mode-only concept — folder-wide has no single path
// whose liveness would even mean anything).
func TestHistoryFolderWideCarriesChangedPaths(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	ctx := context.Background()

	writeCheckout(t, checkout, "projA/claude/notes.md", "notes\n")
	if _, err := engine.commitProjects(ctx, fixedStamp); err != nil {
		t.Fatal(err)
	}
	writeCheckout(t, checkout, "projA/claude/other.md", "other\n")
	if _, err := engine.commitProjects(ctx, fixedStamp); err != nil {
		t.Fatal(err)
	}

	versions, err := engine.History(ctx, "projA", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 {
		t.Fatalf("len(versions) = %d, want 2", len(versions))
	}
	// Newest first: index 0 touched other.md, index 1 touched notes.md.
	if diff := cmp.Diff([]string{"claude/other.md"}, versions[0].Paths); diff != "" {
		t.Errorf("versions[0].Paths mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"claude/notes.md"}, versions[1].Paths); diff != "" {
		t.Errorf("versions[1].Paths mismatch (-want +got):\n%s", diff)
	}
	for i, version := range versions {
		if version.Live {
			t.Errorf("versions[%d].Live = true, want false in folder-wide mode", i)
		}
	}
}

// TestHistoryHonorsLimitClamp pins the [1, historyMaxLimit] clamp: an
// explicit limit under the total is honored exactly, zero falls back to
// the default (which comfortably covers this test's small commit count),
// and an oversized limit is a ceiling, never a rejection.
func TestHistoryHonorsLimitClamp(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	ctx := context.Background()

	for i := range 5 {
		writeCheckout(t, checkout, "projA/claude/notes.md", fmt.Sprintf("content %d\n", i))
		if _, err := engine.commitProjects(ctx, fixedStamp); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name  string
		limit int
		want  int
	}{
		{"explicit limit below the total", 2, 2},
		{"zero falls back to the default", 0, 5},
		{"a limit above the total is a ceiling, not a demand", 9999, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			versions, err := engine.History(ctx, "projA", "claude/notes.md", tt.limit)
			if err != nil {
				t.Fatal(err)
			}
			if len(versions) != tt.want {
				t.Fatalf("len(versions) = %d, want %d", len(versions), tt.want)
			}
		})
	}
}

// TestClampHistoryLimitBounds pins the clamp helper directly: the 500
// ceiling cannot be distinguished from an unclamped pass-through by the
// end-to-end tests (nobody builds 501 commits), so the pure function
// carries its own regression pin.
func TestClampHistoryLimitBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		limit int
		want  int
	}{
		{"negative falls back to the default", -1, historyDefaultLimit},
		{"zero falls back to the default", 0, historyDefaultLimit},
		{"in-range passes through", 3, 3},
		{"the ceiling itself passes through", historyMaxLimit, historyMaxLimit},
		{"past the ceiling clamps to it", historyMaxLimit + 9499, historyMaxLimit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := clampHistoryLimit(tt.limit); got != tt.want {
				t.Fatalf("clampHistoryLimit(%d) = %d, want %d", tt.limit, got, tt.want)
			}
		})
	}
}

// TestHistoryAndBlobAcceptGlobalFolder positively pins the GlobalFolder
// carve-out in validateHistoryInputs: repo.ValidateFolderName rejects
// "_global" by design (reserved for the on-disk global-scope directory),
// so without the carve-out both lookups would refuse the one folder
// global-scope providers actually live in.
func TestHistoryAndBlobAcceptGlobalFolder(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	ctx := context.Background()

	const content = "global memory\n"
	writeCheckout(t, checkout, repo.GlobalFolder+"/codex/memories/global.md", content)
	if _, err := engine.commitProjects(ctx, fixedStamp); err != nil {
		t.Fatal(err)
	}

	versions, err := engine.History(ctx, repo.GlobalFolder, "codex/memories/global.md", 0)
	if err != nil {
		t.Fatalf("History(%q) = %v, want success", repo.GlobalFolder, err)
	}
	if len(versions) != 1 {
		t.Fatalf("len(versions) = %d, want 1", len(versions))
	}
	if versions[0].Host != "host-a" {
		t.Errorf("versions[0].Host = %q, want host-a", versions[0].Host)
	}

	blob, err := engine.BlobAt(ctx, repo.GlobalFolder, "codex/memories/global.md", versions[0].Rev)
	if err != nil {
		t.Fatalf("BlobAt(%q) = %v, want success", repo.GlobalFolder, err)
	}
	if string(blob) != content {
		t.Errorf("BlobAt content = %q, want %q", blob, content)
	}
}

// TestHistoryForeignSubjectsVerbatim pins that a subject outside the
// engine's own capture-subject convention renders honestly: Subject
// verbatim, Host empty, Stamp zero — never an error and never a mangled
// partial parse.
func TestHistoryForeignSubjectsVerbatim(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	ctx := context.Background()

	writeCheckout(t, checkout, "projA/claude/notes.md", "notes\n")
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "--quiet", "-m", "merge stuff")

	versions, err := engine.History(ctx, "projA", "claude/notes.md", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 {
		t.Fatalf("len(versions) = %d, want 1", len(versions))
	}
	version := versions[0]
	if version.Subject != "merge stuff" {
		t.Errorf("Subject = %q, want verbatim %q", version.Subject, "merge stuff")
	}
	if version.Host != "" {
		t.Errorf("Host = %q, want empty for a foreign subject", version.Host)
	}
	if !version.Stamp.IsZero() {
		t.Errorf("Stamp = %v, want zero for a foreign subject", version.Stamp)
	}
}

// TestHistoryEmptyForPathWithNoCommits pins the empty-history edge case: a
// folder/path that has never been touched is an ordinary empty result, not
// an error — `git log -- <pathspec>` exits 0 with no output for a pathspec
// that never matched, and markLive's HEAD resolve failing (path does not
// exist) also just marks nothing live rather than erroring the whole call.
func TestHistoryEmptyForPathWithNoCommits(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	ctx := context.Background()

	versions, err := engine.History(ctx, "projA", "claude/notes.md", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 0 {
		t.Fatalf("len(versions) = %d, want 0 for a path with no history", len(versions))
	}
}

// TestHistoryRejectsBadInputs pins validate-before-git-runs for folder and
// path: every case here would either be misread as a git option or escape
// the intended pathspec, so each must fail with an engine-namespaced
// "history:" error rather than ever reaching a git subprocess.
func TestHistoryRejectsBadInputs(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	ctx := context.Background()

	tests := []struct {
		name   string
		folder string
		path   string
	}{
		{"folder contains a path separator", "no/slash", ""},
		{"path escapes the checkout with a traversal segment", "projA", "../escape"},
		{"folder shaped like a git flag", "-rf", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := engine.History(ctx, tt.folder, tt.path, 0)
			if err == nil {
				t.Fatalf("History(%q, %q) = nil error, want a validation failure", tt.folder, tt.path)
			}
			if !strings.HasPrefix(err.Error(), "history:") {
				t.Fatalf("History(%q, %q) error = %q, want a %q-prefixed error", tt.folder, tt.path, err.Error(), "history:")
			}
		})
	}
}

// TestBlobAtReturnsPlaintextAtRev proves BlobAt rides the checkout's own
// decrypt wiring rather than a bespoke path of its own: against a checkout
// with REAL filters (the real binary, never the test binary — see
// testmain_test.go's engineBinaryPath), BlobAt at the first capture's rev
// returns that version's original plaintext while HEAD (and the worktree)
// hold the third; a manual `git cat-file --textconv` of the same rev must
// return byte-identical content, and the remote's stored blob must be
// genuinely encrypted (never a false-positive from an unwired fixture).
//
// Non-parallel: t.Setenv gives the real-binary filters a private keyset
// directory, and t.Setenv forbids t.Parallel.
func TestBlobAtReturnsPlaintextAtRev(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", configDir)
	keysetPath := filepath.Join(configDir, "keyset.json")
	if err := keys.Generate(keysetPath); err != nil {
		t.Fatal(err)
	}

	checkout, bare := newEncryptedCheckout(t)
	engine := newTestEngine(t, checkout)
	ctx := context.Background()

	const firstContent, secondContent, thirdContent = "first fact\n", "second fact\n", "third fact\n"
	writeCheckout(t, checkout, "projA/claude/notes.md", firstContent)
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "--quiet", "-m", "memory: host-a projA "+fixedStamp)
	firstRev := strings.TrimSpace(mustGit(t, checkout, "rev-parse", "HEAD").Stdout)

	writeCheckout(t, checkout, "projA/claude/notes.md", secondContent)
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "--quiet", "-m", "memory: host-a projA "+fixedStamp)

	writeCheckout(t, checkout, "projA/claude/notes.md", thirdContent)
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "--quiet", "-m", "memory: host-a projA "+fixedStamp)
	mustGit(t, checkout, "push", "--quiet", "origin", "main")

	stored := remoteBlobBytes(t, bare, "projA/claude/notes.md")
	if !strings.HasPrefix(stored, magicPrefix) {
		t.Fatal("precondition: fixture is not genuinely encrypted (filters not wired?)")
	}

	got, err := engine.BlobAt(ctx, "projA", "claude/notes.md", firstRev)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != firstContent {
		t.Fatalf("BlobAt(firstRev) = %q, want %q", got, firstContent)
	}
	if worktree := readCheckout(t, checkout, "projA/claude/notes.md"); worktree != thirdContent {
		t.Fatalf("worktree = %q, want unchanged %q — BlobAt must read history, never the live file", worktree, thirdContent)
	}

	manual := mustGit(t, checkout, "cat-file", "--textconv", firstRev+":projA/claude/notes.md")
	if manual.Stdout != string(got) {
		t.Fatalf("BlobAt = %q, diverges from manual git cat-file --textconv = %q", got, manual.Stdout)
	}
}

// TestBlobAtRefusesOversizeAndBinary pins the guard order past validation:
// an oversize STORED blob is refused by the cheap size probe before any
// content is fetched, and content that is not valid UTF-8 (a lone
// continuation byte, or an embedded NUL) is refused after the fetch. A
// plain (unfiltered) checkout keeps the stored size exactly equal to the
// authored content's byte length, isolating this test from encryption
// overhead entirely.
func TestBlobAtRefusesOversizeAndBinary(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	ctx := context.Background()

	oversizeContent := strings.Repeat("A", 300*1024)
	writeCheckout(t, checkout, "projA/claude/big.md", oversizeContent)
	nulByteContent := "before\x00after\n"
	writeCheckout(t, checkout, "projA/claude/binary.md", nulByteContent)
	invalidUTF8Content := "before\x80after\n" // a lone continuation byte, no NUL
	writeCheckout(t, checkout, "projA/claude/invalid-utf8.md", invalidUTF8Content)
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "--quiet", "-m", "memory: host-a projA "+fixedStamp)
	rev := strings.TrimSpace(mustGit(t, checkout, "rev-parse", "HEAD").Stdout)

	tests := []struct {
		name    string
		path    string
		wantErr error
	}{
		{"oversize blob", "claude/big.md", ErrBlobTooLarge},
		{"embedded NUL byte", "claude/binary.md", ErrBlobBinary},
		{"invalid UTF-8 without a NUL byte", "claude/invalid-utf8.md", ErrBlobBinary},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := engine.BlobAt(ctx, "projA", tt.path, rev)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("BlobAt(%q) error = %v, want errors.Is(_, %v)", tt.path, err, tt.wantErr)
			}
		})
	}
}

// TestBlobAtRejectsBadRev pins validate-before-git-runs for rev: neither a
// symbolic ref nor a shell-metacharacter-bearing string matches
// revPattern, so both must fail with an engine-namespaced "history:" error
// rather than ever reaching a git subprocess.
func TestBlobAtRejectsBadRev(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	ctx := context.Background()

	writeCheckout(t, checkout, "projA/claude/notes.md", "notes\n")
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "--quiet", "-m", "memory: host-a projA "+fixedStamp)

	for _, rev := range []string{"HEAD", "abc$(rm)"} {
		t.Run(rev, func(t *testing.T) {
			t.Parallel()
			_, err := engine.BlobAt(ctx, "projA", "claude/notes.md", rev)
			if err == nil {
				t.Fatalf("BlobAt(rev=%q) = nil error, want a validation failure", rev)
			}
			if !strings.HasPrefix(err.Error(), "history:") {
				t.Fatalf("BlobAt(rev=%q) error = %q, want a %q-prefixed error", rev, err.Error(), "history:")
			}
		})
	}
}

// TestParseHistoryRecordsHandlesZeroPathsAndHostileSubjects unit-tests the
// raw `--format=%x01%H%x00%s --name-only -z` parser directly (byte layout
// confirmed against real git output), independent of git: a merge-shaped
// zero-path record, single- and multi-path records (proving the leading
// blank-line newline is stripped from exactly the first path and no
// other), and a subject that embeds the record separator itself, which
// must garble only its own entry and never panic regardless of how few
// NUL-delimited fields the resulting fragment has.
func TestParseHistoryRecordsHandlesZeroPathsAndHostileSubjects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want []HistoryVersion
	}{
		{
			name: "zero-path record (merge simplification shape)",
			raw:  "\x01aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\x00integrate: merge side into main\x00",
			want: []HistoryVersion{
				{Rev: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Subject: "integrate: merge side into main"},
			},
		},
		{
			name: "single changed path strips the leading blank-line newline",
			raw:  "\x01bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\x00memory: host-a projA 2026-07-08T12:00:00Z\x00\nprojA/claude/notes.md\x00",
			want: []HistoryVersion{
				{
					Rev:     "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
					Subject: "memory: host-a projA 2026-07-08T12:00:00Z",
					Host:    "host-a",
					Stamp:   time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
					Paths:   []string{"claude/notes.md"},
				},
			},
		},
		{
			name: "multiple changed paths",
			raw:  "\x01cccccccccccccccccccccccccccccccccccccccc\x00memory: host-a projA 2026-07-08T12:00:00Z\x00\nprojA/claude/notes.md\x00projA/claude/other.md\x00",
			want: []HistoryVersion{
				{
					Rev:     "cccccccccccccccccccccccccccccccccccccccc",
					Subject: "memory: host-a projA 2026-07-08T12:00:00Z",
					Host:    "host-a",
					Stamp:   time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
					Paths:   []string{"claude/notes.md", "claude/other.md"},
				},
			},
		},
		{
			name: "a subject embedding the record separator garbles only its own entry",
			raw:  "\x01dddddddddddddddddddddddddddddddddddddddd\x00evil\x01subject\x00",
			want: []HistoryVersion{
				{Rev: "dddddddddddddddddddddddddddddddddddddddd", Subject: "evil"},
				{Rev: "subject"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseHistoryRecords(tt.raw, "projA")
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("parseHistoryRecords mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
