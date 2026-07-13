package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// commitProjects implements spec §4 step 2: one commit per project
// folder with changes, message exactly `memory: <host> <project>
// <timestamp>`; a clean tree is a no-op. Returns the commit subjects.
func (e *Engine) commitProjects(ctx context.Context, stamp string) ([]string, error) {
	folders, err := e.changedTopLevels(ctx)
	if err != nil {
		return nil, err
	}
	var subjects []string
	for _, folder := range folders {
		if folder == repo.MetaDirName {
			continue // .agent-brain commits via commitMeta
		}
		subject := fmt.Sprintf("memory: %s %s %s", e.host, folder, stamp)
		created, err := e.commitPaths(ctx, subject, folder)
		if err != nil {
			return subjects, err
		}
		if created {
			subjects = append(subjects, subject)
		}
	}
	return subjects, nil
}

// commitMeta commits .agent-brain/** (manifest and registry deltas)
// under its own deterministic subject — the manifest spans projects, so
// it never rides an arbitrary project's commit. Returns "" when clean.
func (e *Engine) commitMeta(ctx context.Context, stamp string) (string, error) {
	subject := fmt.Sprintf("memory: %s manifest %s", e.host, stamp)
	created, err := e.commitPaths(ctx, subject, repo.MetaDirName)
	if err != nil || !created {
		return "", err
	}
	return subject, nil
}

// changedTopLevels parses `git status --porcelain -z` into the sorted
// set of top-level path segments with any change (worktree or index).
func (e *Engine) changedTopLevels(ctx context.Context) ([]string, error) {
	res, err := gitx.Run(ctx, e.checkout, "status", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	// -z: NUL-terminated `XY path` records; rename records carry a
	// second NUL-terminated origin path immediately after.
	fields := strings.Split(res.Stdout, "\x00")
	for i := 0; i < len(fields); i++ {
		record := fields[i]
		if len(record) < 4 {
			continue
		}
		statusXY, changedPath := record[:2], record[3:]
		set[topSegment(changedPath)] = true
		if statusXY[0] == 'R' || statusXY[0] == 'C' {
			i++ // consume the rename/copy origin path record
			if i < len(fields) && fields[i] != "" {
				set[topSegment(fields[i])] = true
			}
		}
	}
	folders := make([]string, 0, len(set))
	for folder := range set {
		folders = append(folders, folder)
	}
	sort.Strings(folders)
	return folders, nil
}

func topSegment(p string) string {
	if before, _, ok := strings.Cut(p, "/"); ok {
		return before
	}
	return p
}

// commitPaths stages pathspec and commits when it has changes; a clean
// pathspec is a no-op. The status gate only guarantees there is
// something to commit — not that `git add -A -- <pathspec>` will
// succeed. `git status` reports staged changes even when pathspec no
// longer exists anywhere (e.g. mirror-in's deletion pass `git rm`ing a
// project's last file removes the now-empty folder from the working
// tree, leaving the deletion staged in the index only), but `git add`
// matches pathspecs against the working tree/index and fails ("pathspec
// did not match any files") once the folder is gone from both. So the
// add only runs while pathspec still exists in the working tree;
// otherwise the deletions are already staged by `git rm` and we go
// straight to the commit.
func (e *Engine) commitPaths(ctx context.Context, subject, pathspec string) (bool, error) {
	changed, err := gitx.Run(ctx, e.checkout, "status", "--porcelain", "-z", "--", pathspec)
	if err != nil {
		return false, err
	}
	if len(changed.Stdout) == 0 {
		return false, nil // nothing under pathspec to commit
	}
	if _, err := os.Lstat(filepath.Join(e.checkout, pathspec)); err == nil {
		if _, err := gitx.Run(ctx, e.checkout, "add", "-A", "--", pathspec); err != nil {
			return false, err
		}
	}
	staged, err := gitx.RunStatus(ctx, e.checkout, "diff", "--cached", "--quiet")
	if err != nil {
		return false, err
	}
	if staged.ExitCode == 0 {
		return false, nil // nothing staged
	}
	if _, err := gitx.Run(ctx, e.checkout, "commit", "--quiet", "-m", subject); err != nil {
		return false, err
	}
	return true, nil
}
