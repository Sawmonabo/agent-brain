// Package providertest provides a configurable in-memory Provider for
// engine, repo, and daemon tests.
package providertest

import (
	"context"
	"sync"

	"github.com/Sawmonabo/agent-brain/internal/provider"
)

// IdentifyCall records one Identify invocation's arguments.
type IdentifyCall struct {
	Discovered  provider.Discovered
	ProjectPath string
}

// Fake implements provider.Provider with a fixed table and recorded
// ReconcileIndex/Discover/Identify calls. Safe for concurrent use.
type Fake struct {
	name     string
	scope    provider.Scope
	patterns []provider.Pattern

	mu             sync.Mutex
	reconcileCalls []string
	// ReconcileFunc, when non-nil, runs inside ReconcileIndex after the
	// call is recorded — lets tests mutate the dir or inject errors.
	ReconcileFunc func(ctx context.Context, dir string) error

	discoverCalls int
	// DiscoverResult is returned by Discover; DiscoverErr, when non-nil,
	// short-circuits it instead.
	DiscoverResult []provider.Discovered
	DiscoverErr    error

	identifyCalls []IdentifyCall
	// IdentifyResult is returned by Identify; IdentifyErr, when non-nil,
	// short-circuits it instead.
	IdentifyResult provider.Identity
	IdentifyErr    error
}

// New constructs a Fake. patterns may be nil (everything ClassFact).
func New(name string, scope provider.Scope, patterns []provider.Pattern) *Fake {
	return &Fake{name: name, scope: scope, patterns: patterns}
}

// Name returns the name New was constructed with.
func (f *Fake) Name() string { return f.name }

// Scope returns the scope New was constructed with.
func (f *Fake) Scope() provider.Scope { return f.scope }

// Patterns returns the pattern table New was constructed with.
func (f *Fake) Patterns() []provider.Pattern { return f.patterns }

// ReconcileIndex records dir, then delegates to ReconcileFunc if set.
func (f *Fake) ReconcileIndex(ctx context.Context, dir string) error {
	f.mu.Lock()
	f.reconcileCalls = append(f.reconcileCalls, dir)
	fn := f.ReconcileFunc
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, dir)
	}
	return nil
}

// ReconcileCalls returns the dirs ReconcileIndex was called with, in order.
func (f *Fake) ReconcileCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reconcileCalls...)
}

// Discover records the call, then returns DiscoverResult, or
// DiscoverErr instead when set.
func (f *Fake) Discover(_ context.Context) ([]provider.Discovered, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.discoverCalls++
	if f.DiscoverErr != nil {
		return nil, f.DiscoverErr
	}
	return f.DiscoverResult, nil
}

// DiscoverCalls returns how many times Discover was called.
func (f *Fake) DiscoverCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.discoverCalls
}

// Identify records d and projectPath, then returns IdentifyResult, or
// IdentifyErr instead when set.
func (f *Fake) Identify(_ context.Context, d provider.Discovered, projectPath string) (provider.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.identifyCalls = append(f.identifyCalls, IdentifyCall{Discovered: d, ProjectPath: projectPath})
	if f.IdentifyErr != nil {
		return provider.Identity{}, f.IdentifyErr
	}
	return f.IdentifyResult, nil
}

// IdentifyCalls returns the arguments Identify was called with, in order.
func (f *Fake) IdentifyCalls() []IdentifyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]IdentifyCall(nil), f.identifyCalls...)
}
