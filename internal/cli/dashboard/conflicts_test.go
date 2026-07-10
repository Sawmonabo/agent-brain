package dashboard

import (
	"errors"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/config"
)

func TestConflictsView(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		records    []config.ConflictRecord
		err        error
		loaded     bool
		wantSubstr []string
	}{
		{
			name:       "not loaded shows loading",
			loaded:     false,
			wantSubstr: []string{"Conflicts", "loading"},
		},
		{
			name:       "loaded empty shows reassurance",
			loaded:     true,
			wantSubstr: []string{"no retained conflicts"},
		},
		{
			name:   "records render time path mode",
			loaded: true,
			records: []config.ConflictRecord{
				{Time: "2026-07-09T11:00:00Z", Path: "MEMORY.md", Mode: "retain-both"},
			},
			wantSubstr: []string{"TIME", "PATH", "MODE", "2026-07-09T11:00:00Z", "MEMORY.md", "retain-both"},
		},
		{
			name:       "error surfaces plainly",
			loaded:     true,
			err:        errors.New("permission denied"),
			wantSubstr: []string{"conflict log unavailable", "permission denied"},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var view conflictsView
			if testCase.loaded {
				view.set(testCase.records, testCase.err)
			}
			body := plain(view.view())
			for _, want := range testCase.wantSubstr {
				if !strings.Contains(body, want) {
					t.Errorf("conflicts view missing %q; got:\n%s", want, body)
				}
			}
		})
	}
}
