package views

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	keybinding "charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// MigrateCandidate is one un-imported bash-era store the migrate picker offers
// (spec §10). Provider is always "claude" — the bash-era importer's only
// domain. Slug is the legacy directory name AND the daemon's idempotency
// marker key; SeedDir is the legacy tree to import; PathGuess is the adapter's
// lossy project-path reversal the confirm input prefills. LiveDir is the live
// provider dir for the GUESSED path, recomputed by MigrateActions.LiveDirFor
// only if the user corrects the path before confirming.
type MigrateCandidate struct {
	Provider  string
	Slug      string
	SeedDir   string
	PathGuess string
	LiveDir   string
}

// MigrateActions bundles the closures the cli root injects for the migrate
// flow (spec §10), the same composition-at-the-edge pattern as TrackActions:
// the dashboard package cannot import cli or compose provider adapters, so
// each closure carries exactly one capability. Preflight is the once-per-
// session chezmoi gate; Discover enumerates un-imported legacy stores; Identify
// is the SAME registry-backed resolver the add flow uses (reused verbatim, so
// migrate and enroll can never disagree about a project's identity); LiveDirFor
// maps a confirmed project path to the live provider dir to enroll.
type MigrateActions struct {
	Preflight  func(context.Context) error
	Discover   func(context.Context) ([]MigrateCandidate, error)
	Identify   func(ctx context.Context, providerName string, root TrackRoot, projectPath string) (provider.Identity, error)
	LiveDirFor func(providerName, projectPath string) (string, error)
}

// MigrateAvailable reports whether the migrate flow can run end to end: every
// injected closure must be wired, since the flow calls each in turn and a nil
// one would panic mid-flow. It is the single choke point the root's footer,
// palette, and the m key all gate on, so m stays dead and unadvertised on a
// build that did not wire migrate.
func (a MigrateActions) MigrateAvailable() bool {
	return a.Preflight != nil && a.Discover != nil && a.Identify != nil && a.LiveDirFor != nil
}

// MigrateStage is the migrate flow's modal state machine, owned by
// ProjectsView — the spec §10 importer's stages in progression order.
type MigrateStage int

// MigrateNone is idle; MigratePreflighting through MigrateMigrating are the
// migrate flow's stages, in the order the flow progresses through them.
const (
	MigrateNone MigrateStage = iota
	MigratePreflighting
	MigrateDiscovering
	MigratePicking
	MigrateConfirmPath
	MigrateIdentifying
	MigrateNamingFolder
	MigrateMigrating
)

type (
	// MigratePreflightMsg answers migratePreflightCmd: the chezmoi gate's
	// verdict (spec §10). Err is exported because the root reads it directly to
	// decide between advancing the flow and surfacing the refusal verbatim.
	MigratePreflightMsg struct{ Err error }
	// MigrateDiscoverMsg answers migrateDiscoverCmd: the un-imported legacy
	// stores, or an error. Exported type, unexported fields — only ever passed
	// through whole to OnMigrateDiscover, never built from a literal outside
	// this file (the DiscoverMsg convention).
	MigrateDiscoverMsg struct {
		candidates []MigrateCandidate
		err        error
	}
	// MigrateIdentifyMsg answers migrateIdentifyCmd: the resolved identity, or
	// an error. Exported type, unexported fields, same reason as DiscoverMsg.
	MigrateIdentifyMsg struct {
		identity provider.Identity
		err      error
	}
	// MigrateResultMsg answers migrateCmd: the daemon's SeedReport for one
	// store plus the failure if one occurred. All fields are exported — the
	// root reads them to compose the outcome toast and to decide (from Err)
	// between the whole-fleet sync and a projects-only refresh.
	MigrateResultMsg struct {
		Slug    string
		Folder  string
		Files   int
		Skipped bool
		Err     error
	}
)

// Toast renders a completed migrate's outcome line (spec §10): the seeded file
// count for a fresh import, or the already-imported note when the daemon skipped
// the re-seed — in which case the live dir was still enrolled, so the wording
// says "enrolled only" rather than implying nothing happened.
func (msg MigrateResultMsg) Toast() string {
	text := fmt.Sprintf("migrated %s → %s (%d files)", msg.Slug, msg.Folder, msg.Files)
	if msg.Skipped {
		text += " — already imported — enrolled only"
	}
	return text
}

// migratePreflightCmd runs the injected chezmoi gate off the UI thread. It uses
// a background context, NOT RequestTimeout: the closure binds its own deadline
// from config.MigrateSettings.PreflightTimeout (a cold NFS home or a huge legacy
// tree can exceed the 2s-poll bound), so wrapping it in the shorter poll timeout
// would cut a legitimate check short.
func migratePreflightCmd(migrate MigrateActions) tea.Cmd {
	return func() tea.Msg {
		return MigratePreflightMsg{Err: migrate.Preflight(context.Background())}
	}
}

// migrateDiscoverCmd enumerates the un-imported bash-era stores. The RequestTimeout
// bounds it like every other data Cmd; the closure's work is a local filesystem
// walk, so the bound is belt-and-suspenders.
func migrateDiscoverCmd(migrate MigrateActions) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
		defer cancel()
		candidates, err := migrate.Discover(ctx)
		return MigrateDiscoverMsg{candidates: candidates, err: err}
	}
}

// migrateIdentifyCmd resolves the confirmed project path's cross-machine
// identity through the SAME closure the add flow uses, with providerName
// "claude" and a ZERO TrackRoot — reproducing migrateOne's
// claudeProvider.Identify(ctx, provider.Discovered{}, projectPath) exactly.
func migrateIdentifyCmd(migrate MigrateActions, candidate MigrateCandidate, projectPath string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
		defer cancel()
		identity, err := migrate.Identify(ctx, candidate.Provider, TrackRoot{}, projectPath)
		return MigrateIdentifyMsg{identity: identity, err: err}
	}
}

// migrateCmd submits one store's MigrateRequest (spec §10 step 4): SeedDir is
// the legacy tree, LocalDir is the live provider dir the daemon enrolls (so
// enrollment's own mirror-in overlays the seed), and Slug is the marker key the
// daemon uses to skip an already-imported store idempotently. A background
// context lets a large seed finish under the UDS client's own ceiling rather
// than a hard mid-seed deadline (the same posture the doctor-fix Cmd takes).
func migrateCmd(data DataSource, candidate MigrateCandidate, liveDir string, identity provider.Identity) tea.Cmd {
	return func() tea.Msg {
		resp, err := data.Migrate(context.Background(), api.MigrateRequest{
			Provider:        candidate.Provider,
			ProjectID:       identity.ProjectID,
			PreferredFolder: identity.PreferredFolder,
			LocalDir:        liveDir,
			Slug:            candidate.Slug,
			SeedDir:         candidate.SeedDir,
		})
		return MigrateResultMsg{Slug: candidate.Slug, Folder: resp.Folder, Files: resp.Files, Skipped: resp.Skipped, Err: err}
	}
}

// updateMigrate routes keys while the migrate flow owns the keyboard. Like
// updateAdd it returns (handled, cmd); handled is true whenever the flow is
// active so the caller swallows everything else.
func (v *ProjectsView) updateMigrate(msg tea.KeyPressMsg, data DataSource, migrate MigrateActions) (bool, tea.Cmd) {
	if v.Migrating == MigrateNone {
		return false, nil
	}
	if keybinding.Matches(msg, DashboardKeys.Cancel) {
		v.ResetMigrate()
		v.notice = "migrate cancelled"
		return true, nil
	}
	switch v.Migrating {
	case MigratePicking:
		switch {
		case keybinding.Matches(msg, DashboardKeys.Select):
			// Membership gate, then the concrete key picks the direction — the
			// same single-select idiom the add picker used before multi-select.
			switch msg.String() {
			case "up", "k":
				if v.migrateCursor > 0 {
					v.migrateCursor--
				}
			default:
				if v.migrateCursor < len(v.migrateCandidates)-1 {
					v.migrateCursor++
				}
			}
		case keybinding.Matches(msg, DashboardKeys.Accept):
			v.migrateChoice = v.migrateCandidates[v.migrateCursor]
			v.Migrating = MigrateConfirmPath
			v.migrateInput.SetValue(v.migrateChoice.PathGuess)
			return true, v.migrateInput.Focus()
		}
		return true, nil

	case MigrateConfirmPath:
		if keybinding.Matches(msg, DashboardKeys.Accept) {
			projectPath := strings.TrimSpace(v.migrateInput.Value())
			if projectPath == "" {
				v.notice = "project path cannot be empty"
				return true, nil
			}
			// The guessed live dir is already computed for the unchanged path;
			// recompute only when the user corrects it (the LiveDir field's own
			// contract), so a corrected path never enrolls the wrong dir.
			liveDir := v.migrateChoice.LiveDir
			if projectPath != v.migrateChoice.PathGuess {
				resolved, err := migrate.LiveDirFor(v.migrateChoice.Provider, projectPath)
				if err != nil {
					v.ResetMigrate()
					v.notice = fmt.Sprintf("resolve live dir failed: %v", err)
					return true, nil
				}
				liveDir = resolved
			}
			v.migrateProjectPath = projectPath
			v.migrateLiveDir = liveDir
			v.Migrating = MigrateIdentifying
			return true, migrateIdentifyCmd(migrate, v.migrateChoice, projectPath)
		}
		var cmd tea.Cmd
		v.migrateInput, cmd = v.migrateInput.Update(msg)
		return true, cmd

	case MigrateNamingFolder:
		if keybinding.Matches(msg, DashboardKeys.Accept) {
			folderName := strings.TrimSpace(v.migrateInput.Value())
			// The daemon re-validates fail-closed on Migrate; validating here
			// too keeps a bad name a local correction, not a wire error — the
			// same belt-and-suspenders the add flow applies.
			if err := repo.ValidateFolderName(folderName); err != nil {
				v.notice = err.Error()
				return true, nil
			}
			v.Migrating = MigrateMigrating
			return true, migrateCmd(data, v.migrateChoice, v.migrateLiveDir, provider.NamedIdentity(folderName))
		}
		var cmd tea.Cmd
		v.migrateInput, cmd = v.migrateInput.Update(msg)
		return true, cmd

	default: // MigratePreflighting, MigrateDiscovering, MigrateIdentifying, MigrateMigrating: waiting on a Cmd
		return true, nil
	}
}

// ResetMigrate clears the migrate flow back to idle. migratePreflighted is
// deliberately NOT cleared — the chezmoi gate is a once-per-session cost, so a
// cancelled or completed migrate does not force the next one to re-run it. It
// is exported because the root drives the reset when the pre-flight refuses
// (the one migrate outcome the root, not this view, surfaces).
func (v *ProjectsView) ResetMigrate() {
	v.Migrating = MigrateNone
	v.migrateCandidates = nil
	v.migrateCursor = 0
	v.migrateChoice = MigrateCandidate{}
	v.migrateProjectPath = ""
	v.migrateLiveDir = ""
}

// OnMigratePreflightOK advances the flow once the chezmoi gate passes: it
// latches migratePreflighted (so the gate never runs again this session) and
// fires discovery. The root calls it only on a nil-error MigratePreflightMsg;
// the stage guard drops a pass that arrives after the user esc'd the wait.
func (v *ProjectsView) OnMigratePreflightOK(migrate MigrateActions) tea.Cmd {
	if v.Migrating != MigratePreflighting {
		return nil
	}
	v.migratePreflighted = true
	v.Migrating = MigrateDiscovering
	return migrateDiscoverCmd(migrate)
}

// OnMigrateDiscover advances the flow once discovery answers — the migrate
// twin of OnDiscover.
func (v *ProjectsView) OnMigrateDiscover(msg MigrateDiscoverMsg) {
	if v.Migrating != MigrateDiscovering {
		return // a stale answer after esc must not resurrect the flow
	}
	if msg.err != nil {
		v.ResetMigrate()
		v.notice = fmt.Sprintf("discover legacy stores failed: %v", msg.err)
		return
	}
	if len(msg.candidates) == 0 {
		v.ResetMigrate()
		v.notice = "nothing to migrate — no un-imported legacy stores found"
		return
	}
	v.Migrating = MigratePicking
	v.migrateCandidates = msg.candidates
	v.migrateCursor = 0
}

// OnMigrateIdentify advances the flow once identity resolution answers: a
// canonical id migrates immediately; an empty one (remoteless project) opens
// the folder naming input, prefilled with the confirmed path's base name —
// migrateOne's own remoteless seed (filepath.Base(projectPath)), not the add
// flow's PreferredFolder prefill.
func (v *ProjectsView) OnMigrateIdentify(msg MigrateIdentifyMsg, data DataSource) tea.Cmd {
	if v.Migrating != MigrateIdentifying {
		return nil // a stale answer after esc must not resurrect the flow
	}
	if msg.err != nil {
		v.ResetMigrate()
		v.notice = fmt.Sprintf("identify failed: %v", msg.err)
		return nil
	}
	if msg.identity.ProjectID != "" {
		v.Migrating = MigrateMigrating
		return migrateCmd(data, v.migrateChoice, v.migrateLiveDir, msg.identity)
	}
	v.Migrating = MigrateNamingFolder
	v.migrateInput.SetValue(filepath.Base(v.migrateProjectPath))
	return v.migrateInput.Focus()
}

// OnMigrateResult clears the flow once a migrate completes. The reset is gated
// on MigrateMigrating so a result landing after the user esc'd (or started a
// fresh migrate) cannot stomp the new state — exactly OnTrackResult's guard.
// The outcome itself (toast + fleet sync, or the failure) is the root's, since
// only the root holds the toast chrome and the sync orchestration.
func (v *ProjectsView) OnMigrateResult(_ MigrateResultMsg) {
	if v.Migrating == MigrateMigrating {
		v.ResetMigrate()
	}
}

// migrateView renders the migrate flow in place of the projects table while
// active — the migrate twin of addView.
func (v ProjectsView) migrateView() string {
	var b strings.Builder
	switch v.Migrating {
	case MigratePreflighting:
		b.WriteString(v.styles.Dim.Render("checking legacy store…"))
	case MigrateDiscovering:
		b.WriteString(v.styles.Dim.Render("discovering legacy stores…"))
	case MigratePicking:
		b.WriteString("Select a legacy store to migrate\n\n")
		for i, candidate := range v.migrateCandidates {
			cursor := "  "
			if i == v.migrateCursor {
				cursor = "→ "
			}
			b.WriteString(cursor + candidate.Slug + " → " + candidate.PathGuess + "\n")
		}
		b.WriteString("\n")
		b.WriteString(v.styles.Dim.Render(HelpLine(DashboardKeys.ForMigrateModal(MigratePicking))))
	case MigrateConfirmPath:
		b.WriteString("Confirm this project's path\n\n")
		b.WriteString(v.migrateInput.View())
		b.WriteString("\n\n")
		b.WriteString(v.styles.Dim.Render(HelpLine(DashboardKeys.ForMigrateModal(MigrateConfirmPath))))
	case MigrateIdentifying:
		b.WriteString(v.styles.Dim.Render("resolving project identity…"))
	case MigrateNamingFolder:
		b.WriteString("This project has no git remote — choose a folder name\n\n")
		b.WriteString(v.migrateInput.View())
		b.WriteString("\n\n")
		b.WriteString(v.styles.Dim.Render(HelpLine(DashboardKeys.ForMigrateModal(MigrateNamingFolder))))
	case MigrateMigrating:
		b.WriteString(v.styles.Dim.Render("migrating…"))
	}
	// The waiting stages carry no notice; the interactive ones surface a local
	// correction (an empty path, a bad folder name) the same way addView does.
	if v.Migrating != MigratePreflighting && v.Migrating != MigrateDiscovering && v.notice != "" {
		b.WriteString("\n")
		b.WriteString(v.styles.Warn.Render(v.notice))
	}
	return b.String()
}
