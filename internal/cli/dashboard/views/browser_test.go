package views

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/providertest"
)

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
		Now:      time.Now,
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
		Now:      time.Now,
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

// TestBrowserLintBadge pins the ⚠ badge: a memory whose body names a
// [[wiki-link]] target that matches no other memory in the listing is
// flagged; a memory with no such dangling reference is not.
func TestBrowserLintBadge(t *testing.T) {
	t.Parallel()
	registry := browserFixtureRegistry(t)
	dir := t.TempDir()
	writeBrowserFile(t, dir, "has-link.md", "---\nname: Has Link\n---\n", time.Now())
	writeBrowserFile(t, dir, "clean.md", "---\nname: Clean Memory\n---\n", time.Now())

	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}
	bodies := map[string]string{
		"claude/has-link.md": "sabotage: see [[Ghost Reference]] for details\n",
		"claude/clean.md":    "no links here\n",
	}
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      time.Now,
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
		Now:      time.Now,
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
		Now:      time.Now,
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
		Now:      time.Now,
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
		Now:      time.Now,
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

// TestBrowserEscClearsFilterThenPops pins the Screen contract's consumption
// rule directly: while the in-browser filter is open, esc clears it and
// produces NO PopScreenMsg-bearing Cmd (the screen consumed the key); the
// next esc, with the filter already closed, does produce one.
func TestBrowserEscClearsFilterThenPops(t *testing.T) {
	t.Parallel()
	browser := NewBrowser(BrowserDeps{
		Folder:   "acme",
		Now:      time.Now,
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
	writeBrowserFile(t, dir, "one.md", "---\nname: One\n---\n", time.Now())
	units := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: dir}}

	var readCalls int
	browser := NewBrowser(BrowserDeps{
		Registry: registry,
		Units:    units,
		Folder:   "acme",
		Now:      time.Now,
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
	// the mutation is what the readCalls assertions below observe.
	browser.Update(RefreshMsg{})
	if readCalls != afterConstruct {
		t.Errorf("ReadBody called again for an unchanged listing: %d calls, want %d", readCalls, afterConstruct)
	}

	// Touch the file's mtime (a genuine change) and confirm a rescan does happen.
	if err := os.Chtimes(filepath.Join(dir, "one.md"), time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	browser.Update(RefreshMsg{})
	if readCalls <= afterConstruct {
		t.Errorf("ReadBody was not called again after the listing changed: %d calls, want > %d", readCalls, afterConstruct)
	}
}

// TestBrowserTitleIsFolder pins the breadcrumb segment contract.
func TestBrowserTitleIsFolder(t *testing.T) {
	t.Parallel()
	browser := NewBrowser(BrowserDeps{
		Folder:   "acme",
		Now:      time.Now,
		ReadBody: fakeReadBody(nil),
		List:     func() ([]memoryfs.Memory, error) { return nil, nil },
	})
	if got := browser.Title(); got != "acme" {
		t.Errorf("Title() = %q, want %q", got, "acme")
	}
}

var (
	_ Screen  = (*Browser)(nil)
	_ tea.Msg = RefreshMsg{}
)
