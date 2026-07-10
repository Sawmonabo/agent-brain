package engine

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/renameio/v2"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// The admin operations (RegisterProject, PurgeProject, SeedProject) are the
// checkout mutations enrollment/purge/migrate need. They run ONLY on the
// daemon's single engine goroutine (ADR 03 as an API shape): the daemon
// serves them from the same select that owns sync cycles, and each acquires
// the engine's busy guard so a mid-Sync admin call fails loudly with ErrBusy
// rather than interleaving git writes. The CLI never calls these directly —
// it is a pure UDS client.
//
// SECURITY (spec §5): each is a COMMIT-CREATING entry point that runs
// outside the sync cycle, so each calls prepareCheckout right after the busy
// guard — recovering a crashed rebase and scrubbing resident git-meta poison
// before any `git add` of its own. Without it, a machine whose checkout was
// cloned from a poisoned main would commit the seed layer as plaintext
// (F1, Phase-3 final review). Their own input-side git-meta refusals (the
// seed's source-tree scrub below) are a DIFFERENT half of the contract:
// they keep hostile git-meta out of the repo; prepareCheckout keeps
// already-resident git-meta from unscoping the filter under them.

// SeedReport says what a seed did.
type SeedReport struct {
	Folder  string
	Files   int  // files copied into the seed commit
	Skipped bool // imported-from marker already present → no-op
}

// ReencryptReport says what a full re-encrypt did. Files is the number of
// filtered blobs re-sealed under the new primary (0 when the primary was
// unchanged — a clean no-op). Pushed/PushQueued mirror the engine's push
// outcome for the single re-encrypt commit.
type ReencryptReport struct {
	Files      int
	Pushed     bool
	PushQueued bool
}

// reencryptSubject is the deterministic commit message a full re-encrypt
// carries (spec §5: "Rotation costs one full re-encrypt commit").
const reencryptSubject = "chore(key): rotate primary key"

// RegisterProject records id in the shared projects registry, creates the
// project/provider dir, commits the registration, and returns the folder
// actually recorded (collision-disambiguated by repo.Projects.Add). The
// registration is machine-shared metadata, so it commits through commit.go's
// meta convention (subject `memory: <host> manifest <stamp>`), the same path
// the manifest itself uses.
//
// Idempotent: an already-registered id returns its existing folder with no
// new commit. Global-scope providers never call this — their folder is
// repo.GlobalFolder by construction, with no projects.toml entry.
func (e *Engine) RegisterProject(ctx context.Context, providerName, id, preferredFolder string) (string, error) {
	if !e.busy.CompareAndSwap(false, true) {
		return "", ErrBusy
	}
	defer e.busy.Store(false)

	if _, err := e.prepareCheckout(ctx); err != nil {
		return "", err
	}

	projectsPath := e.layout.ProjectsFile()
	projects, err := repo.LoadProjects(projectsPath)
	if err != nil {
		return "", err
	}
	if existing, ok := projects.FolderFor(id); ok {
		// Idempotent: ensure the provider dir exists (a fresh clone lacks the
		// empty dir git cannot track) but make no commit.
		if err := os.MkdirAll(e.layout.UnitDir(existing, providerName), 0o750); err != nil {
			return "", err
		}
		return existing, nil
	}
	folder, err := projects.Add(id, preferredFolder)
	if err != nil {
		return "", err
	}
	// Projects.Save does not create its parent; the meta dir may not exist yet
	// on the first registration in a fresh checkout.
	if err := os.MkdirAll(e.layout.MetaDir(), 0o750); err != nil {
		return "", fmt.Errorf("register %s: create meta dir: %w", folder, err)
	}
	if err := projects.Save(projectsPath); err != nil {
		return "", err
	}
	if err := os.MkdirAll(e.layout.UnitDir(folder, providerName), 0o750); err != nil {
		return "", err
	}
	if _, err := e.commitMeta(ctx, e.stamp()); err != nil {
		return "", err
	}
	return folder, nil
}

// PurgeProject removes the project folder from the checkout AND its
// projects.toml entry, in one commit (history retains it, spec §7).
//
// Honest semantics: this is a THIS-MACHINE-WAS-THE-LAST-TRACKER operation.
// Another machine still tracking this project will re-seed the folder on its
// next cycle (its local memory mirrors back out) — purge does not, and cannot,
// erase the project fleet-wide.
func (e *Engine) PurgeProject(ctx context.Context, folder string) error {
	if !e.busy.CompareAndSwap(false, true) {
		return ErrBusy
	}
	defer e.busy.Store(false)

	if _, err := e.prepareCheckout(ctx); err != nil {
		return err
	}

	if err := repo.ValidateFolderName(folder); err != nil {
		return fmt.Errorf("purge: %w", err)
	}
	projectsPath := e.layout.ProjectsFile()
	projects, err := repo.LoadProjects(projectsPath)
	if err != nil {
		return err
	}
	delete(projects.Entries, folder)
	if err := os.MkdirAll(e.layout.MetaDir(), 0o750); err != nil {
		return fmt.Errorf("purge %s: create meta dir: %w", folder, err)
	}
	if err := projects.Save(projectsPath); err != nil {
		return err
	}
	// Drop the folder from the index + worktree, then sweep any untracked
	// residue. --ignore-unmatch keeps an already-empty folder from failing.
	if _, err := gitx.Run(ctx, e.checkout, "rm", "-r", "--quiet", "--ignore-unmatch", "--", folder); err != nil {
		return err
	}
	// folder is ValidateFolderName-checked (no traversal, no separators), so
	// the join stays inside the checkout; RemoveAll never follows a symlink at
	// the folder path (it unlinks the link itself).
	if err := os.RemoveAll(filepath.Join(e.checkout, folder)); err != nil {
		return fmt.Errorf("purge %s: remove worktree dir: %w", folder, err)
	}
	// Stage the projects.toml delta beside the folder removal so both land in
	// ONE commit.
	if _, err := gitx.Run(ctx, e.checkout, "add", "-A", "--", repo.MetaDirName); err != nil {
		return err
	}
	staged, err := gitx.RunStatus(ctx, e.checkout, "diff", "--cached", "--quiet")
	if err != nil {
		return err
	}
	if staged.ExitCode == 0 {
		return nil // folder was neither tracked nor registered — nothing to purge
	}
	subject := fmt.Sprintf("purge: %s (%s)", folder, e.host)
	if _, err := gitx.Run(ctx, e.checkout, "commit", "--quiet", "-m", subject); err != nil {
		return err
	}
	return nil
}

// SeedProject imports a bash-era memory tree as the SEED layer (spec §10
// steps 3–5): it copies srcDir's files into <folder>/<provider>/, scrubbing
// git-meta and skipping .lock/.sync-pending droppings, and commits them
// together with the host manifest's imported-from marker (slug → folder) in
// ONE commit. The marker makes every later call a no-op.
//
// The daemon composes it with enrollment ORDER-SENSITIVELY (register → seed →
// enroll → cycle) so the live overlay lands as the second layer over the seed
// (spec §10 step 4). The copy loop's git-meta refusal is this path's slice of
// the spec §5 scrub contract: a hostile .gitattributes in the legacy tree
// never reaches a git object, so it cannot unscope the encryption filter.
func (e *Engine) SeedProject(ctx context.Context, folder, providerName, slug, srcDir string) (SeedReport, error) {
	if !e.busy.CompareAndSwap(false, true) {
		return SeedReport{}, ErrBusy
	}
	defer e.busy.Store(false)

	if _, err := e.prepareCheckout(ctx); err != nil {
		return SeedReport{}, err
	}

	if err := repo.ValidateFolderName(folder); err != nil {
		return SeedReport{}, fmt.Errorf("seed: %w", err)
	}
	manifestPath := e.layout.ManifestFile(e.host)
	manifest, err := repo.LoadManifest(manifestPath)
	if err != nil {
		return SeedReport{}, err
	}
	if existing, done := manifest.ImportedFrom[slug]; done {
		return SeedReport{Folder: existing, Skipped: true}, nil
	}

	destDir := e.layout.UnitDir(folder, providerName)
	copied := 0
	err = filepath.WalkDir(srcDir, func(abs string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(srcDir, abs)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		name := path.Base(rel)
		if isGitMetaPath(rel) || name == ".lock" || strings.HasSuffix(name, ".sync-pending") {
			return nil // bash-era droppings and git-meta never enter the seed
		}
		if err := repo.ValidateRelPath(rel); err != nil {
			return fmt.Errorf("seed %s: hostile path: %w", slug, err)
		}
		content, readErr := os.ReadFile(abs) //nolint:gosec // G304: abs is a file under the user-named legacy seed dir; rel is git-meta-scrubbed and ValidateRelPath-checked above
		if readErr != nil {
			return readErr
		}
		target := filepath.Join(destDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		if err := renameio.WriteFile(target, content, 0o644); err != nil {
			return err
		}
		copied++
		return nil
	})
	if err != nil {
		return SeedReport{}, err
	}

	if manifest.ImportedFrom == nil {
		manifest.ImportedFrom = map[string]string{}
	}
	manifest.ImportedFrom[slug] = folder
	if err := manifest.Save(manifestPath); err != nil {
		return SeedReport{}, err
	}

	// Stage the seed files (when any landed) beside the manifest marker, then
	// commit both in ONE commit so the layer and its marker are atomic.
	manifestRel, err := filepath.Rel(e.checkout, manifestPath)
	if err != nil {
		return SeedReport{}, err
	}
	addArgs := []string{"add", "--"}
	if copied > 0 {
		addArgs = append(addArgs, folder)
	}
	addArgs = append(addArgs, filepath.ToSlash(manifestRel))
	if _, err := gitx.Run(ctx, e.checkout, addArgs...); err != nil {
		return SeedReport{}, err
	}
	subject := fmt.Sprintf("migrate: seed %s from %s:%s", folder, e.host, slug)
	if _, err := gitx.Run(ctx, e.checkout, "commit", "--quiet", "-m", subject); err != nil {
		return SeedReport{}, err
	}
	return SeedReport{Folder: folder, Files: copied}, nil
}

// ReencryptAll re-encrypts every filtered file under the keyset's CURRENT
// primary and pushes the result (spec §5's rotation re-encrypt). It is the
// admin op behind POST /v0/reencrypt: like the others it acquires the busy
// guard and runs prepareCheckout first (recover a crashed rebase, scrub
// resident git-meta poison) so no `git add` ever consults hostile attributes.
//
// The mechanism is `git add --renormalize .`: git re-runs the clean filter over
// every tracked filter-subject file and re-stages any whose stored bytes
// changed. Deterministic AEAD means a NEW primary re-seals every memory blob to
// fresh ciphertext (each changes exactly once), while an UNCHANGED primary
// re-seals to identical bytes — nothing stages, and the op is a clean no-op
// (Files == 0, no commit). The one commit it does make carries every re-sealed
// blob; the existing push path delivers it, queuing on a non-fast-forward the
// same way a sync cycle does. Old keys are never removed here, so history blobs
// and not-yet-rotated peers keep decrypting.
func (e *Engine) ReencryptAll(ctx context.Context) (ReencryptReport, error) {
	if !e.busy.CompareAndSwap(false, true) {
		return ReencryptReport{}, ErrBusy
	}
	defer e.busy.Store(false)

	if _, err := e.prepareCheckout(ctx); err != nil {
		return ReencryptReport{}, err
	}

	if _, err := gitx.Run(ctx, e.checkout, "add", "--renormalize", "."); err != nil {
		return ReencryptReport{}, err
	}
	files, err := e.stagedFileCount(ctx)
	if err != nil {
		return ReencryptReport{}, err
	}
	if files == 0 {
		return ReencryptReport{}, nil // primary unchanged → renormalize is a no-op
	}
	if _, err := gitx.Run(ctx, e.checkout, "commit", "--quiet", "-m", reencryptSubject); err != nil {
		return ReencryptReport{}, err
	}
	pushed, err := e.push(ctx)
	if err != nil {
		return ReencryptReport{}, err
	}
	return ReencryptReport{Files: files, Pushed: pushed.Pushed, PushQueued: pushed.Queued}, nil
}

// stagedFileCount counts the paths currently staged in the index (-z: NUL
// separated, robust to any path bytes). It is how ReencryptAll turns
// renormalize's outcome into both the report's Files and the commit/no-commit
// decision.
func (e *Engine) stagedFileCount(ctx context.Context) (int, error) {
	res, err := gitx.Run(ctx, e.checkout, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return 0, err
	}
	count := 0
	for _, path := range strings.Split(res.Stdout, "\x00") {
		if path != "" {
			count++
		}
	}
	return count, nil
}
