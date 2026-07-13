package views

import (
	"context"
	"fmt"
	"strings"
	"time"

	keybinding "charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// TrackRoot is one memory root of a track candidate — the (LocalDir,
// RepoSubdir) pair a TrackRequest enrolls.
type TrackRoot struct {
	LocalDir   string
	RepoSubdir string
}

// TrackCandidate is one row the add picker offers: a discovered-but-
// unenrolled memory root, or — for a global-scope provider — ALL of its
// unenrolled roots grouped as one row, mirroring the cli enrollment picker's
// semantics (picking it enrolls them together under _global).
type TrackCandidate struct {
	Provider  string
	Label     string
	PathGuess string // per-project only: the adapter's lossy project-path guess
	Global    bool
	Roots     []TrackRoot // len ≥ 1; > 1 only for a grouped global candidate
}

// TrackActions bundles the two closures the cli root injects (the
// offlineDoctorRunner pattern): discovery of untracked roots and identity
// resolution for a confirmed project path. The dashboard package cannot
// import cli, and providers/registry composition lives outside its import
// allowlist — the closures carry exactly the capability, nothing else.
type TrackActions struct {
	Discover func(context.Context) ([]TrackCandidate, error)
	Identify func(ctx context.Context, providerName string, root TrackRoot, projectPath string) (provider.Identity, error)
}

// AddAvailable reports whether the add flow can run end to end: discovery
// finds candidates AND identity resolution can name a picked per-project
// candidate. Both closures must be wired — a build with Discover but not
// Identify would panic the instant a per-project candidate is picked
// (identifyCmd calls Identify on nil), so availability is the single choke
// point the root's footer and the dispatch both gate on, and the a key stays
// dead and unadvertised unless both are present.
func (a TrackActions) AddAvailable() bool {
	return a.Discover != nil && a.Identify != nil
}

// AddStage is the add flow's modal state machine, owned by ProjectsView.
type AddStage int

// AddNone is idle; AddDiscovering through AddTracking are the add flow's
// stages, in the order the flow progresses through them.
const (
	AddNone AddStage = iota
	AddDiscovering
	AddPicking
	AddConfirmPath
	AddIdentifying
	AddNamingFolder
	AddTracking
)

type (
	// DiscoverMsg answers a discoverCmd: the discovered-but-unenrolled
	// candidates, or an error. It is exported because the root's Update
	// switches on it directly (spec §15's views split); its fields stay
	// unexported because it is only ever passed through whole, never built
	// from a literal outside this file.
	DiscoverMsg struct {
		candidates []TrackCandidate
		err        error
	}
	// IdentifyMsg answers an identifyCmd: the resolved identity, or an
	// error. Exported for the same reason as DiscoverMsg, and for the same
	// reason its fields stay unexported.
	IdentifyMsg struct {
		identity provider.Identity
		err      error
	}
	// TrackResultMsg answers a trackCmd: the folders successfully enrolled
	// before any failure, and the failure itself if one occurred. Both the
	// type and its fields are exported: the root's Update reads Err
	// directly to decide its own follow-up Cmd, and the root's test suite
	// constructs a TrackResultMsg literal directly (a stale-result
	// regression pin).
	TrackResultMsg struct {
		Folders []string
		Err     error
	}
)

func discoverCmd(actions TrackActions) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
		defer cancel()
		candidates, err := actions.Discover(ctx)
		return DiscoverMsg{candidates: candidates, err: err}
	}
}

func identifyCmd(actions TrackActions, candidate TrackCandidate, projectPath string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
		defer cancel()
		identity, err := actions.Identify(ctx, candidate.Provider, candidate.Roots[0], projectPath)
		return IdentifyMsg{identity: identity, err: err}
	}
}

// trackCmd enrolls every root of the chosen candidate. A grouped global
// candidate is several TrackRequests by design (the daemon enrolls one root
// per request); the timeout scales so a slow daemon cannot strand a
// multi-root enrollment halfway through its budget.
func trackCmd(data DataSource, candidate TrackCandidate, identity provider.Identity) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout*time.Duration(len(candidate.Roots)))
		defer cancel()
		folders := make([]string, 0, len(candidate.Roots))
		for _, root := range candidate.Roots {
			response, err := data.Track(ctx, api.TrackRequest{
				Provider:        candidate.Provider,
				ProjectID:       identity.ProjectID,
				PreferredFolder: identity.PreferredFolder,
				LocalDir:        root.LocalDir,
				RepoSubdir:      root.RepoSubdir,
			})
			if err != nil {
				return TrackResultMsg{Folders: folders, Err: err}
			}
			folders = append(folders, response.Folder)
		}
		return TrackResultMsg{Folders: folders}
	}
}

// updateAdd routes keys while the add flow owns the keyboard. It returns
// (handled, cmd); handled is true whenever the flow is active so the caller
// swallows everything else, exactly like the untrack confirm.
func (v *ProjectsView) updateAdd(msg tea.KeyPressMsg, data DataSource, actions TrackActions) (bool, tea.Cmd) {
	if v.Adding == AddNone {
		return false, nil
	}
	if keybinding.Matches(msg, DashboardKeys.Cancel) {
		v.resetAdd()
		v.notice = "add cancelled"
		return true, nil
	}
	switch v.Adding {
	case AddPicking:
		switch {
		case keybinding.Matches(msg, DashboardKeys.Select):
			// Membership gate, then the concrete key picks the direction — the
			// TabSwitch idiom (Select bundles up/down/k/j); the default arm is
			// exactly down/j, never a catch-all.
			switch msg.String() {
			case "up", "k":
				if v.addCursor > 0 {
					v.addCursor--
				}
			default:
				if v.addCursor < len(v.addCandidates)-1 {
					v.addCursor++
				}
			}
		case keybinding.Matches(msg, DashboardKeys.Accept):
			choice := v.addCandidates[v.addCursor]
			v.addChoice = choice
			if choice.Global {
				v.Adding = AddTracking
				return true, trackCmd(data, choice, provider.Identity{})
			}
			v.Adding = AddConfirmPath
			v.addInput.SetValue(choice.PathGuess)
			return true, v.addInput.Focus()
		}
		return true, nil

	case AddConfirmPath:
		if keybinding.Matches(msg, DashboardKeys.Accept) {
			projectPath := strings.TrimSpace(v.addInput.Value())
			if projectPath == "" {
				v.notice = "project path cannot be empty"
				return true, nil
			}
			v.Adding = AddIdentifying
			return true, identifyCmd(actions, v.addChoice, projectPath)
		}
		var cmd tea.Cmd
		v.addInput, cmd = v.addInput.Update(msg)
		return true, cmd

	case AddNamingFolder:
		if keybinding.Matches(msg, DashboardKeys.Accept) {
			folderName := strings.TrimSpace(v.addInput.Value())
			// The daemon re-validates fail-closed on Track; validating here
			// too keeps a bad name a local correction, not a wire error.
			if err := repo.ValidateFolderName(folderName); err != nil {
				v.notice = err.Error()
				return true, nil
			}
			v.Adding = AddTracking
			return true, trackCmd(data, v.addChoice, provider.NamedIdentity(folderName))
		}
		var cmd tea.Cmd
		v.addInput, cmd = v.addInput.Update(msg)
		return true, cmd

	default: // AddDiscovering, AddIdentifying, AddTracking: waiting on a Cmd
		return true, nil
	}
}

func (v *ProjectsView) resetAdd() {
	v.Adding = AddNone
	v.addCandidates = nil
	v.addCursor = 0
	v.addChoice = TrackCandidate{}
}

// OnDiscover advances the flow once discovery answers.
func (v *ProjectsView) OnDiscover(msg DiscoverMsg) {
	if v.Adding != AddDiscovering {
		return // a stale answer after esc must not resurrect the flow
	}
	if msg.err != nil {
		v.resetAdd()
		v.notice = fmt.Sprintf("discover failed: %v", msg.err)
		return
	}
	if len(msg.candidates) == 0 {
		v.resetAdd()
		v.notice = "no new memory roots discovered"
		return
	}
	v.Adding = AddPicking
	v.addCandidates = msg.candidates
	v.addCursor = 0
}

// OnIdentify advances the flow once identity resolution answers: a canonical
// id tracks immediately; an empty one (remoteless project) opens the folder
// naming input, prefilled with Identify's PreferredFolder — the same prefill
// contract the cli flow uses, since an accepted empty answer must be a value
// we are willing to enroll under.
func (v *ProjectsView) OnIdentify(msg IdentifyMsg, data DataSource) tea.Cmd {
	if v.Adding != AddIdentifying {
		return nil // a stale answer after esc must not resurrect the flow
	}
	if msg.err != nil {
		v.resetAdd()
		v.notice = fmt.Sprintf("identify failed: %v", msg.err)
		return nil
	}
	if msg.identity.ProjectID != "" {
		v.Adding = AddTracking
		return trackCmd(data, v.addChoice, msg.identity)
	}
	v.Adding = AddNamingFolder
	v.addInput.SetValue(msg.identity.PreferredFolder)
	return v.addInput.Focus()
}

// OnTrackResult records an enrollment's outcome. Only the reset is gated on
// AddTracking — a result landing after the user esc'd and reopened the flow
// must not stomp the new flow's stage back to AddNone — while the notice
// always fires, because the enrollment already happened and a stale result
// is still a real outcome whose notice stays truthful. (The root decides
// separately, from msg.Err, whether a stale-or-fresh success also fires a
// whole-fleet sync — that orchestration is root's, not this view's.)
func (v *ProjectsView) OnTrackResult(msg TrackResultMsg) {
	if v.Adding == AddTracking {
		v.resetAdd()
	}
	if msg.Err != nil {
		v.notice = fmt.Sprintf("track failed: %v", msg.Err)
		if len(msg.Folders) > 0 {
			v.notice = fmt.Sprintf("track failed after enrolling %s: %v", strings.Join(msg.Folders, ", "), msg.Err)
		}
		return
	}
	v.notice = fmt.Sprintf("tracked %s — syncing…", strings.Join(msg.Folders, ", "))
}

// addView renders the add flow in place of the projects table while active.
func (v ProjectsView) addView() string {
	var b strings.Builder
	switch v.Adding {
	case AddDiscovering:
		b.WriteString(v.styles.Dim.Render("discovering memory roots…"))
	case AddPicking:
		b.WriteString("Select a memory root to enroll\n\n")
		for i, candidate := range v.addCandidates {
			cursor := "  "
			if i == v.addCursor {
				cursor = "→ "
			}
			b.WriteString(cursor + candidate.Label + "\n")
		}
		b.WriteString("\n")
		b.WriteString(v.styles.Dim.Render(HelpLine(DashboardKeys.ForModal(false, AddPicking))))
	case AddConfirmPath:
		b.WriteString("Confirm this project's path\n\n")
		b.WriteString(v.addInput.View())
		b.WriteString("\n\n")
		b.WriteString(v.styles.Dim.Render(HelpLine(DashboardKeys.ForModal(false, AddConfirmPath))))
	case AddIdentifying:
		b.WriteString(v.styles.Dim.Render("resolving project identity…"))
	case AddNamingFolder:
		b.WriteString("This project has no git remote — choose a folder name\n\n")
		b.WriteString(v.addInput.View())
		b.WriteString("\n\n")
		b.WriteString(v.styles.Dim.Render(HelpLine(DashboardKeys.ForModal(false, AddNamingFolder))))
	case AddTracking:
		b.WriteString(v.styles.Dim.Render("enrolling…"))
	}
	if v.Adding != AddDiscovering && v.notice != "" {
		b.WriteString("\n")
		b.WriteString(v.styles.Warn.Render(v.notice))
	}
	return b.String()
}
