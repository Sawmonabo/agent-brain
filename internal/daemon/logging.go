package daemon

import (
	"fmt"
	"log/slog"
	"os"
)

// maxLogSize triggers start-time rotation; one .1 generation is kept.
// A resident single-user daemon does not need a log-management stack.
const maxLogSize = 10 << 20

// openLogger rotates an oversized log and returns a JSON slog logger
// plus the file to close on shutdown.
func openLogger(logPath string) (*slog.Logger, *os.File, error) {
	if info, err := os.Stat(logPath); err == nil && info.Size() > maxLogSize {
		if err := os.Rename(logPath, logPath+".1"); err != nil {
			return nil, nil, fmt.Errorf("rotate log: %w", err)
		}
	}
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // G304: logPath is the program-derived daemon log location (config.Paths.DaemonLogFile), not untrusted input
	if err != nil {
		return nil, nil, fmt.Errorf("open log: %w", err)
	}
	return slog.New(slog.NewJSONHandler(file, nil)), file, nil
}
