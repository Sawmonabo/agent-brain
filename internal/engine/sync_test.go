package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func TestSyncFullCycleLocalToRemote(t *testing.T) {
	t.Parallel()
	checkout, bare := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	writeLocal(t, u, "memories/fact.md", "a fact\n")

	report, err := engine.Sync(context.Background(), []repo.Unit{u})
	if err != nil {
		t.Fatal(err)
	}
	if report.MirrorIn.Copied != 1 || !report.Pushed || report.PushQueued {
		t.Fatalf("report = %+v, want 1 copied + pushed", report)
	}
	wantSubject := "memory: host-a alpha " + fixedStamp
	found := false
	for _, s := range report.Commits {
		if s == wantSubject {
			found = true
		}
	}
	if !found {
		t.Fatalf("Commits = %v, want to include %q", report.Commits, wantSubject)
	}
	remoteFiles := mustGit(t, bare, "ls-tree", "-r", "--name-only", "main")
	if !strings.Contains(remoteFiles.Stdout, "alpha/claude/memories/fact.md") {
		t.Fatalf("remote tree missing synced file:\n%s", remoteFiles.Stdout)
	}
	if !strings.Contains(remoteFiles.Stdout, ".agent-brain/manifests/host-a.json") {
		t.Fatalf("remote tree missing host manifest:\n%s", remoteFiles.Stdout)
	}
	// Second cycle with no changes is a true no-op.
	second, err := engine.Sync(context.Background(), []repo.Unit{u})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Commits) != 0 || second.Pushed || second.PushQueued {
		t.Fatalf("idle cycle produced work: %+v", second)
	}
}

func TestSyncTwoCheckoutsConverge(t *testing.T) {
	t.Parallel()
	checkoutA, bare := newTestCheckout(t)
	engineA := newTestEngine(t, checkoutA)
	unitA := unit(t, "alpha")
	writeLocal(t, unitA, "memories/from-a.md", "A's fact\n")
	if _, err := engineA.Sync(context.Background(), []repo.Unit{unitA}); err != nil {
		t.Fatal(err)
	}

	// "Machine B": its own clone, host identity, and provider dir.
	checkoutB := secondClone(t, bare)
	engineB, err := New(checkoutB, "host-b", testRegistry(t), fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	unitB := repo.Unit{Provider: "claude", ProjectID: "id-alpha", Folder: "alpha", LocalDir: filepath.Join(t.TempDir(), "memory")}
	if err := os.MkdirAll(unitB.LocalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reportB, err := engineB.Sync(context.Background(), []repo.Unit{unitB})
	if err != nil {
		t.Fatal(err)
	}
	if reportB.MirrorOut.Copied != 1 {
		t.Fatalf("reportB = %+v, want A's file mirrored out on B", reportB)
	}
	data, err := os.ReadFile(filepath.Join(unitB.LocalDir, "memories", "from-a.md"))
	if err != nil || string(data) != "A's fact\n" {
		t.Fatalf("B's provider dir = %q, %v", data, err)
	}
}

func TestSyncRefusesReentry(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	engine.busy.Store(true)
	if _, err := engine.Sync(context.Background(), nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
}

// TestSyncBusyGuardSerializesConcurrentCalls hammers one engine with many
// concurrent Sync calls under -race. The CAS guard must serialize them:
// every call returns either nil or ErrBusy (never a git error — unguarded
// concurrent cycles would collide on .git/index.lock), and at least one
// wins the CAS and completes the cycle.
func TestSyncBusyGuardSerializesConcurrentCalls(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	writeLocal(t, u, "memories/fact.md", "a fact\n")

	const goroutines = 16
	var (
		start     sync.WaitGroup
		done      sync.WaitGroup
		succeeded atomic.Int64
	)
	start.Add(1)
	errs := make([]error, goroutines)
	for i := range goroutines {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait() // release all goroutines at once for maximum contention
			_, err := engine.Sync(context.Background(), []repo.Unit{u})
			errs[i] = err
			if err == nil {
				succeeded.Add(1)
			}
		}()
	}
	start.Done()
	done.Wait()

	for i, err := range errs {
		if err != nil && !errors.Is(err, ErrBusy) {
			t.Fatalf("goroutine %d: unexpected error %v (want nil or ErrBusy)", i, err)
		}
	}
	if succeeded.Load() == 0 {
		t.Fatal("no Sync call won the guard and completed the cycle")
	}
}
