package cli

import "testing"

func TestRoot(t *testing.T) {
	t.Parallel()
	root := Root()
	if root.Use != "agent-brain" {
		t.Fatalf("root.Use = %q, want %q", root.Use, "agent-brain")
	}
	if root.Version == "" {
		t.Fatal("root.Version is empty; want a default like \"dev\"")
	}
}
