package views

import (
	"fmt"
	"strings"
	"time"

	keybinding "charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/search"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
)

// searchDebounceInterval is how long typing must pause before the overlay
// actually queries: long enough to collapse a burst of keystrokes into one
// fleet-wide search, short enough to still feel live (spec §7's "live
// results (debounced)").
const searchDebounceInterval = 250 * time.Millisecond

// searchDisplayCap bounds how many result rows the overlay ever renders.
// Hits beyond it are declared by the "+N more" line rather than silently
// dropped, and the cursor never ranges past it — a selection the display
// refuses to show would be worse than not offering it.
const searchDisplayCap = 30

// searchOverlayChromeLines is everything View renders around the result
// rows: the title, its blank, the input line, its blank (four above), the
// reserved "+N more" slot, and the blank + key-hint line below (three
// more). The height budget subtracts this before windowing rows, so a full
// overlay never overflows the terminal. The marker slot is reserved
// unconditionally — one line under budget when no marker renders — because
// reserving it only when needed would make the budget depend on the row
// count it is itself computing.
const searchOverlayChromeLines = 7

// SearchDebounceMsg is the debounce's delayed heartbeat: every
// value-changing keystroke stamps the overlay's generation counter and
// schedules a tea.Tick carrying the stamped value; when one arrives, only a
// Generation still equal to the counter's current value runs the query —
// every earlier stamp was superseded by a newer keystroke and is dropped
// unanswered. Exported because the tick surfaces at the ROOT's Update (the
// overlay is root chrome, not a stacked Screen), which must recognize the
// message to forward it back here.
type SearchDebounceMsg struct{ Generation int }

// SearchResultsMsg carries one completed query's hits back into the
// overlay. Generation is the stamp of the keystroke the query ran for:
// results for a superseded generation are dropped, so a slow query can
// never clobber the hits of a newer one that happened to finish first. Err
// is a Collect failure (enumerating the fleet's memories), surfaced
// verbatim rather than rendered as a silently empty result list.
type SearchResultsMsg struct {
	Generation int
	Hits       []search.Hit
	Err        error
}

// SearchChoiceMsg reports the memory chosen from the overlay — enter on a
// result row. The root is the only consumer: it pushes the memory's reading
// view with a lazily built per-folder link index. The overlay itself knows
// nothing about screens or stacks.
type SearchChoiceMsg struct{ Memory memoryfs.Memory }

// SearchOverlayDeps is everything the global search overlay needs, injected
// once — the consumer-side-seam idiom shared with BrowserDeps/ReadingDeps.
type SearchOverlayDeps struct {
	// Collect enumerates EVERY tracked project's memories, fresh per query —
	// the root binds it over its provider registry and the latest fleet
	// snapshot, and re-binds it on every forwarded message (SetCollect, the
	// palette's SetAvailable discipline) so the overlay never holds stale
	// fleet state. Only ever called inside the query Cmd, never during
	// Update or View.
	Collect func() ([]memoryfs.Memory, error)
	// ReadBody reads one memory's full content for the engine's body tier —
	// memoryfs.ReadBody in production. Same Cmd-only calling rule as
	// Collect.
	ReadBody func(memoryfs.Memory) (string, error)
	Styles   theme.Styles
}

// SearchOverlay is spec §7's global search: one input, debounced live
// results across every tracked project, opened by `/` from any root view.
// It is root chrome like the palette — never a Screen on the navigation
// stack — so it satisfies no Screen contract; the root routes every message
// here while it is open and reads Closed afterwards, exactly the
// PaletteModel lifecycle, in pointer form because a textinput plus result
// slices are naturally mutable state (the Browser precedent).
type SearchOverlay struct {
	deps  SearchOverlayDeps
	input textinput.Model

	// generation counts value-changing keystrokes. Each one stamps the new
	// count into the tea.Tick it schedules; the SearchDebounceMsg and
	// SearchResultsMsg handlers compare their carried stamp against this
	// counter's CURRENT value and drop anything superseded, so of any burst
	// of pending ticks only the newest ever queries, and of any overlapping
	// queries only the newest ever lands.
	generation int

	hits []search.Hit
	// answered reports whether any query has completed since the overlay
	// opened — it separates "queried, nothing matched" (worth a notice)
	// from the blank instant before the first debounce fires (not).
	answered bool
	queryErr error

	// cursor indexes hits, never past the display cap — see
	// selectableRowCount.
	cursor int

	// Closed latches true once esc or enter has resolved the overlay's
	// lifecycle; the root reads it right after Update to drop the overlay —
	// the PaletteModel idiom, no message round-trip.
	Closed bool
}

// NewSearchOverlay builds a ready overlay with its input focused. The blink
// Cmd textinput.Focus returns is deliberately discarded: focus itself is
// synchronous state — keystroke handling gates on it, verified against
// bubbles v2.1.1 (textinput.go's Focus sets m.focus before returning the
// cursor's blink Cmd) — so the only loss is the cursor-blink animation,
// accepted to keep this constructor the plain single-value shape the brief
// specifies.
func NewSearchOverlay(deps SearchOverlayDeps) *SearchOverlay {
	input := textinput.New()
	input.Placeholder = "search every tracked project…"
	input.Focus()
	return &SearchOverlay{deps: deps, input: input}
}

// SetStyles installs a new theme — root-propagated on a background-color
// swap, the same treatment every tab view and pushed screen gets. The
// overlay caches nothing rendered, so no invalidation pairs with this:
// every View renders fresh from the current styles.
func (s *SearchOverlay) SetStyles(styles theme.Styles) {
	s.deps.Styles = styles
}

// SetCollect re-binds the fleet-enumeration seam. The root calls this on
// every message it forwards here rather than trusting the closure captured
// at construction: the root Model has value semantics, so a bound method
// value closes over a frozen snapshot of that instant's fleet — the same
// staleness the palette's SetAvailable exists to prevent.
func (s *SearchOverlay) SetCollect(collect func() ([]memoryfs.Memory, error)) {
	s.deps.Collect = collect
}

// Update handles one message while the overlay owns the keyboard. Both
// generation-stamped message types funnel through the same staleness rule:
// a stamp that no longer equals the current counter belongs to a superseded
// keystroke and is dropped without effect.
func (s *SearchOverlay) Update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return s.updateKey(msg)
	case SearchDebounceMsg:
		if msg.Generation != s.generation {
			return nil // superseded by a newer keystroke: never query stale text
		}
		return s.queryCmd()
	case SearchResultsMsg:
		if msg.Generation != s.generation {
			return nil // a newer keystroke owns the overlay now; stale hits must not clobber it
		}
		s.answered = true
		s.queryErr = msg.Err
		s.hits = msg.Hits
		s.cursor = clampCursor(s.cursor, s.selectableRowCount())
		return nil
	}
	return nil
}

// selectableRowCount reports how many hits the cursor can actually reach:
// the display cap bounds it because rows past the cap are only ever
// declared by the "+N more" line, never rendered — a cursor allowed onto
// one would select something invisible.
func (s *SearchOverlay) selectableRowCount() int {
	return min(len(s.hits), searchDisplayCap)
}

func (s *SearchOverlay) updateKey(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case keybinding.Matches(msg, DashboardKeys.Cancel):
		// esc dismisses outright — even mid-query, even with text typed. It
		// deliberately does NOT clear-then-close the way the browser
		// filter's esc does: the overlay is transient chrome whose next
		// open starts fresh anyway, so a two-stage esc would only cost a
		// keystroke.
		s.Closed = true
		return nil
	case keybinding.Matches(msg, DashboardKeys.Accept):
		if s.selectableRowCount() == 0 {
			return nil
		}
		s.Closed = true
		chosen := s.hits[s.cursor].Memory
		return func() tea.Msg { return SearchChoiceMsg{Memory: chosen} }
	}

	// Arrow keys only — never k/j — for the same reason as the palette: the
	// overlay owns a free-text query, so both letters must stay typable.
	switch msg.String() {
	case "up":
		if s.cursor > 0 {
			s.cursor--
		}
		return nil
	case "down":
		if s.cursor < s.selectableRowCount()-1 {
			s.cursor++
		}
		return nil
	}

	previousQuery := s.input.Value()
	var inputCmd tea.Cmd
	s.input, inputCmd = s.input.Update(msg)
	if s.input.Value() == previousQuery {
		// Nothing about the query changed (a cursor move inside the input,
		// a swallowed modifier chord) — re-stamping would only invalidate a
		// pending tick that is still going to run the right text.
		return inputCmd
	}

	s.generation++
	stampedGeneration := s.generation
	// tea.Tick delivers exactly one message after the interval — verified
	// against bubbletea v2.0.8 (commands.go: one time.NewTimer per call, no
	// repetition) — so every value-changing keystroke schedules its own
	// tick, stamped, and the staleness rule in Update decides whether the
	// one that eventually arrives is still current. The query itself runs
	// in the Cmd the current tick's handler returns — never here in Update.
	return tea.Batch(inputCmd, tea.Tick(searchDebounceInterval, func(time.Time) tea.Msg {
		return SearchDebounceMsg{Generation: stampedGeneration}
	}))
}

// queryCmd runs the tiered engine over a fresh fleet enumeration, entirely
// inside the returned Cmd — Update stays I/O-free. The limit handed to
// search.Query is the enumeration's own size: each memory contributes at
// most one Hit (Query's contract), so that limit can never truncate, and
// the overlay needs the FULL hit count to render an honest "+N more" line —
// its display cap applies at render time instead. (An empty enumeration
// passes 0, which Query maps to its own default — moot with nothing to
// match.)
func (s *SearchOverlay) queryCmd() tea.Cmd {
	collect := s.deps.Collect
	readBody := s.deps.ReadBody
	query := s.input.Value()
	stampedGeneration := s.generation
	return func() tea.Msg {
		memories, err := collect()
		if err != nil {
			return SearchResultsMsg{Generation: stampedGeneration, Err: err}
		}
		return SearchResultsMsg{
			Generation: stampedGeneration,
			Hits:       search.Query(memories, readBody, query, len(memories)),
		}
	}
}

// View renders the overlay at the root's full content area — it owns the
// whole screen while open, like the palette. Result rows are windowed
// around the cursor within the height budget (the browser's visibleWindow),
// and the display never exceeds searchDisplayCap rows: hits beyond the cap
// are declared by the "+N more" line, so truncation is always visible,
// never silent. height <= 0 (a root that has not measured the terminal yet)
// disables the height windowing but not the cap. The budget's floor is one
// row: below the ~9 lines the chrome itself needs, the overlay keeps the
// cursor's own row visible and overflows the already-impossible budget —
// the browser and reading screens' degrade-don't-blank rule.
func (s *SearchOverlay) View(width, height int) string {
	lines := []string{
		s.deps.Styles.Title.Render("Global search"),
		"",
		s.input.View(),
		"",
	}
	lines = append(lines, s.resultLines(height)...)
	lines = append(lines, "", s.deps.Styles.Dim.Render("↑/↓ move · enter open · esc close"))

	body := strings.Join(lines, "\n")
	if width > 0 {
		// MaxWidth hard-truncates each line (ANSI-aware) — a long fragment
		// must cost columns, not wrap onto a second terminal row the height
		// budget above never accounted for.
		body = lipgloss.NewStyle().MaxWidth(width).Render(body)
	}
	return body
}

// resultLines renders the region between the input and the key hints: a
// Collect failure, the no-match notice, or the cursor-windowed result rows
// plus the overflow marker. Nothing at all before the first query answers —
// the input's placeholder is the only guidance an untouched overlay needs.
func (s *SearchOverlay) resultLines(height int) []string {
	if s.queryErr != nil {
		return []string{s.deps.Styles.Fail.Render(fmt.Sprintf("search unavailable: %v", s.queryErr))}
	}
	if len(s.hits) == 0 {
		if s.answered && strings.TrimSpace(s.input.Value()) != "" {
			return []string{s.deps.Styles.Dim.Render("no memories match")}
		}
		return nil
	}

	windowBudget := 0 // no measured height yet: cap only, no windowing (visibleWindow's identity case)
	if height > 0 {
		windowBudget = max(height-searchOverlayChromeLines, 1)
	}
	displayRowCount := s.selectableRowCount()
	start, end := visibleWindow(s.cursor, displayRowCount, windowBudget)

	lines := make([]string, 0, end-start+1)
	for row := start; row < end; row++ {
		lines = append(lines, s.renderHitRow(row))
	}
	if overflow := len(s.hits) - displayRowCount; overflow > 0 {
		lines = append(lines, s.deps.Styles.Dim.Render(fmt.Sprintf("+%d more", overflow)))
	}
	return lines
}

// renderHitRow renders one result row: cursor marker, the memory's
// fleet-unique identity, then the matched fragment dimmed and tagged with
// the tier it matched at — spec §7's "project · provider · memory ·
// matched fragment".
func (s *SearchOverlay) renderHitRow(row int) string {
	hit := s.hits[row]
	marker := "  "
	if row == s.cursor {
		marker = "> "
	}
	identity := hit.Memory.Folder + " · " + hit.Memory.Provider + " · " + hit.Memory.Name
	line := marker + identity + " · " + s.deps.Styles.Dim.Render(tierTag(hit)+": "+hit.Fragment)
	if row == s.cursor {
		line = s.deps.Styles.Selected.Render(line)
	}
	return line
}

// tierTag names the tier a Hit matched at for its row's fragment prefix.
// The body tier carries the 1-based line the fragment came from (Hit.Line)
// so a long memory's match is locatable at a glance; the other two tiers
// have no line of their own (Hit.Line is 0 there by contract).
func tierTag(hit search.Hit) string {
	switch hit.Tier {
	case search.TierName:
		return "name"
	case search.TierDescription:
		return "description"
	case search.TierBody:
		return fmt.Sprintf("body:%d", hit.Line)
	default:
		// Unreachable with the engine's three tiers; a plain fallback beats
		// a panic if a fourth ever appears.
		return "match"
	}
}
