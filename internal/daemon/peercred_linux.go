//go:build linux

package daemon

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// defaultPeerUID reads SO_PEERCRED: the kernel-attested UID of the
// process that connected (ADR 09).
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
		ucred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			credErr = err
			return
		}
		uid = int(ucred.Uid)
	})
	if controlErr != nil {
		return 0, controlErr
	}
	return uid, credErr
}
