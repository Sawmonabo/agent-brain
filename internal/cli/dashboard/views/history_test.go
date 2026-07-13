package views

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
)

// fakeHistoryData is the injectable HistoryDataSource for the History screen's
// suite: canned History/Blob answers plus recorded calls, so a test can prove
// what a keypress fetched without a socket. Blobs are keyed by rev; a rev with
// no entry answers empty content (never an error unless blobErr is set).
type fakeHistoryData struct {
	historyResp api.HistoryResponse
	historyErr  error
	blobs       map[string]string
	blobErr     error

	historyCalls []historyCall
	blobCalls    []blobCall
}

type historyCall struct {
	folder string
	path   string
	limit  int
}

type blobCall struct {
	folder string
	path   string
	rev    string
}

func (f *fakeHistoryData) History(_ context.Context, folder, path string, limit int) (api.HistoryResponse, error) {
	f.historyCalls = append(f.historyCalls, historyCall{folder: folder, path: path, limit: limit})
	return f.historyResp, f.historyErr
}

func (f *fakeHistoryData) Blob(_ context.Context, folder, path, rev string) (api.BlobResponse, error) {
	f.blobCalls = append(f.blobCalls, blobCall{folder: folder, path: path, rev: rev})
	if f.blobErr != nil {
		return api.BlobResponse{}, f.blobErr
	}
	return api.BlobResponse{Content: f.blobs[rev]}, nil
}

// historyNow is the fixed clock every History fixture renders relative time
// against — the injected seam the screen stores, never time.Now.
var historyNow = time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

// captureVersions is the canonical three-version fixture: all capture commits
// (Timestamp + Host set), newest first, with the newest flagged Live.
func captureVersions() []api.HistoryVersion {
	t1 := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	return []api.HistoryVersion{
		{Rev: "aaaaaaaaaaaa1111", Subject: "memory: host-a acme 2026-07-13T10:00:00Z", Host: "host-a", Timestamp: &t1, Live: true},
		{Rev: "bbbbbbbbbbbb2222", Subject: "memory: host-b acme 2026-07-13T08:00:00Z", Host: "host-b", Timestamp: &t2},
		{Rev: "cccccccccccc3333", Subject: "memory: host-a acme 2026-07-12T09:00:00Z", Host: "host-a", Timestamp: &t3},
	}
}

// newHistoryFixture builds a History over claude/notes.md wired to fake, with
// overrides applied before construction, then delivers the initial version
// fetch synchronously so the screen starts loaded (the tests that pin the
// loading state build the screen directly instead).
func newHistoryFixture(t *testing.T, fake *fakeHistoryData, override func(*HistoryDeps)) *History {
	t.Helper()
	deps := HistoryDeps{
		Memory:   memoryfs.Memory{Folder: "acme", RepoPath: "claude/notes.md", Name: "notes"},
		Folder:   "acme",
		RepoPath: "claude/notes.md",
		Live:     func() (string, error) { return "keep\nLIVE-NOW\n", nil },
		Data:     fake,
		Now:      historyNow,
	}
	if override != nil {
		override(&deps)
	}
	history := NewHistory(deps)
	return feedHistory(t, history, history.InitCmd())
}

// feedHistory drives every leaf message a Cmd produces (flattening tea.Batch)
// back through the screen's Update, the standard filesystem-free way to run a
// screen's async fetches without a program loop.
func feedHistory(t *testing.T, history *History, cmd tea.Cmd) *History {
	t.Helper()
	for _, msg := range leafMessages(cmd) {
		next, _ := history.Update(msg)
		history = next.(*History)
	}
	return history
}

func leafMessages(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, child := range batch {
			out = append(out, leafMessages(child)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

// pressHistory sends one key and drives every Cmd it produced back in, so a
// keypress that triggers a blob fetch renders its result on the next View.
func pressHistory(t *testing.T, history *History, name string) *History {
	t.Helper()
	next, cmd := history.Update(key(name))
	return feedHistory(t, next.(*History), cmd)
}

// TestHistoryListRendersVersions pins the list shape: each capture row carries
// the short rev (12), the absolute stamp, a relative age, the host, and the
// newest row additionally the live tag.
func TestHistoryListRendersVersions(t *testing.T) {
	t.Parallel()
	fake := &fakeHistoryData{historyResp: api.HistoryResponse{Versions: captureVersions()}}
	history := newHistoryFixture(t, fake, nil)

	if len(fake.historyCalls) != 1 {
		t.Fatalf("history fetched %d times, want exactly 1", len(fake.historyCalls))
	}
	if got := fake.historyCalls[0]; got.folder != "acme" || got.path != "claude/notes.md" {
		t.Errorf("history fetch = %+v, want folder acme path claude/notes.md", got)
	}

	got := plain(history.View(120, 30))
	for _, want := range []string{
		"aaaaaaaaaaaa",     // short rev (12), never the full 16
		"2026-07-13 10:00", // absolute stamp
		"2h ago",           // relative age against historyNow
		"host-a",           // host
		"live",             // the newest row's live tag
		"bbbbbbbbbbbb",     // the older revs render too
		"cccccccccccc",     // oldest
	} {
		if !strings.Contains(got, want) {
			t.Errorf("list view missing %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "aaaaaaaaaaaa1111") {
		t.Errorf("row rendered the FULL rev, not the 12-char short form; got:\n%s", got)
	}
}

// TestHistoryForeignCommitFallsBackToSubject pins the honest-render rule for a
// foreign (non-capture) commit: nil Timestamp and empty Host must never be
// dereferenced — the row falls back to the raw Subject instead.
func TestHistoryForeignCommitFallsBackToSubject(t *testing.T) {
	t.Parallel()
	versions := []api.HistoryVersion{
		{Rev: "ffffffffffff0000", Subject: "Merge branch 'main' into feature"},
	}
	fake := &fakeHistoryData{historyResp: api.HistoryResponse{Versions: versions}}
	history := newHistoryFixture(t, fake, nil)

	got := plain(history.View(120, 30))
	if !strings.Contains(got, "ffffffffffff") {
		t.Errorf("foreign row missing its short rev; got:\n%s", got)
	}
	if !strings.Contains(got, "Merge branch 'main' into feature") {
		t.Errorf("foreign row did not fall back to the Subject; got:\n%s", got)
	}
}

// TestHistoryTitleIsMemoryName pins the breadcrumb segment.
func TestHistoryTitleIsMemoryName(t *testing.T) {
	t.Parallel()
	fake := &fakeHistoryData{historyResp: api.HistoryResponse{Versions: captureVersions()}}
	history := newHistoryFixture(t, fake, nil)
	if got := history.Title(); !strings.Contains(got, "notes") {
		t.Errorf("Title() = %q, want it to name the memory (notes)", got)
	}
}

// TestHistoryLoadingState pins the pre-fetch state: a freshly constructed
// screen (its InitCmd result not yet delivered) renders a loading notice, not
// a blank or a panic.
func TestHistoryLoadingState(t *testing.T) {
	t.Parallel()
	fake := &fakeHistoryData{historyResp: api.HistoryResponse{Versions: captureVersions()}}
	history := NewHistory(HistoryDeps{
		Memory:   memoryfs.Memory{Folder: "acme", RepoPath: "claude/notes.md", Name: "notes"},
		Folder:   "acme",
		RepoPath: "claude/notes.md",
		Live:     func() (string, error) { return "", nil },
		Data:     fake,
		Now:      historyNow,
	})
	if got := plain(history.View(120, 30)); !strings.Contains(strings.ToLower(got), "loading") {
		t.Errorf("unloaded screen missing a loading notice; got:\n%s", got)
	}
}

// TestHistoryFetchErrorRendersInScreen pins that a daemon history error is an
// IN-SCREEN state rendered verbatim (quiesce/not-initialized wording
// included), never a toast and never a blank list.
func TestHistoryFetchErrorRendersInScreen(t *testing.T) {
	t.Parallel()
	fake := &fakeHistoryData{historyErr: errors.New("daemon returned 503: quiesced until 15:04:05")}
	history := newHistoryFixture(t, fake, nil)

	got := plain(history.View(120, 30))
	if !strings.Contains(got, "quiesced until 15:04:05") {
		t.Errorf("history fetch error not rendered verbatim in-screen; got:\n%s", got)
	}
}

// TestHistoryEnterRendersBlob pins enter: the selected version's blob is
// fetched and rendered through the injected Render seam in a scroll viewport.
func TestHistoryEnterRendersBlob(t *testing.T) {
	t.Parallel()
	fake := &fakeHistoryData{
		historyResp: api.HistoryResponse{Versions: captureVersions()},
		blobs:       map[string]string{"aaaaaaaaaaaa1111": "the historical body\n"},
	}
	history := newHistoryFixture(t, fake, func(deps *HistoryDeps) {
		deps.Render = func(markdown string, _ int) string { return "GLAMOUR:" + markdown }
	})

	history = pressHistory(t, history, "enter")

	if len(fake.blobCalls) != 1 || fake.blobCalls[0].rev != "aaaaaaaaaaaa1111" {
		t.Fatalf("blob fetch = %+v, want exactly one for the selected rev", fake.blobCalls)
	}
	got := plain(history.View(120, 30))
	if !strings.Contains(got, "GLAMOUR:the historical body") {
		t.Errorf("blob view did not render the fetched body through Render; got:\n%s", got)
	}
}

// TestHistoryBlobErrorRendersInScreen pins that a blob fetch failure is an
// in-screen error too, not a toast and not a blank viewport.
func TestHistoryBlobErrorRendersInScreen(t *testing.T) {
	t.Parallel()
	fake := &fakeHistoryData{
		historyResp: api.HistoryResponse{Versions: captureVersions()},
		blobErr:     errors.New("daemon returned 500: blob is binary"),
	}
	history := newHistoryFixture(t, fake, nil)

	history = pressHistory(t, history, "enter")

	if got := plain(history.View(120, 30)); !strings.Contains(got, "blob is binary") {
		t.Errorf("blob fetch error not rendered in-screen; got:\n%s", got)
	}
}

// TestHistoryDiffVsLive pins d: a unified diff of the selected version against
// the live provider file, with the removed/added lines present.
func TestHistoryDiffVsLive(t *testing.T) {
	t.Parallel()
	versions := []api.HistoryVersion{
		{Rev: "sel000000000aaaa", Subject: "memory: host acme 2026-07-13T10:00:00Z", Host: "host", Timestamp: new(historyNow.Add(-time.Hour))},
	}
	fake := &fakeHistoryData{
		historyResp: api.HistoryResponse{Versions: versions},
		blobs:       map[string]string{"sel000000000aaaa": "keep\nREMOVED\n"},
	}
	history := newHistoryFixture(t, fake, func(deps *HistoryDeps) {
		deps.Live = func() (string, error) { return "keep\nADDED\n", nil }
	})

	history = pressHistory(t, history, "d")

	got := plain(history.View(120, 30))
	if !strings.Contains(got, "-REMOVED") {
		t.Errorf("diff-vs-live missing the removed line; got:\n%s", got)
	}
	if !strings.Contains(got, "+ADDED") {
		t.Errorf("diff-vs-live missing the added line; got:\n%s", got)
	}
	if !strings.Contains(got, "live") {
		t.Errorf("diff-vs-live missing the 'live' label on the new side; got:\n%s", got)
	}
}

// TestHistoryDiffVsOlder pins D: a unified diff of the selected version
// against the one adjacent-older to it.
func TestHistoryDiffVsOlder(t *testing.T) {
	t.Parallel()
	versions := []api.HistoryVersion{
		{Rev: "newer0000000aaaa", Subject: "memory: host acme 2026-07-13T10:00:00Z", Host: "host", Timestamp: new(historyNow.Add(-time.Hour))},
		{Rev: "older0000000bbbb", Subject: "memory: host acme 2026-07-13T08:00:00Z", Host: "host", Timestamp: new(historyNow.Add(-3 * time.Hour))},
	}
	fake := &fakeHistoryData{
		historyResp: api.HistoryResponse{Versions: versions},
		blobs: map[string]string{
			"newer0000000aaaa": "keep\nMIDDLE\n",
			"older0000000bbbb": "keep\nOLDEST\n",
		},
	}
	history := newHistoryFixture(t, fake, nil)

	history = pressHistory(t, history, "D") // cursor on the newest; diff against its older neighbour

	got := plain(history.View(120, 30))
	if !strings.Contains(got, "-OLDEST") {
		t.Errorf("diff-vs-older missing the older line as removed; got:\n%s", got)
	}
	if !strings.Contains(got, "+MIDDLE") {
		t.Errorf("diff-vs-older missing the selected line as added; got:\n%s", got)
	}
}

// TestHistoryDiffOlderAtOldestNotice pins the edge: D on the oldest version
// has no adjacent-older to diff against, so it says so rather than diffing
// against nothing.
func TestHistoryDiffOlderAtOldestNotice(t *testing.T) {
	t.Parallel()
	versions := []api.HistoryVersion{
		{Rev: "onlyone00000aaaa", Subject: "memory: host acme 2026-07-13T10:00:00Z", Host: "host", Timestamp: new(historyNow.Add(-time.Hour))},
	}
	fake := &fakeHistoryData{
		historyResp: api.HistoryResponse{Versions: versions},
		blobs:       map[string]string{"onlyone00000aaaa": "solo\n"},
	}
	history := newHistoryFixture(t, fake, nil)

	history = pressHistory(t, history, "D")

	if got := plain(history.View(120, 30)); !strings.Contains(strings.ToLower(got), "no older") {
		t.Errorf("D on the oldest version should note there is nothing older; got:\n%s", got)
	}
}

// TestHistoryRestoreConfirmEmitsRequest pins R → confirm → y: the confirm
// modal appears, y emits a RestoreRequestMsg carrying the selected version's
// blob content and its target, and n aborts with no message.
func TestHistoryRestoreConfirmEmitsRequest(t *testing.T) {
	t.Parallel()
	fake := &fakeHistoryData{
		historyResp: api.HistoryResponse{Versions: captureVersions()},
		blobs:       map[string]string{"aaaaaaaaaaaa1111": "restored body\n"},
	}
	history := newHistoryFixture(t, fake, nil)

	// R opens the confirm; the fetched blob is what y will carry.
	history = pressHistory(t, history, "R")
	if got := plain(history.View(120, 30)); !strings.Contains(strings.ToLower(got), "restore this version") {
		t.Fatalf("R did not open the restore confirm; got:\n%s", got)
	}

	// n aborts: no request, confirm closed.
	nAborted, cmd := history.Update(key("n"))
	history = nAborted.(*History)
	if cmd != nil {
		if msg := cmd(); msg != nil {
			t.Errorf("n on the confirm produced %#v, want nothing", msg)
		}
	}
	if got := plain(history.View(120, 30)); strings.Contains(strings.ToLower(got), "restore this version") {
		t.Errorf("n did not close the confirm; got:\n%s", got)
	}

	// R again, then y emits the request.
	history = pressHistory(t, history, "R")
	_, cmd = history.Update(key("y"))
	if cmd == nil {
		t.Fatal("y on the confirm produced no Cmd; want a RestoreRequestMsg")
	}
	request, ok := cmd().(RestoreRequestMsg)
	if !ok {
		t.Fatalf("y produced %#v, want RestoreRequestMsg", cmd())
	}
	if request.Folder != "acme" || request.RepoPath != "claude/notes.md" {
		t.Errorf("restore request targets %s/%s, want acme/claude/notes.md", request.Folder, request.RepoPath)
	}
	if request.Content != "restored body\n" {
		t.Errorf("restore request carries %q, want the selected version's blob", request.Content)
	}
}

// TestHistoryEscOrdering pins the Screen esc-consumption rule: esc backs a
// blob/diff sub-view out to the list first, closes an open restore confirm
// first, and only pops the screen when nothing internal is open.
func TestHistoryEscOrdering(t *testing.T) {
	t.Parallel()
	// Each parallel subtest builds its OWN fake: the fixture's InitCmd fetch
	// appends to fake.historyCalls/blobCalls, so a shared fake would have three
	// goroutines writing its slices unsynchronised (a real data race under
	// -race). Every sibling History test already constructs its own.
	newFake := func() *fakeHistoryData {
		return &fakeHistoryData{
			historyResp: api.HistoryResponse{Versions: captureVersions()},
			blobs:       map[string]string{"aaaaaaaaaaaa1111": "body\n"},
		}
	}

	t.Run("esc leaves the blob view for the list, no pop", func(t *testing.T) {
		t.Parallel()
		history := newHistoryFixture(t, newFake(), nil)
		history = pressHistory(t, history, "enter")
		next, cmd := history.Update(key("esc"))
		history = next.(*History)
		if cmd != nil {
			if _, isPop := cmd().(PopScreenMsg); isPop {
				t.Fatal("esc that left the blob view must not also pop the screen")
			}
		}
		if got := plain(history.View(120, 30)); !strings.Contains(got, "aaaaaaaaaaaa") {
			t.Errorf("esc did not return to the version list; got:\n%s", got)
		}
	})

	t.Run("esc closes the restore confirm, no pop", func(t *testing.T) {
		t.Parallel()
		history := newHistoryFixture(t, newFake(), nil)
		history = pressHistory(t, history, "R")
		next, cmd := history.Update(key("esc"))
		history = next.(*History)
		if cmd != nil {
			if _, isPop := cmd().(PopScreenMsg); isPop {
				t.Fatal("esc that closed the confirm must not also pop the screen")
			}
		}
		if got := plain(history.View(120, 30)); strings.Contains(strings.ToLower(got), "restore this version") {
			t.Errorf("esc did not close the restore confirm; got:\n%s", got)
		}
	})

	t.Run("esc on the bare list pops", func(t *testing.T) {
		t.Parallel()
		history := newHistoryFixture(t, newFake(), nil)
		_, cmd := history.Update(key("esc"))
		if cmd == nil {
			t.Fatal("esc on the bare list produced no Cmd; want a PopScreenMsg")
		}
		if _, isPop := cmd().(PopScreenMsg); !isPop {
			t.Fatalf("esc on the bare list produced %#v, want PopScreenMsg", cmd())
		}
	})
}

// TestHistoryStaleMessagesIgnored pins the stack-forwarding staleness guard: a
// fetch result whose RepoPath is not this screen's is dropped (it belongs to a
// different screen the root forwarded to), never adopted. Both stale messages
// carry content that WOULD land if the guard were absent — a successful
// version list that would replace this screen's own, and a blob that would
// show under the selected rev — so the guard is genuinely load-bearing rather
// than masked by the refresh-error path that keeps the last-good list anyway.
func TestHistoryStaleMessagesIgnored(t *testing.T) {
	t.Parallel()
	fake := &fakeHistoryData{historyResp: api.HistoryResponse{Versions: captureVersions()}}
	history := newHistoryFixture(t, fake, nil)

	staleVersions := HistoryVersionsMsg{
		Folder:   "acme",
		RepoPath: "claude/other.md",
		Versions: []api.HistoryVersion{{Rev: "staleeeeeeee0000", Subject: "memory: other file"}},
	}
	next, _ := history.Update(staleVersions)
	history = next.(*History)

	got := plain(history.View(120, 30))
	if strings.Contains(got, "staleeeeeeee") {
		t.Errorf("screen adopted a stale version list for a different path; got:\n%s", got)
	}
	if !strings.Contains(got, "aaaaaaaaaaaa") {
		t.Errorf("screen lost its own versions after a stale message; got:\n%s", got)
	}

	// enter the blob view (blob still fetching), then deliver a blob for the
	// selected rev but keyed to a DIFFERENT path: the guard must drop it, so
	// the sub-view stays on its loading notice rather than showing foreign
	// content under this rev.
	history = pressHistory(t, history, "enter")
	staleBlob := HistoryBlobMsg{
		Folder:   "acme",
		RepoPath: "claude/other.md",
		Rev:      "aaaaaaaaaaaa1111",
		Content:  "STALE-BLOB-CONTENT",
	}
	next, _ = history.Update(staleBlob)
	history = next.(*History)
	if got := plain(history.View(120, 30)); strings.Contains(got, "STALE-BLOB-CONTENT") {
		t.Errorf("blob view adopted a stale blob for a different path; got:\n%s", got)
	}
}

// TestHistoryViewportModesFillHeightExactly pins the height contract for the
// scroll modes (blob and diff): the viewport space-fills to EXACTLY the
// requested height at and above the chrome floor (header line + its blank =
// 2), the Reading precedent.
func TestHistoryViewportModesFillHeightExactly(t *testing.T) {
	t.Parallel()
	fake := &fakeHistoryData{
		historyResp: api.HistoryResponse{Versions: captureVersions()},
		blobs:       map[string]string{"aaaaaaaaaaaa1111": strings.Repeat("body line\n", 50)},
	}
	for _, height := range []int{6, 12, 30} {
		history := newHistoryFixture(t, fake, nil)
		history = pressHistory(t, history, "enter")
		got := history.View(80, height)
		if lineCount := strings.Count(got, "\n") + 1; lineCount != height {
			t.Errorf("blob view rendered %d lines at height %d, want exact fill; got:\n%s", lineCount, height, plain(got))
		}
	}
}

// TestHistoryListHeightBudget pins the list mode's height ceiling: like the
// browser, it windows rows so the rendered body never exceeds the height
// budget, keeping the cursor's own row visible.
func TestHistoryListHeightBudget(t *testing.T) {
	t.Parallel()
	versions := make([]api.HistoryVersion, 40)
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i := range versions {
		stamp := base.Add(time.Duration(i) * time.Hour)
		versions[i] = api.HistoryVersion{
			Rev:       strings.Repeat("0", 11) + string(rune('a'+i%26)) + "xxxx",
			Subject:   "memory: host acme stamp",
			Host:      "host",
			Timestamp: new(stamp),
		}
	}
	fake := &fakeHistoryData{historyResp: api.HistoryResponse{Versions: versions}}
	history := newHistoryFixture(t, fake, nil)

	const height = 10
	got := plain(history.View(80, height))
	if lineCount := strings.Count(got, "\n") + 1; lineCount > height {
		t.Errorf("list view rendered %d lines, want <= %d (height budget); got:\n%s", lineCount, height, got)
	}
}

// stampedVersions builds n capture versions with distinct revs and stamps — a
// helper for the truncation and height-budget tests that need a specific count.
func stampedVersions(n int) []api.HistoryVersion {
	versions := make([]api.HistoryVersion, n)
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i := range versions {
		stamp := base.Add(time.Duration(i) * time.Hour)
		versions[i] = api.HistoryVersion{
			Rev:       strings.Repeat("0", 11) + string(rune('a'+i%26)) + "xxxx",
			Subject:   "memory: host acme stamp",
			Host:      "host",
			Timestamp: new(stamp),
		}
	}
	return versions
}

// TestHistoryListDisclosesTruncation pins the silent-cap disclosure: a version
// list that came back at exactly historyVersionLimit says older history was not
// scanned (the scan is capped there), while a shorter list makes no such claim.
// The disclosure must also respect the height budget — it reserves its own row.
func TestHistoryListDisclosesTruncation(t *testing.T) {
	t.Parallel()
	t.Run("at the limit discloses, within budget", func(t *testing.T) {
		t.Parallel()
		fake := &fakeHistoryData{historyResp: api.HistoryResponse{Versions: stampedVersions(historyVersionLimit)}}
		history := newHistoryFixture(t, fake, nil)
		const height = 20
		got := plain(history.View(120, height))
		if !strings.Contains(got, "older history not scanned") {
			t.Errorf("a capped list did not disclose the truncation; got:\n%s", got)
		}
		if lineCount := strings.Count(got, "\n") + 1; lineCount > height {
			t.Errorf("capped list overflowed its height budget: %d lines > %d", lineCount, height)
		}
	})
	t.Run("below the limit does not disclose", func(t *testing.T) {
		t.Parallel()
		fake := &fakeHistoryData{historyResp: api.HistoryResponse{Versions: stampedVersions(historyVersionLimit - 1)}}
		history := newHistoryFixture(t, fake, nil)
		if got := plain(history.View(120, 40)); strings.Contains(got, "older history not scanned") {
			t.Errorf("a full-but-uncapped list wrongly disclosed truncation; got:\n%s", got)
		}
	})
}

// TestHistoryConfirmFillsHeightExactly pins the restore confirm's honest-height
// contract: like the blob/diff viewports, it renders EXACTLY the height it was
// handed — even at the tight budgets a short terminal produces (4 and 6) — so it
// can never push the footer, or its own y/N prompt, off-frame.
func TestHistoryConfirmFillsHeightExactly(t *testing.T) {
	t.Parallel()
	fake := &fakeHistoryData{
		historyResp: api.HistoryResponse{Versions: captureVersions()},
		blobs:       map[string]string{"aaaaaaaaaaaa1111": "body\n"},
	}
	for _, height := range []int{4, 6, 12} {
		history := newHistoryFixture(t, fake, nil)
		history = pressHistory(t, history, "R") // open the restore confirm
		got := history.View(80, height)
		if lineCount := strings.Count(got, "\n") + 1; lineCount != height {
			t.Errorf("restore confirm rendered %d lines at height %d, want exact fill; got:\n%s", lineCount, height, plain(got))
		}
		if !strings.Contains(plain(got), "cancel") {
			t.Errorf("restore confirm dropped its y/N prompt at height %d; got:\n%s", height, plain(got))
		}
	}
}

// TestHistoryDiffThemingAndLabels pins two otherwise-unpinned diff properties:
// each +/- line is coloured through the theme (Styles.OK / Styles.Fail), and a
// diff side's label carries the <shortRev> (<stamp>) shape. Asserted on the raw
// (unstripped) diff content so neutering styleDiff — or dropping the stamp from
// revLabel — actually fails here.
func TestHistoryDiffThemingAndLabels(t *testing.T) {
	t.Parallel()
	styles := theme.Default(true)
	versions := []api.HistoryVersion{
		{Rev: "sel000000000aaaa", Subject: "memory: host acme 2026-07-13T11:00:00Z", Host: "host", Timestamp: new(historyNow.Add(-time.Hour))},
	}
	fake := &fakeHistoryData{
		historyResp: api.HistoryResponse{Versions: versions},
		blobs:       map[string]string{"sel000000000aaaa": "keep\nREMOVED\n"},
	}
	history := newHistoryFixture(t, fake, func(deps *HistoryDeps) {
		deps.Styles = styles
		deps.Live = func() (string, error) { return "keep\nADDED\n", nil }
	})
	history = pressHistory(t, history, "d")

	content, _ := history.diffContent()
	if !strings.Contains(content, styles.OK.Render("+ADDED")) {
		t.Errorf("added line not themed through Styles.OK; raw diff:\n%q", content)
	}
	if !strings.Contains(content, styles.Fail.Render("-REMOVED")) {
		t.Errorf("removed line not themed through Styles.Fail; raw diff:\n%q", content)
	}
	if !strings.Contains(plain(content), "sel000000000 (2026-07-13 11:00)") {
		t.Errorf("diff label missing the <shortRev> (<stamp>) shape; got:\n%s", plain(content))
	}
}

// TestHistoryNilDataIssuesNoFetch pins the nil-Data guard: a History built with
// no Data (reachable only in a test — production always wires it) issues no
// fetch Cmd, rather than one that nil-derefs Data the moment it runs.
func TestHistoryNilDataIssuesNoFetch(t *testing.T) {
	t.Parallel()
	history := NewHistory(HistoryDeps{Folder: "acme", RepoPath: "claude/notes.md", Now: historyNow})
	if cmd := history.InitCmd(); cmd != nil {
		t.Errorf("InitCmd with nil Data returned a Cmd; want nil (nothing to fetch through)")
	}
	if cmd := history.versionsCmd(); cmd != nil {
		t.Errorf("versionsCmd with nil Data returned a Cmd; want nil")
	}
	if cmd := history.blobCmd("aaaaaaaaaaaa1111"); cmd != nil {
		t.Errorf("blobCmd with nil Data returned a Cmd; want nil")
	}
}

// TestHistoryDiffVsLiveRefreshesChangedError pins the live-side refresh nit: when
// a diff-vs-live's live read starts failing with a DIFFERENT error that has the
// same (empty) rendered text, onRefresh must still update the displayed message
// rather than leave the stale first error on screen.
func TestHistoryDiffVsLiveRefreshesChangedError(t *testing.T) {
	t.Parallel()
	liveErr := errors.New("first live error")
	versions := []api.HistoryVersion{
		{Rev: "sel000000000aaaa", Subject: "memory: host acme 2026-07-13T11:00:00Z", Host: "host", Timestamp: new(historyNow.Add(-time.Hour))},
	}
	fake := &fakeHistoryData{
		historyResp: api.HistoryResponse{Versions: versions},
		blobs:       map[string]string{"sel000000000aaaa": "body\n"},
	}
	history := newHistoryFixture(t, fake, func(deps *HistoryDeps) {
		deps.Live = func() (string, error) { return "", liveErr }
	})
	history = pressHistory(t, history, "d")
	if got := plain(history.View(120, 30)); !strings.Contains(got, "first live error") {
		t.Fatalf("diff-vs-live did not surface the first live error; got:\n%s", got)
	}

	liveErr = errors.New("second live error") // a different failure, same empty text
	next, _ := history.Update(RefreshMsg{Now: historyNow})
	history = next.(*History)

	got := plain(history.View(120, 30))
	if strings.Contains(got, "first live error") {
		t.Errorf("diff still shows the stale first error after refresh; got:\n%s", got)
	}
	if !strings.Contains(got, "second live error") {
		t.Errorf("diff did not update to the new live error after refresh; got:\n%s", got)
	}
}

var _ Screen = (*History)(nil)
