package cli

import (
	"errors"
	"io"
	"os"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/spf13/pflag"
)

// stubReExecModel is a tea.Model that also answers ReExecRequested — the seam
// maybeReExec asserts against, so the re-exec decision is testable with a
// recording execFn instead of a real bubbletea program and syscall.Exec.
type stubReExecModel struct{ requested bool }

func (m stubReExecModel) Init() tea.Cmd                       { return nil }
func (m stubReExecModel) Update(tea.Msg) (tea.Model, tea.Cmd) { return m, nil }
func (m stubReExecModel) View() tea.View                      { return tea.NewView("") }
func (m stubReExecModel) ReExecRequested() bool               { return m.requested }

// stubBareModel is a tea.Model with NO ReExecRequested method — the defensive
// path where the final model is not a re-exec requester at all.
type stubBareModel struct{}

func (m stubBareModel) Init() tea.Cmd                       { return nil }
func (m stubBareModel) Update(tea.Msg) (tea.Model, tea.Cmd) { return m, nil }
func (m stubBareModel) View() tea.View                      { return tea.NewView("") }

// TestMaybeReExec pins launchHub's post-Run re-exec seam (spec §11): a model
// that latched the R restart execs the resolved binary with the current argv
// and environment (so the restarted hub is the same invocation on the new
// binary); every other case leaves the process alone, and an exec failure
// propagates.
func TestMaybeReExec(t *testing.T) {
	t.Parallel()
	binary, err := resolveBinary()
	if err != nil {
		t.Fatalf("resolveBinary: %v", err)
	}

	t.Run("requested re-exec runs the resolved binary with argv+env", func(t *testing.T) {
		t.Parallel()
		called := false
		var gotArgv0 string
		var gotArgv, gotEnv []string
		exec := func(argv0 string, argv, env []string) error {
			called, gotArgv0, gotArgv, gotEnv = true, argv0, argv, env
			return nil
		}
		if err := maybeReExec(stubReExecModel{requested: true}, exec); err != nil {
			t.Fatalf("maybeReExec returned %v", err)
		}
		if !called {
			t.Fatal("execFn not called when a re-exec was requested")
		}
		if gotArgv0 != binary {
			t.Errorf("argv0 = %q, want the resolved binary %q", gotArgv0, binary)
		}
		if diff := cmp.Diff(os.Args, gotArgv); diff != "" {
			t.Errorf("argv mismatch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(os.Environ(), gotEnv); diff != "" {
			t.Errorf("env mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("no request leaves the process alone", func(t *testing.T) {
		t.Parallel()
		called := false
		exec := func(string, []string, []string) error { called = true; return nil }
		if err := maybeReExec(stubReExecModel{requested: false}, exec); err != nil {
			t.Fatalf("maybeReExec returned %v", err)
		}
		if called {
			t.Error("execFn called when no re-exec was requested")
		}
	})

	t.Run("a non-requester model does not exec", func(t *testing.T) {
		t.Parallel()
		called := false
		exec := func(string, []string, []string) error { called = true; return nil }
		if err := maybeReExec(stubBareModel{}, exec); err != nil {
			t.Fatalf("maybeReExec returned %v", err)
		}
		if called {
			t.Error("execFn called for a model that is not a re-exec requester")
		}
	})

	t.Run("exec failure propagates", func(t *testing.T) {
		t.Parallel()
		wantErr := errors.New("exec: no such file or directory")
		exec := func(string, []string, []string) error { return wantErr }
		if err := maybeReExec(stubReExecModel{requested: true}, exec); !errors.Is(err, wantErr) {
			t.Errorf("maybeReExec err = %v, want %v", err, wantErr)
		}
	})
}

// TestDecideHubEntryMatrix pins spec §1's bare-invocation matrix (ADR 20
// decision 1) as a pure function: all 8 (initialized × tty × agentEnv)
// combinations. decideHubEntry is total even over rows runHub itself never
// reaches with tty=false (runHub's own TTY gate routes those to their
// wording before ever consulting this function) — the total definition here
// is the executable specification the matrix pins, independent of exactly
// which rows production code happens to walk through it.
func TestDecideHubEntryMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		initialized bool
		tty         bool
		agentEnv    bool
		want        hubEntryDecision
	}{
		// Initialized + TTY: the hub opens regardless of agent-env — the
		// wizard risk ADR 20 D1 gates against does not exist once the
		// machine is already set up; there is no wizard to protect an
		// agent from here.
		{name: "initialized, tty, no agent env", initialized: true, tty: true, agentEnv: false, want: hubOpen},
		{name: "initialized, tty, agent env", initialized: true, tty: true, agentEnv: true, want: hubOpen},

		// Initialized + non-TTY: a refusal regardless of agent-env (runHub
		// picks the dashboard-refusal wording from the initialized flag it
		// already has, not from this enum value).
		{name: "initialized, non-tty, no agent env", initialized: true, tty: false, agentEnv: false, want: hubPointerExit},
		{name: "initialized, non-tty, agent env", initialized: true, tty: false, agentEnv: true, want: hubPointerExit},

		// Uninitialized + human TTY + no agent fingerprint: the only row
		// that launches guided init.
		{name: "uninitialized, tty, no agent env", initialized: false, tty: true, agentEnv: false, want: hubGuidedInit},

		// Uninitialized + TTY + agent env: gated even with a real TTY — an
		// agent cannot answer the wizard's interactive forms.
		{name: "uninitialized, tty, agent env", initialized: false, tty: true, agentEnv: true, want: hubPointerExit},

		// Uninitialized + non-TTY: the pointer, regardless of agent-env.
		{name: "uninitialized, non-tty, no agent env", initialized: false, tty: false, agentEnv: false, want: hubPointerExit},
		{name: "uninitialized, non-tty, agent env", initialized: false, tty: false, agentEnv: true, want: hubPointerExit},
	}
	if len(tests) != 8 {
		t.Fatalf("matrix has %d rows, want all 8 initialized×tty×agentEnv combinations", len(tests))
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := decideHubEntry(tt.initialized, tt.tty, tt.agentEnv)
			if got != tt.want {
				t.Errorf("decideHubEntry(initialized=%v, tty=%v, agentEnv=%v) = %v, want %v",
					tt.initialized, tt.tty, tt.agentEnv, got, tt.want)
			}
		})
	}
}

// TestAgentEnvDetected exercises every fingerprint variable in agentEnvVars
// individually (set to a non-empty value → true; set but empty → false),
// plus the all-clear case — a fake getenv map, never the real process
// environment, so every row runs under t.Parallel without t.Setenv
// interference.
func TestAgentEnvDetected(t *testing.T) {
	t.Parallel()

	type testCase struct {
		name string
		env  map[string]string
		want bool
	}

	tests := []testCase{
		{name: "no fingerprint variables set", env: map[string]string{}, want: false},
	}
	for _, fingerprintVariable := range agentEnvVars {
		tests = append(
			tests,
			testCase{
				name: fingerprintVariable + " set to a non-empty value",
				env:  map[string]string{fingerprintVariable: "1"},
				want: true,
			},
			testCase{
				name: fingerprintVariable + " set but empty",
				env:  map[string]string{fingerprintVariable: ""},
				want: false,
			},
		)
	}
	if len(tests) != 1+2*len(agentEnvVars) {
		t.Fatalf("got %d cases, want one none-set case plus two per fingerprint variable", len(tests))
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(key string) string { return tt.env[key] }
			if got := agentEnvDetected(getenv); got != tt.want {
				t.Errorf("agentEnvDetected() = %v, want %v (env=%v)", got, tt.want, tt.env)
			}
		})
	}
}

// TestAgentEnvVarsExactList pins the fingerprint list against the spec §1
// roster by value: the matrix and detection tests range over agentEnvVars
// itself, so without this pin an accidental drop or addition would stay
// self-consistently green.
func TestAgentEnvVarsExactList(t *testing.T) {
	t.Parallel()
	want := []string{
		"CLAUDECODE", "CURSOR_AGENT", "CODEX_SANDBOX", "CODEX_THREAD_ID",
		"CODEX_CI", "GEMINI_CLI", "CLINE_ACTIVE", "OPENCODE",
		"OPENCLAW_SHELL", "ANTIGRAVITY_CLI_ALIAS",
	}
	if diff := cmp.Diff(want, agentEnvVars); diff != "" {
		t.Fatalf("agentEnvVars drifted from the spec §1 fingerprint roster (-want +got):\n%s", diff)
	}
}

// TestGuidedInitStateMatchesInitFlagDefaults holds guidedInitState and
// newInitCmd's flag-driven literal together: the hub's guided first run
// must behave exactly like unflagged `agent-brain init`, and the two are
// deliberately independent literals (neither delegates to the other), so
// every registered flag default is compared against the guided state's
// corresponding field, and a completeness guard forces any FUTURE init
// flag to be either mirrored into guidedInitState or consciously recorded
// here.
func TestGuidedInitStateMatchesInitFlagDefaults(t *testing.T) {
	t.Parallel()
	state := guidedInitState(io.Discard, nil)
	flags := newInitCmd().Flags()

	defaultOf := func(name string) string {
		flag := flags.Lookup(name)
		if flag == nil {
			t.Fatalf("init flag --%s no longer exists — update the guided-init equivalence pin", name)
		}
		return flag.DefValue
	}

	boolPins := []struct {
		flag string
		got  bool
	}{
		{"non-interactive", state.nonInteractive},
		{"generate-key", state.generateKey},
		{"import-key", state.importKey},
		{"skip-service", state.skipService},
	}
	for _, pin := range boolPins {
		if want := defaultOf(pin.flag) == "true"; pin.got != want {
			t.Errorf("guidedInitState.%s = %v, but --%s defaults to %q", pin.flag, pin.got, pin.flag, defaultOf(pin.flag))
		}
	}
	if state.enrollMode != defaultOf("enroll") {
		t.Errorf("guidedInitState.enrollMode = %q, but --enroll defaults to %q", state.enrollMode, defaultOf("enroll"))
	}
	if state.repoName != defaultOf("repo-name") {
		t.Errorf("guidedInitState.repoName = %q, but --repo-name defaults to %q", state.repoName, defaultOf("repo-name"))
	}
	if state.importArmored != "" {
		t.Errorf("guidedInitState.importArmored = %q, want empty (import-key is flag-only)", state.importArmored)
	}

	handled := map[string]bool{
		"non-interactive": true,
		"generate-key":    true,
		"import-key":      true,
		"skip-service":    true,
		"enroll":          true,
		"repo-name":       true,
	}
	flags.VisitAll(func(flag *pflag.Flag) {
		if !handled[flag.Name] {
			t.Errorf("init flag --%s is not covered by the guided-init equivalence pin — mirror its default into guidedInitState or record it here", flag.Name)
		}
	})
}
