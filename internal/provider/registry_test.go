package provider_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/providertest"
)

func TestRegistryDeterministicOrderAndLookup(t *testing.T) {
	t.Parallel()
	codex := providertest.New("codex", provider.ScopeGlobal, nil)
	claude := providertest.New("claude", provider.ScopePerProject, nil)

	reg, err := provider.NewRegistry(codex, claude)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range reg.All() {
		names = append(names, p.Name())
	}
	if diff := cmp.Diff([]string{"claude", "codex"}, names); diff != "" {
		t.Fatalf("All() order (-want +got):\n%s", diff)
	}
	if _, ok := reg.Get("claude"); !ok {
		t.Fatal("Get(claude) = false, want true")
	}
	if _, ok := reg.Get("gemini"); ok {
		t.Fatal("Get(gemini) = true, want false")
	}
}

func TestRegistryRejectsDuplicatesBadNamesAndBadGlobs(t *testing.T) {
	t.Parallel()
	a := providertest.New("claude", provider.ScopePerProject, nil)
	b := providertest.New("claude", provider.ScopePerProject, nil)
	if _, err := provider.NewRegistry(a, b); err == nil {
		t.Fatal("duplicate provider name accepted; want error")
	}
	bad := providertest.New("bad", provider.ScopePerProject, []provider.Pattern{
		{Glob: "bad[range.md", Class: provider.ClassFact},
	})
	if _, err := provider.NewRegistry(bad); err == nil {
		t.Fatal("malformed glob accepted at construction; want error")
	}
	// Provider names become repo path segments (<project>/<name>/) and
	// .gitattributes pattern segments — the interface contract says
	// lowercase and path-safe, and the registry is where it's enforced.
	for _, name := range []string{"", "Claude", "co dex", "a/b", "..", "_global", ".hidden"} {
		p := providertest.New(name, provider.ScopePerProject, nil)
		if _, err := provider.NewRegistry(p); err == nil {
			t.Fatalf("provider name %q accepted; want error", name)
		}
	}
}
