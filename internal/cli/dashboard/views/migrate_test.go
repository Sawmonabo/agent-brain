package views

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/provider"
)

// migrateCandidate is the shared fixture the migrate-flow tests drive: one
// bash-era store whose guessed path resolves to a remote-backed project.
func migrateCandidateFixture() MigrateCandidate {
	return MigrateCandidate{
		Provider:  "claude",
		Slug:      "-g-acme",
		SeedDir:   "/home/u/.agent-brain/-g-acme",
		PathGuess: "/g/acme",
		LiveDir:   "/home/u/.claude/projects/-g-acme/memory",
	}
}

// TestMigrateHappyPathSubmitsFieldExactRequest walks the whole spec §10 flow
// through fakes — m → preflight → discover → pick → confirm → identify →
// Migrate — and pins the submitted request field-for-field: SeedDir is the
// legacy tree, LocalDir is the candidate's precomputed live dir (the path was
// accepted unchanged), and the identity's ProjectID/PreferredFolder carry
// through.
func TestMigrateHappyPathSubmitsFieldExactRequest(t *testing.T) {
	t.Parallel()
	fake := &fakeData{migrateResp: api.MigrateResponse{Folder: "acme", Files: 5}}
	candidate := migrateCandidateFixture()
	identity := provider.Identity{ProjectID: "github.com/o/acme", PreferredFolder: "acme"}
	migrate := migrateActionsFor([]MigrateCandidate{candidate}, identity, nil, "/unused/corrected")
	view := NewProjectsView()

	driveMigrate(t, &view, fake, migrate, key("m")) // preflight → discover → picker
	if view.Migrating != MigratePicking {
		t.Fatalf("after m: Migrating = %v, want MigratePicking", view.Migrating)
	}
	if got := plain(view.migrateView()); !strings.Contains(got, "-g-acme → /g/acme") {
		t.Fatalf("picker = %q, want the 'slug → path guess' row", got)
	}

	driveMigrate(t, &view, fake, migrate, key("enter")) // pick → path confirm, prefilled with PathGuess
	if got := plain(view.migrateView()); !strings.Contains(got, "/g/acme") {
		t.Fatalf("path-confirm view = %q, want the PathGuess prefill visible", got)
	}
	driveMigrate(t, &view, fake, migrate, key("enter")) // accept path → identify → Migrate

	want := []api.MigrateRequest{{
		Provider:        "claude",
		ProjectID:       "github.com/o/acme",
		PreferredFolder: "acme",
		LocalDir:        "/home/u/.claude/projects/-g-acme/memory",
		Slug:            "-g-acme",
		SeedDir:         "/home/u/.agent-brain/-g-acme",
	}}
	if diff := cmp.Diff(want, fake.migrateCalls); diff != "" {
		t.Errorf("migrate request mismatch (-want +got):\n%s", diff)
	}
	if view.Migrating != MigrateNone {
		t.Errorf("Migrating = %v, want MigrateNone once the import landed", view.Migrating)
	}
}

// TestMigrateRemotelessNamesFolder covers the remoteless branch: an empty
// ProjectID opens the folder-name input prefilled with the confirmed path's
// base name, an invalid name is refused locally before any wire call, and a
// valid one submits with migrateOne's exact "named/<folder>" ProjectID.
func TestMigrateRemotelessNamesFolder(t *testing.T) {
	t.Parallel()
	fake := &fakeData{migrateResp: api.MigrateResponse{Folder: "acme", Files: 3}}
	candidate := migrateCandidateFixture()
	// Identify resolves no remote: empty ProjectID.
	migrate := migrateActionsFor([]MigrateCandidate{candidate}, provider.Identity{}, nil, "")
	view := NewProjectsView()

	driveMigrate(t, &view, fake, migrate, key("m"))     // → picker
	driveMigrate(t, &view, fake, migrate, key("enter")) // → path confirm
	driveMigrate(t, &view, fake, migrate, key("enter")) // accept path → identify → remoteless → naming

	if view.Migrating != MigrateNamingFolder {
		t.Fatalf("Migrating = %v, want MigrateNamingFolder for a remoteless project", view.Migrating)
	}
	// The naming input is prefilled with the confirmed path's base (filepath.Base).
	if got := view.migrateInput.Value(); got != "acme" {
		t.Fatalf("naming prefill = %q, want %q (base of the confirmed path)", got, "acme")
	}

	// An invalid name is a local correction, never a wire call.
	view.migrateInput.SetValue("bad/name")
	driveMigrate(t, &view, fake, migrate, key("enter"))
	if len(fake.migrateCalls) != 0 {
		t.Fatalf("invalid folder name reached the daemon: %v", fake.migrateCalls)
	}

	view.migrateInput.SetValue("acme")
	driveMigrate(t, &view, fake, migrate, key("enter"))
	if len(fake.migrateCalls) != 1 {
		t.Fatalf("migrateCalls = %v, want exactly one after a valid name", fake.migrateCalls)
	}
	if got := fake.migrateCalls[0].ProjectID; got != "named/acme" {
		t.Errorf("ProjectID = %q, want %q (provider.NamedIdentity contract)", got, "named/acme")
	}
}

// TestMigratePathCorrectionRecomputesLiveDir pins the LiveDir field's contract:
// the candidate's precomputed live dir is used when the path is accepted
// unchanged, but a CORRECTED path recomputes it through LiveDirFor — a bad
// correction must never enroll the guess's stale dir.
func TestMigratePathCorrectionRecomputesLiveDir(t *testing.T) {
	t.Parallel()
	fake := &fakeData{migrateResp: api.MigrateResponse{Folder: "acme", Files: 1}}
	candidate := migrateCandidateFixture() // LiveDir = /home/u/.claude/projects/-g-acme/memory
	identity := provider.Identity{ProjectID: "github.com/o/acme", PreferredFolder: "acme"}
	migrate := migrateActionsFor([]MigrateCandidate{candidate}, identity, nil, "/recomputed/live/dir")
	view := NewProjectsView()

	driveMigrate(t, &view, fake, migrate, key("m"))     // → picker
	driveMigrate(t, &view, fake, migrate, key("enter")) // → path confirm
	// The user corrects the guessed path before accepting.
	view.migrateInput.SetValue("/g/acme-corrected")
	driveMigrate(t, &view, fake, migrate, key("enter")) // accept corrected path → identify → Migrate

	if len(fake.migrateCalls) != 1 {
		t.Fatalf("migrateCalls = %v, want exactly one", fake.migrateCalls)
	}
	if got := fake.migrateCalls[0].LocalDir; got != "/recomputed/live/dir" {
		t.Errorf("LocalDir = %q, want the LiveDirFor recomputation for the corrected path", got)
	}
}

// TestMigrateIdentifyFailureAborts covers OnMigrateIdentify's error branch: when
// identity resolution fails the flow resets to the tab and surfaces the reason
// verbatim as a notice, and nothing reaches the daemon — the migrate twin of the
// add flow's identify-failure abort.
func TestMigrateIdentifyFailureAborts(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	candidate := migrateCandidateFixture()
	migrate := migrateActionsFor([]MigrateCandidate{candidate}, provider.Identity{}, errors.New("no provider matched"), "")
	view := NewProjectsView()

	driveMigrate(t, &view, fake, migrate, key("m"))     // → picker
	driveMigrate(t, &view, fake, migrate, key("enter")) // → path confirm
	driveMigrate(t, &view, fake, migrate, key("enter")) // accept path → identify → fails

	if view.Migrating != MigrateNone {
		t.Fatalf("Migrating = %v, want MigrateNone after an identify failure", view.Migrating)
	}
	if got := plain(view.View("")); !strings.Contains(got, "identify failed: no provider matched") {
		t.Fatalf("view = %q, want the verbatim identify-failure notice", got)
	}
	if len(fake.migrateCalls) != 0 {
		t.Fatalf("identify failure still migrated: %v", fake.migrateCalls)
	}
}

// TestMigrateDiscoverEmptyShowsNothingToMigrate covers the empty-discovery
// branch: with no un-imported stores the flow resets and surfaces the reason,
// never reaching the picker or the daemon.
func TestMigrateDiscoverEmptyShowsNothingToMigrate(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	migrate := migrateActionsFor(nil, provider.Identity{}, nil, "")
	view := NewProjectsView()

	driveMigrate(t, &view, fake, migrate, key("m"))

	if view.Migrating != MigrateNone {
		t.Fatalf("Migrating = %v, want MigrateNone after an empty discovery", view.Migrating)
	}
	if got := plain(view.View("")); !strings.Contains(got, "nothing to migrate") {
		t.Fatalf("view = %q, want a 'nothing to migrate' notice", got)
	}
	if len(fake.migrateCalls) != 0 {
		t.Fatalf("empty discovery still migrated: %v", fake.migrateCalls)
	}
}

// TestMigrateDiscoverErrorShowsNoticeAndResets pins OnMigrateDiscover's error
// branch (the discover twin of the identify-failure abort): a failed legacy-store
// enumeration resets the flow and surfaces the reason verbatim as a notice, never
// reaching the picker. Only the empty-discovery branch was pinned before.
func TestMigrateDiscoverErrorShowsNoticeAndResets(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	migrate := MigrateActions{
		Preflight: func(context.Context) error { return nil },
		Discover: func(context.Context) ([]MigrateCandidate, error) {
			return nil, errors.New("legacy tree unreadable")
		},
		Identify: func(context.Context, string, TrackRoot, string) (provider.Identity, error) {
			return provider.Identity{}, nil
		},
		LiveDirFor: func(string, string) (string, error) { return "", nil },
	}
	view := NewProjectsView()

	driveMigrate(t, &view, fake, migrate, key("m")) // m → preflight → discover → error branch

	if view.Migrating != MigrateNone {
		t.Fatalf("Migrating = %v, want MigrateNone after a discover error", view.Migrating)
	}
	if got := plain(view.View("")); !strings.Contains(got, "discover legacy stores failed: legacy tree unreadable") {
		t.Fatalf("view = %q, want the verbatim discover-failure notice", got)
	}
	if len(fake.migrateCalls) != 0 {
		t.Fatalf("a discover error still migrated: %v", fake.migrateCalls)
	}
}

// TestMigrateResultToastWording pins the outcome line both the fresh-import and
// the already-imported (Skipped) cases render (spec §10) — the pure method the
// root toasts through, so the wording is asserted without any flow plumbing.
func TestMigrateResultToastWording(t *testing.T) {
	t.Parallel()
	fresh := MigrateResultMsg{Slug: "-g-acme", Folder: "acme", Files: 7}
	if got, want := fresh.Toast(), "migrated -g-acme → acme (7 files)"; got != want {
		t.Errorf("fresh Toast() = %q, want %q", got, want)
	}
	skipped := MigrateResultMsg{Slug: "-g-acme", Folder: "acme", Skipped: true}
	if got, want := skipped.Toast(), "migrated -g-acme → acme (0 files) — already imported — enrolled only"; got != want {
		t.Errorf("skipped Toast() = %q, want %q", got, want)
	}
}

// TestMigratePreflightRunsOncePerSession pins spec §10's once-per-session gate:
// the first m runs the chezmoi preflight, but after backing out a second m goes
// straight to discovery — ResetMigrate must NOT clear the latch.
func TestMigratePreflightRunsOncePerSession(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	var preflightCalls int
	candidate := migrateCandidateFixture()
	migrate := MigrateActions{
		Preflight: func(context.Context) error { preflightCalls++; return nil },
		Discover: func(context.Context) ([]MigrateCandidate, error) {
			return []MigrateCandidate{candidate}, nil
		},
		Identify: func(context.Context, string, TrackRoot, string) (provider.Identity, error) {
			return provider.Identity{}, nil
		},
		LiveDirFor: func(string, string) (string, error) { return "", nil },
	}
	view := NewProjectsView()

	driveMigrate(t, &view, fake, migrate, key("m"))
	if view.Migrating != MigratePicking {
		t.Fatalf("first m: Migrating = %v, want MigratePicking", view.Migrating)
	}
	if preflightCalls != 1 {
		t.Fatalf("preflightCalls = %d after first m, want 1", preflightCalls)
	}

	driveMigrate(t, &view, fake, migrate, key("esc")) // back out to the tab
	if view.Migrating != MigrateNone {
		t.Fatalf("esc: Migrating = %v, want MigrateNone", view.Migrating)
	}

	driveMigrate(t, &view, fake, migrate, key("m")) // second m: skip the gate
	if view.Migrating != MigratePicking {
		t.Fatalf("second m: Migrating = %v, want MigratePicking (straight to discovery)", view.Migrating)
	}
	if preflightCalls != 1 {
		t.Errorf("preflightCalls = %d after second m, want it to STAY 1 (gate is once per session)", preflightCalls)
	}
}
