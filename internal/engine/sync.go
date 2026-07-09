package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// Sync runs one full cycle (spec §4): recover → mirror-in → commit →
// integrate → reconcile → commit → mirror-out → commit meta → push.
// It is the engine's only exported behavior and must never run
// concurrently with itself.
func (e *Engine) Sync(ctx context.Context, units []repo.Unit) (Report, error) {
	if !e.busy.CompareAndSwap(false, true) {
		return Report{}, ErrBusy
	}
	defer e.busy.Store(false)

	var report Report
	stamp := e.stamp()

	// Recover a crashed rebase/merge AND scrub git-meta poison already
	// resident in the checkout, BEFORE mirror-in copies memory and
	// commitProjects commits it. On a checkout freshly cloned from a
	// poisoned main, this is the only scrub that runs before the first
	// `git add` (see prepareCheckout, spec §5).
	scrubbed, err := e.prepareCheckout(ctx)
	if err != nil {
		return report, err
	}
	report.Scrubbed = append(report.Scrubbed, scrubbed...)

	manifestPath := e.layout.ManifestFile(e.host)
	manifest, err := repo.LoadManifest(manifestPath)
	if err != nil {
		return report, err
	}

	inStats, snapshot, err := e.mirrorIn(ctx, units, manifest)
	report.MirrorIn = inStats
	if err != nil {
		return report, err
	}
	if err := manifest.Save(manifestPath); err != nil {
		return report, err
	}
	subjects, err := e.commitProjects(ctx, stamp)
	report.Commits = append(report.Commits, subjects...)
	if err != nil {
		return report, err
	}
	if metaSubject, err := e.commitMeta(ctx, stamp); err != nil {
		return report, err
	} else if metaSubject != "" {
		report.Commits = append(report.Commits, metaSubject)
	}

	integ, err := e.integrate(ctx)
	if err != nil {
		return report, err
	}
	// The in-memory manifest stays authoritative across integrate:
	// manifests/<host>.json is written only by this host, so a rebase
	// cannot change it underneath us.
	skip := map[string]bool{}
	for _, folder := range integ.Degraded {
		skip[folder] = true
	}
	if integ.DegradedAll {
		for _, u := range units {
			skip[u.Folder] = true
		}
	}
	report.Degraded = sortedKeys(skip)

	if integ.Integrated {
		// SECURITY CONTRACT (spec §5), ENFORCED by scrubIntegrated:
		// integrate may have delivered git-meta poison — a
		// .gitattributes/.gitignore at ANY depth (including folder level,
		// one above the unit dirs mirror-in's scrub covers) or a rewritten
		// root .gitattributes that unscopes the encryption filter. git
		// consults the WORKTREE .gitattributes at add time, so committing
		// beside poison stores plaintext. scrubIntegrated runs FIRST here,
		// before any post-integrate write: it deletes every non-root
		// git-meta path and heals the root file byte-identical to the
		// canonical generation. Every post-integrate writer (reconcile's
		// index writes) is covered ONLY while it stays inside this block's
		// ordering: scrub → write → commit. The commitProjects below commits
		// both the scrub's healing and reconcile's output, so fixes
		// propagate on push.
		//
		// This block covers poison this cycle's integrate just delivered.
		// Poison already RESIDENT when the cycle started — a fresh clone of
		// a poisoned main — is scrubbed by prepareCheckout at the top of
		// Sync, which is also what covers the standalone admin commits
		// (register/purge/seed) that never enter this block at all.
		healed, err := e.scrubIntegrated(ctx)
		if err != nil {
			return report, err
		}
		report.Scrubbed = append(report.Scrubbed, healed...)
		if err := e.reconcile(ctx, units, skip); err != nil {
			return report, err
		}
		subjects, err := e.commitProjects(ctx, stamp)
		report.Commits = append(report.Commits, subjects...)
		if err != nil {
			return report, err
		}
	}

	outStats, err := e.mirrorOut(ctx, units, manifest, snapshot, skip)
	report.MirrorOut = outStats
	if err != nil {
		return report, err
	}
	if err := manifest.Save(manifestPath); err != nil {
		return report, err
	}
	if metaSubject, err := e.commitMeta(ctx, stamp); err != nil {
		return report, err
	} else if metaSubject != "" {
		report.Commits = append(report.Commits, metaSubject)
	}

	if !integ.Integrated {
		// Offline or degraded: a push is known-doomed; queue instead
		// of burning the retry loop (git-native queue, spec §11).
		queued, err := e.hasUnpushed(ctx)
		if err != nil {
			return report, err
		}
		report.PushQueued = queued
		return report, nil
	}
	pushed, err := e.push(ctx)
	if err != nil {
		return report, err
	}
	report.Pushed = pushed.Pushed
	report.PushQueued = pushed.Queued
	for _, folder := range pushed.Degraded {
		if !skip[folder] {
			skip[folder] = true
		}
	}
	if pushed.DegradedAll {
		for _, u := range units {
			skip[u.Folder] = true
		}
	}
	report.Degraded = sortedKeys(skip)
	return report, nil
}

func (e *Engine) hasUnpushed(ctx context.Context) (bool, error) {
	ahead, err := gitx.Run(ctx, e.checkout, "rev-list", "--count", upstreamRef+"..HEAD")
	if err != nil {
		return false, fmt.Errorf("unpushed count: %w", err)
	}
	return strings.TrimSpace(ahead.Stdout) != "0", nil
}

func sortedKeys(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
