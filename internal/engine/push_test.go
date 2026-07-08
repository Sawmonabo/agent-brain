package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestPushNothingToPush(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	outcome, err := engine.push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Pushed || outcome.Queued {
		t.Fatalf("outcome = %+v, want no-op", outcome)
	}
}

func TestPushDeliversLocalCommits(t *testing.T) {
	t.Parallel()
	checkout, bare := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	commitFileOn(t, checkout, "alpha/claude/memories/a.md", "fact\n", "memory: host-a alpha ts")

	outcome, err := engine.push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Pushed || outcome.Queued {
		t.Fatalf("outcome = %+v, want Pushed", outcome)
	}
	remoteLog := mustGit(t, bare, "log", "--format=%s", "-n", "1", "main")
	if got := strings.TrimSpace(remoteLog.Stdout); got != "memory: host-a alpha ts" {
		t.Fatalf("remote tip = %q", got)
	}
}

func TestPushOfflineQueues(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	commitFileOn(t, checkout, "alpha/claude/memories/a.md", "fact\n", "memory: host-a alpha ts")
	mustGit(t, checkout, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "vanished.git"))

	outcome, err := engine.push(context.Background())
	if err != nil {
		t.Fatal("offline push must queue, not error:", err)
	}
	if outcome.Pushed || !outcome.Queued {
		t.Fatalf("outcome = %+v, want Queued", outcome)
	}
}

// TestPushRaceLoserReintegratesAndWins is spec §4 step 6 end to end:
// the other machine pushes first, our push is rejected non-fast-forward,
// we re-integrate (rebase) and the retry lands both histories.
func TestPushRaceLoserReintegratesAndWins(t *testing.T) {
	t.Parallel()
	checkout, bare := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	commitFileOn(t, checkout, "alpha/claude/memories/ours.md", "ours\n", "memory: host-a alpha ts")
	other := secondClone(t, bare)
	commitFileOn(t, other, "beta/claude/memories/theirs.md", "theirs\n", "memory: host-b beta ts")
	mustGit(t, other, "push", "origin", "main")

	outcome, err := engine.push(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Pushed {
		t.Fatalf("outcome = %+v, want Pushed after race retry", outcome)
	}
	remoteFiles := mustGit(t, bare, "ls-tree", "-r", "--name-only", "main")
	for _, rel := range []string{"alpha/claude/memories/ours.md", "beta/claude/memories/theirs.md"} {
		if !strings.Contains(remoteFiles.Stdout, rel) {
			t.Fatalf("remote missing %s after race resolution:\n%s", rel, remoteFiles.Stdout)
		}
	}
}
