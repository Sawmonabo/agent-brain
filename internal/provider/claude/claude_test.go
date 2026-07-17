package claude_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/claude"
)

// var _ provider.Provider = (*claude.Adapter)(nil) pins the adapter against
// the full Provider interface at compile time.
var _ provider.Provider = (*claude.Adapter)(nil)

func TestNewNameAndScope(t *testing.T) {
	t.Parallel()
	adapter := claude.New(t.TempDir())
	if got := adapter.Name(); got != "claude" {
		t.Errorf("Name() = %q, want %q", got, "claude")
	}
	if got := adapter.Scope(); got != provider.ScopePerProject {
		t.Errorf("Scope() = %v, want %v", got, provider.ScopePerProject)
	}
}

// TestPrimaryIndexPath pins Claude's human-facing index at the provider-dir
// root — the identity the browser sorts first, kept distinct from the
// MEMORY.md merge class (ClassDerivedIndex) it happens to share a file with.
func TestPrimaryIndexPath(t *testing.T) {
	t.Parallel()
	adapter := claude.New(t.TempDir())
	if got := adapter.PrimaryIndexPath(); got != "MEMORY.md" {
		t.Errorf("PrimaryIndexPath() = %q, want %q", got, "MEMORY.md")
	}
}

// TestDiscover fabricates a ~/.claude/projects tree with one enrollable
// slug (has a memory/ dir), one non-enrollable slug (no memory/ dir), and
// a stray non-directory entry, then asserts Discover finds exactly the
// one enrollable root.
func TestDiscover(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	projectsDir := filepath.Join(home, ".claude", "projects")

	// slugA is enrollable: it has a memory/ subdirectory.
	const slugA = "-nonexistent-agent-brain-fixture-alpha"
	if err := os.MkdirAll(filepath.Join(projectsDir, slugA, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	// slugB exists but has no memory/ dir yet (Claude created the project
	// folder but auto-memory has not written anything there).
	const slugB = "-nonexistent-agent-brain-fixture-beta"
	if err := os.MkdirAll(filepath.Join(projectsDir, slugB), 0o755); err != nil {
		t.Fatal(err)
	}
	// A stray file directly under projects/ must never be treated as a slug.
	if err := os.WriteFile(filepath.Join(projectsDir, "stray-file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	adapter := claude.New(home)
	got, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Discover() returned %d roots, want 1: %+v", len(got), got)
	}
	want := provider.Discovered{
		LocalDir:   filepath.Join(projectsDir, slugA, "memory"),
		RepoSubdir: "",
		Label:      slugA,
		// The naive reversal: nothing under "/nonexistent/..." exists on
		// the machine running this test, so the filesystem-guided walk
		// falls back to it exactly (slug.go's documented last resort).
		PathGuess: "/nonexistent/agent/brain/fixture/alpha",
	}
	if got[0] != want {
		t.Fatalf("Discover()[0] = %+v, want %+v", got[0], want)
	}
}

// TestDiscoverPrefersSessionCWD pins discovery's authoritative-source
// order: Claude Code records the project's absolute path as "cwd" in
// every session .jsonl line, so when a session file exists its recorded
// path wins over any slug reconstruction. The fixture path is
// deliberately unrecoverable from the slug alone (unicode and a space
// both fold to '-') AND nonexistent on the test machine — only the
// session record can produce it, so a pass proves the source.
func TestDiscoverPrefersSessionCWD(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	const cwd = "/nonexistent/pröbe x/dev.proj"
	slug := claude.SlugFor(cwd)
	projectDir := filepath.Join(home, ".claude", "projects", slug)
	if err := os.MkdirAll(filepath.Join(projectDir, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	session := `{"type":"summary","cwd":"` + cwd + `","version":"2.1.205"}` + "\n" +
		`{"type":"user","cwd":"` + cwd + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(projectDir, "session-a.jsonl"), []byte(session), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := claude.New(home).Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Discover() returned %d roots, want 1: %+v", len(got), got)
	}
	if got[0].PathGuess != cwd {
		t.Fatalf("PathGuess = %q, want session cwd %q", got[0].PathGuess, cwd)
	}
}

// TestDiscoverSessionCWDNewestWins pins which session speaks for a
// project that MOVED: the newest-mtime session file records where the
// project lives now; older sessions record where it used to.
func TestDiscoverSessionCWDNewestWins(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	const oldCWD = "/nonexistent/old.home"
	const newCWD = "/nonexistent/new.home"
	slug := claude.SlugFor(newCWD)
	projectDir := filepath.Join(home, ".claude", "projects", slug)
	if err := os.MkdirAll(filepath.Join(projectDir, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, cwd string, age time.Duration) {
		t.Helper()
		p := filepath.Join(projectDir, name)
		if err := os.WriteFile(p, []byte(`{"cwd":"`+cwd+`"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		stamp := time.Now().Add(-age)
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	write("zz-older.jsonl", oldCWD, time.Hour)
	write("aa-newer.jsonl", newCWD, time.Minute)

	got, err := claude.New(home).Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(got) != 1 || got[0].PathGuess != newCWD {
		t.Fatalf("PathGuess = %+v, want newest session's cwd %q", got, newCWD)
	}
}

// TestDiscoverFallsBackWhenSessionsUnreadable pins the degradation
// chain: session files that are malformed (or record no absolute cwd)
// must not fail discovery — PathGuess falls back to slug reconstruction,
// here the documented naive last resort.
func TestDiscoverFallsBackWhenSessionsUnreadable(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	const slug = "-nonexistent-agent-brain-fixture-gamma"
	projectDir := filepath.Join(home, ".claude", "projects", slug)
	if err := os.MkdirAll(filepath.Join(projectDir, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	corrupt := "not json at all\n"
	if err := os.WriteFile(filepath.Join(projectDir, "bad.jsonl"), []byte(corrupt), 0o644); err != nil {
		t.Fatal(err)
	}
	relative := `{"cwd":"relative/not/absolute"}` + "\n"
	if err := os.WriteFile(filepath.Join(projectDir, "rel.jsonl"), []byte(relative), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := claude.New(home).Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Discover() returned %d roots, want 1: %+v", len(got), got)
	}
	if want := "/nonexistent/agent/brain/fixture/gamma"; got[0].PathGuess != want {
		t.Fatalf("PathGuess = %q, want naive fallback %q", got[0].PathGuess, want)
	}
}

// TestDiscoverMissingRoot pins the "Claude not installed" contract: a
// missing ~/.claude/projects is not an error.
func TestDiscoverMissingRoot(t *testing.T) {
	t.Parallel()
	adapter := claude.New(t.TempDir())
	got, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover() error = %v, want nil for a missing root", err)
	}
	if got != nil {
		t.Fatalf("Discover() = %v, want nil", got)
	}
}

func TestIdentifyWithRemote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	projectPath := t.TempDir()
	if _, err := gitx.Run(ctx, projectPath, "init", "--quiet"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Run(ctx, projectPath, "remote", "add", "origin", "git@github.com:o/r.git"); err != nil {
		t.Fatal(err)
	}

	adapter := claude.New(t.TempDir())
	got, err := adapter.Identify(ctx, provider.Discovered{}, projectPath)
	if err != nil {
		t.Fatalf("Identify() error = %v", err)
	}
	want := provider.Identity{ProjectID: "github.com/o/r", PreferredFolder: "r"}
	if got != want {
		t.Fatalf("Identify() = %+v, want %+v", got, want)
	}
}

func TestIdentifyNoRemote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	projectPath := t.TempDir()
	if _, err := gitx.Run(ctx, projectPath, "init", "--quiet"); err != nil {
		t.Fatal(err)
	}

	adapter := claude.New(t.TempDir())
	got, err := adapter.Identify(ctx, provider.Discovered{}, projectPath)
	if err != nil {
		t.Fatalf("Identify() error = %v, want nil (remoteless is not an error)", err)
	}
	want := provider.Identity{PreferredFolder: filepath.Base(projectPath)}
	if got != want {
		t.Fatalf("Identify() = %+v, want %+v", got, want)
	}
}

func TestIdentifyNotAGitRepo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	projectPath := t.TempDir() // never git-inited

	adapter := claude.New(t.TempDir())
	got, err := adapter.Identify(ctx, provider.Discovered{}, projectPath)
	if err != nil {
		t.Fatalf("Identify() error = %v, want nil (the picker already warned)", err)
	}
	want := provider.Identity{PreferredFolder: filepath.Base(projectPath)}
	if got != want {
		t.Fatalf("Identify() = %+v, want %+v", got, want)
	}
}

func TestIdentifyEmptyProjectPath(t *testing.T) {
	t.Parallel()
	adapter := claude.New(t.TempDir())
	if _, err := adapter.Identify(context.Background(), provider.Discovered{}, ""); err == nil {
		t.Fatal("Identify() with empty projectPath: error = nil, want error")
	}
}

func TestPatternsClassification(t *testing.T) {
	t.Parallel()
	adapter := claude.New(t.TempDir())

	tests := []struct {
		rel  string
		want provider.Class
	}{
		{"MEMORY.md", provider.ClassDerivedIndex},
		{"topic.md", provider.ClassFact},
		{".DS_Store", provider.ClassIgnore},
		{"sub/.DS_Store", provider.ClassIgnore},
	}
	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			t.Parallel()
			if got := provider.Classify(adapter, tt.rel); got != tt.want {
				t.Errorf("Classify(%q) = %v, want %v", tt.rel, got, tt.want)
			}
		})
	}
}
