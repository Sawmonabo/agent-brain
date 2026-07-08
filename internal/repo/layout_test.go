package repo_test

import (
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func TestLayoutPaths(t *testing.T) {
	t.Parallel()
	l := repo.NewLayout("/data/memories")

	tests := []struct{ name, got, want string }{
		{"root", l.Root(), "/data/memories"},
		{"meta", l.MetaDir(), filepath.Join("/data/memories", ".agent-brain")},
		{"projects", l.ProjectsFile(), filepath.Join("/data/memories", ".agent-brain", "projects.toml")},
		{"manifests", l.ManifestDir(), filepath.Join("/data/memories", ".agent-brain", "manifests")},
		{"manifest file sanitizes", l.ManifestFile("host/../x"), filepath.Join("/data/memories", ".agent-brain", "manifests", "host-..-x.json")},
		{"attributes", l.AttributesFile(), filepath.Join("/data/memories", ".gitattributes")},
		{"project unit", l.UnitDir("agent-brain", "claude"), filepath.Join("/data/memories", "agent-brain", "claude")},
		{"global unit", l.UnitDir(repo.GlobalFolder, "codex"), filepath.Join("/data/memories", "_global", "codex")},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Fatalf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}
