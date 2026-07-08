package provider_test

import (
	"context"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/providertest"
)

func TestClassString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		c    provider.Class
		want string
	}{
		{"fact", provider.ClassFact, "fact"},
		{"derived index", provider.ClassDerivedIndex, "derived-index"},
		{"regenerated", provider.ClassRegenerated, "regenerated"},
		{"ignore", provider.ClassIgnore, "ignore"},
		{"unknown out-of-range value", provider.Class(99), "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.c.String(); got != tt.want {
				t.Fatalf("Class(%d).String() = %q, want %q", tt.c, got, tt.want)
			}
		})
	}
}

func TestScopeString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		s    provider.Scope
		want string
	}{
		{"per-project", provider.ScopePerProject, "per-project"},
		{"global", provider.ScopeGlobal, "global"},
		{"unknown out-of-range value", provider.Scope(99), "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.s.String(); got != tt.want {
				t.Fatalf("Scope(%d).String() = %q, want %q", tt.s, got, tt.want)
			}
		})
	}
}

// claudeLikeTable mirrors the spec §6 Claude classification so the contract
// is exercised against the real Phase-3 shape without shipping the adapter.
func claudeLikeTable() []provider.Pattern {
	return []provider.Pattern{
		{Glob: "MEMORY.md", Class: provider.ClassDerivedIndex},
		{Glob: "*.md", Class: provider.ClassFact},
	}
}

func TestClassifyFirstMatchWins(t *testing.T) {
	t.Parallel()
	fake := providertest.New("claude", provider.ScopePerProject, claudeLikeTable())

	tests := []struct {
		name string
		rel  string
		want provider.Class
	}{
		{"derived index beats star-md", "MEMORY.md", provider.ClassDerivedIndex},
		{"topic file is fact", "debugging.md", provider.ClassFact},
		{"nested topic file is fact via default", "notes/deep.md", provider.ClassFact},
		{"unknown extension defaults to fact", "scratch.txt", provider.ClassFact},
		{"unmatched never drops data", "bin/blob.dat", provider.ClassFact},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := provider.Classify(fake, tt.rel); got != tt.want {
				t.Fatalf("Classify(%q) = %v, want %v", tt.rel, got, tt.want)
			}
		})
	}
}

func TestClassifyIgnoreAndRegenerated(t *testing.T) {
	t.Parallel()
	fake := providertest.New("codexlike", provider.ScopeGlobal, []provider.Pattern{
		{Glob: ".lock/**", Class: provider.ClassIgnore},
		{Glob: "memory_summary.md", Class: provider.ClassRegenerated},
		{Glob: "rollout_summaries/*", Class: provider.ClassRegenerated},
		{Glob: "skills/**/SKILL.md", Class: provider.ClassFact},
	})

	tests := []struct {
		rel  string
		want provider.Class
	}{
		{".lock/pid", provider.ClassIgnore},
		{"memory_summary.md", provider.ClassRegenerated},
		{"rollout_summaries/2026-07-08.md", provider.ClassRegenerated},
		{"skills/git/SKILL.md", provider.ClassFact},
		{"skills/SKILL.md", provider.ClassFact}, // ** matches zero segments
		{"raw_memories.md", provider.ClassFact}, // default
	}
	for _, tt := range tests {
		if got := provider.Classify(fake, tt.rel); got != tt.want {
			t.Fatalf("Classify(%q) = %v, want %v", tt.rel, got, tt.want)
		}
	}
}

func TestFakeRecordsReconcileCalls(t *testing.T) {
	t.Parallel()
	fake := providertest.New("claude", provider.ScopePerProject, nil)
	if err := fake.ReconcileIndex(context.Background(), "/tmp/x"); err != nil {
		t.Fatal(err)
	}
	if got := fake.ReconcileCalls(); len(got) != 1 || got[0] != "/tmp/x" {
		t.Fatalf("ReconcileCalls() = %v, want [/tmp/x]", got)
	}
}
