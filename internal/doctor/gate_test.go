package doctor_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/doctor"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func TestSafetyGateHealthyMachineIsNil(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	if err := doctor.SafetyGate(context.Background(), fx.deps.Paths, fx.deps.Registry, fx.deps.BinaryPath); err != nil {
		t.Fatalf("SafetyGate() = %v, want nil", err)
	}
}

// TestSafetyGateNamesTheBrokenAxis breaks exactly one of the four
// sync-blocking axes at a time and asserts the gate's error names it —
// the daemon surfaces this string verbatim as StatusDetail.
func TestSafetyGateNamesTheBrokenAxis(t *testing.T) {
	tests := []struct {
		name    string
		breakIt func(t *testing.T, fx fixture)
		want    string
	}{
		{
			name: "checkout",
			breakIt: func(t *testing.T, fx fixture) {
				t.Helper()
				if err := os.RemoveAll(fx.dir); err != nil {
					t.Fatal(err)
				}
			},
			want: "checkout",
		},
		{
			name: "keyset",
			breakIt: func(t *testing.T, fx fixture) {
				t.Helper()
				if err := os.Remove(fx.deps.Paths.Keyset()); err != nil {
					t.Fatal(err)
				}
			},
			want: "keyset",
		},
		{
			name: "filters",
			breakIt: func(t *testing.T, fx fixture) {
				t.Helper()
				mustGit(t, fx.dir, "config", "--local", "filter.agentbrain.required", "false")
			},
			want: "filters",
		},
		{
			name: "attributes",
			breakIt: func(t *testing.T, fx fixture) {
				t.Helper()
				if err := os.WriteFile(repo.NewLayout(fx.dir).AttributesFile(), []byte("corrupted\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "attributes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fx := newFixture(t)
			tt.breakIt(t, fx)
			err := doctor.SafetyGate(context.Background(), fx.deps.Paths, fx.deps.Registry, fx.deps.BinaryPath)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("SafetyGate() = %v, want an error naming %q", err, tt.want)
			}
		})
	}
}

// TestSafetyGateEmptyBinaryPathFailsClosed pins Q3 gate finding M4 at the
// gate level (doctor_test.go's TestRunFiltersEmptyBinaryPathFailsClosed
// pins it at the checkFilters level): SafetyGate is the exported,
// daemon-facing entry point, so an empty binaryPath argument — however it
// got there — must produce a named failure rather than a vacuous pass.
func TestSafetyGateEmptyBinaryPathFailsClosed(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	err := doctor.SafetyGate(context.Background(), fx.deps.Paths, fx.deps.Registry, "")
	if err == nil || !strings.Contains(err.Error(), "filters") {
		t.Fatalf("SafetyGate() with empty binaryPath = %v, want an error naming filters", err)
	}
}
