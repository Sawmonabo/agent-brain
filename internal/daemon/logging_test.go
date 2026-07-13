package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestRotatingWriterRotatesMidRun(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "daemon.log")
	writer, err := newRotatingWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	writer.limit = 64 // tiny, so the second write crosses it

	first := []byte(strings.Repeat("A", 49) + "\n")  // 50 bytes, under the limit
	second := []byte(strings.Repeat("B", 49) + "\n") // 50 bytes, crosses it
	if _, err := writer.Write(first); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(second); err != nil {
		t.Fatal(err)
	}

	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal("crossing the limit did not produce a .1 generation:", err)
	}
	if diff := cmp.Diff(first, rotated); diff != "" {
		t.Fatalf(".1 must hold the pre-rotation bytes (-want +got):\n%s", diff)
	}
	live, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(second, live); diff != "" {
		t.Fatalf("live file must restart with only the post-rotation write (-want +got):\n%s", diff)
	}
}

func TestNewRotatingWriterRotatesPreexistingOversizedFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "daemon.log")
	old := make([]byte, maxLogSize+1)
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	writer, err := newRotatingWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()

	rotated, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatal("a preexisting oversized log must rotate at construction:", err)
	}
	if rotated.Size() != int64(len(old)) {
		t.Fatalf(".1 size = %d, want %d", rotated.Size(), len(old))
	}
	live, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if live.Size() != 0 {
		t.Fatalf("fresh log size = %d, want 0", live.Size())
	}
}

func TestRotatingWriterConcurrentWritesAreRaceFree(t *testing.T) {
	t.Parallel()
	writer, err := newRotatingWriter(filepath.Join(t.TempDir(), "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	writer.limit = 1 << 10 // small, so concurrent writers force real rotations

	// slog handlers call Write from many goroutines; -race is the assertion
	// that rotation never corrupts the writer's file/size invariant.
	line := []byte(strings.Repeat("x", 63) + "\n")
	var group sync.WaitGroup
	for range 16 {
		group.Go(func() {
			for range 200 {
				if _, err := writer.Write(line); err != nil {
					t.Errorf("concurrent write: %v", err)
					return
				}
			}
		})
	}
	group.Wait()
}

func TestRotateIfOversized(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Missing file: no-op, no error, no .1 conjured.
	missing := filepath.Join(dir, "missing.jsonl")
	if err := rotateIfOversized(missing, 10); err != nil {
		t.Fatalf("missing file must be a no-op, got %v", err)
	}
	if _, err := os.Stat(missing + ".1"); !os.IsNotExist(err) {
		t.Fatal("missing file must not produce a .1")
	}

	// Small file: no-op.
	small := filepath.Join(dir, "small.jsonl")
	if err := os.WriteFile(small, []byte("tiny"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rotateIfOversized(small, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(small + ".1"); !os.IsNotExist(err) {
		t.Fatal("a file at or under the limit must not rotate")
	}

	// Oversized file: renamed to .1, original path cleared for the next
	// writer to recreate.
	big := filepath.Join(dir, "big.jsonl")
	payload := []byte("this content is comfortably larger than ten bytes\n")
	if err := os.WriteFile(big, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rotateIfOversized(big, 10); err != nil {
		t.Fatal(err)
	}
	rotated, err := os.ReadFile(big + ".1")
	if err != nil {
		t.Fatal("oversized file was not rotated:", err)
	}
	if diff := cmp.Diff(payload, rotated); diff != "" {
		t.Fatalf("rotated content (-want +got):\n%s", diff)
	}
	if _, err := os.Stat(big); !os.IsNotExist(err) {
		t.Fatal("oversized file must be gone after rotation (renamed to .1)")
	}
}
