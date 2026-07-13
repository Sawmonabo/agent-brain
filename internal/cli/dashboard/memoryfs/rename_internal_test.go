package memoryfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRenameNoClobberFallback white-box pins the non-hardlink path's own
// no-clobber decision, which production only reaches on filesystems whose
// os.Link fails EPERM/ENOTSUP: the errno routing in renameNoClobber cannot
// be forced deterministically, but the fallback it dispatches to is a pure
// path-only function, so its behavior is pinned directly. The dangling-
// symlink row is what makes Lstat (not Stat) load-bearing: Stat would
// dereference, report ErrNotExist, and clobber the link.
func TestRenameNoClobberFallback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// arrange returns oldPath/newPath inside dir, with any
		// pre-existing target in place.
		arrange         func(t *testing.T, dir string) (oldPath, newPath string)
		wantErrIs       error
		wantSourceKept  bool
		wantTargetBytes string // asserted only when non-empty
	}{
		{
			name: "existing target refuses with ErrTargetExists and both files survive",
			arrange: func(t *testing.T, dir string) (string, string) {
				t.Helper()
				oldPath := filepath.Join(dir, "old.md")
				newPath := filepath.Join(dir, "new.md")
				writeRenameFixture(t, oldPath, "source bytes")
				writeRenameFixture(t, newPath, "target bytes")
				return oldPath, newPath
			},
			wantErrIs:       ErrTargetExists,
			wantSourceKept:  true,
			wantTargetBytes: "target bytes",
		},
		{
			name: "dangling symlink at target still counts as occupied",
			arrange: func(t *testing.T, dir string) (string, string) {
				t.Helper()
				oldPath := filepath.Join(dir, "old.md")
				newPath := filepath.Join(dir, "new.md")
				writeRenameFixture(t, oldPath, "source bytes")
				if err := os.Symlink(filepath.Join(dir, "does-not-exist"), newPath); err != nil {
					t.Fatal(err)
				}
				return oldPath, newPath
			},
			wantErrIs:      ErrTargetExists,
			wantSourceKept: true,
		},
		{
			name: "free target renames cleanly",
			arrange: func(t *testing.T, dir string) (string, string) {
				t.Helper()
				oldPath := filepath.Join(dir, "old.md")
				writeRenameFixture(t, oldPath, "source bytes")
				return oldPath, filepath.Join(dir, "new.md")
			},
			wantSourceKept:  false,
			wantTargetBytes: "source bytes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			oldPath, newPath := tt.arrange(t, dir)

			err := renameNoClobberFallback(oldPath, newPath)

			if tt.wantErrIs != nil {
				if !errors.Is(err, tt.wantErrIs) {
					t.Fatalf("renameNoClobberFallback() error = %v, want errors.Is %v", err, tt.wantErrIs)
				}
			} else if err != nil {
				t.Fatalf("renameNoClobberFallback() error = %v, want nil", err)
			}

			if _, statErr := os.Lstat(oldPath); tt.wantSourceKept != (statErr == nil) {
				t.Fatalf("source presence after fallback = %v, want kept=%v (stat err %v)", statErr == nil, tt.wantSourceKept, statErr)
			}
			if tt.wantTargetBytes != "" {
				got, readErr := os.ReadFile(newPath)
				if readErr != nil {
					t.Fatalf("read target: %v", readErr)
				}
				if string(got) != tt.wantTargetBytes {
					t.Fatalf("target bytes = %q, want %q", got, tt.wantTargetBytes)
				}
			}
		})
	}
}

func writeRenameFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
