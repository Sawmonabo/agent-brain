package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
)

// TestRefuseManualSyncIfQuiesced pins the M-N1 re-check the loop's syncRequests
// arm performs before running a manual cycle: a quiesce that lands after
// TriggerSync's entry check but before the loop services the request is caught
// here, on the engine goroutine, so no manual cycle runs inside a hold.
//
// It drives the decision directly rather than through a live loop. Injecting
// into the unexported syncRequests channel would need a running loop (real
// engine + ticker + watcher) just to exercise a pure decision — the repo's
// deterministic-test-substitution rule. The loop wires this in at the top of the
// syncRequests arm (daemon.go), a one-line call inspected rather than timed.
func TestRefuseManualSyncIfQuiesced(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		quiesce     bool
		wantRefused bool
	}{
		{name: "quiesced refuses before the cycle", quiesce: true, wantRefused: true},
		{name: "not quiesced proceeds to the cycle", quiesce: false, wantRefused: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := &Daemon{}
			if tc.quiesce {
				d.Quiesce(60)
			}
			request := syncRequest{reply: make(chan syncReply, 1)}

			refused := d.refuseManualSyncIfQuiesced(request, time.Now())
			if refused != tc.wantRefused {
				t.Fatalf("refuseManualSyncIfQuiesced = %v, want %v", refused, tc.wantRefused)
			}

			select {
			case reply := <-request.reply:
				if !tc.wantRefused {
					t.Fatalf("not quiesced but a reply was sent: %+v (the loop must run the cycle and reply itself)", reply)
				}
				if reply.err == nil || !strings.Contains(reply.err.Error(), "quiesced until") {
					t.Fatalf("refusal reply err = %v, want one naming the quiesce expiry", reply.err)
				}
				if reply.response != (api.SyncResponse{}) {
					t.Fatalf("refusal carried a response %+v, want the zero SyncResponse", reply.response)
				}
			default:
				if tc.wantRefused {
					t.Fatal("refused the request but sent no reply — TriggerSync would block on request.reply")
				}
			}
		})
	}
}
