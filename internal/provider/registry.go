package provider

import (
	"fmt"
	"regexp"
	"sort"
)

// Registry holds the configured providers with deterministic iteration
// order (sorted by name). Immutable after construction.
type Registry struct {
	byName  map[string]Provider
	ordered []Provider
}

// nameRE pins the Provider.Name contract: lowercase, path-safe, starts
// alphanumeric — names become repo path segments and .gitattributes
// pattern segments, so '_'-prefixed (reserved: _global), '.'-prefixed,
// separator-bearing, and uppercase names are all construction errors.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// NewRegistry validates and indexes providers: names must be unique and
// satisfy the name contract; every pattern glob must validate. Fail-fast
// contract — a bad table is a startup error, never a silent
// misclassification or a corrupted attributes file.
func NewRegistry(providers ...Provider) (*Registry, error) {
	registry := &Registry{byName: make(map[string]Provider, len(providers))}
	for _, p := range providers {
		name := p.Name()
		if !nameRE.MatchString(name) {
			return nil, fmt.Errorf("provider name %q violates the name contract (%s)", name, nameRE)
		}
		if _, dup := registry.byName[name]; dup {
			return nil, fmt.Errorf("duplicate provider %q", name)
		}
		for _, pat := range p.Patterns() {
			if err := ValidateGlob(pat.Glob); err != nil {
				return nil, fmt.Errorf("provider %q: %w", name, err)
			}
		}
		registry.byName[name] = p
		registry.ordered = append(registry.ordered, p)
	}
	sort.Slice(registry.ordered, func(i, j int) bool {
		return registry.ordered[i].Name() < registry.ordered[j].Name()
	})
	return registry, nil
}

// Get returns the provider registered under name.
func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.byName[name]
	return p, ok
}

// All returns every provider, sorted by name. Callers must not mutate
// the returned slice.
func (r *Registry) All() []Provider {
	return r.ordered
}
