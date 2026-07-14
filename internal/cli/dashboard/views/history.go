package views

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	keybinding "charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	udiff "github.com/aymanbagabas/go-udiff"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
)

// historyVersionLimit bounds how many versions a /v0/history fetch asks for —
// the same ceiling the browser's deleted-memories scan uses, generous enough
// that a real memory's whole timeline fits while still capping a pathological
// history at a bounded transfer.
const historyVersionLimit = 200

// historyStampLayout formats a version's absolute capture instant. Spec §6
// wants the exact instant beside the relative age, the same both-clocks shape
// the reading header uses for its modified stamp.
const historyStampLayout = "2006-01-02 15:04"

// historyShortRevLen is how many leading hex characters of a rev a row shows —
// long enough to be unambiguous in one memory's timeline, short enough that
// the stamp/host columns still fit a narrow pane.
const historyShortRevLen = 12

// detailChromeLines is everything the blob/diff sub-view renders above its
// scroll viewport: the title line and its trailing blank. The viewport fills
// the remaining height exactly (the reading view's honest-height contract).
const detailChromeLines = 2

// HistoryVersionsMsg carries a completed /v0/history fetch back to the screen
// that asked for it. Folder+RepoPath are the exact keys the fetch ran for and
// double as a staleness guard: the root forwards every such message to the
// stack top, and a receiver drops any whose keys are not its own — the
// browser's folder-wide deleted scan (RepoPath "") and a per-memory History
// screen (RepoPath set) can both have a fetch in flight when the stack top
// changes, so each accepts only messages addressed to it. Err is a daemon or
// transport failure, surfaced verbatim rather than rendered as an empty list.
type HistoryVersionsMsg struct {
	Folder   string
	RepoPath string
	Versions []api.HistoryVersion
	Err      error
}

// HistoryBlobMsg carries a completed /v0/blob fetch. Rev names which version's
// content this is; Folder+RepoPath are the same staleness key as
// HistoryVersionsMsg. Content is decrypted memory text — it is never logged,
// only rendered, on either the History screen or the root's forwarding path.
type HistoryBlobMsg struct {
	Folder   string
	RepoPath string
	Rev      string
	Content  string
	Err      error
}

// HistoryDataSource is the read-only version surface the History screen
// consumes — the consumer-side seam (defined here, at the caller). The root's
// own m.data (a full DataSource) satisfies it, so a browser or reading view
// can build a History screen without threading anything narrower. Both methods
// run only inside a returned Cmd, never during Update or View.
type HistoryDataSource interface {
	History(ctx context.Context, folder, path string, limit int) (api.HistoryResponse, error)
	Blob(ctx context.Context, folder, path, rev string) (api.BlobResponse, error)
}

// HistoryDeps is everything the History screen needs, injected once — the
// consumer-side-seam idiom shared with BrowserDeps/ReadingDeps.
type HistoryDeps struct {
	// Memory is the browser/reading snapshot this history belongs to, used for
	// the breadcrumb/header name. Zero for the deleted-recovery variant (a
	// memory absent from HEAD has no live snapshot), where the header falls
	// back to RepoPath's base name.
	Memory memoryfs.Memory
	// Folder and RepoPath are the /v0/history + /v0/blob path keys
	// (RepoPath == "<provider>[/<repo_subdir>]/<rel>"); they also key every
	// fetch message this screen accepts (see HistoryVersionsMsg).
	Folder   string
	RepoPath string
	// Live reads the current provider-file content for the diff-vs-live view —
	// a local file read the root binds over memoryfs, the same documented
	// exception to "no I/O outside a Cmd" the browser/reading refresh rely on.
	// It returns "" for a deleted memory (nothing live to diff against). A nil
	// Live (some tests) diffs against empty.
	Live func() (string, error)
	// Data is the read-only version/blob surface, only ever called inside a
	// returned Cmd. A nil Data is reachable only in tests (production always
	// wires it, via the browser/reading Data seam): versionsCmd and blobCmd then
	// return a nil Cmd rather than a closure that would nil-deref when run, so
	// InitCmd is a no-op and the screen holds its loading notice — never a panic.
	Data HistoryDataSource
	// Render markdown-renders a blob body at a width — the root-owned glamour
	// seam shared with the browser preview and reading view. nil (some tests)
	// shows the raw blob text.
	Render func(md string, width int) string
	Styles theme.Styles
	// Now seeds the screen's own stored clock (History.now) at construction —
	// a plain value, not a func() time.Time, so it is never mistaken for a live
	// source: after construction every render reads the stored field, advanced
	// only by RefreshMsg's own Now (screen.go's clock-in-the-tick contract).
	Now time.Time
}

type historyMode int

const (
	modeList historyMode = iota
	modeBlob
	modeDiff
)

type historyDiffKind int

const (
	diffVsLive historyDiffKind = iota
	diffVsOlder
)

// blobEntry is one fetched blob: its content, or the error the fetch returned.
// Errors are recorded (not dropped) so the sub-view can show why a version
// would not load rather than spin on "loading" forever; RefreshMsg drops the
// errored entries so a transient failure self-heals on the next tick, while
// successful entries stay cached indefinitely — a blob at a fixed rev is
// immutable, so it can never go stale.
type blobEntry struct {
	content string
	err     error
}

// historyDetailCache memoizes the rendered blob/diff body for the exact inputs
// it was computed from. The sub-view re-renders on every keypress and every
// ~2s tick, and a glamour render over a full blob (or a unified diff over two
// large ones) is the expensive step; without this each of those would redo it
// even when nothing changed. Only a fully resolved body is cached — a loading
// or error state is recomputed every View (cheap, and never a stale success).
// The Render seam is deliberately not part of the key: a func value has no
// equality, so SetRender clears validity unconditionally instead.
type historyDetailCache struct {
	valid      bool
	mode       historyMode
	diffKind   historyDiffKind
	primaryRev string
	olderRev   string
	width      int
	content    string
}

func (c historyDetailCache) matches(mode historyMode, diffKind historyDiffKind, primaryRev, olderRev string, width int) bool {
	return c.valid && c.mode == mode && c.diffKind == diffKind &&
		c.primaryRev == primaryRev && c.olderRev == olderRev && c.width == width
}

// History is the per-memory version-history screen (spec §6): a version list
// that drills into a blob view, unified diffs (against live or the adjacent-
// older version), and a restore that lands a historical blob as a new capture.
// Pointer-receiver Screen mutating in place, the Browser/Reading precedent —
// a viewport, caches, and fetch bookkeeping are naturally mutable state.
//
// It is the first stacked Screen to run its own async daemon fetches: the
// version list and every blob arrive as Cmds whose result messages the root
// forwards back to the stack top, matched here by Folder+RepoPath so a fetch
// for one screen never lands in another (see HistoryVersionsMsg).
type History struct {
	deps HistoryDeps

	// now is the screen's own stored clock, seeded from deps.Now and advanced
	// by every RefreshMsg — never time.Now(), so relative ages render
	// deterministically and stay live against the root's tick (screen.go).
	now time.Time

	versions []api.HistoryVersion
	loaded   bool
	loadErr  error
	cursor   int

	mode     historyMode
	diffKind historyDiffKind
	// primaryRev is the version the blob view shows, or the newer side of a
	// diff; olderRev is the older side of a diff-vs-older ("" when the cursor
	// sits on the oldest version, so the view says there is nothing older).
	primaryRev string
	olderRev   string
	// liveText/liveErr snapshot Live() at diff-vs-live entry and refresh, so
	// the diff's new side is the current provider file without re-reading it
	// on every render.
	liveText string
	liveErr  error

	confirming bool
	confirmRev string

	// blobCache holds fetched blobs by rev (see blobEntry); fetching guards
	// against issuing a duplicate request for a rev already in flight.
	blobCache map[string]blobEntry
	fetching  map[string]bool

	detailViewport viewport.Model
	detailCache    historyDetailCache
}

// NewHistory builds a ready History screen. It performs no I/O: unlike the
// browser and reading views (whose first frame comes from a cheap local read),
// the version list is a daemon fetch, so it is issued as InitCmd after the
// root pushes the screen — the first frame shows a loading notice until the
// fetch answers.
func NewHistory(deps HistoryDeps) *History {
	detailViewport := viewport.New()
	detailViewport.KeyMap = readingViewportKeyMap()
	return &History{
		deps:           deps,
		now:            deps.Now,
		detailViewport: detailViewport,
		blobCache:      make(map[string]blobEntry),
		fetching:       make(map[string]bool),
	}
}

// InitCmd is the initial version fetch, issued by the root right after it
// pushes the screen (the initScreen seam) rather than batched into the push
// itself — a push that also carried the fetch would race the screen onto the
// stack against its own first result.
func (h *History) InitCmd() tea.Cmd {
	return h.versionsCmd()
}

// Title names the breadcrumb segment: the memory's display name, or (for the
// deleted-recovery variant, which has no live snapshot) the file's base name.
func (h *History) Title() string {
	return h.memoryName()
}

// Target reports the folder and repo path this history is over — the /v0/blob
// path key restore lands into. Exported for the root's restore-availability
// gate (fact-class ∧ no active handoff), outside the Screen interface for the
// same root-reaches-the-concrete-type reason as Browser.Selected.
func (h *History) Target() (folder, repoPath string) {
	return h.deps.Folder, h.deps.RepoPath
}

func (h *History) memoryName() string {
	if h.deps.Memory.Name != "" {
		return h.deps.Memory.Name
	}
	return path.Base(h.deps.RepoPath)
}

// SetStyles installs a new theme — root-propagated on a background-color swap,
// the same treatment every pushed screen gets. Styles feed only chrome, which
// re-renders every View, so nothing pairs with this. Not part of the Screen
// interface (kept to Update/View/Title), the Browser/Reading seam.
func (h *History) SetStyles(styles theme.Styles) {
	h.deps.Styles = styles
}

// SetRender installs a new markdown-render seam, invalidating the detail cache
// unconditionally: a func value has no equality, so clearing here is the only
// way a theme swap reliably forces the next blob render through the new
// renderer instead of a string cached under the old one.
func (h *History) SetRender(render func(md string, width int) string) {
	h.deps.Render = render
	h.detailCache.valid = false
}

// Update handles one message. The two fetch-result messages funnel through the
// same staleness guard — a message whose Folder/RepoPath is not this screen's
// was forwarded to us but belongs to a different screen's in-flight fetch, and
// is dropped without effect.
func (h *History) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case RefreshMsg:
		return h.onRefresh(msg.Now)
	case HistoryVersionsMsg:
		if msg.Folder != h.deps.Folder || msg.RepoPath != h.deps.RepoPath {
			return h, nil
		}
		h.adoptVersions(msg)
		return h, nil
	case HistoryBlobMsg:
		if msg.Folder != h.deps.Folder || msg.RepoPath != h.deps.RepoPath {
			return h, nil
		}
		delete(h.fetching, msg.Rev)
		h.blobCache[msg.Rev] = blobEntry{content: msg.Content, err: msg.Err}
		return h, nil
	case tea.KeyPressMsg:
		return h.updateKey(msg)
	}
	return h, nil
}

// onRefresh advances the stored clock and keeps the screen live: the version
// list is re-fetched (spec §6's "history only grows" — a restore's new capture
// appears within a tick), cached blob errors are dropped so a transient
// failure retries, and a diff-vs-live re-reads the provider file so an
// external edit to it shows immediately.
func (h *History) onRefresh(now time.Time) (Screen, tea.Cmd) {
	h.now = now
	for rev, entry := range h.blobCache {
		if entry.err != nil {
			delete(h.blobCache, rev)
		}
	}
	if h.mode == modeDiff && h.diffKind == diffVsLive {
		text, err := h.readLive()
		// Compare the error's MESSAGE, not just its presence: a live read can
		// start failing with a DIFFERENT error whose rendered diff text is the
		// same empty string as the last one (both errors render "" content), and
		// an (err == nil) != (h.liveErr == nil) test would see no change and
		// leave the stale first message on screen.
		if text != h.liveText || errorText(err) != errorText(h.liveErr) {
			h.liveText, h.liveErr = text, err
			h.detailCache.valid = false
		}
	}
	return h, tea.Batch(h.versionsCmd(), h.ensureDetailCmd())
}

// errorText is an error's message, or "" for nil — a comparable summary of an
// error's identity, so two different non-nil errors register as a change even
// when both drive the same (empty) rendered body.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// adoptVersions installs a successful fetch's versions and clamps the cursor.
// A refresh error keeps the last-good list (the reading view's degrade-don't-
// blank rule): a transient failure mid-browse must not blank a timeline the
// user is reading. Only the very first fetch's error surfaces as loadErr — the
// screen has nothing else to show yet.
func (h *History) adoptVersions(msg HistoryVersionsMsg) {
	if msg.Err != nil {
		if !h.loaded {
			h.loadErr = msg.Err
			h.loaded = true
		}
		return
	}
	h.versions = msg.Versions
	h.loadErr = nil
	h.loaded = true
	h.cursor = clampCursor(h.cursor, len(h.versions))
}

func (h *History) updateKey(msg tea.KeyPressMsg) (Screen, tea.Cmd) {
	// esc consumes internal state first (screen.go's ordering rule): an open
	// confirm, then a blob/diff sub-view, and only a bare list esc pops.
	if h.confirming {
		return h.updateConfirm(msg)
	}
	if h.mode != modeList {
		return h.updateDetail(msg)
	}
	return h.updateList(msg)
}

func (h *History) updateList(msg tea.KeyPressMsg) (Screen, tea.Cmd) {
	switch {
	case keybinding.Matches(msg, DashboardKeys.HistoryBack):
		return h, func() tea.Msg { return PopScreenMsg{} }
	case keybinding.Matches(msg, DashboardKeys.HistoryView):
		return h.enterBlob()
	case keybinding.Matches(msg, DashboardKeys.HistoryDiff):
		return h.enterDiff(diffVsLive)
	case keybinding.Matches(msg, DashboardKeys.HistoryDiffOlder):
		return h.enterDiff(diffVsOlder)
	case keybinding.Matches(msg, DashboardKeys.HistoryRestore):
		return h.startRestore()
	case keybinding.Matches(msg, DashboardKeys.Select):
		h.moveCursor(msg.String())
		return h, nil
	}
	return h, nil
}

// updateDetail handles keys while a blob/diff sub-view owns the screen: esc
// backs out to the list (consumed locally — no PopScreenMsg, so the root does
// not also pop), g/G jump to top/bottom, and everything else is the viewport's
// own scroll set (readingViewportKeyMap).
func (h *History) updateDetail(msg tea.KeyPressMsg) (Screen, tea.Cmd) {
	switch {
	case keybinding.Matches(msg, DashboardKeys.HistoryBack):
		h.mode = modeList
		return h, nil
	case msg.String() == "g":
		h.detailViewport.GotoTop()
		return h, nil
	case msg.String() == "G":
		h.detailViewport.GotoBottom()
		return h, nil
	}
	var cmd tea.Cmd
	h.detailViewport, cmd = h.detailViewport.Update(msg)
	return h, cmd
}

// updateConfirm handles the restore confirm's keys: esc or n aborts, y lands
// the restore — but only once the version's blob is in hand. y while the blob
// is still fetching (or failed) keeps the confirm open rather than restore
// nothing; the confirm view shows which state it is in.
func (h *History) updateConfirm(msg tea.KeyPressMsg) (Screen, tea.Cmd) {
	switch {
	case keybinding.Matches(msg, DashboardKeys.Cancel):
		h.confirming = false
		return h, nil
	case keybinding.Matches(msg, DashboardKeys.ConfirmDecision):
		if strings.ToLower(msg.String()) != "y" {
			h.confirming = false
			return h, nil
		}
		entry, ok := h.blobCache[h.confirmRev]
		if !ok || entry.err != nil {
			return h, nil // content not ready or failed to load: keep the confirm open
		}
		h.confirming = false
		folder, repoPath, content := h.deps.Folder, h.deps.RepoPath, entry.content
		return h, func() tea.Msg {
			return RestoreRequestMsg{Folder: folder, RepoPath: repoPath, Content: content}
		}
	}
	return h, nil
}

func (h *History) moveCursor(name string) {
	switch name {
	case "up", "k":
		if h.cursor > 0 {
			h.cursor--
		}
	case "down", "j":
		if h.cursor < len(h.versions)-1 {
			h.cursor++
		}
	}
}

func (h *History) selectedVersion() (api.HistoryVersion, bool) {
	if h.cursor < 0 || h.cursor >= len(h.versions) {
		return api.HistoryVersion{}, false
	}
	return h.versions[h.cursor], true
}

// olderVersion is the version adjacent-older to the cursor. Versions are
// newest-first (the engine's order), so the older neighbour is the next index.
func (h *History) olderVersion() (api.HistoryVersion, bool) {
	next := h.cursor + 1
	if next >= len(h.versions) {
		return api.HistoryVersion{}, false
	}
	return h.versions[next], true
}

func (h *History) enterBlob() (Screen, tea.Cmd) {
	version, ok := h.selectedVersion()
	if !ok {
		return h, nil
	}
	h.mode = modeBlob
	h.primaryRev = version.Rev
	h.olderRev = ""
	return h, h.ensureBlobCmd(version.Rev)
}

// enterDiff opens the diff sub-view. diff-vs-live snapshots the provider file
// now (a local read, the documented exception) as the new side; diff-vs-older
// needs the adjacent-older version's blob too. The detail cache is cleared
// because the live side can change between entries even when rev and width do
// not, and the cache key does not include it.
func (h *History) enterDiff(kind historyDiffKind) (Screen, tea.Cmd) {
	version, ok := h.selectedVersion()
	if !ok {
		return h, nil
	}
	h.mode = modeDiff
	h.diffKind = kind
	h.primaryRev = version.Rev
	h.detailCache.valid = false

	switch kind {
	case diffVsLive:
		h.liveText, h.liveErr = h.readLive()
		h.olderRev = ""
		return h, h.ensureBlobCmd(version.Rev)
	case diffVsOlder:
		older, ok := h.olderVersion()
		if !ok {
			h.olderRev = ""
			return h, h.ensureBlobCmd(version.Rev)
		}
		h.olderRev = older.Rev
		return h, tea.Batch(h.ensureBlobCmd(version.Rev), h.ensureBlobCmd(older.Rev))
	}
	return h, nil
}

// startRestore opens the restore confirm for the selected version and ensures
// its blob is fetched — the content y will land. The confirm can open before
// the blob arrives; updateConfirm holds y until it does.
func (h *History) startRestore() (Screen, tea.Cmd) {
	version, ok := h.selectedVersion()
	if !ok {
		return h, nil
	}
	h.confirming = true
	h.confirmRev = version.Rev
	return h, h.ensureBlobCmd(version.Rev)
}

func (h *History) readLive() (string, error) {
	if h.deps.Live == nil {
		return "", nil
	}
	return h.deps.Live()
}

// ensureBlobCmd issues a blob fetch for rev unless it is already cached or in
// flight — so re-entering a sub-view, or a per-tick ensureDetailCmd, never
// duplicates a request.
func (h *History) ensureBlobCmd(rev string) tea.Cmd {
	if rev == "" {
		return nil
	}
	if _, cached := h.blobCache[rev]; cached {
		return nil
	}
	if h.fetching[rev] {
		return nil
	}
	h.fetching[rev] = true
	return h.blobCmd(rev)
}

// ensureDetailCmd re-issues the fetches the current sub-view needs — used by
// RefreshMsg so a blob that failed (and was just dropped) or never arrived is
// retried. It is nil in list mode, which needs no blob.
func (h *History) ensureDetailCmd() tea.Cmd {
	switch h.mode {
	case modeBlob:
		return h.ensureBlobCmd(h.primaryRev)
	case modeDiff:
		return tea.Batch(h.ensureBlobCmd(h.primaryRev), h.ensureBlobCmd(h.olderRev))
	}
	return nil
}

func (h *History) versionsCmd() tea.Cmd {
	data, folder, repoPath := h.deps.Data, h.deps.Folder, h.deps.RepoPath
	if data == nil {
		return nil // nothing to fetch through (see HistoryDeps.Data) — never a closure that would nil-deref when run
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
		defer cancel()
		resp, err := data.History(ctx, folder, repoPath, historyVersionLimit)
		return HistoryVersionsMsg{Folder: folder, RepoPath: repoPath, Versions: resp.Versions, Err: err}
	}
}

func (h *History) blobCmd(rev string) tea.Cmd {
	data, folder, repoPath := h.deps.Data, h.deps.Folder, h.deps.RepoPath
	if data == nil {
		return nil // nothing to fetch through (see HistoryDeps.Data) — never a closure that would nil-deref when run
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
		defer cancel()
		resp, err := data.Blob(ctx, folder, repoPath, rev)
		return HistoryBlobMsg{Folder: folder, RepoPath: repoPath, Rev: rev, Content: resp.Content, Err: err}
	}
}

// View renders whichever surface is active: the restore confirm, a blob or
// diff sub-view, or the version list. width/height arrive fresh from the root
// every call, so a resize is handled by construction.
func (h *History) View(width, height int) string {
	switch {
	case h.confirming:
		return h.confirmView(height)
	case h.mode == modeBlob, h.mode == modeDiff:
		return h.detailView(width, height)
	default:
		return h.listView(height)
	}
}

// listView renders the version list: a title, then a load error, a loading
// notice, an empty notice, or the cursor-windowed rows. Rows are windowed to
// the height budget (the browser's visibleWindow), so a long timeline never
// walks the cursor off-screen or overflows the pane.
func (h *History) listView(height int) string {
	var body strings.Builder
	body.WriteString(sectionTitle(h.deps.Styles, "History: "+h.memoryName()))
	body.WriteString("\n\n")

	switch {
	case h.loadErr != nil:
		fmt.Fprintf(&body, "history unavailable: %v", h.loadErr)
		return strings.TrimRight(body.String(), "\n")
	case !h.loaded:
		body.WriteString(h.deps.Styles.Dim.Render("loading history…"))
		return strings.TrimRight(body.String(), "\n")
	case len(h.versions) == 0:
		body.WriteString(h.deps.Styles.Dim.Render("no history for this memory yet"))
		return strings.TrimRight(body.String(), "\n")
	}

	// A list that came back at exactly the fetch cap is (over-)approximated as
	// truncated: older history was not scanned, so the newest-slice-only view is
	// disclosed rather than passed off as the whole timeline. The disclosure
	// reserves its own row out of the height budget so it never overflows.
	truncated := len(h.versions) == historyVersionLimit
	budget := max(height-2, 1) // title line + its trailing blank
	if truncated {
		budget = max(budget-1, 1) // the disclosure row
	}
	start, end := visibleWindow(h.cursor, len(h.versions), budget)
	lines := make([]string, 0, end-start+1)
	for row := start; row < end; row++ {
		lines = append(lines, h.renderVersionRow(row))
	}
	if truncated {
		lines = append(lines, h.deps.Styles.Dim.Render(historyTruncationNotice(historyVersionLimit)))
	}
	body.WriteString(strings.Join(lines, "\n"))
	return strings.TrimRight(body.String(), "\n")
}

// historyTruncationNotice discloses that a scan came back at its fetch cap — the
// newest `limit` commits only, older history not scanned. The caller passes its
// own cap because the callers do NOT share one: the per-memory list and the
// folder-wide deleted scan use historyVersionLimit, insights' activity sections
// use insightsHistoryLimit. The count is the caller's actual limit, so the
// wording stays exactly true at the len == limit boundary this fires on (an
// over-approximation: a scan with exactly limit commits and no more is disclosed
// too, the safe direction).
func historyTruncationNotice(limit int) string {
	return fmt.Sprintf("showing the newest %d commits — older history not scanned", limit)
}

func (h *History) renderVersionRow(row int) string {
	version := h.versions[row]
	marker := "  "
	if row == h.cursor {
		marker = "> "
	}
	line := marker + h.versionSummary(version)
	if row == h.cursor {
		line = h.deps.Styles.Selected.Render(line)
	}
	return line
}

// versionSummary renders one version's one-line summary. A capture commit
// (Timestamp set) shows short rev · absolute stamp · relative age · host, plus
// a live tag when its blob equals HEAD. A foreign commit (nil Timestamp, empty
// Host — a merge or an unrelated change touching the file) has no capture
// fields to show, so it falls back to its raw subject rather than dereference
// a nil Timestamp.
func (h *History) versionSummary(version api.HistoryVersion) string {
	short := shortRev(version.Rev)
	if version.Timestamp == nil {
		return short + "  " + version.Subject
	}
	host := version.Host
	if host == "" {
		host = "—"
	}
	summary := fmt.Sprintf("%s  %s  %s  %s",
		short, version.Timestamp.Format(historyStampLayout),
		relativeTime(*version.Timestamp, h.now), host)
	if version.Live {
		summary += "  " + h.deps.Styles.Badge.Render("live")
	}
	return summary
}

// confirmView renders the restore confirm: the always-visible warning, the
// target version, and a state line — fetching, a load failure, or ready — so y
// is never a blind keystroke over content the screen does not yet hold. Like
// the blob/diff viewports it honours the honest-height contract, rendering
// EXACTLY height rows (fitLinesToHeight) so a short terminal can never push the
// footer — or the confirm's own y/N prompt — off-frame.
func (h *History) confirmView(height int) string {
	version, _ := h.versionForRev(h.confirmRev)
	lines := []string{
		sectionTitle(h.deps.Styles, "History: "+h.memoryName()),
		"",
		h.deps.Styles.Warn.Render("Restore this version? It becomes a NEW capture — history only grows."),
		h.deps.Styles.Dim.Render(h.versionSummary(version)),
	}
	switch entry, ok := h.blobCache[h.confirmRev]; {
	case !ok:
		lines = append(lines, h.deps.Styles.Dim.Render("fetching version content…"))
	case entry.err != nil:
		lines = append(lines, h.deps.Styles.Fail.Render(fmt.Sprintf("cannot restore: %v", entry.err)))
	default:
		lines = append(lines, h.deps.Styles.OK.Render("ready to restore"))
	}
	lines = append(lines, "", h.deps.Styles.Dim.Render("y restore · N cancel"))
	return fitLinesToHeight(lines, height)
}

// fitLinesToHeight renders lines to EXACTLY height rows: blank-padded at the
// bottom when it has fewer (the viewport modes' space-fill, so a short body can
// never leave the footer floating over dead space), and — when a tight terminal
// budgets fewer rows than it has — trimmed to the leading height-1 lines plus
// the LAST, so a trailing prompt line survives the trim rather than scrolling
// off with the content above it.
func fitLinesToHeight(lines []string, height int) string {
	if height < 1 {
		height = 1
	}
	switch {
	case len(lines) < height:
		lines = append(lines, make([]string, height-len(lines))...)
	case len(lines) > height && height == 1:
		lines = lines[len(lines)-1:]
	case len(lines) > height:
		lines = append(lines[:height-1:height-1], lines[len(lines)-1])
	}
	return strings.Join(lines, "\n")
}

// detailView renders a blob or diff in the scroll viewport, filling the height
// below the title line + blank exactly (the reading view's honest-height
// contract).
func (h *History) detailView(width, height int) string {
	var view strings.Builder
	view.WriteString(sectionTitle(h.deps.Styles, h.detailTitle()))
	view.WriteString("\n\n")
	h.syncDetailViewport(width, height)
	view.WriteString(h.detailViewport.View())
	return strings.TrimRight(view.String(), "\n")
}

func (h *History) detailTitle() string {
	base := "History: " + h.memoryName()
	if h.mode == modeDiff {
		return base + " · diff"
	}
	return base + " · " + shortRev(h.primaryRev)
}

// syncDetailViewport sizes the viewport and installs the current body — but
// only re-renders (and re-wraps) when an input actually changed, serving the
// detail cache otherwise. A loading/error body is never cached, so it is
// recomputed each View until the blob resolves; the cost is a tiny string, not
// a glamour pass.
func (h *History) syncDetailViewport(width, height int) {
	h.detailViewport.SetWidth(width)
	h.detailViewport.SetHeight(max(height-detailChromeLines, 1))
	if h.detailCache.matches(h.mode, h.diffKind, h.primaryRev, h.olderRev, width) {
		return
	}
	content, cacheable := h.detailContent(width)
	h.detailViewport.SetContent(content)
	if cacheable {
		h.detailCache = historyDetailCache{
			valid: true, mode: h.mode, diffKind: h.diffKind,
			primaryRev: h.primaryRev, olderRev: h.olderRev, width: width, content: content,
		}
	}
}

// detailContent computes the current sub-view's body and whether it is a
// resolved result worth caching (a loading or error placeholder is not).
func (h *History) detailContent(width int) (string, bool) {
	if h.mode == modeBlob {
		return h.blobContent(width)
	}
	return h.diffContent()
}

func (h *History) blobContent(width int) (string, bool) {
	entry, ok := h.blobCache[h.primaryRev]
	if !ok {
		return h.deps.Styles.Dim.Render("loading version…"), false
	}
	if entry.err != nil {
		return h.deps.Styles.Fail.Render(fmt.Sprintf("version unavailable: %v", entry.err)), false
	}
	if h.deps.Render != nil {
		return h.deps.Render(entry.content, width), true
	}
	return entry.content, true
}

func (h *History) diffContent() (string, bool) {
	newer, ok := h.blobCache[h.primaryRev]
	if !ok {
		return h.deps.Styles.Dim.Render("loading version…"), false
	}
	if newer.err != nil {
		return h.deps.Styles.Fail.Render(fmt.Sprintf("version unavailable: %v", newer.err)), false
	}

	var oldLabel, newLabel, oldText, newText string
	switch h.diffKind {
	case diffVsLive:
		if h.liveErr != nil {
			return h.deps.Styles.Fail.Render(fmt.Sprintf("live content unavailable: %v", h.liveErr)), false
		}
		oldLabel, newLabel = h.revLabel(h.primaryRev), "live"
		oldText, newText = newer.content, h.liveText
	case diffVsOlder:
		if h.olderRev == "" {
			return h.deps.Styles.Dim.Render("no older version to diff against"), true
		}
		older, ok := h.blobCache[h.olderRev]
		if !ok {
			return h.deps.Styles.Dim.Render("loading version…"), false
		}
		if older.err != nil {
			return h.deps.Styles.Fail.Render(fmt.Sprintf("version unavailable: %v", older.err)), false
		}
		oldLabel, newLabel = h.revLabel(h.olderRev), h.revLabel(h.primaryRev)
		oldText, newText = older.content, newer.content
	}

	diff := udiff.Unified(oldLabel, newLabel, oldText, newText)
	if strings.TrimSpace(diff) == "" {
		return h.deps.Styles.Dim.Render("no differences"), true
	}
	return h.styleDiff(diff), true
}

// styleDiff colours a unified diff by line class through the theme: additions
// green, deletions red, hunk headers blue, context unstyled. The file header
// lines (+++/---) fall in with additions/deletions, which reads fine and keeps
// the classifier a simple prefix test.
func (h *History) styleDiff(diff string) string {
	var out strings.Builder
	for i, line := range strings.Split(diff, "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}
		switch {
		case strings.HasPrefix(line, "@@"):
			out.WriteString(h.deps.Styles.Info.Render(line))
		case strings.HasPrefix(line, "+"):
			out.WriteString(h.deps.Styles.OK.Render(line))
		case strings.HasPrefix(line, "-"):
			out.WriteString(h.deps.Styles.Fail.Render(line))
		default:
			out.WriteString(line)
		}
	}
	return out.String()
}

// revLabel is a diff side's label: short rev plus its capture stamp, or bare
// short rev for a foreign commit (no stamp to show).
func (h *History) revLabel(rev string) string {
	version, ok := h.versionForRev(rev)
	if !ok || version.Timestamp == nil {
		return shortRev(rev)
	}
	return fmt.Sprintf("%s (%s)", shortRev(rev), version.Timestamp.Format(historyStampLayout))
}

func (h *History) versionForRev(rev string) (api.HistoryVersion, bool) {
	for _, version := range h.versions {
		if version.Rev == rev {
			return version, true
		}
	}
	return api.HistoryVersion{}, false
}

func shortRev(rev string) string {
	if len(rev) <= historyShortRevLen {
		return rev
	}
	return rev[:historyShortRevLen]
}
