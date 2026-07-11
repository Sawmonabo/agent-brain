package provider_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/provider"
)

func TestNamedIdentity(t *testing.T) {
	t.Parallel()
	got := provider.NamedIdentity("notes")
	want := provider.Identity{ProjectID: "named/notes", PreferredFolder: "notes"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("NamedIdentity mismatch (-want +got):\n%s", diff)
	}
}

// TestNamedIdentityStaysTwoSegments pins the collision contract: a named id
// has exactly 2 slash-separated segments, so it can never collide with a
// remote-derived id (always ≥ 3 segments: host/owner/repo...). The folder
// name itself contains no "/" — repo.ValidateFolderName guarantees that at
// every prompt and the daemon re-checks it on Track.
func TestNamedIdentityStaysTwoSegments(t *testing.T) {
	t.Parallel()
	for _, folderName := range []string{"notes", "my-project", "a_b.c"} {
		id := provider.NamedIdentity(folderName).ProjectID
		if got := strings.Count(id, "/"); got != 1 {
			t.Errorf("NamedIdentity(%q).ProjectID = %q has %d slashes, want exactly 1", folderName, id, got)
		}
	}
}
