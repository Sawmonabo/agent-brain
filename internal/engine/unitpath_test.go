package engine

import (
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func TestUnitDirAndClassifyRel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		unit        repo.Unit
		rel         string
		wantDirTail string // relative to checkout root, OS-joined
		wantRel     string
	}{
		{
			name:        "claude project unit (no subdir) — Phase-2 behavior",
			unit:        repo.Unit{Provider: "claude", Folder: "agent-brain", LocalDir: "/x"},
			rel:         "MEMORY.md",
			wantDirTail: filepath.Join("agent-brain", "claude"),
			wantRel:     "MEMORY.md",
		},
		{
			name:        "codex memories unit",
			unit:        repo.Unit{Provider: "codex", Folder: repo.GlobalFolder, LocalDir: "/x", RepoSubdir: "memories"},
			rel:         "memory_summary.md",
			wantDirTail: filepath.Join("_global", "codex", "memories"),
			wantRel:     "memories/memory_summary.md",
		},
		{
			name:        "codex chronicle unit, nested file",
			unit:        repo.Unit{Provider: "codex", Folder: repo.GlobalFolder, LocalDir: "/x", RepoSubdir: "chronicle"},
			rel:         "2026/07/log.md",
			wantDirTail: filepath.Join("_global", "codex", "chronicle"),
			wantRel:     "chronicle/2026/07/log.md",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// newTestEngine takes an explicit checkout (built by
			// newTestCheckout) rather than a zero-arg constructor — that
			// helper already exists in helpers_test.go and every other
			// engine test reuses it this way.
			checkout, _ := newTestCheckout(t)
			e := newTestEngine(t, checkout)
			gotDir := e.unitDir(tt.unit)
			if want := filepath.Join(e.checkout, tt.wantDirTail); gotDir != want {
				t.Errorf("unitDir = %q, want %q", gotDir, want)
			}
			if got := classifyRel(tt.unit, tt.rel); got != tt.wantRel {
				t.Errorf("classifyRel = %q, want %q", got, tt.wantRel)
			}
		})
	}
}
