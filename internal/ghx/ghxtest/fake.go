// Package ghxtest provides a scripted ghx.Runner test double so any package
// that depends on ghx.Client can be tested without a real gh binary or
// network access.
package ghxtest

import (
	"context"
	"slices"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/ghx"
)

// Call is one scripted expectation: Run must be invoked with exactly Args
// (in order); the invocation returns Result and Err.
type Call struct {
	Args   []string
	Result ghx.Result
	Err    error
}

// Fake is an ordered, scripted ghx.Runner: it expects Run to be called with
// exactly the given Calls, in order. A call whose args don't match the next
// expectation, or one made after every expectation is consumed, fails the
// test immediately via TB — never returned as a "soft" error the code under
// test might swallow. New also registers a Cleanup that fails the test if
// fewer calls than expected were made: a scripted mock is a contract on both
// ends.
type Fake struct {
	tb    testing.TB
	calls []Call
	next  int
}

// New builds a Fake expecting exactly calls, in order.
func New(tb testing.TB, calls ...Call) *Fake {
	tb.Helper()
	fake := &Fake{tb: tb, calls: calls}
	tb.Cleanup(func() {
		if fake.next != len(fake.calls) {
			tb.Errorf("ghxtest.Fake: %d of %d expected calls were made", fake.next, len(fake.calls))
		}
	})
	return fake
}

// Run implements ghx.Runner.
func (f *Fake) Run(_ context.Context, args ...string) (ghx.Result, error) {
	f.tb.Helper()
	if f.next >= len(f.calls) {
		f.tb.Fatalf("ghxtest.Fake: unexpected call gh %v (all %d expectations already consumed)", args, len(f.calls))
		return ghx.Result{}, nil
	}
	expected := f.calls[f.next]
	f.next++
	if !slices.Equal(expected.Args, args) {
		f.tb.Fatalf("ghxtest.Fake: call %d args = gh %v, want gh %v", f.next, args, expected.Args)
	}
	return expected.Result, expected.Err
}
