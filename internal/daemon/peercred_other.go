//go:build !linux && !darwin

package daemon

import (
	"errors"
	"net"
)

// defaultPeerUID fails closed on platforms without a verified peer
// credential API — every request is rejected rather than trusted.
var defaultPeerUID peerUIDFunc = func(net.Conn) (int, error) {
	return 0, errors.New("peer credentials unsupported on this platform")
}
