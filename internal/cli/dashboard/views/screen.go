package views

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// Screen is one drill-in surface on the root's navigation stack (spec §2):
// entering a project's memory browser, then a memory's reading view, then
// its history, each pushes one more Screen. While the stack is non-empty
// the root renders header + breadcrumb + the top screen's View + a
// scope-filtered footer in place of the tab body, and forwards every
// message — ticks and keys alike — to the top of the stack.
//
// esc pops one level unless the top screen's own Update consumes it first
// (e.g. clearing an open in-screen filter): a screen signals "not consumed"
// by returning a Cmd whose message is PopScreenMsg, and "consumed" by
// returning a nil Cmd (or one that produces something else) instead. The
// root reducer stays a dumb forwarder — it never inspects a screen's
// internal state, only the Push/PopScreenMsg values a screen's Cmd
// produces.
type Screen interface {
	// Update handles one message and returns a replacement Screen (usually
	// itself) plus a Cmd. All I/O must stay inside a returned Cmd or a
	// documented synchronous exception (Browser's refresh — see browser.go
	// — treats a local memoryfs read as cheap enough to run inline), never
	// as an undocumented side effect of Update.
	Update(msg tea.Msg) (Screen, tea.Cmd)
	// View renders the screen at the given content area. The root computes
	// width/height once — terminal size minus its own header/breadcrumb/
	// footer chrome — and passes them down on every render; a Screen never
	// reads tea.WindowSizeMsg itself, so a resize is handled by construction
	// rather than by any state the screen would need to keep in sync.
	View(width, height int) string
	// Title names the screen's breadcrumb segment (e.g. a project folder).
	Title() string
}

// PushScreenMsg asks the root to push Screen onto the stack. It is produced
// as a Cmd's result — never appended to the stack directly by the view that
// wants it pushed — so the same plumbing works whether the push originates
// from a tab view (Projects' enter-to-browse) or from inside another Screen
// (the Browser pushing a Reading, a Reading pushing the next link target).
type PushScreenMsg struct{ Screen Screen }

// PopScreenMsg asks the root to pop the top of the stack.
type PopScreenMsg struct{}

// ToastMsg asks the root to surface Text in its persistent status area —
// the screen→root channel for a local refusal or notice that needs no
// state change (the reading view's enter on a dangling link). Produced as
// a Cmd's result, exactly like Push/PopScreenMsg, so a pushed screen never
// needs a reference to the root to explain itself.
type ToastMsg struct{ Text string }

// RefreshMsg is forwarded to the top screen on every root tick, in addition
// to the root's own status/tab reload, so a drill-in surface stays live
// against writes an external agent makes to the same files while the user
// is browsing — without inventing a screen-specific timer. The root's own
// tick message type is package-private (internal/cli/dashboard), so it
// cannot be named from this package; RefreshMsg is the exported message the
// root translates it to before forwarding.
//
// "Fresh reality" includes the clock, not just the filesystem: Now carries
// the root's current tick time, and a Screen that renders anything
// relative-time-shaped (an "X ago" label) must store the latest Now it
// receives here and render from that stored field — never from a closure
// captured at construction/push time. Model has value semantics (spec §2),
// so a func() time.Time closure built once, at push time, over that
// moment's Model copy stays frozen at that moment forever; it never observes
// a later tick's advanced clock the way a value delivered fresh on every
// RefreshMsg does. Seed the field's initial value at construction (before
// the first RefreshMsg ever arrives) so the very first render is already
// correct, not just the first one after a tick.
type RefreshMsg struct{ Now time.Time }
