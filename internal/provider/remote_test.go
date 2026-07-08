package provider_test

import (
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/provider"
)

func TestNormalizeRemoteURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw, want string
		wantErr   bool
	}{
		{raw: "git@github.com:Sawmonabo/agent-brain.git", want: "github.com/Sawmonabo/agent-brain"},
		{raw: "https://github.com/Sawmonabo/agent-brain.git", want: "github.com/Sawmonabo/agent-brain"},
		{raw: "https://github.com/Sawmonabo/agent-brain", want: "github.com/Sawmonabo/agent-brain"},
		{raw: "ssh://git@github.com/Sawmonabo/agent-brain.git", want: "github.com/Sawmonabo/agent-brain"},
		{raw: "https://gitlab.com/group/sub/project.git", want: "gitlab.com/group/sub/project"},
		{raw: "https://user:tok@github.com/o/r.git", want: "github.com/o/r"}, // credentials stripped, never stored
		{raw: "", wantErr: true},
		{raw: "not a url", wantErr: true},
		{raw: "file:///local/path", wantErr: true}, // machine-local — not a cross-machine identity
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			t.Parallel()
			got, err := provider.NormalizeRemoteURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NormalizeRemoteURL(%q) = %q, want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeRemoteURL(%q) unexpected error: %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeRemoteURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
