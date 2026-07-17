package dashboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Sawmonabo/agent-brain/internal/doctor"
	"github.com/Sawmonabo/agent-brain/internal/ghx"
)

// ghRow builds a doctor report whose gh check has the given status and detail —
// the one row the attention logic reads.
func ghReport(status doctor.Status, detail string) doctor.Report {
	return doctor.Report{Results: []doctor.CheckResult{
		{Name: "gh", Status: status, Detail: detail, Fix: "run `gh auth login`"},
	}}
}

// authInvalidDetail is a real `gh auth status` failure line (the doctor probe's
// Detail on an expired keyring token), the exact corpus the classifier keys on.
const authInvalidDetail = "gh auth status: The token in keyring is invalid. (run `gh auth login`)"

// --- Step 2: surfacing ---

func TestAuthAttentionSurfacesInHeader(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{})
	m.status = readyStatus()

	if got := plain(m.statusHeader()); strings.Contains(got, "gh auth invalid") {
		t.Fatalf("header shows the attention with a valid token: %q", got)
	}

	m.authInvalid = true
	got := plain(m.statusHeader())
	for _, want := range []string{"gh auth invalid", "Doctor tab", "f re-authenticates"} {
		if !strings.Contains(got, want) {
			t.Errorf("armed header %q missing %q", got, want)
		}
	}
	// It joins the existing status line, adding no header row — the frame-height
	// invariant Task 1's exact-fill frames depend on.
	if lines := strings.Count(got, "\n"); lines != 0 {
		t.Errorf("attention added %d header rows; want it inline on the status line (%q)", lines, got)
	}
}

// TestAuthAttentionIsStickyNotToast pins that the segment is driven by a flag no
// timer clears: it survives a clock advance that expires an info toast beside
// it. That contrast is the whole point — it is not a TTL toast.
func TestAuthAttentionIsStickyNotToast(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{})
	m.status = readyStatus()
	m.now = time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	m.authInvalid = true
	m.pushToast("ephemeral note") // an info toast, visible-stamped at m.now

	// Advance the model clock well past the toast TTL and run the lifecycle.
	m.now = m.now.Add(toastTTL + time.Second)
	m.advanceToasts()

	if m.toast != nil {
		t.Fatal("info toast did not expire; the sticky contrast is not established")
	}
	if got := plain(m.statusHeader()); !strings.Contains(got, "gh auth invalid") {
		t.Fatalf("attention vanished after the toast TTL elapsed (not sticky): %q", got)
	}
}

// --- Step 3: detection ---

func TestUpdateCheckAuthInvalidArmsAttention(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		msg  updateCheckedMsg
		want bool
	}{
		{
			name: "auth-invalid error arms",
			msg:  updateCheckedMsg{err: fmt.Errorf("gh release list --repo owner/repo: HTTP 401: Bad credentials: %w", ghx.ErrAuthInvalid)},
			want: true,
		},
		{
			name: "non-auth error does not arm",
			msg:  updateCheckedMsg{err: errors.New("gh release list --repo owner/repo: HTTP 404: Not Found")},
			want: false,
		},
		{
			name: "clean up-to-date check does not arm",
			msg:  updateCheckedMsg{tag: "", err: nil},
			want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			m := newTestModel(&fakeData{})
			m, _ = step(m, test.msg)
			if m.authInvalid != test.want {
				t.Errorf("authInvalid = %v, want %v", m.authInvalid, test.want)
			}
		})
	}
}

// TestUpdateCheckAuthInvalidStillNoBanner pins that the auth-invalid check arms
// the attention WITHOUT ever opening an update banner — the two are mutually
// exclusive, and a dead token must never masquerade as an offer.
func TestUpdateCheckAuthInvalidStillNoBanner(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{})
	m, _ = step(m, updateCheckedMsg{err: fmt.Errorf("boom: %w", ghx.ErrAuthInvalid)})
	if !m.authInvalid {
		t.Fatal("auth-invalid check did not arm the attention")
	}
	if m.updatePhase != updateIdle {
		t.Errorf("updatePhase = %v, want updateIdle (no banner on an auth-invalid check)", m.updatePhase)
	}
}

func TestDoctorReportFeedsAuthAttention(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		start   bool // authInvalid before the report lands
		report  doctor.Report
		wantEnd bool
	}{
		{
			name:    "auth-invalid gh row arms",
			start:   false,
			report:  ghReport(doctor.StatusFail, authInvalidDetail),
			wantEnd: true,
		},
		{
			name:    "passing gh row clears a prior attention",
			start:   true,
			report:  ghReport(doctor.StatusOK, "gh installed and authenticated"),
			wantEnd: false,
		},
		{
			name:    "offline gh row leaves a prior attention untouched",
			start:   true,
			report:  ghReport(doctor.StatusFail, "gh auth status: dial tcp: lookup api.github.com: no such host"),
			wantEnd: true,
		},
		{
			name:    "report with no gh row leaves it untouched",
			start:   true,
			report:  doctor.Report{Results: []doctor.CheckResult{{Name: "daemon", Status: doctor.StatusFail, Detail: "down"}}},
			wantEnd: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			m := newTestModel(&fakeData{})
			m.authInvalid = test.start
			m, _ = step(m, doctorMsg{report: test.report})
			if m.authInvalid != test.wantEnd {
				t.Errorf("authInvalid = %v, want %v", m.authInvalid, test.wantEnd)
			}
		})
	}
}

// --- Step 4/5: the re-auth handoff ---

// writeMarkerFakeGH installs a fake `gh` that records which subcommand ran by
// touching a marker file — the e2e fakegh precedent, scoped to the two
// subcommands the handoff drives. auth login proves the child ran; auth status
// (exit 0) proves the re-probe fired and lets it report success.
func writeMarkerFakeGH(t *testing.T, dir, loginMarker, statusMarker string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
set -eu
case "${1:-} ${2:-}" in
"auth login") : > %q ;;
"auth status") : > %q; exit 0 ;;
*) echo "fakegh: unhandled: $*" >&2; exit 2 ;;
esac
`, loginMarker, statusMarker)
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	return path
}

func TestDoctorFixReauthHandoff(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	loginMarker := filepath.Join(dir, "login-ran")
	statusMarker := filepath.Join(dir, "status-ran")
	fakeGH := writeMarkerFakeGH(t, dir, loginMarker, statusMarker)

	probeCalls := 0
	cfg := Config{
		Data:         &fakeData{},
		StartService: func() error { return nil },
		RunDoctorFix: func(context.Context) (doctor.Report, error) {
			t.Error("standard doctor --fix ran; want the re-auth handoff")
			return doctor.Report{}, nil
		},
		ReauthGH: func() *exec.Cmd { return exec.Command(fakeGH, "auth", "login", "-h", "github.com") },
		ProbeGHAuth: func(context.Context) error {
			probeCalls++
			return exec.Command(fakeGH, "auth", "status").Run()
		},
	}
	m := New(cfg)
	m.active = tabDoctor
	m.authInvalid = true
	m.doctor.Set(ghReport(doctor.StatusFail, authInvalidDetail), nil)

	// f routes to the handoff: the Cmd is tea.ExecProcess's deferred request (NOT
	// ghAuthFinishedMsg), and merely running it must not launch gh — only the
	// suspended program loop does, the editor-handoff contract.
	m, cmd := step(m, key("f"))
	if cmd == nil {
		t.Fatal("f on the gh-invalid row produced no Cmd; want the re-auth handoff")
	}
	if _, ok := cmd().(ghAuthFinishedMsg); ok {
		t.Fatal("f ran gh directly; the handoff must go through tea.ExecProcess (suspend/resume)")
	}
	if _, err := os.Stat(loginMarker); err == nil {
		t.Fatal("running the dispatch Cmd launched gh outside the program loop")
	}

	// The child DOES run under the suspended program: the command ReauthGH builds
	// writes its marker when actually executed — what tea.ExecProcess does on
	// suspend. Proving the command is correctly constructed proves the child runs.
	if err := cfg.ReauthGH().Run(); err != nil {
		t.Fatalf("reauth command run: %v", err)
	}
	if _, err := os.Stat(loginMarker); err != nil {
		t.Fatalf("gh auth login child did not run (no marker): %v", err)
	}

	// The return path: ghAuthFinishedMsg re-asserts 1007 and fires the re-probe.
	_, finishCmd := step(m, ghAuthFinishedMsg{})
	probeMsgs := drain(finishCmd)
	if probeCalls == 0 {
		t.Fatal("ghAuthFinishedMsg did not fire the re-probe")
	}
	if _, err := os.Stat(statusMarker); err != nil {
		t.Fatalf("re-probe did not run gh auth status (no marker): %v", err)
	}
	if !containsMsg[ghAuthProbedMsg](probeMsgs) {
		t.Fatalf("ghAuthFinishedMsg did not yield a ghAuthProbedMsg: %v", probeMsgs)
	}

	// A successful re-probe clears the sticky attention.
	cleared, _ := step(m, ghAuthProbedMsg{err: nil})
	if cleared.authInvalid {
		t.Fatal("a successful re-probe did not clear the attention")
	}
	if got := plain(cleared.statusHeader()); strings.Contains(got, "gh auth invalid") {
		t.Fatalf("header still shows the attention after a successful re-probe: %q", got)
	}
}

func TestDoctorFixStandardPathWhenNotAuthInvalid(t *testing.T) {
	t.Parallel()
	fixRan := false
	reauthBuilt := false
	cfg := Config{
		Data:         &fakeData{},
		StartService: func() error { return nil },
		RunDoctorFix: func(context.Context) (doctor.Report, error) {
			fixRan = true
			return ghReport(doctor.StatusOK, "gh installed and authenticated"), nil
		},
		ReauthGH: func() *exec.Cmd {
			reauthBuilt = true
			return exec.Command("false")
		},
		ProbeGHAuth: func(context.Context) error { return nil },
	}
	m := New(cfg)
	m.active = tabDoctor
	m.authInvalid = false
	// A fixable, non-gh failure so f is offered but auth is not the issue.
	m.doctor.Set(doctor.Report{Results: []doctor.CheckResult{
		{Name: "filters", Status: doctor.StatusFail, Detail: "clean filter missing", Fix: "run `agent-brain doctor --fix`"},
	}}, nil)

	m, cmd := step(m, key("f"))
	drain(cmd)
	if reauthBuilt {
		t.Error("a non-auth failure built the re-auth command; want the standard fix")
	}
	if !fixRan {
		t.Error("the standard doctor --fix did not run")
	}
}

func TestGHAuthProbeFailureKeepsAttention(t *testing.T) {
	t.Parallel()
	m := newTestModel(&fakeData{})
	m.authInvalid = true

	m, _ = step(m, ghAuthProbedMsg{err: errors.New("still bad")})
	if !m.authInvalid {
		t.Fatal("a failed re-probe cleared the attention; it must stay until a probe succeeds")
	}
	if m.stickyToast == nil || !strings.Contains(m.stickyToast.text, "gh auth login -h github.com") {
		t.Fatalf("failure toast does not name the manual command: %+v", m.stickyToast)
	}
}

// TestGHReauthFinishReassertsAlternateScroll mirrors the editor round-trip: the
// gh handoff's return re-asserts 1007 (ADR 21) through the shared seam, on the
// same enabled/disabled gate the editor path uses.
func TestGHReauthFinishReassertsAlternateScroll(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		enabled bool
		want    bool
	}{
		{name: "enabled re-asserts", enabled: true, want: true},
		{name: "disabled re-asserts nothing", enabled: false, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			m := New(Config{Data: &fakeData{}, ProbeGHAuth: func(context.Context) error { return nil }})
			m.settings.Dashboard.AlternateScroll = test.enabled

			_, cmd := step(m, ghAuthFinishedMsg{})
			found := false
			for _, message := range drain(cmd) {
				if raw, ok := message.(tea.RawMsg); ok && fmt.Sprint(raw.Msg) == "\x1b[?1007h" {
					found = true
				}
			}
			if found != test.want {
				t.Errorf("ghAuthFinishedMsg emitted set-1007 RawMsg = %v, want %v", found, test.want)
			}
		})
	}
}
