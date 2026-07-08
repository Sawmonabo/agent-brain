//go:build unix

package daemon

import "golang.org/x/sys/unix"

// raiseFDLimit lifts RLIMIT_NOFILE toward 4096 (capped by the hard
// limit). kqueue on macOS consumes one descriptor per watched
// directory (ADR 07); default soft limits (256 on macOS) are too low
// for comfort. Best-effort: failure is logged, not fatal.
func raiseFDLimit() error {
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		return err
	}
	const want = 4096
	if limit.Cur >= want {
		return nil
	}
	limit.Cur = want
	if limit.Max < want {
		limit.Cur = limit.Max
	}
	return unix.Setrlimit(unix.RLIMIT_NOFILE, &limit)
}
