package dashboard

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

// trackActions bundles the two closures the cli root injects (the
// offlineDoctorRunner pattern): discovery of untracked roots and identity
// resolution for a confirmed project path. The dashboard package cannot
// import cli, and providers/registry composition lives outside its import
// allowlist — the closures carry exactly the capability, nothing else.
type trackActions struct {
	discover func(context.Context) ([]TrackCandidate, error)
	identify func(ctx context.Context, providerName string, root TrackRoot, projectPath string) (provider.Identity, error)
}

// addAvailable reports whether the add flow can run end to end: discovery finds
// candidates AND identity resolution can name a picked per-project candidate.
// Both closures must be wired — a build with discover but not identify would
// panic the instant a per-project candidate is picked (identifyCmd calls
// identify on nil), so availability is the single choke point footer() and the
// dispatch both gate on, and the a key stays dead and unadvertised unless both
// are present.
func (a trackActions) addAvailable() bool {
	return a.discover != nil && a.identify != nil
}

// addStage is the add flow's modal state machine, owned by projectsView.
type addStage int

const (
	addNone addStage = iota
	addDiscovering
	addPicking
	addConfirmPath
	addIdentifying
	addNamingFolder
	addTracking
)

type (
	discoverMsg struct {
		candidates []TrackCandidate
		err        error
	}
	identifyMsg struct {
		identity provider.Identity
		err      error
	}
	trackResultMsg struct {
		folders []string
		err     error
	}
)

func discoverCmd(actions trackActions) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		candidates, err := actions.discover(ctx)
		return discoverMsg{candidates: candidates, err: err}
	}
}

func identifyCmd(actions trackActions, candidate TrackCandidate, projectPath string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		identity, err := actions.identify(ctx, candidate.Provider, candidate.Roots[0], projectPath)
		return identifyMsg{identity: identity, err: err}
	}
}

// trackCmd enrolls every root of the chosen candidate. A grouped global
// candidate is several TrackRequests by design (the daemon enrolls one root
// per request); the timeout scales so a slow daemon cannot strand a
// multi-root enrollment halfway through its budget.
func trackCmd(data dashboardData, candidate TrackCandidate, identity provider.Identity) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout*time.Duration(len(candidate.Roots)))
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
				return trackResultMsg{folders: folders, err: err}
			}
			folders = append(folders, response.Folder)
		}
		return trackResultMsg{folders: folders}
	}
}

// updateAdd routes keys while the add flow owns the keyboard. It returns
// (handled, cmd); handled is true whenever the flow is active so the caller
// swallows everything else, exactly like the untrack confirm.
func (v *projectsView) updateAdd(msg tea.KeyPressMsg, data dashboardData, actions trackActions) (bool, tea.Cmd) {
	if v.adding == addNone {
		return false, nil
	}
	if keybinding.Matches(msg, dashboardKeys.Cancel) {
		v.resetAdd()
		v.notice = "add cancelled"
		return true, nil
	}
	switch v.adding {
	case addPicking:
		switch {
		case keybinding.Matches(msg, dashboardKeys.Select):
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
		case keybinding.Matches(msg, dashboardKeys.Accept):
			choice := v.addCandidates[v.addCursor]
			v.addChoice = choice
			if choice.Global {
				v.adding = addTracking
				return true, trackCmd(data, choice, provider.Identity{})
			}
			v.adding = addConfirmPath
			v.addInput.SetValue(choice.PathGuess)
			return true, v.addInput.Focus()
		}
		return true, nil

	case addConfirmPath:
		if keybinding.Matches(msg, dashboardKeys.Accept) {
			projectPath := strings.TrimSpace(v.addInput.Value())
			if projectPath == "" {
				v.notice = "project path cannot be empty"
				return true, nil
			}
			v.adding = addIdentifying
			return true, identifyCmd(actions, v.addChoice, projectPath)
		}
		var cmd tea.Cmd
		v.addInput, cmd = v.addInput.Update(msg)
		return true, cmd

	case addNamingFolder:
		if keybinding.Matches(msg, dashboardKeys.Accept) {
			folderName := strings.TrimSpace(v.addInput.Value())
			// The daemon re-validates fail-closed on Track; validating here
			// too keeps a bad name a local correction, not a wire error.
			if err := repo.ValidateFolderName(folderName); err != nil {
				v.notice = err.Error()
				return true, nil
			}
			v.adding = addTracking
			return true, trackCmd(data, v.addChoice, provider.NamedIdentity(folderName))
		}
		var cmd tea.Cmd
		v.addInput, cmd = v.addInput.Update(msg)
		return true, cmd

	default: // addDiscovering, addIdentifying, addTracking: waiting on a Cmd
		return true, nil
	}
}

func (v *projectsView) resetAdd() {
	v.adding = addNone
	v.addCandidates = nil
	v.addCursor = 0
	v.addChoice = TrackCandidate{}
}

func (v *projectsView) onDiscover(msg discoverMsg) {
	if v.adding != addDiscovering {
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
	v.adding = addPicking
	v.addCandidates = msg.candidates
	v.addCursor = 0
}

// onIdentify advances the flow once identity resolution answers: a canonical
// id tracks immediately; an empty one (remoteless project) opens the folder
// naming input, prefilled with Identify's PreferredFolder — the same prefill
// contract the cli flow uses, since an accepted empty answer must be a value
// we are willing to enroll under.
func (v *projectsView) onIdentify(msg identifyMsg, data dashboardData) tea.Cmd {
	if v.adding != addIdentifying {
		return nil // a stale answer after esc must not resurrect the flow
	}
	if msg.err != nil {
		v.resetAdd()
		v.notice = fmt.Sprintf("identify failed: %v", msg.err)
		return nil
	}
	if msg.identity.ProjectID != "" {
		v.adding = addTracking
		return trackCmd(data, v.addChoice, msg.identity)
	}
	v.adding = addNamingFolder
	v.addInput.SetValue(msg.identity.PreferredFolder)
	return v.addInput.Focus()
}

// onTrackResult records an enrollment's outcome. Only the reset is gated on
// addTracking — a result landing after the user esc'd and reopened the flow
// must not stomp the new flow's stage back to addNone — while the notice and
// fleet sync always fire, because the enrollment already happened and a stale
// result is still a real outcome whose notice stays truthful and whose sync is
// legitimate.
func (v *projectsView) onTrackResult(msg trackResultMsg) {
	if v.adding == addTracking {
		v.resetAdd()
	}
	if msg.err != nil {
		v.notice = fmt.Sprintf("track failed: %v", msg.err)
		if len(msg.folders) > 0 {
			v.notice = fmt.Sprintf("track failed after enrolling %s: %v", strings.Join(msg.folders, ", "), msg.err)
		}
		return
	}
	v.notice = fmt.Sprintf("tracked %s — syncing…", strings.Join(msg.folders, ", "))
}

// addView renders the add flow in place of the projects table while active.
func (v projectsView) addView() string {
	var b strings.Builder
	switch v.adding {
	case addDiscovering:
		b.WriteString(dimStyle.Render("discovering memory roots…"))
	case addPicking:
		b.WriteString("Select a memory root to enroll\n\n")
		for i, candidate := range v.addCandidates {
			cursor := "  "
			if i == v.addCursor {
				cursor = "→ "
			}
			b.WriteString(cursor + candidate.Label + "\n")
		}
		b.WriteString("\n")
		b.WriteString(dimStyle.Render(helpLine(dashboardKeys.forModal(false, addPicking))))
	case addConfirmPath:
		b.WriteString("Confirm this project's path\n\n")
		b.WriteString(v.addInput.View())
		b.WriteString("\n\n")
		b.WriteString(dimStyle.Render(helpLine(dashboardKeys.forModal(false, addConfirmPath))))
	case addIdentifying:
		b.WriteString(dimStyle.Render("resolving project identity…"))
	case addNamingFolder:
		b.WriteString("This project has no git remote — choose a folder name\n\n")
		b.WriteString(v.addInput.View())
		b.WriteString("\n\n")
		b.WriteString(dimStyle.Render(helpLine(dashboardKeys.forModal(false, addNamingFolder))))
	case addTracking:
		b.WriteString(dimStyle.Render("enrolling…"))
	}
	if v.adding != addDiscovering && v.notice != "" {
		b.WriteString("\n")
		b.WriteString(warnStyle.Render(v.notice))
	}
	return b.String()
}
