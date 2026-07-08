// Package repo models the agent-brain-memories repository: its on-disk
// layout, name contracts, canonical .gitattributes, registries, and
// per-host manifests (spec §3).
package repo

import (
	"fmt"
	"regexp"
	"strings"
)

// folderRE pins project-folder names: start alphanumeric, then a
// path-safe set, capped at 100. Leading '.' and '_' are excluded by the
// start class — '.' collides with meta/VCS space, '_' is the reserved
// prefix (GlobalFolder).
var folderRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// reservedFolders are rejected regardless of charset. folderRE already
// blocks '.'/'_' starts; these entries document the collision class and
// defend it even if the regexp is ever loosened.
var reservedFolders = map[string]bool{
	".":              true,
	"..":             true,
	MetaDirName:      true,
	GlobalFolder:     true,
	".git":           true,
	".gitattributes": true,
}

// ValidateFolderName gates every project-folder name before it becomes a
// repo path segment: registry writes, enrollment, and migration all call
// this. POSIX-only targets (macOS/Linux/WSL2-ext4), so Windows reserved
// device names are deliberately out of scope.
func ValidateFolderName(name string) error {
	if reservedFolders[name] {
		return fmt.Errorf("folder name %q is reserved", name)
	}
	if !folderRE.MatchString(name) {
		return fmt.Errorf("folder name %q violates the name contract (%s)", name, folderRE)
	}
	return nil
}

// hostSafe is the byte set preserved by SanitizeHostname.
var hostSafe = regexp.MustCompile(`[^A-Za-z0-9.-]`)

// SanitizeHostname makes a hostname safe for manifest filenames and
// commit-message tokens: every byte outside [A-Za-z0-9.-] becomes '-',
// output is capped at 100 bytes, and empty input becomes "unknown-host".
// '/' is outside the allowed set, so no output can traverse directories.
func SanitizeHostname(host string) string {
	cleaned := hostSafe.ReplaceAllString(host, "-")
	if len(cleaned) > 100 {
		cleaned = cleaned[:100]
	}
	if cleaned == "" || strings.Trim(cleaned, ".-") == "" {
		return "unknown-host"
	}
	return cleaned
}
