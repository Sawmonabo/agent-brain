package views

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/lint"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/providertest"
)

// adversarialName is a real-length snake_case memory name (48 display cols) of
// the kind live-hub testing turned up — long enough to overflow the browser's
// 46-col list pane on its own, before any description or age is composed on.
const adversarialName = "feedback_security_invariant_scope_counterexample"

// lineEndingWithAge returns the sole rendered list row — the one ending in a
// relative-age suffix like "(4d ago)". Fails the test if zero or many match, so
// the age-visibility assertions target an unambiguous line.
func lineEndingWithAge(t *testing.T, view string) string {
	t.Helper()
	var found []string
	for line := range strings.SplitSeq(view, "\n") {
		if strings.HasSuffix(line, "ago)") {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one age-suffixed row, found %d in:\n%s", len(found), view)
	}
	return found[0]
}

// browserFixtureRegistry builds a two-provider registry (a per-project
// "claude" and a global "codex", both with an empty pattern table — every
// file classifies ClassFact) — enough to exercise memoryfs.List's real
// classification without pulling in either adapter's Discover/Identify.
func browserFixtureRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	claudeFake := providertest.New("claude", provider.ScopePerProject, nil)
	codexFake := providertest.New("codex", provider.ScopeGlobal, nil)
	registry, err := provider.NewRegistry(claudeFake, codexFake)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

// writeBrowserFile seeds one fixture file at an explicit mtime — os.Chtimes
// afterward, rather than relying on write-order timing, so "newest first"
// assertions never depend on filesystem mtime resolution or test speed.
func writeBrowserFile(t *testing.T, dir, rel, content string, modTime time.Time) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(full, modTime, modTime); err != nil {
		t.Fatal(err)
	}
}

// fakeReadBody returns a canned body keyed by RepoPath, defaulting to an
// empty (link-free) body for any memory a test did not bother to name —
// keeping every fixture's lint scan a deliberate no-op unless the test
// wants otherwise.
func fakeReadBody(bodies map[string]string) func(memoryfs.Memory) (string, error) {
	return func(m memoryfs.Memory) (string, error) {
		return bodies[m.RepoPath], nil
	}
}

// TestBrowserListsGroupedByProviderNewestFirst pins the browser's default
// render shape: memories grouped by provider (alphabetical group order),
// newest-first within each group, and `o` flipping the within-group order
// to name order — without disturbing the provider grouping itself.
func TestBrowserListsGroupedByProviderNewestFirst(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	claudeDir, codexDir := t.TempDir(), t.TempDir()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	writeBrowserFile(t, claudeDir, "alpha.md", "---\nname: Alpha Notes\n---\n", base)
	writeBrowserFile(t, claudeDir, "zulu.md", "---\nname: Zulu Notes\n---\n", base.Add(time.Hour))
	writeBrowserFile(t, codexDir, "gamma.md", "---\nname: Gamma Notes\n---\n", base.Add(30*time.Minute))

	units := []api.UnitInfo{
		{Provider: "claude", Folder: "acme", LocalDir: claudeDir},
		{Provider: "codex", Folder: "acme", LocalDir: codexDir},
	}
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: fakeReadBody(nil),
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
	})

	got := plain(browser.View(80, 40))
	idx := func(s string) int {
		i := strings.Index(got, s)
		if i == -1 {
			t.Fatalf("view missing %q; got:\n%s", s, got)
		}
		return i
	}
	claudeHeader, zulu, alpha := idx("claude"), idx("Zulu Notes"), idx("Alpha Notes")
	codexHeader, gamma := idx("codex"), idx("Gamma Notes")
	if claudeHeader >= zulu || zulu >= alpha || alpha >= codexHeader || codexHeader >= gamma {
		t.Fatalf("default (recency) render order wrong: claude=%d zulu=%d alpha=%d codex=%d gamma=%d; got:\n%s",
			claudeHeader, zulu, alpha, codexHeader, gamma, got)
	}

	next, _ := browser.Update(key("o"))
	browser = next.(*Browser)
	got = plain(browser.View(80, 40))
	alpha, zulu = strings.Index(got, "Alpha Notes"), strings.Index(got, "Zulu Notes")
	if alpha == -1 || zulu == -1 || alpha >= zulu {
		t.Fatalf("o did not switch to name order (want Alpha before Zulu); got:\n%s", got)
	}
}

// TestBrowserFilterNarrows pins the in-browser filter: opening it and
// typing narrows to name/description matches; esc restores the full list.
func TestBrowserFilterNarrows(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	writeBrowserFile(t, dir, "auth.md", "---\nname: Auth Service\n---\n", time.Now())
	writeBrowserFile(t, dir, "billing.md", "---\nname: Billing\ndescription: authentication tokens\n---\n", time.Now())
	writeBrowserFile(t, dir, "logging.md", "---\nname: Logging\ndescription: log shipping\n---\n", time.Now())

	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: fakeReadBody(nil),
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
	})

	full := plain(browser.View(80, 40))
	for _, want := range []string{"Auth Service", "Billing", "Logging"} {
		if !strings.Contains(full, want) {
			t.Fatalf("initial view missing %q; got:\n%s", want, full)
		}
	}

	next, _ := browser.Update(key("/"))
	browser = next.(*Browser)
	for _, r := range "auth" {
		next, _ = browser.Update(key(string(r)))
		browser = next.(*Browser)
	}

	filtered := plain(browser.View(80, 40))
	if !strings.Contains(filtered, "Auth Service") || !strings.Contains(filtered, "Billing") {
		t.Errorf("filtered view missing a name/description match; got:\n%s", filtered)
	}
	if strings.Contains(filtered, "Logging") {
		t.Errorf("filtered view still shows a non-matching row; got:\n%s", filtered)
	}

	next, _ = browser.Update(key("esc"))
	browser = next.(*Browser)
	restored := plain(browser.View(80, 40))
	for _, want := range []string{"Auth Service", "Billing", "Logging"} {
		if !strings.Contains(restored, want) {
			t.Errorf("esc did not restore the full list; missing %q in:\n%s", want, restored)
		}
	}
}

// TestBrowserLintBadge pins the ⚠ badge against one of lint.Check's several
// rules — dangling links: a memory whose body names a [[wiki-link]] target
// that matches no other memory in the listing is flagged; a memory with no
// such dangling reference is not. Both fixture files carry a complete
// frontmatter block (name AND description) so the frontmatter rule cannot
// also flag "Clean Memory" and confound what this test isolates —
// TestBrowserLintBadgeStaleness covers the staleness rule the same way.
func TestBrowserLintBadge(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	writeBrowserFile(t, dir, "has-link.md", "---\nname: Has Link\ndescription: references another memory\n---\n", time.Now())
	writeBrowserFile(t, dir, "clean.md", "---\nname: Clean Memory\ndescription: a genuinely clean memory\n---\n", time.Now())

	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	bodies := map[string]string{
		"claude/has-link.md": "sabotage: see [[Ghost Reference]] for details\n",
		"claude/clean.md":    "no links here\n",
	}
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: fakeReadBody(bodies),
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
	})

	got := plain(browser.View(80, 40))
	lineFor := func(name string) string {
		for line := range strings.SplitSeq(got, "\n") {
			if strings.Contains(line, name) {
				return line
			}
		}
		t.Fatalf("no row for %q; view:\n%s", name, got)
		return ""
	}
	if !strings.Contains(lineFor("Has Link"), "⚠") {
		t.Errorf("dangling-link memory row missing ⚠; view:\n%s", got)
	}
	if strings.Contains(lineFor("Clean Memory"), "⚠") {
		t.Errorf("clean memory row wrongly flagged ⚠; view:\n%s", got)
	}
}

// TestBrowserLintBadgeStaleness pins the ⚠ badge against lint.Check's
// staleness rule specifically, with no dangling link anywhere in play: a
// memory unmodified for exactly the configured threshold is not yet stale
// (checkStale's own contract is "strictly more than" the threshold, not an
// exact match); one calendar day past it, delivered via RefreshMsg's own
// Now rather than a fresh construction, it is.
func TestBrowserLintBadgeStaleness(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	writeBrowserFile(t, dir, "old.md", "---\nname: Old Note\ndescription: an aging memory\n---\n", base)

	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	browser := NewBrowser(BrowserDeps{
		Registry:       registry,
		Units:          units,
		Folder:         "acme",
		Now:            base.Add(10 * 24 * time.Hour), // exactly at the threshold
		StaleAfterDays: 10,
		ReadBody:       fakeReadBody(map[string]string{"claude/old.md": "no links here\n"}),
		List:           func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
	})

	atThreshold := plain(browser.View(80, 40))
	if strings.Contains(atThreshold, "⚠") {
		t.Errorf("memory exactly at the staleness threshold is already flagged; view:\n%s", atThreshold)
	}

	next, _ := browser.Update(RefreshMsg{Now: base.Add(11 * 24 * time.Hour)}) // one day past
	browser = next.(*Browser)

	pastThreshold := plain(browser.View(80, 40))
	if !strings.Contains(pastThreshold, "⚠") {
		t.Errorf("memory one day past the staleness threshold is not flagged; view:\n%s", pastThreshold)
	}
}

// TestBrowserPreviewRendersSelection pins the split-pane threshold: at a
// roomy width the selection's body is markdown-rendered through the
// injected Render seam on the right; below the threshold there is no
// preview pane at all, and Render is never even called (proving the gate
// short-circuits rather than just hiding the output).
func TestBrowserPreviewRendersSelection(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	writeBrowserFile(t, dir, "solo.md", "---\nname: Solo Memory\n---\n", time.Now())

	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	var renderCalls int
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: fakeReadBody(map[string]string{"claude/solo.md": "Selected body content"}),
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
		Render: func(md string, _ int) string {
			renderCalls++
			return "RENDERED:" + md
		},
	})

	wide := plain(browser.View(120, 30))
	if !strings.Contains(wide, "RENDERED:Selected body content") {
		t.Errorf("wide view missing the rendered preview; got:\n%s", wide)
	}
	if renderCalls != 1 {
		t.Fatalf("Render called %d times at width 120, want 1", renderCalls)
	}

	narrow := plain(browser.View(80, 30))
	if strings.Contains(narrow, "RENDERED:") {
		t.Errorf("narrow view still shows a preview pane; got:\n%s", narrow)
	}
	if renderCalls != 1 {
		t.Errorf("Render called %d times at width 80, want still 1 (no preview means no render call)", renderCalls)
	}
}

// TestBrowserEmptyListShowsGuidance covers the empty-project edge case: a
// folder with zero memory files must render a plain guidance line, never a
// blank or panicking list/preview split.
func TestBrowserEmptyListShowsGuidance(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: t.TempDir()}}
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: fakeReadBody(nil),
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
	})

	got := plain(browser.View(120, 30))
	if !strings.Contains(strings.ToLower(got), "no memories") {
		t.Errorf("empty browser view missing guidance text; got:\n%s", got)
	}
}

// TestBrowserLoadErrorSurfaces covers a List failure (e.g. a unit naming an
// unregistered provider): the browser must show the error, not crash.
func TestBrowserLoadErrorSurfaces(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("boom: provider not registered")
	browser := NewBrowser(BrowserDeps{
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: fakeReadBody(nil),
		List:     func() ([]memoryfs.Memory, error) { return nil, wantErr },
	})

	got := plain(browser.View(120, 30))
	if !strings.Contains(got, "boom: provider not registered") {
		t.Errorf("load-error view missing the error text; got:\n%s", got)
	}
}

// TestBrowserPreviewUnavailableOnReadError covers a file that vanishes (or
// otherwise fails to read) between listing and previewing it: the preview
// pane must show a plain unavailable notice, never propagate the error into
// a crash or render garbage through Render.
func TestBrowserPreviewUnavailableOnReadError(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	writeBrowserFile(t, dir, "vanishing.md", "---\nname: Vanishing\n---\n", time.Now())
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}

	readErr := errors.New("open vanishing.md: no such file or directory")
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: func(memoryfs.Memory) (string, error) { return "", readErr },
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
		Render:   func(md string, _ int) string { return "RENDERED:" + md },
	})

	got := plain(browser.View(120, 30))
	if !strings.Contains(got, "preview unavailable") {
		t.Errorf("view missing the preview-unavailable notice; got:\n%s", got)
	}
	if strings.Contains(got, "RENDERED:") {
		t.Errorf("view rendered through Render despite a ReadBody error; got:\n%s", got)
	}
}

// TestBrowserPreviewRenderIsCached pins the preview cache: View runs on
// every keypress and on every ~2s RefreshMsg tick while the browser sits
// open idle, so without a cache, renderPreview would re-read the selected
// memory's full body (up to memoryfs.ReadBody's own size cap) and re-run
// glamour over it that often — real, avoidable cost at any real project's
// body sizes. The cache is keyed on (RepoPath, ModTime, width); this
// proves a repeated View with
// none of the three changed costs one read, and each one changing on its
// own forces exactly one more.
func TestBrowserPreviewRenderIsCached(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	writeBrowserFile(t, dir, "alpha.md", "---\nname: Alpha\n---\n", base)
	writeBrowserFile(t, dir, "zulu.md", "---\nname: Zulu\n---\n", base.Add(time.Hour))
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}

	var readCalls int
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      base,
		ReadBody: func(m memoryfs.Memory) (string, error) {
			readCalls++
			return "body of " + m.Name, nil
		},
		List: func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
	})
	// Construction's own refresh() already ran links.BuildIndex and
	// lint.Check over both memories, each of which calls ReadBody in its
	// own right — every assertion below counts up from this baseline
	// rather than from zero, so it stays a pure preview-cache seam.
	base0 := readCalls
	if base0 == 0 {
		t.Fatal("setup: construction never scanned any memory body")
	}

	_ = browser.View(120, 30)
	if readCalls != base0+1 {
		t.Fatalf("readCalls = %d after the first View, want %d", readCalls, base0+1)
	}

	_ = browser.View(120, 30)
	if readCalls != base0+1 {
		t.Errorf("readCalls = %d after a second identical View, want still %d (cache hit)", readCalls, base0+1)
	}

	next, _ := browser.Update(key("down"))
	browser = next.(*Browser)
	_ = browser.View(120, 30)
	if readCalls != base0+2 {
		t.Errorf("readCalls = %d after the selection changed, want %d", readCalls, base0+2)
	}
	_ = browser.View(120, 30)
	if readCalls != base0+2 {
		t.Errorf("readCalls = %d after a second identical View post-selection-change, want still %d (cache hit)", readCalls, base0+2)
	}

	_ = browser.View(140, 30)
	if readCalls != base0+3 {
		t.Errorf("readCalls = %d after the width changed, want %d", readCalls, base0+3)
	}
	_ = browser.View(140, 30)
	if readCalls != base0+3 {
		t.Errorf("readCalls = %d after a second identical View post-width-change, want still %d (cache hit)", readCalls, base0+3)
	}

	// The selected file (Alpha, cursor 1) rewritten with a later ModTime
	// that still keeps it sorted after Zulu — base+30m, short of Zulu's
	// own base+1h — so the cursor still lands back on Alpha after refresh
	// re-sorts, isolating a genuine ModTime-only change from a
	// selection-changed-by-resort one. RefreshMsg is how refresh's own doc
	// says a rewrite reaches the browser in production; that same refresh
	// also re-scans both bodies for lint, adding its own two calls on top
	// of the one the subsequent View below adds for the preview cache miss.
	writeBrowserFile(t, dir, "alpha.md", "---\nname: Alpha\n---\n", base.Add(30*time.Minute))
	next, cmd := browser.Update(RefreshMsg{Now: base})
	if cmd != nil {
		t.Fatal("RefreshMsg produced a Cmd; want none")
	}
	browser = next.(*Browser)
	afterRefresh := readCalls
	_ = browser.View(140, 30)
	if readCalls != afterRefresh+1 {
		t.Errorf("readCalls = %d after the selected file's ModTime changed, want %d", readCalls, afterRefresh+1)
	}
}

// TestBrowserEscClearsFilterThenPops pins the Screen contract's consumption
// rule directly: while the in-browser filter is open, esc clears it and
// produces NO PopScreenMsg-bearing Cmd (the screen consumed the key); the
// next esc, with the filter already closed, does produce one.
func TestBrowserEscClearsFilterThenPops(t *testing.T) {
	t.Parallel()
	browser := NewBrowser(BrowserDeps{
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: fakeReadBody(nil),
		List:     func() ([]memoryfs.Memory, error) { return nil, nil },
	})

	next, _ := browser.Update(key("/"))
	browser = next.(*Browser)
	next, _ = browser.Update(key("a"))
	browser = next.(*Browser)
	if browser.filter.Value() != "a" {
		t.Fatalf("filter value = %q, want %q", browser.filter.Value(), "a")
	}

	next, cmd := browser.Update(key("esc"))
	browser = next.(*Browser)
	if browser.filtering {
		t.Fatal("esc did not close filter mode")
	}
	if browser.filter.Value() != "" {
		t.Fatalf("esc did not clear the filter text, got %q", browser.filter.Value())
	}
	if cmd != nil {
		if _, isPop := cmd().(PopScreenMsg); isPop {
			t.Fatal("esc that only cleared the filter must not also signal a pop")
		}
	}

	_, cmd = browser.Update(key("esc"))
	if cmd == nil {
		t.Fatal("esc with no filter open must signal a pop")
	}
	if _, isPop := cmd().(PopScreenMsg); !isPop {
		t.Fatal("esc with no filter open did not produce a PopScreenMsg")
	}
}

// TestBrowserRelintSkipsUnchangedListing pins a performance-correctness
// property: RefreshMsg re-lists on every tick, but the (expensive, full-body)
// dangling-link scan only re-runs when the listing's (RepoPath, ModTime)
// fingerprint actually changed — an idle browsing session costs zero extra
// body reads per tick, not one per memory every refresh.
func TestBrowserRelintSkipsUnchangedListing(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	now := time.Now()
	writeBrowserFile(t, dir, "one.md", "---\nname: One\n---\n", now)
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}

	var readCalls int
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      now,
		ReadBody: func(_ memoryfs.Memory) (string, error) {
			readCalls++
			return "", nil
		},
		List: func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
	})
	afterConstruct := readCalls
	if afterConstruct == 0 {
		t.Fatal("construction never scanned any memory body")
	}

	// Update mutates the *Browser in place and returns the same pointer
	// (Screen's "usually itself"), so neither return value is needed here —
	// the mutation is what the readCalls assertions below observe. Now
	// stays the same instant both calls below: a bare zero-value RefreshMsg
	// would otherwise jump the browser's stored clock to year 1, and the
	// fingerprint's own day-bucket component (spec §8 staleness has day
	// granularity) would then see a changed bucket and force a rescan this
	// test does not intend to exercise.
	browser.Update(RefreshMsg{Now: now})
	if readCalls != afterConstruct {
		t.Errorf("ReadBody called again for an unchanged listing: %d calls, want %d", readCalls, afterConstruct)
	}

	// Touch the file's mtime (a genuine change) and confirm a rescan does happen.
	if err := os.Chtimes(filepath.Join(dir, "one.md"), now.Add(time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	browser.Update(RefreshMsg{Now: now})
	if readCalls <= afterConstruct {
		t.Errorf("ReadBody was not called again after the listing changed: %d calls, want > %d", readCalls, afterConstruct)
	}
}

// TestBrowserRelintFiresOnDayBucketRoll pins the fingerprint's blast-radius
// fix: lint.Check's staleness rule crosses its threshold purely as a
// function of elapsed wall-clock time, so a memory can need a fresh lint
// pass while the browser sits open across a calendar day even though no
// file's ModTime ever changes. Proven the same way
// TestBrowserRelintSkipsUnchangedListing proves the ordinary
// ModTime-changed case — a ReadBody call-count seam — except here nothing
// about any file changes at all; only now's day bucket does.
func TestBrowserRelintFiresOnDayBucketRoll(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	writeBrowserFile(t, dir, "one.md", "---\nname: One\n---\n", base)
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}

	var readCalls int
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      base,
		ReadBody: func(_ memoryfs.Memory) (string, error) {
			readCalls++
			return "", nil
		},
		List: func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
	})
	afterConstruct := readCalls
	if afterConstruct == 0 {
		t.Fatal("construction never scanned any memory body")
	}

	// Same instant delivered again: no file changed and no day rolled, so
	// no rescan is expected.
	browser.Update(RefreshMsg{Now: base})
	if readCalls != afterConstruct {
		t.Errorf("ReadBody called again for an unchanged instant: %d calls, want %d", readCalls, afterConstruct)
	}

	// One calendar day later: still no file changed, only now's day bucket
	// rolled — the fingerprint must still change and force a rescan.
	browser.Update(RefreshMsg{Now: base.Add(24 * time.Hour)})
	if readCalls <= afterConstruct {
		t.Errorf("ReadBody was not called again after the day bucket rolled: %d calls, want > %d", readCalls, afterConstruct)
	}
}

// TestBrowserRefreshUpdatesRelativeTimeClock pins the fix for a frozen-clock
// bug: BrowserDeps.Now used to be a func() time.Time closure built once, at
// push time, over the root's Model — but Model has value semantics, so that
// closure stayed pinned to whatever m.now was at that single moment forever,
// never observing a later tick's advanced clock the way every other
// relative-time render in this package already does (Activity's View takes
// now as a live per-call parameter). Now is a plain seed value; the
// Browser's own stored clock (b.now) is what every render reads, and only a
// RefreshMsg's own Now field ever advances it. This drives that exact path:
// construct with one instant, cross an hour boundary via RefreshMsg, and
// confirm the rendered label actually changes.
func TestBrowserRefreshUpdatesRelativeTimeClock(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	writeBrowserFile(t, dir, "note.md", "---\nname: Note\n---\n", base)

	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      base.Add(50 * time.Minute), // ModTime 50m in the past
		ReadBody: fakeReadBody(nil),
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
	})

	initial := plain(browser.View(80, 40))
	if !strings.Contains(initial, "50m ago") {
		t.Fatalf("initial view missing %q; got:\n%s", "50m ago", initial)
	}

	next, _ := browser.Update(RefreshMsg{Now: base.Add(65 * time.Minute)}) // crosses the 1h boundary
	browser = next.(*Browser)

	later := plain(browser.View(80, 40))
	if strings.Contains(later, "50m ago") {
		t.Errorf("view still shows the stale 50m label after RefreshMsg advanced the clock; got:\n%s", later)
	}
	if !strings.Contains(later, "1h ago") {
		t.Errorf("view missing the updated %q label after RefreshMsg advanced the clock; got:\n%s", "1h ago", later)
	}
}

// TestVisibleWindowKeepsCursorInBounds pins visibleWindow's own math
// directly: a table exercising the identity (no-scroll) case, both edges,
// and the centered middle, independent of any rendering.
func TestVisibleWindowKeepsCursorInBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                  string
		cursor, total, height int
		wantStart, wantEnd    int
	}{
		{"fits without scrolling", 5, 8, 10, 0, 8},
		{"height <= 0 is unwindowed", 5, 30, 0, 0, 30},
		{"cursor at top", 0, 30, 10, 0, 10},
		{"cursor at bottom", 29, 30, 10, 20, 30},
		{"cursor near bottom stays clamped", 25, 30, 10, 20, 30},
		{"cursor centers mid-list", 15, 30, 10, 10, 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			start, end := visibleWindow(tt.cursor, tt.total, tt.height)
			if start != tt.wantStart || end != tt.wantEnd {
				t.Errorf("visibleWindow(%d, %d, %d) = (%d, %d), want (%d, %d)",
					tt.cursor, tt.total, tt.height, start, end, tt.wantStart, tt.wantEnd)
			}
			if tt.cursor < start || tt.cursor >= end {
				t.Errorf("visibleWindow(%d, %d, %d) = (%d, %d) excludes its own cursor",
					tt.cursor, tt.total, tt.height, start, end)
			}
		})
	}
}

// TestBrowserListWindowsAroundCursor pins the scroll-follow behavior
// end-to-end: a project with more memories than fit the pane must keep the
// cursor's own row visible rather than let it walk off-screen. 30 memories
// at height 10 (five windows' worth) — cursor at construction's default (0)
// must show the top slice; walking it down to 25 must bring that row into
// view.
func TestBrowserListWindowsAroundCursor(t *testing.T) {
	t.Parallel()
	const rowCount = 30
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	memories := make([]memoryfs.Memory, rowCount)
	for i := range memories {
		memories[i] = memoryfs.Memory{
			Provider: "claude",
			Name:     fmt.Sprintf("Memory %02d", i),
			RepoPath: fmt.Sprintf("claude/memory-%02d.md", i),
			// Descending ModTime so visibleRows's newest-first default
			// sorts index i to row i directly, with no resort surprises.
			ModTime: base.Add(time.Duration(rowCount-i) * time.Hour),
		}
	}
	browser := NewBrowser(BrowserDeps{
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: fakeReadBody(nil),
		List:     func() ([]memoryfs.Memory, error) { return memories, nil },
	})

	got := plain(browser.View(80, 10))
	if !strings.Contains(got, "Memory 00") {
		t.Errorf("cursor at 0: view missing the top row; got:\n%s", got)
	}
	if strings.Contains(got, "Memory 25") {
		t.Errorf("cursor at 0: view already shows row 25; window did not start at the top; got:\n%s", got)
	}

	for range 25 {
		next, _ := browser.Update(key("down"))
		browser = next.(*Browser)
	}

	got = plain(browser.View(80, 10))
	if !strings.Contains(got, "Memory 25") {
		t.Errorf("cursor at 25: view missing the cursor's own row; got:\n%s", got)
	}
	if strings.Contains(got, "Memory 00") {
		t.Errorf("cursor at 25: view still shows row 0; cursor was not kept visible; got:\n%s", got)
	}
}

// TestBrowserViewHeightBudgetHoldsAboveTheClampFloor pins the height
// contract for heights at or above listRowBudget's clamp floor — chrome
// (2 lines for the title and its blank, plus 2 more when the filter is
// open) plus countDistinctProviders(rows) plus 1 — where View renders no
// more than height lines, no matter how much chrome (the title line and
// its blank, the filter line and its blank when open, and however many
// provider-group header lines the visible window happens to include) it
// layers on top of the windowed list rows. Below that floor the contract
// is different: see TestBrowserViewClampsToOneRowBelowTheClampFloor. This
// fixture (30 memories, 3 providers) uses height 10 throughout, which
// clears the floor for either filter state (6 closed, 8 open) with room to
// spare. Runs at several cursor positions — including both ends of the
// list, where visibleWindow's clamping is most likely to surface an
// off-by-one — with the filter both closed and open, since opening it
// changes the chrome budget listRowBudget must account for.
func TestBrowserViewHeightBudgetHoldsAboveTheClampFloor(t *testing.T) {
	t.Parallel()
	const rowCount = 30
	const groupSize = 10
	const height = 10
	providerNames := []string{"claude", "codex", "gemini"}
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	memories := make([]memoryfs.Memory, rowCount)
	for i := range memories {
		provider := providerNames[i/groupSize]
		memories[i] = memoryfs.Memory{
			Provider: provider,
			Name:     fmt.Sprintf("Memory %02d", i),
			RepoPath: fmt.Sprintf("%s/memory-%02d.md", provider, i),
			// Descending ModTime so visibleRows's newest-first default
			// sorts index i to row i directly, with no resort surprises.
			ModTime: base.Add(time.Duration(rowCount-i) * time.Hour),
		}
	}

	tests := []struct {
		name      string
		cursor    int
		filtering bool
	}{
		{"top, filter closed", 0, false},
		{"bottom, filter closed", rowCount - 1, false},
		{"middle, filter closed", 15, false},
		{"top, filter open", 0, true},
		{"bottom, filter open", rowCount - 1, true},
		{"middle, filter open", 15, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			browser := NewBrowser(BrowserDeps{
				Folder:   "acme",
				Now:      time.Now(),
				ReadBody: fakeReadBody(nil),
				List:     func() ([]memoryfs.Memory, error) { return memories, nil },
			})
			for range tt.cursor {
				next, _ := browser.Update(key("down"))
				browser = next.(*Browser)
			}
			if tt.filtering {
				next, _ := browser.Update(key("/"))
				browser = next.(*Browser)
			}

			got := plain(browser.View(80, height))
			lineCount := strings.Count(got, "\n") + 1
			if lineCount > height {
				t.Errorf("View rendered %d lines, want <= %d (height budget); got:\n%s", lineCount, height, got)
			}
			wantRow := fmt.Sprintf("Memory %02d", tt.cursor)
			if !strings.Contains(got, wantRow) {
				t.Errorf("cursor row %q not visible; got:\n%s", wantRow, got)
			}
		})
	}
}

// TestBrowserViewClampsToOneRowBelowTheClampFloor pins the max(budget, 1)
// floor in listRowBudget itself: below chrome+countDistinctProviders(rows)+1,
// height alone can't fit even one list row once chrome is accounted for, so
// the budget is clamped to 1 rather than left at zero or negative — a zero
// or negative rowBudget would hit visibleWindow's own "height <= 0" identity
// branch and render every row unwindowed, which is the one outcome
// guaranteed to overflow an already-tight height instead of degrading
// gracefully. Deliberately does not assert lineCount <= height — below the
// floor that bound does not hold by design (see
// TestBrowserViewHeightBudgetHoldsAboveTheClampFloor for the contract that
// does hold, above it): this asserts instead that the clamp holds the
// window to exactly one row and does not expand toward the full listing.
//
// Reuses the crowded three-provider fixture (30 memories, groups of 10) at
// height 3 — below that fixture's own floor of 6 (chrome 2 + 3 providers +
// 1) — where the rendered body is, by design, an irreducible frame larger
// than the requested height: title line, its blank, exactly one
// provider-group header, and the cursor's own row (4 lines, not <= 3).
func TestBrowserViewClampsToOneRowBelowTheClampFloor(t *testing.T) {
	t.Parallel()
	const rowCount = 30
	const groupSize = 10
	const height = 3
	providerNames := []string{"claude", "codex", "gemini"}
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	memories := make([]memoryfs.Memory, rowCount)
	for i := range memories {
		provider := providerNames[i/groupSize]
		memories[i] = memoryfs.Memory{
			Provider: provider,
			Name:     fmt.Sprintf("Memory %02d", i),
			RepoPath: fmt.Sprintf("%s/memory-%02d.md", provider, i),
			ModTime:  base.Add(time.Duration(rowCount-i) * time.Hour),
		}
	}
	browser := NewBrowser(BrowserDeps{
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: fakeReadBody(nil),
		List:     func() ([]memoryfs.Memory, error) { return memories, nil },
	})

	got := plain(browser.View(80, height))

	if !strings.Contains(got, "Memory 00") {
		t.Fatalf("cursor's own row (Memory 00) not visible at the clamped floor; got:\n%s", got)
	}
	if strings.Contains(got, "Memory 29") {
		t.Errorf("clamp did not hold: view reaches rows far outside a single-row window — looks like the full listing rendered unwindowed; got:\n%s", got)
	}
	if lineCount := strings.Count(got, "\n") + 1; lineCount != 4 {
		t.Errorf("clamped view has %d lines, want exactly 4 (title, blank, one provider header, one row); got:\n%s", lineCount, got)
	}
}

// TestBrowserListRowFitsNarrowWidth pins the width invariant on the no-preview
// (narrow) layout: with an adversarially long memory name, every
// rendered list line must fit the content width, or a real terminal soft-wraps
// it into the cramped multi-line soup the fix removes. Unfixed the row runs
// ~100 cols regardless of the 80-col frame.
func TestBrowserListRowFitsNarrowWidth(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	memories := []memoryfs.Memory{
		{Provider: "claude", Name: adversarialName, Description: "a long frontmatter description line that itself would overflow the pane budget several times over", RepoPath: "claude/a.md", ModTime: now.Add(-96 * time.Hour)},
		{Provider: "claude", Name: "short-name", Description: "brief", RepoPath: "claude/b.md", ModTime: now.Add(-time.Hour)},
	}
	browser := NewBrowser(BrowserDeps{Folder: "acme", Now: now, ReadBody: fakeReadBody(nil), List: func() ([]memoryfs.Memory, error) { return memories, nil }})

	const width = 80 // < previewMinWidth: the list gets the full width, no preview pane
	for line := range strings.SplitSeq(plain(browser.View(width, 40)), "\n") {
		if w := ansi.StringWidth(line); w > width {
			t.Errorf("list line width %d exceeds pane width %d (a real terminal wraps it):\n%q", w, width, line)
		}
	}
}

// TestBrowserHeightContractWithLongNames extends the height invariant of
// TestBrowserViewHeightBudgetHoldsAboveTheClampFloor with adversarial names
// at a preview-split width, where the 46-col list pane is the surface that
// wrapped: each long row folded into 2-3 physical lines and blew the row-budget
// math, which counts rows, not physical lines. lineCount(View) must stay within
// height. The empty preview body keeps the preview pane one line tall, so the
// list block alone drives the joined height.
func TestBrowserHeightContractWithLongNames(t *testing.T) {
	t.Parallel()
	const rowCount, groupSize, height, width = 30, 10, 10, 120
	providerNames := []string{"claude", "codex", "gemini"}
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	memories := make([]memoryfs.Memory, rowCount)
	for i := range memories {
		p := providerNames[i/groupSize]
		memories[i] = memoryfs.Memory{
			Provider:    p,
			Name:        fmt.Sprintf("%s_%02d", adversarialName, i), // 48+ cols each
			Description: "and a long description that compounds the overflow well past the list pane",
			RepoPath:    fmt.Sprintf("%s/memory-%02d.md", p, i),
			ModTime:     base.Add(time.Duration(rowCount-i) * time.Hour),
		}
	}
	browser := NewBrowser(BrowserDeps{Folder: "acme", Now: time.Now(), ReadBody: fakeReadBody(nil), List: func() ([]memoryfs.Memory, error) { return memories, nil }})

	got := plain(browser.View(width, height))
	if lineCount := strings.Count(got, "\n") + 1; lineCount > height {
		t.Errorf("View rendered %d lines at height %d — long names wrapped the %d-col list pane; got:\n%s", lineCount, height, listPaneWidth, got)
	}
}

// TestBrowserLongRowKeepsAgeAndTruncatesName pins the row-budget priority: when
// a name cannot fit, the age suffix survives (never truncated) and the NAME is
// what gets the ellipsis, not the age. Unfixed the name renders in full (the old
// rune-truncate only ever touched the description), overflowing the row.
func TestBrowserLongRowKeepsAgeAndTruncatesName(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	longName := adversarialName + "_with_even_more_suffix_to_force_the_truncation" // ~94 cols
	memories := []memoryfs.Memory{
		{Provider: "claude", Name: longName, Description: "desc", RepoPath: "claude/a.md", ModTime: now.Add(-96 * time.Hour)},
	}
	browser := NewBrowser(BrowserDeps{Folder: "acme", Now: now, ReadBody: fakeReadBody(nil), List: func() ([]memoryfs.Memory, error) { return memories, nil }})

	const width = 60
	row := lineEndingWithAge(t, plain(browser.View(width, 40)))
	if w := ansi.StringWidth(row); w > width {
		t.Errorf("row width %d exceeds %d: %q", w, width, row)
	}
	if !strings.HasSuffix(row, "ago)") {
		t.Errorf("age suffix dropped from an over-budget row: %q", row)
	}
	if !strings.Contains(row, "…") {
		t.Errorf("over-budget name not truncated with an ellipsis: %q", row)
	}
	if strings.Contains(row, longName) {
		t.Errorf("full name rendered despite exceeding the row budget: %q", row)
	}
}

// TestBrowserSelectedLongRowIsSingleLine pins that the highlighted row of an
// adversarial name renders as exactly one physical line at a preview-split
// width, rather than wrapping into fragments the reverse-video highlight bleeds
// across. A single-memory fixture makes the count exact: title, its blank, the
// provider header, and the one selected row — four lines. Unfixed the row wraps
// and the count climbs.
func TestBrowserSelectedLongRowIsSingleLine(t *testing.T) {
	t.Parallel()
	memories := []memoryfs.Memory{
		{Provider: "claude", Name: adversarialName, Description: "a description long enough to compound the row overflow past the list pane", RepoPath: "claude/a.md", ModTime: time.Now()},
	}
	browser := NewBrowser(BrowserDeps{Folder: "acme", Now: time.Now(), ReadBody: fakeReadBody(nil), List: func() ([]memoryfs.Memory, error) { return memories, nil }})

	// cursor defaults to row 0 — the sole, selected memory.
	got := plain(browser.View(120, 40))
	const wantLines = 4 // section title + blank + "claude" header + the one row
	if lineCount := strings.Count(got, "\n") + 1; lineCount > wantLines {
		t.Errorf("selected long row spans the view across %d lines, want <= %d (one physical row, not wrapped fragments); got:\n%s", lineCount, wantLines, got)
	}
}

// TestBrowserDeletedViewFitsWidth pins the width invariant on the deleted-
// recovery list: a long deleted repo path must be fit to the content width the
// same way live rows are, so the deleted list never soft-wraps past its own
// height budget. Unfixed deletedView ignores width entirely.
func TestBrowserDeletedViewFitsWidth(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	writeBrowserFile(t, dir, "alive.md", "---\nname: Alive\n---\n", time.Now())
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	when := time.Now()
	longPath := "claude/deeply/nested/" + adversarialName + "/" + adversarialName + ".md" // > 80 cols
	fake := &fakeHistoryData{historyResp: api.HistoryResponse{Versions: []api.HistoryVersion{
		{Rev: "v1", Timestamp: &when, Paths: []string{longPath}},
	}}}
	browser := NewBrowser(BrowserDeps{
		Registry: registry, Units: units, Folder: "acme", Now: time.Now(),
		ReadBody: fakeReadBody(nil),
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
		Data:     fake,
	})

	next, cmd := browser.Update(key("x"))
	browser = next.(*Browser)
	for _, msg := range drain(cmd) {
		next, _ := browser.Update(msg)
		browser = next.(*Browser)
	}

	const width = 80
	got := plain(browser.View(width, 30))
	if !strings.Contains(got, "claude/deeply") {
		t.Fatalf("deleted view did not render the recoverable long path at all; got:\n%s", got)
	}
	for line := range strings.SplitSeq(got, "\n") {
		if w := ansi.StringWidth(line); w > width {
			t.Errorf("deleted-list line width %d exceeds %d (wraps): %q", w, width, line)
		}
	}
}

// TestBrowserRenderListFitsPaneWidthDirectly pins the row-fit Result invariant
// at the exact 46-col list-pane width the preview split confines rows to — the
// width a public View masks, because the MaxWidth pane pads every line to 46
// regardless of whether the row was already that wide or got there by wrapping.
// Every line renderList returns, provider headers and ⚠-badged rows included,
// must ALREADY be <= the pane width so the pane never wraps. Exercised
// white-box because the pre-pane line width is observable at no other seam.
func TestBrowserRenderListFitsPaneWidthDirectly(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	memories := []memoryfs.Memory{
		{Provider: "claude", Name: adversarialName, Description: "a description long enough to be truncated hard against the pane budget", RepoPath: "claude/a.md", ModTime: base.Add(-96 * time.Hour)},
		{Provider: "claude", Name: "brief", Description: "short", RepoPath: "claude/b.md", ModTime: base.Add(-time.Hour)},
		{Provider: "codex", Name: adversarialName + "_two", Description: "another overflowing description that must be cut to fit the list pane", RepoPath: "codex/c.md", ModTime: base.Add(-2 * time.Hour)},
	}
	browser := NewBrowser(BrowserDeps{Folder: "acme", Now: base, ReadBody: fakeReadBody(nil), List: func() ([]memoryfs.Memory, error) { return memories, nil }})

	rows := browser.visibleRows()
	// Flag the adversarial-name memory: the reserved ⚠ badge must fit inside
	// the same width budget as the name and age, never push the row past the
	// pane. Injected after visibleRows so no relint can rebuild the flag set.
	browser.lintFlags = map[string]bool{"claude/a.md": true}
	list := browser.renderList(rows, browser.listRowBudget(rows, 40), listPaneWidth)
	for line := range strings.SplitSeq(list, "\n") {
		if w := ansi.StringWidth(line); w > listPaneWidth {
			t.Errorf("renderList line width %d exceeds the pane width %d: %q", w, listPaneWidth, line)
		}
	}
}

// TestBrowserEnterPushesReadingForSelection pins the browser → reading
// drill-in (spec §4): enter on the selected row produces a PushScreenMsg
// carrying a *Reading for exactly that memory, built over the browser's OWN
// link index (shared, never rebuilt) and its Render/ReadBody/Styles deps.
func TestBrowserEnterPushesReadingForSelection(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	writeBrowserFile(t, dir, "alpha.md", "---\nname: Alpha\ndescription: a\n---\n", base.Add(time.Hour))
	writeBrowserFile(t, dir, "zulu.md", "---\nname: Zulu\ndescription: z\n---\n", base)

	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	bodies := map[string]string{
		"claude/alpha.md": "alpha body [[zulu]]\n",
		"claude/zulu.md":  "zulu body\n",
	}
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      base,
		ReadBody: fakeReadBody(bodies),
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
		Render:   func(md string, _ int) string { return "RENDERED:" + md },
	})

	// Newest-first default order puts Alpha on top; move to Zulu to prove
	// the push tracks the cursor, not just the first row.
	next, _ := browser.Update(key("down"))
	browser = next.(*Browser)
	_, cmd := browser.Update(key("enter"))
	if cmd == nil {
		t.Fatal("enter on a selected row produced no Cmd")
	}
	push, ok := cmd().(PushScreenMsg)
	if !ok {
		t.Fatalf("enter produced %#v, want PushScreenMsg", cmd())
	}
	reading, ok := push.Screen.(*Reading)
	if !ok {
		t.Fatalf("pushed screen is %T, want *Reading", push.Screen)
	}
	if reading.deps.Memory.RepoPath != "claude/zulu.md" {
		t.Errorf("pushed reading is for %q, want the selected %q", reading.deps.Memory.RepoPath, "claude/zulu.md")
	}
	if reading.deps.Index != browser.index {
		t.Error("pushed reading rebuilt the link index instead of sharing the browser's")
	}
	if got := plain(reading.View(120, 30)); !strings.Contains(got, "RENDERED:zulu body") {
		t.Errorf("pushed reading did not thread the browser's Render seam; got:\n%s", got)
	}
}

// TestBrowserEnterWhileFilteringPushesReading pins that the filter's input
// focus does not swallow enter: filtering down to one row and pressing
// enter opens it — esc (which clears the filter wholesale) must never be
// the only path from a filtered row to reading it.
func TestBrowserEnterWhileFilteringPushesReading(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	writeBrowserFile(t, dir, "auth.md", "---\nname: Auth Service\ndescription: tokens\n---\n", time.Now())
	writeBrowserFile(t, dir, "logging.md", "---\nname: Logging\ndescription: shipping\n---\n", time.Now())

	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: fakeReadBody(nil),
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
	})

	next, _ := browser.Update(key("/"))
	browser = next.(*Browser)
	for _, r := range "auth" {
		next, _ = browser.Update(key(string(r)))
		browser = next.(*Browser)
	}

	_, cmd := browser.Update(key("enter"))
	if cmd == nil {
		t.Fatal("enter while filtering produced no Cmd")
	}
	push, ok := cmd().(PushScreenMsg)
	if !ok {
		t.Fatalf("enter while filtering produced %#v, want PushScreenMsg", cmd())
	}
	reading, ok := push.Screen.(*Reading)
	if !ok {
		t.Fatalf("pushed screen is %T, want *Reading", push.Screen)
	}
	if reading.deps.Memory.Name != "Auth Service" {
		t.Errorf("pushed reading is for %q, want the filtered selection %q", reading.deps.Memory.Name, "Auth Service")
	}
}

// TestBrowserEnterOnEmptyListIsInert covers enter with nothing to open — an
// empty project, or a filter that matches nothing — which must do nothing
// rather than push a reading view over a memory that does not exist.
func TestBrowserEnterOnEmptyListIsInert(t *testing.T) {
	t.Parallel()
	browser := NewBrowser(BrowserDeps{
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: fakeReadBody(nil),
		List:     func() ([]memoryfs.Memory, error) { return nil, nil },
	})
	if _, cmd := browser.Update(key("enter")); cmd != nil {
		t.Fatalf("enter on an empty browser produced a message: %#v", cmd())
	}
}

// TestBrowserTitleIsFolder pins the breadcrumb segment contract.
func TestBrowserTitleIsFolder(t *testing.T) {
	t.Parallel()
	browser := NewBrowser(BrowserDeps{
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: fakeReadBody(nil),
		List:     func() ([]memoryfs.Memory, error) { return nil, nil },
	})
	if got := browser.Title(); got != "acme" {
		t.Errorf("Title() = %q, want %q", got, "acme")
	}
}

// TestBrowserFlowKeysEmitRequests pins the browser's half of the edit flow
// (spec §5): e/r/d emit their request message carrying the memory under the
// cursor — proved on the SECOND row, so the emission tracks the cursor, not
// just the top — and n emits the new-request with the folder, its units, and
// the cursor row's provider as the placement hint. The keys only emit; the
// root owns every gate (class, editor, session) and every modal.
func TestBrowserFlowKeysEmitRequests(t *testing.T) {
	t.Parallel()
	newFlowBrowser := func(t *testing.T) (*Browser, []api.UnitInfo) {
		t.Helper()
		registry := browserFixtureRegistry(t)
		dir := t.TempDir()
		base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
		writeBrowserFile(t, dir, "alpha.md", "---\nname: Alpha\n---\n", base.Add(time.Hour))
		writeBrowserFile(t, dir, "zulu.md", "---\nname: Zulu\n---\n", base)
		units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
		browser := NewBrowser(BrowserDeps{
			Registry: registry,
			Units:    units,
			Folder:   "acme",
			Now:      base,
			ReadBody: fakeReadBody(nil),
			List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
		})
		// Newest-first default puts Alpha on top; move to Zulu so the
		// emitted memory proves cursor tracking.
		next, _ := browser.Update(key("down"))
		return next.(*Browser), units
	}
	selectedRepoPath := func(t *testing.T, msg tea.Msg) string {
		t.Helper()
		switch request := msg.(type) {
		case EditRequestMsg:
			return request.Memory.RepoPath
		case RenameRequestMsg:
			return request.Memory.RepoPath
		case DeleteRequestMsg:
			return request.Memory.RepoPath
		default:
			t.Fatalf("unexpected message %#v", msg)
			return ""
		}
	}

	for _, keyName := range []string{"e", "r", "d"} {
		t.Run(keyName+" carries the selected memory", func(t *testing.T) {
			t.Parallel()
			browser, _ := newFlowBrowser(t)
			_, cmd := browser.Update(key(keyName))
			if cmd == nil {
				t.Fatalf("%s on a selected row produced no Cmd", keyName)
			}
			if got := selectedRepoPath(t, cmd()); got != "claude/zulu.md" {
				t.Errorf("%s emitted for %q, want the selected %q", keyName, got, "claude/zulu.md")
			}
		})
	}

	t.Run("n carries folder, units, and the selection's provider", func(t *testing.T) {
		t.Parallel()
		browser, units := newFlowBrowser(t)
		_, cmd := browser.Update(key("n"))
		if cmd == nil {
			t.Fatal("n produced no Cmd")
		}
		request, ok := cmd().(NewRequestMsg)
		if !ok {
			t.Fatalf("n produced %#v, want NewRequestMsg", cmd())
		}
		if request.Folder != "acme" || len(request.Units) != len(units) || request.Provider != "claude" {
			t.Errorf("NewRequestMsg = %+v, want folder acme, the browser's units, provider claude", request)
		}
	})

	t.Run("e r d inert on an empty browser, n still emits", func(t *testing.T) {
		t.Parallel()
		browser := NewBrowser(BrowserDeps{
			Folder:   "acme",
			Now:      time.Now(),
			ReadBody: fakeReadBody(nil),
			List:     func() ([]memoryfs.Memory, error) { return nil, nil },
		})
		for _, keyName := range []string{"e", "r", "d"} {
			if _, cmd := browser.Update(key(keyName)); cmd != nil {
				t.Errorf("%s on an empty browser produced a message: %#v", keyName, cmd())
			}
		}
		_, cmd := browser.Update(key("n"))
		if cmd == nil {
			t.Fatal("n on an empty browser produced no Cmd; creating the first memory must work")
		}
		request, ok := cmd().(NewRequestMsg)
		if !ok || request.Provider != "" {
			t.Errorf("n emitted %#v, want NewRequestMsg with an empty provider hint", cmd())
		}
	})

	t.Run("flow keys stay typable while filtering", func(t *testing.T) {
		t.Parallel()
		browser, _ := newFlowBrowser(t)
		next, _ := browser.Update(key("/"))
		browser = next.(*Browser)
		for _, r := range "end" {
			var cmd tea.Cmd
			next, cmd = browser.Update(key(string(r)))
			browser = next.(*Browser)
			if cmd == nil {
				continue // textinput may or may not return a Cmd per keystroke
			}
			switch msg := cmd().(type) {
			case EditRequestMsg, NewRequestMsg, RenameRequestMsg, DeleteRequestMsg:
				t.Fatalf("typing %q into the filter emitted a flow request: %#v", r, msg)
			}
		}
		if got := browser.filter.Value(); got != "end" {
			t.Errorf("filter value = %q, want %q — e/n/d must reach the input as literal characters", got, "end")
		}
	})
}

// TestBrowserCopyEmitsSelectedBody pins y: the browser reads the selected
// memory's body through ReadBody and emits CopyMemoryMsg carrying that raw body
// and the memory's name — proved on the SECOND row so the emission tracks the
// cursor, not just the top. The root turns the message into the OSC52 clipboard
// write (dashboard_test.go's TestCopyMemoryMsgToastsAndWritesClipboard).
func TestBrowserCopyEmitsSelectedBody(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	writeBrowserFile(t, dir, "alpha.md", "---\nname: Alpha\n---\n", base.Add(time.Hour))
	writeBrowserFile(t, dir, "zulu.md", "---\nname: Zulu\n---\n", base)
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	const zuluBody = "# Zulu\n\nthe selected memory's raw [[body]]\n"
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      base,
		ReadBody: fakeReadBody(map[string]string{"claude/zulu.md": zuluBody}),
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
	})
	// Newest-first puts Alpha on top; move to Zulu so the emitted body proves
	// cursor tracking rather than a hardcoded top row.
	next, _ := browser.Update(key("down"))
	browser = next.(*Browser)

	_, cmd := browser.Update(key("y"))
	if cmd == nil {
		t.Fatal("y on a selected row produced no Cmd")
	}
	copyMemory, ok := cmd().(CopyMemoryMsg)
	if !ok {
		t.Fatalf("y produced %#v, want CopyMemoryMsg", cmd())
	}
	if copyMemory.Body != zuluBody {
		t.Errorf("CopyMemoryMsg.Body = %q, want the selected memory's raw body %q", copyMemory.Body, zuluBody)
	}
	if copyMemory.Label != "Zulu" {
		t.Errorf("CopyMemoryMsg.Label = %q, want the selected %q", copyMemory.Label, "Zulu")
	}
}

// TestBrowserCopyReadErrorToasts pins the copy path's failure arm — the
// asymmetry with the reading view's in-memory copy. The browser's body is not
// resident, so y reads it through fallible I/O (a file deleted or made
// unreadable since the listing); a read error must surface an error toast, never
// emit a CopyMemoryMsg that would copy an empty body to the clipboard silently.
func TestBrowserCopyReadErrorToasts(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	writeBrowserFile(t, dir, "alpha.md", "---\nname: Alpha\n---\n", base)
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	readErr := errors.New("permission denied")
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      base,
		ReadBody: func(memoryfs.Memory) (string, error) { return "", readErr },
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
	})

	_, cmd := browser.Update(key("y"))
	if cmd == nil {
		t.Fatal("y produced no Cmd on a read error")
	}
	switch msg := cmd().(type) {
	case ToastMsg:
		if !strings.Contains(msg.Text, "copy failed") {
			t.Errorf("error toast = %q, want it to name the copy failure", msg.Text)
		}
	case CopyMemoryMsg:
		t.Fatalf("y emitted CopyMemoryMsg %#v despite the read error — an unreadable memory must not copy silently", msg)
	default:
		t.Fatalf("y produced %#v, want a ToastMsg on a read error", msg)
	}
}

// TestBrowserSelectedTracksCursor pins the root-facing selection accessor
// the availability gates read.
func TestBrowserSelectedTracksCursor(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	writeBrowserFile(t, dir, "alpha.md", "---\nname: Alpha\n---\n", base.Add(time.Hour))
	writeBrowserFile(t, dir, "zulu.md", "---\nname: Zulu\n---\n", base)
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      base,
		ReadBody: fakeReadBody(nil),
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
	})

	selected, ok := browser.Selected()
	if !ok || selected.RepoPath != "claude/alpha.md" {
		t.Errorf("Selected() = %+v, %v; want the top (newest) row alpha", selected, ok)
	}
	next, _ := browser.Update(key("down"))
	browser = next.(*Browser)
	selected, ok = browser.Selected()
	if !ok || selected.RepoPath != "claude/zulu.md" {
		t.Errorf("Selected() after down = %+v, %v; want zulu", selected, ok)
	}

	empty := NewBrowser(BrowserDeps{
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: fakeReadBody(nil),
		List:     func() ([]memoryfs.Memory, error) { return nil, nil },
	})
	if _, ok := empty.Selected(); ok {
		t.Error("Selected() on an empty browser reported a selection")
	}
}

// TestBrowserHistoryKeyPushesHistoryForSelection pins spec §6's h: it opens
// the selected memory's version-history screen, keyed to that memory's repo
// path and threaded with the browser's own Data seam.
func TestBrowserHistoryKeyPushesHistoryForSelection(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	writeBrowserFile(t, dir, "alpha.md", "---\nname: Alpha\n---\n", time.Now())
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: fakeReadBody(nil),
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
		Data:     &fakeHistoryData{},
	})

	_, cmd := browser.Update(key("h"))
	if cmd == nil {
		t.Fatal("h on a selected row produced no Cmd")
	}
	push, ok := cmd().(PushScreenMsg)
	if !ok {
		t.Fatalf("h produced %#v, want PushScreenMsg", cmd())
	}
	history, ok := push.Screen.(*History)
	if !ok {
		t.Fatalf("pushed screen is %T, want *History", push.Screen)
	}
	if folder, repoPath := history.Target(); folder != "acme" || repoPath != "claude/alpha.md" {
		t.Errorf("pushed history targets %s/%s, want acme/claude/alpha.md", folder, repoPath)
	}
}

// TestBrowserShowDeletedRecoversDeletedMemories pins spec §6's x: the folder-
// wide history scan (path "") surfaces every path some version touched minus
// the paths HEAD still has on disk, and enter on one opens its History screen
// as the deleted variant (no live snapshot — the title is the path's base).
func TestBrowserShowDeletedRecoversDeletedMemories(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	writeBrowserFile(t, dir, "alive.md", "---\nname: Alive\n---\n", time.Now())
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	when := time.Now()
	fake := &fakeHistoryData{historyResp: api.HistoryResponse{Versions: []api.HistoryVersion{
		{Rev: "v1", Timestamp: &when, Paths: []string{"claude/alive.md", "claude/gone.md"}},
		{Rev: "v2", Timestamp: &when, Paths: []string{"claude/gone.md"}},
	}}}
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: fakeReadBody(nil),
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
		Data:     fake,
	})

	next, cmd := browser.Update(key("x"))
	browser = next.(*Browser)
	if cmd == nil {
		t.Fatal("x produced no folder-wide scan Cmd")
	}
	// Draining runs the scan Cmd (recording the call) and yields the result
	// message the browser then adopts.
	for _, msg := range drain(cmd) {
		next, _ := browser.Update(msg)
		browser = next.(*Browser)
	}
	if len(fake.historyCalls) != 1 || fake.historyCalls[0].path != "" {
		t.Fatalf("deleted scan = %+v, want exactly one folder-wide (path \"\") call", fake.historyCalls)
	}

	got := plain(browser.View(120, 30))
	if !strings.Contains(got, "claude/gone.md") {
		t.Errorf("deleted view missing the recoverable path; got:\n%s", got)
	}
	if strings.Contains(got, "claude/alive.md") {
		t.Errorf("deleted view listed an on-disk memory; got:\n%s", got)
	}

	_, openCmd := browser.Update(key("enter"))
	if openCmd == nil {
		t.Fatal("enter on a deleted row produced no Cmd")
	}
	push, ok := openCmd().(PushScreenMsg)
	if !ok {
		t.Fatalf("enter produced %#v, want PushScreenMsg", openCmd())
	}
	history, ok := push.Screen.(*History)
	if !ok {
		t.Fatalf("pushed screen is %T, want *History", push.Screen)
	}
	if _, repoPath := history.Target(); repoPath != "claude/gone.md" {
		t.Errorf("pushed history targets %q, want the deleted %q", repoPath, "claude/gone.md")
	}
	if title := history.Title(); title != "gone.md" {
		t.Errorf("deleted history Title() = %q, want the path base %q", title, "gone.md")
	}
}

// TestBrowserShowDeletedEscReturnsToBrowser pins the esc-consumes-internal-
// state-first rule for the deleted list: esc leaves it for the normal browser
// rather than popping the browser off the stack.
func TestBrowserShowDeletedEscReturnsToBrowser(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	writeBrowserFile(t, dir, "alive.md", "---\nname: Alive\n---\n", time.Now())
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: fakeReadBody(nil),
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
		Data:     &fakeHistoryData{},
	})

	next, _ := browser.Update(key("x"))
	browser = next.(*Browser)
	nextScreen, cmd := browser.Update(key("esc"))
	browser = nextScreen.(*Browser)
	if cmd != nil {
		if _, isPop := cmd().(PopScreenMsg); isPop {
			t.Fatal("esc in the deleted list popped the browser instead of returning to it")
		}
	}
	if got := plain(browser.View(120, 30)); !strings.Contains(got, "Memory browser:") {
		t.Errorf("esc did not return to the normal browser; got:\n%s", got)
	}
}

// driveDeletedScan toggles the deleted view on and drains the folder-wide scan
// Cmd back through the browser — the shared setup for the deleted-mode tests.
func driveDeletedScan(t *testing.T, browser *Browser) *Browser {
	t.Helper()
	next, cmd := browser.Update(key("x"))
	browser = next.(*Browser)
	for _, msg := range drain(cmd) {
		next, _ := browser.Update(msg)
		browser = next.(*Browser)
	}
	return browser
}

// TestBrowserDeletedViewDisclosesTruncation pins the deleted-mode half of the
// silent-cap disclosure in BOTH directions: a folder-wide scan that came back at
// exactly historyVersionLimit discloses that older history was not read (and its
// empty state drops the unqualified whole-history claim), while a scan under the
// cap discloses nothing — a below-limit scan read the whole timeline, so an
// "older history not scanned" claim over it would be the inverse dishonesty.
func TestBrowserDeletedViewDisclosesTruncation(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	writeBrowserFile(t, dir, "alive.md", "---\nname: Alive\n---\n", time.Now())
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	when := time.Now()

	cappedScan := func(paths []string) *fakeHistoryData {
		versions := make([]api.HistoryVersion, historyVersionLimit)
		for i := range versions {
			versions[i] = api.HistoryVersion{Rev: fmt.Sprintf("r%03d", i), Timestamp: &when, Paths: paths}
		}
		return &fakeHistoryData{historyResp: api.HistoryResponse{Versions: versions}}
	}
	newDeletedBrowser := func(t *testing.T, fake *fakeHistoryData) *Browser {
		t.Helper()
		browser := NewBrowser(BrowserDeps{
			Registry: registry, Units: units, Folder: "acme", Now: when,
			ReadBody: fakeReadBody(nil),
			List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
			Data:     fake,
		})
		return driveDeletedScan(t, browser)
	}

	t.Run("capped scan with deleted rows discloses", func(t *testing.T) {
		t.Parallel()
		browser := newDeletedBrowser(t, cappedScan([]string{"claude/gone.md"}))
		got := plain(browser.View(120, 30))
		if !strings.Contains(got, "claude/gone.md") {
			t.Fatalf("deleted row missing; got:\n%s", got)
		}
		if !strings.Contains(got, "older history not scanned") {
			t.Errorf("capped deleted scan did not disclose truncation; got:\n%s", got)
		}
	})

	t.Run("capped scan with zero deleted qualifies the empty state", func(t *testing.T) {
		t.Parallel()
		browser := newDeletedBrowser(t, cappedScan([]string{"claude/alive.md"})) // only the on-disk file
		got := plain(browser.View(120, 30))
		if strings.Contains(got, "in this project's history") {
			t.Errorf("empty state made an unqualified whole-history claim after a capped scan; got:\n%s", got)
		}
		if !strings.Contains(got, "older history not scanned") {
			t.Errorf("capped empty state did not qualify the scan bound; got:\n%s", got)
		}
	})

	t.Run("below-limit scan with deleted rows does not disclose", func(t *testing.T) {
		t.Parallel()
		// A scan well under the cap read the whole timeline — nothing older went
		// unscanned — so even with a deleted row present, the disclosure must NOT
		// render. Without this negative pin, hoisting the notice out of the
		// truncated branch (or the flag regressing to always-true) would claim
		// "older history not scanned" over a 3-commit repo and stay green.
		belowLimit := &fakeHistoryData{historyResp: api.HistoryResponse{Versions: []api.HistoryVersion{
			{Rev: "r000", Timestamp: &when, Paths: []string{"claude/gone.md"}},
		}}}
		browser := newDeletedBrowser(t, belowLimit)
		got := plain(browser.View(120, 30))
		if !strings.Contains(got, "claude/gone.md") {
			t.Fatalf("deleted row missing; got:\n%s", got)
		}
		if strings.Contains(got, "older history not scanned") {
			t.Errorf("a below-limit deleted scan wrongly disclosed truncation; got:\n%s", got)
		}
	})
}

// TestBrowserDeletedListRefreshesAfterRestore pins the deleted-mode staleness
// fix: once a deleted memory is restored (it reappears on disk), a RefreshMsg
// re-subtracts the on-disk set and drops it from the deleted list within a tick
// — without an x-toggle rescan.
func TestBrowserDeletedListRefreshesAfterRestore(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	writeBrowserFile(t, dir, "alive.md", "---\nname: Alive\n---\n", time.Now())
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	when := time.Now()
	fake := &fakeHistoryData{historyResp: api.HistoryResponse{Versions: []api.HistoryVersion{
		{Rev: "v1", Timestamp: &when, Paths: []string{"claude/alive.md", "claude/gone.md"}},
	}}}
	browser := NewBrowser(BrowserDeps{
		Registry: registry, Units: units, Folder: "acme", Now: when,
		ReadBody: fakeReadBody(nil),
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
		Data:     fake,
	})
	browser = driveDeletedScan(t, browser)
	if got := plain(browser.View(120, 30)); !strings.Contains(got, "claude/gone.md") {
		t.Fatalf("setup: deleted view missing gone.md; got:\n%s", got)
	}

	// The deleted memory is restored on disk; a RefreshMsg re-lists and must drop
	// it from the deleted set without a rescan.
	writeBrowserFile(t, dir, "gone.md", "---\nname: Gone\n---\n", when)
	next, _ := browser.Update(RefreshMsg{Now: when})
	browser = next.(*Browser)
	if got := plain(browser.View(120, 30)); strings.Contains(got, "claude/gone.md") {
		t.Errorf("restored memory still listed as deleted after a refresh; got:\n%s", got)
	}
}

// TestBrowserDeletedHistoryReadsLiveContent pins the deleted-history live seam:
// opening a deleted memory's history binds its diff-vs-live to the CURRENT file
// on disk (mapped via the same LocalTarget restore uses), so a memory restored
// while still listed diffs against its real content — not a frozen empty side.
func TestBrowserDeletedHistoryReadsLiveContent(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	writeBrowserFile(t, dir, "alive.md", "---\nname: Alive\n---\n", time.Now())
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	when := time.Now()
	fake := &fakeHistoryData{historyResp: api.HistoryResponse{Versions: []api.HistoryVersion{
		{Rev: "v1", Timestamp: &when, Paths: []string{"claude/gone.md"}},
	}}}
	browser := NewBrowser(BrowserDeps{
		Registry: registry, Units: units, Folder: "acme", Now: when,
		ReadBody: fakeReadBody(nil),
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
		Data:     fake,
	})
	browser = driveDeletedScan(t, browser)

	// The deleted memory is restored on disk while its row is still listed.
	writeBrowserFile(t, dir, "gone.md", "restored on disk\n", when)
	_, openCmd := browser.Update(key("enter"))
	push, ok := openCmd().(PushScreenMsg)
	if !ok {
		t.Fatalf("enter produced %#v, want PushScreenMsg", openCmd())
	}
	history := push.Screen.(*History)
	live, err := history.deps.Live()
	if err != nil {
		t.Fatalf("deleted history Live() errored: %v", err)
	}
	if !strings.Contains(live, "restored on disk") {
		t.Errorf("deleted history Live() = %q, want the on-disk restored content", live)
	}
}

// TestBrowserIgnoresInsightsDataMsg pins the distinct-message-type guard from the
// browser's side: a deleted-scanning browser awaits a folder-wide
// HistoryVersionsMsg (path ""), and insights' InsightsDataMsg rides the same path
// "", so only the message type keeps the wires apart. An InsightsDataMsg must
// never satisfy the pending deleted scan.
func TestBrowserIgnoresInsightsDataMsg(t *testing.T) {
	t.Parallel()
	browser := NewBrowser(BrowserDeps{
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: func(memoryfs.Memory) (string, error) { return "", nil },
		List:     func() ([]memoryfs.Memory, error) { return nil, nil },
		Data:     &fakeHistoryData{},
	})
	browser.showDeleted = true // deleted-scan mode, awaiting its folder-wide result

	next, _ := browser.Update(InsightsDataMsg{
		Folder:   "acme",
		Versions: []api.HistoryVersion{{Rev: "r1", Paths: []string{"claude/one.md"}}},
	})
	if next.(*Browser).deletedLoaded {
		t.Error("browser adopted an InsightsDataMsg as its deleted scan; the distinct-type guard failed")
	}
}

// TestBrowserIndexMemorySortsFirst pins the index-first order: within each provider group the
// provider's primary index memory (marked IsIndex at enumeration) sorts ahead of
// every fact memory, in BOTH the recency and the name order o toggles between,
// so it is always the default-selected first memory to open. The fixture buries
// the index under either order without the fix — its ModTime is the OLDEST in
// its group (recency would sink it) and its name sorts after a sibling (name
// order would sink it) — and gives both providers an index so grouping (not a
// single global pin) is what the ordering respects. The codex index carries the
// realistic Class ClassRegenerated (its consolidator owns it), not a derived-index
// class, so its sorting first proves the order keys on IsIndex — a display fact —
// not on any merge class. A filter that excludes the index simply yields the
// matching fact rows: no crash, nothing synthetic.
func TestBrowserIndexMemorySortsFirst(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	memories := []memoryfs.Memory{
		{Provider: "claude", RepoPath: "claude/alpha.md", Name: "alpha", Class: provider.ClassFact, ModTime: base.Add(2 * time.Hour)},
		{Provider: "claude", RepoPath: "claude/zulu.md", Name: "zulu", Class: provider.ClassFact, ModTime: base.Add(time.Hour)},
		{Provider: "claude", RepoPath: "claude/MEMORY.md", Name: "MEMORY", Class: provider.ClassDerivedIndex, IsIndex: true, ModTime: base},
		{Provider: "codex", RepoPath: "codex/beta.md", Name: "beta", Class: provider.ClassFact, ModTime: base.Add(2 * time.Hour)},
		{Provider: "codex", RepoPath: "codex/yankee.md", Name: "yankee", Class: provider.ClassFact, ModTime: base.Add(time.Hour)},
		{Provider: "codex", RepoPath: "codex/memories/MEMORY.md", Name: "MEMORY", Class: provider.ClassRegenerated, IsIndex: true, ModTime: base},
	}
	browser := NewBrowser(BrowserDeps{
		Folder:   "acme",
		Now:      base,
		ReadBody: fakeReadBody(nil),
		List:     func() ([]memoryfs.Memory, error) { return append([]memoryfs.Memory(nil), memories...), nil },
	})

	assertIndexFirstPerGroup := func(t *testing.T, mode string) {
		t.Helper()
		lastProvider := ""
		for row, memory := range browser.visibleRows() {
			if memory.Provider == lastProvider {
				continue
			}
			lastProvider = memory.Provider
			if !memory.IsIndex {
				t.Errorf("%s order: provider %q group starts at row %d with %q (IsIndex=%v, class %v), want the provider index first",
					mode, memory.Provider, row, memory.Name, memory.IsIndex, memory.Class)
			}
		}
	}

	assertIndexFirstPerGroup(t, "recency") // orderByRecency defaults true
	next, _ := browser.Update(key("o"))
	browser = next.(*Browser)
	assertIndexFirstPerGroup(t, "name")

	browser.filter.SetValue("beta") // excludes both indexes
	if rows := browser.visibleRows(); len(rows) != 1 || rows[0].Name != "beta" {
		t.Fatalf("filter excluding the index: want exactly [beta], got %d rows", len(rows))
	}
}

// ctrlKey builds a ctrl-modified key press. The shared key() helper only builds
// printable runes and a handful of named specials, so the preview pane's
// ctrl+d/ctrl+u scroll keys are constructed here (msg.String() reports them as
// "ctrl+d"/"ctrl+u", which the preview viewport's keymap matches).
func ctrlKey(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl}
}

// previewOverflowRows is a body line count certain to overflow any preview pane
// a test renders — the adversarial length the preview scroll and focus tests
// need so the pane genuinely scrolls rather than showing the whole body at once.
const previewOverflowRows = 300

// numberedRows builds a previewOverflowRows-long body of uniquely labelled lines
// ("<prefix>row 001" …) so a scroll assertion can tell one preview window from
// another by which rows it contains. A non-empty prefix distinguishes two
// memories' bodies.
func numberedRows(prefix string) string {
	var b strings.Builder
	for i := 1; i <= previewOverflowRows; i++ {
		fmt.Fprintf(&b, "%srow %03d\n", prefix, i)
	}
	return b.String()
}

// longBodyBrowser is a one-memory browser whose sole memory has a 300-line body
// — an adversarial length certain to overflow any real preview pane. Render is
// nil, so the preview is the raw body: unique, unwrapped lines a scroll test can
// track exactly.
func longBodyBrowser(t *testing.T) *Browser {
	t.Helper()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	memories := []memoryfs.Memory{
		{Provider: "claude", RepoPath: "claude/long.md", Name: "long", Class: provider.ClassFact, ModTime: base},
	}
	return NewBrowser(BrowserDeps{
		Folder:   "acme",
		Now:      base,
		ReadBody: fakeReadBody(map[string]string{"claude/long.md": numberedRows("")}),
		List:     func() ([]memoryfs.Memory, error) { return append([]memoryfs.Memory(nil), memories...), nil },
	})
}

// TestBrowserPreviewHeightBounded pins the pane's core height contract: at a preview-split
// width, a long-body selection must not push the frame past its height budget
// (in the real hub that overflow shoves the root's footer — the "option keys" —
// off the terminal and hides the text past the fold). The frame must fit height
// AND still carry its chrome, the head of the body, and the overflow hint —
// without the hint a clipped body is indistinguishable from a complete one, so
// the rest of the memory would look unreachable.
func TestBrowserPreviewHeightBounded(t *testing.T) {
	t.Parallel()
	browser := longBodyBrowser(t)
	const width, height = 120, 30
	got := plain(browser.View(width, height))
	if lines := strings.Count(got, "\n") + 1; lines > height {
		t.Errorf("preview-split View rendered %d lines, want <= %d; a long preview must not overflow its height budget\n%s", lines, height, got)
	}
	if !strings.Contains(got, "Memory browser") {
		t.Errorf("View lost its title chrome under the long preview; got:\n%s", got)
	}
	if !strings.Contains(got, "row 001") {
		t.Errorf("View did not show the head of the previewed body; got:\n%s", got)
	}
	if !strings.Contains(got, "ctrl+d/u pgup/pgdn scroll") {
		t.Errorf("overflowing preview carries no scroll hint; got:\n%s", got)
	}
}

// TestBrowserPreviewScrolls pins that the preview-pane scroll keys move the
// visible window in BOTH directions: ctrl+d (half page) and pgdown (full page)
// each scroll the top of the body out of view, and their counterparts ctrl+u
// and pgup bring it back — the footer advertises the pairs together, so a dead
// up key would leave the user stranded below the fold with no advertised way
// back. The list keeps focus — j/k stay the list cursor — so these keys are
// the only way to reach the rest of a long memory.
func TestBrowserPreviewScrolls(t *testing.T) {
	t.Parallel()
	const width, height = 120, 30
	for _, testCase := range []struct {
		name string
		msg  tea.KeyPressMsg
	}{
		{"ctrl+d half page", ctrlKey('d')},
		{"pgdown full page", tea.KeyPressMsg{Code: tea.KeyPgDown}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			browser := longBodyBrowser(t)
			before := plain(browser.View(width, height)) // renders once so the pane is live
			if !strings.Contains(before, "row 001") {
				t.Fatalf("setup: top of body not visible before scrolling; got:\n%s", before)
			}
			next, _ := browser.Update(testCase.msg)
			browser = next.(*Browser)
			after := plain(browser.View(width, height))
			if after == before {
				t.Fatalf("%s left the preview window unchanged", testCase.name)
			}
			if strings.Contains(after, "row 001") {
				t.Errorf("%s did not scroll the top of the body out of view; got:\n%s", testCase.name, after)
			}
		})
	}
	for _, testCase := range []struct {
		name string
		down tea.KeyPressMsg
		up   tea.KeyPressMsg
	}{
		{"ctrl+u restores after ctrl+d", ctrlKey('d'), ctrlKey('u')},
		{"pgup restores after pgdown", tea.KeyPressMsg{Code: tea.KeyPgDown}, tea.KeyPressMsg{Code: tea.KeyPgUp}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			browser := longBodyBrowser(t)
			if before := plain(browser.View(width, height)); !strings.Contains(before, "row 001") {
				t.Fatalf("setup: top of body not visible before scrolling; got:\n%s", before)
			}
			next, _ := browser.Update(testCase.down)
			browser = next.(*Browser)
			if mid := plain(browser.View(width, height)); strings.Contains(mid, "row 001") {
				t.Fatalf("setup: the down key did not scroll the top out of view; got:\n%s", mid)
			}
			next, _ = browser.Update(testCase.up)
			browser = next.(*Browser)
			if after := plain(browser.View(width, height)); !strings.Contains(after, "row 001") {
				t.Errorf("the up key did not bring the top of the body back; got:\n%s", after)
			}
		})
	}
}

// TestBrowserPreviewHeaderLines pins previewHeaderLines' own contract in
// isolation (its wiring into renderPreviewPane is covered by the View-level
// tests below): a memory with no lint issues gets exactly the one
// unconditional alignment blank and nothing else, while a flagged memory gets
// that same blank followed by one Warn-styled "⚠ <Rule>: <Detail>" line per
// issue. lintIssues is poked directly after construction, the same
// no-relint-can-rebuild-it precedent TestBrowserRenderListFitsPaneWidthDirectly
// already uses for lintFlags.
func TestBrowserPreviewHeaderLines(t *testing.T) {
	t.Parallel()
	styles := theme.Default(true)
	browser := NewBrowser(BrowserDeps{
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: fakeReadBody(nil),
		List:     func() ([]memoryfs.Memory, error) { return nil, nil },
		Styles:   styles,
	})
	browser.lintIssues = map[string][]lint.Issue{
		"claude/flagged.md": {{Rule: "dangling-link", Detail: "[[Ghost]] resolves to no memory in this project"}},
	}

	testCases := []struct {
		name     string
		repoPath string
		want     []string
	}{
		{
			name:     "unflagged memory pads without any warn line",
			repoPath: "claude/clean.md",
			want:     []string{""},
		},
		{
			name:     "flagged memory carries a warn-styled reason after the padding",
			repoPath: "claude/flagged.md",
			want: []string{
				"",
				styles.Warn.Render("⚠ dangling-link: [[Ghost]] resolves to no memory in this project"),
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got := browser.previewHeaderLines(memoryfs.Memory{RepoPath: testCase.repoPath}, 80)
			if diff := cmp.Diff(testCase.want, got); diff != "" {
				t.Errorf("previewHeaderLines(%q) mismatch (-want +got):\n%s", testCase.repoPath, diff)
			}
		})
	}
}

// TestBrowserPreviewShowsLintReasonAboveBody pins the full wiring — refresh's
// real lint.Check output, carried through renderPreviewPane — not just
// previewHeaderLines in isolation: a hovered memory with a real lint finding
// shows the issue's Detail sentence, Warn-styled, above the rendered body.
// This is the live-hub complaint verbatim: a ⚠ badge with no way to learn what
// it meant.
func TestBrowserPreviewShowsLintReasonAboveBody(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	writeBrowserFile(t, dir, "flagged.md", "---\nname: Flagged\ndescription: a memory with a dangling reference\n---\n", time.Now())

	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	styles := theme.Default(true)
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      time.Now(),
		Styles:   styles,
		ReadBody: fakeReadBody(map[string]string{"claude/flagged.md": "see [[Ghost]] for details\n"}),
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
		Render:   func(md string, _ int) string { return "BODYSTART:" + md },
	})

	const width, height = 150, 30
	raw := browser.View(width, height)
	got := plain(raw)

	const wantDetail = "[[Ghost]] resolves to no memory in this project"
	if !strings.Contains(got, wantDetail) {
		t.Fatalf("preview pane missing the lint issue's Detail sentence; got:\n%s", got)
	}
	wantStyled := styles.Warn.Render("⚠ dangling-link: " + wantDetail)
	if !strings.Contains(raw, wantStyled) {
		t.Errorf("lint reason is not Warn-styled; want substring %q in raw view:\n%s", wantStyled, raw)
	}

	reasonIdx := strings.Index(got, wantDetail)
	bodyIdx := strings.Index(got, "BODYSTART:")
	if bodyIdx == -1 {
		t.Fatalf("preview pane never rendered the body; got:\n%s", got)
	}
	if reasonIdx > bodyIdx {
		t.Errorf("lint reason (offset %d) renders below the body (offset %d), want above; got:\n%s", reasonIdx, bodyIdx, got)
	}
}

// TestBrowserPreviewAlignsWithListFirstRow pins the live-hub alignment
// complaint directly: at fixed geometry, the preview pane's first body line
// and the list column's first row must land on the very same rendered line.
// renderList's window always opens on a provider-header line, never a memory
// row, so the preview's own unconditional blank (previewHeaderLines) is what
// keeps the two columns level rather than the preview starting one row higher
// than the list's first actual memory.
func TestBrowserPreviewAlignsWithListFirstRow(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	writeBrowserFile(t, dir, "solo.md", "---\nname: Solo Memory\ndescription: nothing wrong here\n---\n", time.Now())

	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: fakeReadBody(map[string]string{"claude/solo.md": "a clean body, no links\n"}),
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
		Render:   func(md string, _ int) string { return "BODYMARK\n" + md },
	})

	got := plain(browser.View(150, 30))
	lineIndexOf := func(substr string) int {
		before, _, found := strings.Cut(got, substr)
		if !found {
			t.Fatalf("view missing %q; got:\n%s", substr, got)
		}
		return strings.Count(before, "\n")
	}

	rowLine := lineIndexOf("Solo Memory")
	bodyLine := lineIndexOf("BODYMARK")
	if rowLine != bodyLine {
		t.Errorf("preview body starts on line %d but the list's first row is on line %d (not aligned); got:\n%s", bodyLine, rowLine, got)
	}
}

// TestBrowserPreviewHeaderRespectsHeightBudgetWithIssues pins the budget
// contract with the header zone actually populated: a hovered memory with
// three real lint issues AND an adversarially long body must still fit the
// view inside its height budget, with the overflow hint still reachable — the
// header zone's own line count has to come out of the same budget the
// viewport is bounded to, not be layered on top of it (see renderPreviewPane's
// doc for why chromeLines and the header zone are subtracted separately).
func TestBrowserPreviewHeaderRespectsHeightBudgetWithIssues(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	// An empty (but present) frontmatter block yields two issues on its own —
	// missing name, missing description — isolating the third (dangling-link)
	// from any accidental frontmatter interaction, exactly like
	// TestBrowserLintBadge isolates its own single rule.
	writeBrowserFile(t, dir, "tall.md", "---\n---\n", base)

	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      base,
		ReadBody: fakeReadBody(map[string]string{"claude/tall.md": numberedRows("") + "see [[Ghost]] for details\n"}),
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
	})

	const width, height = 150, 30
	got := plain(browser.View(width, height))
	if lines := strings.Count(got, "\n") + 1; lines > height {
		t.Errorf("view rendered %d lines with 3 header issues present, want <= %d; the header zone must be subtracted from the pane's own budget:\n%s", lines, height, got)
	}
	if !strings.Contains(got, "ctrl+d/u pgup/pgdn scroll") {
		t.Errorf("overflowing preview lost its scroll hint under the header zone; got:\n%s", got)
	}
	for _, want := range []string{
		"⚠ frontmatter: frontmatter missing name",
		"⚠ frontmatter: frontmatter missing description",
		"⚠ dangling-link: [[Ghost]] resolves to no memory in this project",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("view missing issue line %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "⚠ +") {
		t.Errorf("exactly 3 issues must not trigger the overflow line; got:\n%s", got)
	}
}

// TestBrowserPreviewHeaderCapsIssuesWithOverflowLine pins the Produces
// contract's overflow shape: a memory with more than three lint issues shows
// only the first three as named reason lines, collapsing the rest into one
// "⚠ +N more" line rather than growing the header zone — and the height
// budget it eats into — without bound.
func TestBrowserPreviewHeaderCapsIssuesWithOverflowLine(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	writeBrowserFile(t, dir, "many.md", "---\nname: Many Issues\ndescription: a memory with four dangling references\n---\n", time.Now())

	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	body := "see [[Ghost1]], [[Ghost2]], [[Ghost3]] and [[Ghost4]] for details\n"
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: fakeReadBody(map[string]string{"claude/many.md": body}),
		List:     func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
	})

	got := plain(browser.View(150, 30))
	if gotCount, want := strings.Count(got, "⚠ dangling-link:"), 3; gotCount != want {
		t.Errorf("got %d named dangling-link reason lines, want exactly %d (the rest must collapse into an overflow line); view:\n%s", gotCount, want, got)
	}
	if !strings.Contains(got, "⚠ +1 more") {
		t.Errorf("view missing the overflow line for the 4th issue; got:\n%s", got)
	}
}

// TestBrowserPreviewHeaderTruncatesPathologicallyLongReason pins the Produces
// clause "ansi-truncated to pane width" as an actual failing-test guard.
// Every OTHER fixture's Detail sentence fits comfortably inside a realistic
// pane width, so this clause had no test that could ever fail on it — a
// Detail built from a pathologically long, space-free dangling-link target
// (Lint Details embed user content, so this is reachable in practice, not
// contrived) is what actually binds it, two ways:
//
//  1. previewHeaderLines' own returned warn line must still fit width — the
//     truncation's actual point of application.
//  2. Because the preview column is later rendered through a
//     Width(previewWidth).MaxWidth(previewWidth) style at View's own call
//     site, a warn line that escaped that bound would word-wrap into an
//     uncounted extra physical line there — invisible to renderPreviewPane's
//     own line-counting, and silent until it pushes the WHOLE view past its
//     height budget. Paired with a long body (as in
//     TestBrowserPreviewHeaderRespectsHeightBudgetWithIssues) so the budget is
//     already tight enough that one such stray line is not just absorbed by
//     slack — it must actually overflow height for this to be a meaningful
//     regression guard, not a cosmetic one.
func TestBrowserPreviewHeaderTruncatesPathologicallyLongReason(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	writeBrowserFile(t, dir, "verbose.md", "---\nname: Verbose\ndescription: a memory with one pathologically long dangling reference\n---\n", time.Now())

	// A single unbroken token far longer than any realistic pane width: with no
	// spaces of its own, a word-wrap fallback could not hide it behind a tidy
	// line break — a missing truncation shows up as a forced mid-word wrap.
	longTarget := strings.Repeat("Overflow", 40)
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      time.Now(),
		ReadBody: fakeReadBody(map[string]string{
			"claude/verbose.md": numberedRows("") + fmt.Sprintf("see [[%s]] for details\n", longTarget),
		}),
		List: func() ([]memoryfs.Memory, error) { return memoryfs.List(registry, units) },
	})

	const width, height = 150, 30
	const previewWidth = width - listPaneWidth - 2

	rows := browser.visibleRows()
	if len(rows) != 1 {
		t.Fatalf("setup: want exactly one memory, got %d", len(rows))
	}
	headerLines := browser.previewHeaderLines(rows[0], previewWidth)
	if len(headerLines) != 2 {
		t.Fatalf("setup: want the padding line plus exactly one warn line, got %d: %q", len(headerLines), headerLines)
	}
	if w := ansi.StringWidth(headerLines[1]); w > previewWidth {
		t.Errorf("warn line width %d exceeds the pane width %d — truncation did not bind: %q", w, previewWidth, headerLines[1])
	}

	got := plain(browser.View(width, height))
	if lines := strings.Count(got, "\n") + 1; lines > height {
		t.Errorf("view rendered %d lines with a pathologically long reason present, want <= %d; an untruncated reason word-wraps into an uncounted extra line:\n%s", lines, height, got)
	}
}

// TestBrowserPreviewResetsOnSelectionChange pins that moving the list cursor to
// a different memory opens its preview at the head — a new document never
// inherits the previous selection's scroll offset.
func TestBrowserPreviewResetsOnSelectionChange(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	memories := []memoryfs.Memory{
		{Provider: "claude", RepoPath: "claude/aaa.md", Name: "aaa", Class: provider.ClassFact, ModTime: base.Add(time.Hour)}, // newer → row 0
		{Provider: "claude", RepoPath: "claude/bbb.md", Name: "bbb", Class: provider.ClassFact, ModTime: base},                // row 1
	}
	browser := NewBrowser(BrowserDeps{
		Folder: "acme",
		Now:    base,
		ReadBody: fakeReadBody(map[string]string{
			"claude/aaa.md": numberedRows("A "),
			"claude/bbb.md": numberedRows("B "),
		}),
		List: func() ([]memoryfs.Memory, error) { return append([]memoryfs.Memory(nil), memories...), nil },
	})
	const width, height = 120, 30

	_ = browser.View(width, height)
	next, _ := browser.Update(ctrlKey('d')) // scroll A's preview down
	browser = next.(*Browser)
	if scrolled := plain(browser.View(width, height)); strings.Contains(scrolled, "A row 001") {
		t.Fatalf("setup: ctrl+d did not scroll A's preview off its top; got:\n%s", scrolled)
	}

	next, _ = browser.Update(key("down")) // move the list cursor to B
	browser = next.(*Browser)
	if bView := plain(browser.View(width, height)); !strings.Contains(bView, "B row 001") {
		t.Errorf("B's preview did not open at its head after the selection changed; got:\n%s", bView)
	}
}

// TestBrowserPreviewScrollInertWithoutPreview pins that a scroll key is a
// harmless no-op when no preview pane is shown (narrow width): the view is
// unchanged and nothing panics.
func TestBrowserPreviewScrollInertWithoutPreview(t *testing.T) {
	t.Parallel()
	browser := longBodyBrowser(t)
	const width, height = 80, 30 // below previewMinWidth: the list owns the full width, no preview
	before := plain(browser.View(width, height))
	next, _ := browser.Update(ctrlKey('d'))
	browser = next.(*Browser)
	if after := plain(browser.View(width, height)); after != before {
		t.Errorf("ctrl+d changed the narrow (no-preview) view; scroll must be inert without a preview pane\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestBrowserFilteringOwnsKeysOverPreviewScroll pins that the in-browser filter
// keeps input focus: while filtering, a typed key lands in the filter, never
// routed to the preview viewport (updateFiltering runs before any scroll
// routing).
func TestBrowserFilteringOwnsKeysOverPreviewScroll(t *testing.T) {
	t.Parallel()
	browser := longBodyBrowser(t)
	const width, height = 120, 30
	_ = browser.View(width, height)

	next, _ := browser.Update(key("/")) // open the filter
	browser = next.(*Browser)
	next, _ = browser.Update(key("r")) // 'r' is rename outside filtering; here it must type
	browser = next.(*Browser)
	if got := browser.filter.Value(); got != "r" {
		t.Errorf("filter value = %q, want %q; filtering must own typed keys", got, "r")
	}
}

// TestBrowserFilterTypesMouseKeyNotToggle pins the browser-side half of the
// mouse-key-as-query-letter routing: while the filter owns input, m types into the query like every other
// letter action, never routing to the mouse-capture toggle. updateKey reaches
// the normal-mode m match only AFTER the filtering branch has returned, so the
// query keeps its m. A regression that hoisted the m match ahead of the
// `if b.filtering` guard would divert it and leave the filter value empty. This
// is the precise browser-scope twin of the root-level end-to-end filter pin.
func TestBrowserFilterTypesMouseKeyNotToggle(t *testing.T) {
	t.Parallel()
	browser := longBodyBrowser(t)
	const width, height = 120, 30
	_ = browser.View(width, height)

	next, _ := browser.Update(key("/")) // open the filter
	browser = next.(*Browser)
	next, _ = browser.Update(key("m")) // m toggles mouse capture outside filtering; here it must TYPE
	browser = next.(*Browser)
	if got := browser.filter.Value(); got != "m" {
		t.Errorf("filter value = %q, want %q; filtering must own m, not route it to the mouse toggle", got, "m")
	}
}

// focusPreviewBrowser is a two-memory browser whose top (default-selected) row
// carries a 300-line body — long enough to overflow any preview pane — and whose
// second row carries a short one. The two rows make the focus distinction
// load-bearing: while the preview holds focus, j must scroll the pane and leave
// the cursor on row 0; once focus returns to the list, j must advance it to row
// 1. Render is nil, so the preview is the raw, uniquely numbered body a scroll
// assertion can track.
func focusPreviewBrowser(t *testing.T) *Browser {
	t.Helper()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	memories := []memoryfs.Memory{
		{Provider: "claude", RepoPath: "claude/long.md", Name: "long", Class: provider.ClassFact, ModTime: base.Add(time.Hour)}, // newest → row 0
		{Provider: "claude", RepoPath: "claude/short.md", Name: "short", Class: provider.ClassFact, ModTime: base},              // row 1
	}
	return NewBrowser(BrowserDeps{
		Folder: "acme",
		Now:    base,
		ReadBody: fakeReadBody(map[string]string{
			"claude/long.md":  numberedRows(""),
			"claude/short.md": "short body\n",
		}),
		List: func() ([]memoryfs.Memory, error) { return append([]memoryfs.Memory(nil), memories...), nil },
	})
}

// TestBrowserPreviewFocusRoutesScrollKeys pins preview-focus mode (spec §3): tab
// focuses the on-screen preview so j/k then scroll the pane's viewport rather
// than the list cursor, and tab (or esc) hands focus back so j/k move the list
// cursor again. The two-row fixture makes the routing load-bearing: while
// focused the cursor must stay on row 0 (only the viewport's YOffset moves), and
// once blurred a j must advance it to row 1.
func TestBrowserPreviewFocusRoutesScrollKeys(t *testing.T) {
	t.Parallel()
	const width, height = 120, 30
	for _, blurKey := range []string{"tab", "esc"} {
		t.Run("blur with "+blurKey, func(t *testing.T) {
			t.Parallel()
			browser := focusPreviewBrowser(t)
			_ = plain(browser.View(width, height)) // render so the preview is on screen
			if browser.previewViewport.YOffset() != 0 {
				t.Fatalf("setup: preview not at its top; YOffset = %d", browser.previewViewport.YOffset())
			}

			next, _ := browser.Update(key("tab")) // focus the preview
			browser = next.(*Browser)
			if !browser.previewFocused {
				t.Fatal("tab did not focus the preview pane")
			}
			// The focused pane renders its cue so the reader can see the preview,
			// not the list, now owns the scroll keys (spec §3's focus affordance).
			if focused := plain(browser.View(width, height)); !strings.Contains(focused, "preview focused") {
				t.Errorf("focused preview shows no focus cue; got:\n%s", focused)
			}

			next, _ = browser.Update(key("j")) // scrolls the pane, not the list
			browser = next.(*Browser)
			if browser.previewViewport.YOffset() == 0 {
				t.Error("j while focused did not scroll the preview viewport (YOffset still 0)")
			}
			if browser.cursor != 0 {
				t.Errorf("j while focused moved the list cursor to %d; it must scroll only the preview", browser.cursor)
			}
			next, _ = browser.Update(key("k")) // scrolls back toward the top
			browser = next.(*Browser)
			if browser.previewViewport.YOffset() != 0 {
				t.Errorf("k while focused did not scroll the preview back to its top; YOffset = %d", browser.previewViewport.YOffset())
			}

			next, _ = browser.Update(key(blurKey)) // return focus to the list
			browser = next.(*Browser)
			if browser.previewFocused {
				t.Fatalf("%s did not return focus to the list", blurKey)
			}
			next, _ = browser.Update(key("j")) // the list owns j again
			browser = next.(*Browser)
			if browser.cursor != 1 {
				t.Errorf("j after blur left the cursor at %d; the list must own j again", browser.cursor)
			}
		})
	}
}

// TestBrowserPreviewFocusShowsCueForFittingPreview pins the freeze fix at its
// exact vacuity: a SHORT preview that FITS its pane renders no scroll hint, so
// when the focus cue lived only in that hint, focusing a fitting preview showed
// NOTHING — no on-screen sign the preview now owned the keys — and every keypress
// read as dead (the reported "freeze until esc"). The cue must appear the moment a
// FITTING preview is focused, and must be absent while the list holds focus.
func TestBrowserPreviewFocusShowsCueForFittingPreview(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	memories := []memoryfs.Memory{
		{Provider: "claude", RepoPath: "claude/short.md", Name: "short", Class: provider.ClassFact, ModTime: base},
	}
	browser := NewBrowser(BrowserDeps{
		Folder:   "acme",
		Now:      base,
		ReadBody: fakeReadBody(map[string]string{"claude/short.md": "a short body that fits its pane\n"}),
		List:     func() ([]memoryfs.Memory, error) { return append([]memoryfs.Memory(nil), memories...), nil },
	})
	const width, height = 120, 30

	unfocused := plain(browser.View(width, height))
	if strings.Contains(unfocused, previewFocusCue) {
		t.Errorf("fitting preview shows the focus cue while the LIST holds focus; the cue must appear only while the pane is focused:\n%s", unfocused)
	}

	next, _ := browser.Update(key("tab")) // focus the fitting preview
	browser = next.(*Browser)
	if !browser.PreviewFocused() {
		t.Fatal("tab did not focus the fitting preview (is the pane on screen at this width?)")
	}
	focused := plain(browser.View(width, height))
	if !strings.Contains(focused, previewFocusCue) {
		t.Errorf("focused fitting preview shows no cue — the reported freeze; want %q in:\n%s", previewFocusCue, focused)
	}
	// The freeze case is precisely a preview with NO scroll hint (the body fits),
	// so the cue is the ONLY focus signal here — this guards that the fixture is
	// genuinely the fitting case and not an overflow that would render a hint.
	if strings.Contains(focused, "scroll") {
		t.Errorf("fitting-preview fixture unexpectedly rendered a scroll affordance; the freeze case under test has none:\n%s", focused)
	}
}

// TestBrowserPreviewFocusCopyEmitsSelectedBody pins that y copies the previewed
// memory WHILE the preview holds focus. The focused footer advertises "y copy", so
// it must not be a dead key there: y is reached in the focused key block, not only
// the list-focused switch, so a copy works identically whether the list or the
// pane is focused, and the copy leaves the pane focused (y copies, it does not
// blur).
func TestBrowserPreviewFocusCopyEmitsSelectedBody(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	const body = "the raw body to copy\n"
	memories := []memoryfs.Memory{
		{Provider: "claude", RepoPath: "claude/short.md", Name: "Short", Class: provider.ClassFact, ModTime: base},
	}
	browser := NewBrowser(BrowserDeps{
		Folder:   "acme",
		Now:      base,
		ReadBody: fakeReadBody(map[string]string{"claude/short.md": body}),
		List:     func() ([]memoryfs.Memory, error) { return append([]memoryfs.Memory(nil), memories...), nil },
	})
	const width, height = 120, 30
	_ = browser.View(width, height) // render so the preview is on screen

	next, _ := browser.Update(key("tab")) // focus the preview
	browser = next.(*Browser)
	if !browser.PreviewFocused() {
		t.Fatal("tab did not focus the preview")
	}

	next, cmd := browser.Update(key("y")) // copy while focused
	browser = next.(*Browser)
	if cmd == nil {
		t.Fatal("y while focused produced no Cmd; the focused footer advertises \"y copy\", so it must not be a dead key")
	}
	copyMemory, ok := cmd().(CopyMemoryMsg)
	if !ok {
		t.Fatalf("y while focused produced %#v, want CopyMemoryMsg", cmd())
	}
	if copyMemory.Body != body {
		t.Errorf("CopyMemoryMsg.Body = %q, want the previewed memory's raw body %q", copyMemory.Body, body)
	}
	if !browser.previewFocused {
		t.Error("y while focused cleared the focus; copy must leave the pane focused")
	}
}

// TestBrowserPreviewFocusGGJumpEnds pins that g/G jump the focused preview to
// its head and foot — the reading view's own end-jump keys, handled by
// Browser.updateKey directly (the viewport exposes GotoTop/GotoBottom but binds
// no keys to them), reachable only while the pane holds focus.
func TestBrowserPreviewFocusGGJumpEnds(t *testing.T) {
	t.Parallel()
	browser := focusPreviewBrowser(t)
	const width, height = 120, 30
	_ = plain(browser.View(width, height))
	next, _ := browser.Update(key("tab"))
	browser = next.(*Browser)

	next, _ = browser.Update(key("G")) // jump to the foot
	browser = next.(*Browser)
	if browser.previewViewport.YOffset() == 0 {
		t.Error("G while focused did not jump the preview to its foot")
	}
	if !browser.previewViewport.AtBottom() {
		t.Errorf("G while focused did not land at the bottom; YOffset = %d", browser.previewViewport.YOffset())
	}
	next, _ = browser.Update(key("g")) // jump back to the head
	browser = next.(*Browser)
	if browser.previewViewport.YOffset() != 0 {
		t.Errorf("g while focused did not jump the preview back to its head; YOffset = %d", browser.previewViewport.YOffset())
	}
}

// TestBrowserPreviewFocusInertWithoutPreview pins that tab is inert when no
// preview pane is on screen (narrow width): it must not arm a focus the reader
// cannot see, and j must keep moving the list cursor.
func TestBrowserPreviewFocusInertWithoutPreview(t *testing.T) {
	t.Parallel()
	browser := focusPreviewBrowser(t)
	const width, height = 80, 30 // < previewMinWidth: the list owns the full width, no preview
	_ = plain(browser.View(width, height))
	if browser.previewShown {
		t.Fatalf("setup: preview shown at width %d, want none", width)
	}

	next, _ := browser.Update(key("tab"))
	browser = next.(*Browser)
	if browser.previewFocused {
		t.Error("tab focused the preview with no pane on screen; it must be inert")
	}
	next, _ = browser.Update(key("j"))
	browser = next.(*Browser)
	if browser.cursor != 1 {
		t.Errorf("j after an inert tab left the cursor at %d; the list must still own j", browser.cursor)
	}
}

// TestBrowserPreviewFocusGuardKeepsListLiveAfterNarrowResize pins the
// previewShown half of the focused-key guard (spec §3): a focus armed at a
// preview-split width, then a resize below previewMinWidth, must not swallow
// j into an off-screen viewport. Without the && previewShown guard the focused
// block would keep intercepting keys against a pane that is no longer on screen,
// freezing the list cursor; with it, j moves the list again.
func TestBrowserPreviewFocusGuardKeepsListLiveAfterNarrowResize(t *testing.T) {
	t.Parallel()
	const wide, narrow, height = 120, 80, 30
	browser := focusPreviewBrowser(t)
	_ = plain(browser.View(wide, height))
	next, _ := browser.Update(key("tab")) // focus the preview
	browser = next.(*Browser)
	if !browser.previewFocused {
		t.Fatal("setup: tab did not focus the preview")
	}
	_ = plain(browser.View(narrow, height)) // narrow resize: pane gone, focus lingers
	if browser.previewShown || !browser.previewFocused {
		t.Fatalf("setup: want the dangling state (previewShown=false, previewFocused=true); got shown=%v focused=%v",
			browser.previewShown, browser.previewFocused)
	}

	next, _ = browser.Update(key("j"))
	browser = next.(*Browser)
	if browser.cursor != 1 {
		t.Errorf("j left the cursor at %d after a narrow resize dropped the focused pane; the list must stay live", browser.cursor)
	}
}

// TestBrowserPreviewFocusClearsOnModeChange pins that a preview focus never
// leaks into a mode that owns the keyboard for itself. The dangling-focus state
// is reachable by a narrow resize: focus is armed at a preview-split width, then
// a resize below previewMinWidth drops the pane (previewShown false) while the
// bool lingers — entering the filter or the deleted-recovery list must clear the
// stale bool so it cannot resurrect on the next wide frame.
func TestBrowserPreviewFocusClearsOnModeChange(t *testing.T) {
	t.Parallel()
	const wide, narrow, height = 120, 80, 30

	danglingFocus := func(t *testing.T) *Browser {
		t.Helper()
		browser := focusPreviewBrowser(t)
		_ = plain(browser.View(wide, height)) // preview on screen
		next, _ := browser.Update(key("tab")) // focus it
		browser = next.(*Browser)
		if !browser.previewFocused {
			t.Fatal("setup: tab did not focus the preview")
		}
		_ = plain(browser.View(narrow, height)) // narrow resize drops the pane, focus lingers
		if browser.previewShown || !browser.previewFocused {
			t.Fatalf("setup: want the dangling state; got shown=%v focused=%v", browser.previewShown, browser.previewFocused)
		}
		return browser
	}

	t.Run("entering filter clears focus", func(t *testing.T) {
		t.Parallel()
		browser := danglingFocus(t)
		next, _ := browser.Update(key("/"))
		browser = next.(*Browser)
		if !browser.filtering {
			t.Fatal("/ did not enter filter mode")
		}
		if browser.previewFocused {
			t.Error("entering filter mode did not clear the dangling preview focus")
		}
	})

	t.Run("entering deleted mode clears focus", func(t *testing.T) {
		t.Parallel()
		browser := danglingFocus(t)
		next, _ := browser.Update(key("x"))
		browser = next.(*Browser)
		if !browser.showDeleted {
			t.Fatal("x did not enter deleted-recovery mode")
		}
		if browser.previewFocused {
			t.Error("entering deleted mode did not clear the dangling preview focus")
		}
	})
}

// TestBrowserWantsMouseReflectsPreview pins WantsMouse to the preview pane's
// on-screen state: true exactly while a preview is drawn (the wide render), and
// false once a narrow width has dropped it. The root reads this AFTER the
// browser's View has run, to gate the frame's MouseMode, so it must reflect the
// last render's previewShown rather than any width the browser might recompute
// on its own.
func TestBrowserWantsMouseReflectsPreview(t *testing.T) {
	t.Parallel()
	browser := longBodyBrowser(t)
	if browser.WantsMouse() {
		t.Fatal("WantsMouse true before any render; no preview pane has been drawn yet")
	}
	_ = browser.View(120, 30) // wide: draws the preview split
	if !browser.WantsMouse() {
		t.Error("WantsMouse false after a wide render that drew the preview pane")
	}
	_ = browser.View(80, 30) // narrow: below previewMinWidth, so no preview pane
	if browser.WantsMouse() {
		t.Error("WantsMouse true after a narrow render that drew no preview pane")
	}
}

// previewColumn is an X coordinate safely inside the preview pane (past the list
// pane and its two-space gap), and listColumn one safely inside the list pane —
// the column geometry the mouse handlers route on (overPreview).
const (
	previewColumn = listPaneWidth + 2 + 5
	listColumn    = 5
)

// TestBrowserWheelScrollsPreview pins the terminal-native hover-scroll: a wheel
// notch over the preview column scrolls the pane a few lines — down then back up
// — WITHOUT moving the list cursor and WITHOUT changing focus. Wheel is a
// hover affordance (like claude-code's own preview), so only a click may focus;
// the cursor- and focus-unchanged assertions are the load-bearing half.
func TestBrowserWheelScrollsPreview(t *testing.T) {
	t.Parallel()
	const width, height = 120, 30
	browser := longBodyBrowser(t)
	_ = browser.View(width, height) // render so previewShown is set and the pane is live
	if browser.previewViewport.YOffset() != 0 {
		t.Fatalf("setup: preview not at its top; YOffset = %d", browser.previewViewport.YOffset())
	}

	next, _ := browser.Update(tea.MouseWheelMsg{X: previewColumn, Button: tea.MouseWheelDown})
	browser = next.(*Browser)
	scrolled := browser.previewViewport.YOffset()
	if scrolled == 0 {
		t.Error("wheel-down over the preview did not scroll it (YOffset still 0)")
	}
	if browser.cursor != 0 {
		t.Errorf("wheel over the preview moved the list cursor to %d; it must scroll only the preview", browser.cursor)
	}
	if browser.previewFocused {
		t.Error("wheel over the preview focused it; only a click may change focus")
	}

	next, _ = browser.Update(tea.MouseWheelMsg{X: previewColumn, Button: tea.MouseWheelUp})
	browser = next.(*Browser)
	if back := browser.previewViewport.YOffset(); back >= scrolled {
		t.Errorf("wheel-up did not scroll the preview back up; YOffset went %d -> %d", scrolled, back)
	}
}

// TestBrowserWheelOverListMovesCursor pins the other half of the wheel routing:
// a notch over the list column moves the list cursor one row per notch (down
// then back up), the same nudge j/k give it. The two-row fixture makes the move
// observable.
func TestBrowserWheelOverListMovesCursor(t *testing.T) {
	t.Parallel()
	const width, height = 120, 30
	browser := focusPreviewBrowser(t) // two rows, so the cursor can actually move
	_ = browser.View(width, height)
	if browser.cursor != 0 {
		t.Fatalf("setup: cursor = %d, want 0", browser.cursor)
	}

	next, _ := browser.Update(tea.MouseWheelMsg{X: listColumn, Button: tea.MouseWheelDown})
	browser = next.(*Browser)
	if browser.cursor != 1 {
		t.Errorf("wheel-down over the list column moved cursor to %d, want 1", browser.cursor)
	}

	next, _ = browser.Update(tea.MouseWheelMsg{X: listColumn, Button: tea.MouseWheelUp})
	browser = next.(*Browser)
	if browser.cursor != 0 {
		t.Errorf("wheel-up over the list column moved cursor to %d, want 0", browser.cursor)
	}
}

// TestBrowserClickFocusesPreview pins click-to-focus: a left-click in the
// preview column focuses the pane (so the full scroll keymap then drives it),
// the mouse counterpart of Tab.
func TestBrowserClickFocusesPreview(t *testing.T) {
	t.Parallel()
	const width, height = 120, 30
	browser := focusPreviewBrowser(t)
	_ = browser.View(width, height) // render so the preview is on screen
	if browser.previewFocused {
		t.Fatal("setup: preview already focused before any click")
	}

	next, _ := browser.Update(tea.MouseClickMsg{X: previewColumn, Button: tea.MouseLeft})
	browser = next.(*Browser)
	if !browser.previewFocused {
		t.Error("left-click in the preview column did not focus the preview")
	}
}

// TestBrowserClickListBlursAndRestoresKeymap pins the click-to-blur invariant
// (the keymap-state consistency blurPreview established): a left-click on the
// list column must not just flip previewFocused off but also restore the
// unfocused preview keymap, exactly as blurPreview does. Otherwise the focused
// keymap — lazily installed on the first focused keystroke — outlives the focus
// it belonged to. The setup deliberately installs that focused keymap first
// (tab, then a focused key), so a bare `previewFocused = overPreview(x)` blur
// would leave KeyMap.Down still bound to the pane and fail the keymap assertion.
func TestBrowserClickListBlursAndRestoresKeymap(t *testing.T) {
	t.Parallel()
	const width, height = 120, 30
	browser := focusPreviewBrowser(t)
	_ = browser.View(width, height)

	next, _ := browser.Update(key("tab")) // focus the preview
	browser = next.(*Browser)
	next, _ = browser.Update(key("j")) // a focused keystroke lazily installs the focused keymap
	browser = next.(*Browser)
	if len(browser.previewViewport.KeyMap.Down.Keys()) == 0 {
		t.Fatal("setup: focused keymap not installed (Down binds no keys); the blur pin would be vacuous")
	}

	next, _ = browser.Update(tea.MouseClickMsg{X: listColumn, Button: tea.MouseLeft})
	browser = next.(*Browser)
	if browser.previewFocused {
		t.Error("left-click in the list column did not blur the preview")
	}
	if got := browser.previewViewport.KeyMap.Down.Keys(); len(got) != 0 {
		t.Errorf("click-to-blur left the focused keymap installed (Down still binds %v); blur must restore the unfocused keymap so j/k drive the list", got)
	}
}

// TestBrowserMouseInertOutsideNormalBody pins that the wheel and click are a
// no-op while the filter input or the deleted-recovery list owns the browser —
// the two modes updateKey itself bails to before the normal body. Without the
// guard a stale previewShown (set by the last normal render) would let a wheel
// scroll the hidden preview or leak the list cursor, and a click would focus a
// pane the reader cannot act on.
func TestBrowserMouseInertOutsideNormalBody(t *testing.T) {
	t.Parallel()
	const width, height = 120, 30

	t.Run("wheel inert while filtering", func(t *testing.T) {
		t.Parallel()
		browser := longBodyBrowser(t)
		_ = browser.View(width, height)
		next, _ := browser.Update(key("/")) // enter filter mode
		browser = next.(*Browser)
		before := browser.previewViewport.YOffset()
		next, _ = browser.Update(tea.MouseWheelMsg{X: previewColumn, Button: tea.MouseWheelDown})
		browser = next.(*Browser)
		if after := browser.previewViewport.YOffset(); after != before {
			t.Errorf("wheel scrolled the preview while filtering (YOffset %d -> %d); mouse must be inert there", before, after)
		}
	})

	t.Run("click inert while filtering", func(t *testing.T) {
		t.Parallel()
		browser := longBodyBrowser(t)
		_ = browser.View(width, height)
		next, _ := browser.Update(key("/"))
		browser = next.(*Browser)
		next, _ = browser.Update(tea.MouseClickMsg{X: previewColumn, Button: tea.MouseLeft})
		browser = next.(*Browser)
		if browser.previewFocused {
			t.Error("click focused the preview while filtering; mouse must be inert there")
		}
	})

	t.Run("wheel inert while showing deleted", func(t *testing.T) {
		t.Parallel()
		browser := focusPreviewBrowser(t) // two rows, so a leaked cursor move would be observable
		_ = browser.View(width, height)
		next, _ := browser.Update(key("x")) // enter deleted-recovery mode
		browser = next.(*Browser)
		if !browser.showDeleted {
			t.Fatal("setup: x did not enter deleted-recovery mode")
		}
		next, _ = browser.Update(tea.MouseWheelMsg{X: listColumn, Button: tea.MouseWheelDown})
		browser = next.(*Browser)
		if browser.cursor != 0 {
			t.Errorf("wheel moved the memory-list cursor to %d while the deleted list was showing; mouse must be inert there", browser.cursor)
		}
	})

	t.Run("click inert while showing deleted", func(t *testing.T) {
		t.Parallel()
		browser := focusPreviewBrowser(t)
		_ = browser.View(width, height)
		next, _ := browser.Update(key("x"))
		browser = next.(*Browser)
		next, _ = browser.Update(tea.MouseClickMsg{X: previewColumn, Button: tea.MouseLeft})
		browser = next.(*Browser)
		if browser.previewFocused {
			t.Error("click focused the preview while the deleted list was showing; mouse must be inert there")
		}
	})
}

// clickRowBrowser builds a two-provider browser (claude, then codex) with six
// memories in each group — two groups so a click has to cross a provider-header
// line, and twelve rows so a short height windows the list. Descending ModTime
// within each group sorts index i to that group's row i (newest first), so the
// visibleRows order is c0..c5 then x0..x5 with no resort surprises. The preview
// body is a constant carrying no row-name digits, so a name search over the
// rendered frame never collides with the preview pane beside the list.
func clickRowBrowser(t *testing.T) *Browser {
	t.Helper()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	var memories []memoryfs.Memory
	for i := range 6 {
		memories = append(
			memories,
			memoryfs.Memory{Provider: "claude", RepoPath: fmt.Sprintf("claude/c%d.md", i), Name: fmt.Sprintf("c%d", i), Class: provider.ClassFact, ModTime: base.Add(time.Duration(6-i) * time.Hour)},
			memoryfs.Memory{Provider: "codex", RepoPath: fmt.Sprintf("codex/x%d.md", i), Name: fmt.Sprintf("x%d", i), Class: provider.ClassFact, ModTime: base.Add(time.Duration(6-i) * time.Minute)},
		)
	}
	return NewBrowser(BrowserDeps{
		Folder:   "acme",
		Now:      base.Add(7 * time.Hour),
		ReadBody: func(memoryfs.Memory) (string, error) { return "preview line\n", nil },
		List:     func() ([]memoryfs.Memory, error) { return append([]memoryfs.Memory(nil), memories...), nil },
	})
}

// lineContaining returns the 0-based index of the first rendered line holding
// sub — the screen-local Y a click on that line carries — failing if none does.
func lineContaining(t *testing.T, view, sub string) int {
	t.Helper()
	for i, line := range strings.Split(view, "\n") {
		if strings.Contains(line, sub) {
			return i
		}
	}
	t.Fatalf("no rendered line contains %q; view:\n%s", sub, view)
	return -1
}

// TestBrowserClickSelectsRow pins the click→row contract over the render-time
// hit-map renderList records: a click on a memory line moves the cursor to that
// memory (proven through Selected), a click on a provider-header line or below
// the last rendered row changes nothing, a click in the preview band focuses the
// pane instead (the unchanged pane-focus contract), and clicks are inert while
// filtering and in the deleted list. The windowed case proves the map stores the
// ABSOLUTE visibleRows index, not a window-relative one. Each case drives View
// first so the map reflects a real frame, then clicks, then asserts.
func TestBrowserClickSelectsRow(t *testing.T) {
	t.Parallel()
	const (
		splitWidth  = 120 // clears previewMinWidth: a preview pane shows beside the list
		narrowWidth = 80  // below previewMinWidth: the list owns the full width, no preview
		tall        = 30  // clears the row budget: all twelve rows render, no windowing
		windowed    = 10  // budget of six rows: the list windows around the cursor
	)
	// Both helpers drive keys through Update, which mutates the browser in place
	// and hands back the same *Browser, so the caller's pointer sees the change
	// with no reassignment.
	down := func(n int) func(*testing.T, *Browser) {
		return func(t *testing.T, b *Browser) {
			t.Helper()
			for range n {
				b.Update(key("down"))
			}
		}
	}
	enter := func(name string) func(*testing.T, *Browser) {
		return func(t *testing.T, b *Browser) {
			t.Helper()
			b.Update(key(name))
		}
	}
	for _, tc := range []struct {
		name      string
		width     int // 0 defaults to splitWidth
		height    int
		setup     func(*testing.T, *Browser) // cursor / mode before the render, nil for none
		clickX    int
		findLine  string // click the line holding this substring; "" uses fixedY
		below     bool   // click one line below findLine's line instead of on it
		fixedY    int    // click Y when findLine is empty (a mode that draws no list)
		wantPath  string // Selected().RepoPath after the click
		wantFocus bool   // previewFocused after the click
	}{
		{
			name:     "first memory of provider A",
			height:   tall,
			setup:    down(3), // start off row 0 so landing on it is a real move
			clickX:   listColumn,
			findLine: "c0",
			wantPath: "claude/c0.md",
		},
		{
			name:     "single-pane list with no preview",
			width:    narrowWidth, // previewShown is false here: the other View branch must still record the map
			height:   tall,
			clickX:   listColumn,
			findLine: "c1",
			wantPath: "claude/c1.md",
		},
		{
			name:     "row of provider B after a header line",
			height:   tall,
			clickX:   listColumn,
			findLine: "x0", // the codex group, reached only across its header line
			wantPath: "codex/x0.md",
		},
		{
			name:     "provider header line selects nothing",
			height:   tall,
			clickX:   listColumn,
			findLine: "codex",
			wantPath: "claude/c0.md", // cursor stays on its initial row 0
		},
		{
			name:     "click below the last rendered row",
			height:   tall,
			clickX:   listColumn,
			findLine: "x5", // the last visible memory
			below:    true,
			wantPath: "claude/c0.md",
		},
		{
			name:      "preview band focuses the pane",
			height:    tall,
			clickX:    previewColumn,
			findLine:  "c0",
			wantPath:  "claude/c0.md", // selection unchanged; only focus moves
			wantFocus: true,
		},
		{
			name:     "inert while filtering",
			height:   tall,
			setup:    enter("/"), // the filter input owns every key
			clickX:   listColumn,
			findLine: "x0", // would select the codex row but for the guard
			wantPath: "claude/c0.md",
		},
		{
			name:     "inert while showing deleted",
			height:   tall,
			setup:    enter("x"), // the deleted-recovery list owns the body
			clickX:   listColumn,
			fixedY:   5, // the deleted view draws no memory list to hit
			wantPath: "claude/c0.md",
		},
		{
			name:     "windowed list maps to the absolute row",
			height:   windowed,
			setup:    down(11), // cursor deep in codex; the window starts past row 0
			clickX:   listColumn,
			findLine: "x0", // rendered on the window's first row, absolute index 6
			wantPath: "codex/x0.md",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			browser := clickRowBrowser(t)
			if tc.setup != nil {
				tc.setup(t, browser)
			}
			width := tc.width
			if width == 0 {
				width = splitWidth
			}
			view := plain(browser.View(width, tc.height))

			y := tc.fixedY
			if tc.findLine != "" {
				y = lineContaining(t, view, tc.findLine)
				if tc.below {
					y++
				}
			}

			next, _ := browser.Update(tea.MouseClickMsg{X: tc.clickX, Y: y, Button: tea.MouseLeft})
			browser = next.(*Browser)

			got, ok := browser.Selected()
			if !ok {
				t.Fatalf("no selection after the click; view:\n%s", view)
			}
			if got.RepoPath != tc.wantPath {
				t.Errorf("click selected %q, want %q; view:\n%s", got.RepoPath, tc.wantPath, view)
			}
			if browser.previewFocused != tc.wantFocus {
				t.Errorf("previewFocused = %v after the click, want %v", browser.previewFocused, tc.wantFocus)
			}
		})
	}
}

var (
	_ Screen  = (*Browser)(nil)
	_ tea.Msg = RefreshMsg{}
)
