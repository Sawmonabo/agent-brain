package provider_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/providertest"
)

// var _ provider.Provider = (*providertest.Fake)(nil) pins the fake
// against the full interface at compile time: Discover/Identify joining
// Provider must fail this build until the fake implements them too.
var _ provider.Provider = (*providertest.Fake)(nil)

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

// TestClassFromString pins ClassFromString as the exact inverse of
// String(): every valid class string round-trips, and an unrecognized
// string is a load-time error naming the bad value (config-overridable
// classification tables, spec §6, validate strictly at LoadSettings).
func TestClassFromString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		s       string
		want    provider.Class
		wantErr bool
	}{
		{"fact", "fact", provider.ClassFact, false},
		{"derived index", "derived-index", provider.ClassDerivedIndex, false},
		{"regenerated", "regenerated", provider.ClassRegenerated, false},
		{"ignore", "ignore", provider.ClassIgnore, false},
		{"unknown string", "bogus", 0, true},
		{"empty string", "", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := provider.ClassFromString(tt.s)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ClassFromString(%q) error = %v, wantErr %v", tt.s, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("ClassFromString(%q) = %v, want %v", tt.s, got, tt.want)
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
// is exercised against the real classification shape without shipping the adapter.
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

func TestFakeDiscoverReturnsConfiguredResultAndRecordsCalls(t *testing.T) {
	t.Parallel()
	fake := providertest.New("claude", provider.ScopePerProject, nil)
	want := []provider.Discovered{
		{LocalDir: "/home/u/.claude/projects/x/memory", Label: "x", PathGuess: "/home/u/dev/x"},
	}
	fake.DiscoverResult = want

	got, err := fake.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Discover() (-want +got):\n%s", diff)
	}
	// Second call: same result, call count accumulates.
	if _, err := fake.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls := fake.DiscoverCalls(); calls != 2 {
		t.Fatalf("DiscoverCalls() = %d, want 2", calls)
	}
}

func TestFakeDiscoverPropagatesConfiguredError(t *testing.T) {
	t.Parallel()
	fake := providertest.New("claude", provider.ScopePerProject, nil)
	fake.DiscoverErr = errors.New("scan failed")

	got, err := fake.Discover(context.Background())
	if err == nil {
		t.Fatal("Discover() error = nil, want the configured error")
	}
	if got != nil {
		t.Fatalf("Discover() result = %v, want nil alongside the error", got)
	}
}

func TestFakeIdentifyReturnsConfiguredResultAndRecordsCalls(t *testing.T) {
	t.Parallel()
	fake := providertest.New("claude", provider.ScopePerProject, nil)
	fake.IdentifyResult = provider.Identity{ProjectID: "github.com/o/r", PreferredFolder: "r"}
	d := provider.Discovered{LocalDir: "/home/u/.claude/projects/x/memory", Label: "x"}

	got, err := fake.Identify(context.Background(), d, "/home/u/dev/r")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(fake.IdentifyResult, got); diff != "" {
		t.Fatalf("Identify() (-want +got):\n%s", diff)
	}
	want := []providertest.IdentifyCall{{Discovered: d, ProjectPath: "/home/u/dev/r"}}
	if diff := cmp.Diff(want, fake.IdentifyCalls()); diff != "" {
		t.Fatalf("IdentifyCalls() (-want +got):\n%s", diff)
	}
}

func TestFakeIdentifyPropagatesConfiguredError(t *testing.T) {
	t.Parallel()
	fake := providertest.New("claude", provider.ScopePerProject, nil)
	fake.IdentifyErr = errors.New("no remote")

	_, err := fake.Identify(context.Background(), provider.Discovered{}, "/home/u/dev/r")
	if err == nil {
		t.Fatal("Identify() error = nil, want the configured error")
	}
}
