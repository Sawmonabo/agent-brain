//go:build darwin

package daemon

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// defaultPeerUID reads LOCAL_PEERCRED: the kernel-attested effective
// UID of the connecting process (ADR 09).
var defaultPeerUID peerUIDFunc = func(conn net.Conn) (int, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("peer credentials: not a unix connection (%T)", conn)
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid int
	var credErr error
	controlErr := raw.Control(func(fd uintptr) {
		xucred, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil {
			credErr = err
			return
		}
		uid = int(xucred.Uid)
	})
	if controlErr != nil {
		return 0, controlErr
	}
	return uid, credErr
}
