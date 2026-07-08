package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sync"
)

// maxLogSize bounds daemon.log; one .1 generation is kept. A resident
// single-user daemon does not need a log-management stack.
const maxLogSize = 10 << 20

// maxConflictLogSize bounds conflicts.jsonl the same way. It is smaller
// than daemon.log: one JSON line per retain-both event is far rarer than
// a per-cycle log line.
const maxConflictLogSize = 5 << 20

// rotatingWriter is an io.Writer for a size-bounded log. slog handlers
// call Write from multiple goroutines, so every field is mutex-guarded.
// When a write would push the file past limit it closes the file,
// renames it to <path>.1 (one generation), and reopens an empty file —
// so a long-lived daemon rotates mid-run, not only at startup.
type rotatingWriter struct {
	mu    sync.Mutex
	path  string
	limit int64
	file  *os.File
	size  int64
}

// newRotatingWriter opens path for append, rotating a preexisting
// oversized file first (start-time rotation, as the pre-mid-run daemon
// did). limit defaults to maxLogSize; tests lower it via the field.
func newRotatingWriter(path string) (*rotatingWriter, error) {
	writer := &rotatingWriter{path: path, limit: maxLogSize}
	if err := rotateIfOversized(path, writer.limit); err != nil {
		return nil, err
	}
	if err := writer.reopen(); err != nil {
		return nil, err
	}
	return writer, nil
}

// reopen (re)opens path for append and records its current size. Caller
// holds mu (or is the constructor, before the writer is shared).
func (writer *rotatingWriter) reopen() error {
	file, err := os.OpenFile(writer.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("stat log: %w", err)
	}
	writer.file = file
	writer.size = info.Size()
	return nil
}

// Write appends p, rotating first if it would cross limit. A never-empty
// file is the rotation trigger (size > 0): a single write larger than
// limit lands in a fresh file rather than looping. A rotation error is
// non-fatal — reopen keeps a usable file so logging degrades, never
// dies, on a full disk.
func (writer *rotatingWriter) Write(p []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.size > 0 && writer.size+int64(len(p)) > writer.limit {
		if err := writer.rotate(); err != nil {
			// Keep going: rotate already reopened a writable file (the
			// original, if the rename failed), so the line still lands.
			_ = err
		}
	}
	if writer.file == nil {
		return 0, errors.New("log writer is closed")
	}
	n, err := writer.file.Write(p)
	writer.size += int64(n)
	return n, err
}

// rotate closes the live file, renames it to <path>.1, and reopens an
// empty file. On a failed rename it reopens the original so writes still
// land (degraded: the file stays over-limit). Caller holds mu.
func (writer *rotatingWriter) rotate() error {
	_ = writer.file.Close()
	if err := os.Rename(writer.path, writer.path+".1"); err != nil {
		writer.file = nil
		_ = writer.reopen() // best-effort: keep logging to the original
		return fmt.Errorf("rotate log: %w", err)
	}
	writer.file = nil
	return writer.reopen()
}

// Close closes the live file. Idempotent.
func (writer *rotatingWriter) Close() error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.file == nil {
		return nil
	}
	err := writer.file.Close()
	writer.file = nil
	return err
}

// rotateIfOversized renames path to <path>.1 when it exceeds limit,
// keeping one generation; a missing or within-limit file is a no-op.
// This is the start-time rotation rotatingWriter applies to itself,
// reused for conflicts.jsonl at the top of each cycle (see runCycle).
func rotateIfOversized(path string, limit int64) error {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Size() <= limit {
		return nil
	}
	if err := os.Rename(path, path+".1"); err != nil {
		return fmt.Errorf("rotate %s: %w", path, err)
	}
	return nil
}

// openLogger returns a JSON slog logger over a size-rotating writer, plus
// the writer to close on shutdown.
func openLogger(logPath string) (*slog.Logger, *rotatingWriter, error) {
	writer, err := newRotatingWriter(logPath)
	if err != nil {
		return nil, nil, err
	}
	return slog.New(slog.NewJSONHandler(writer, nil)), writer, nil
}
