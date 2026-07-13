package engine

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// History and BlobAt are the data source for spec 01-dashboard-hub-spec.md
// §6 (history & time-travel): pure git reads over the checkout the engine
// already owns. The daemon (Task 2) serializes calls onto the engine
// goroutine so these reads never race the sync writer, but neither
// function here stages, commits, or writes anything itself — that is the
// whole point of exposing them as reads.

const (
	historyDefaultLimit = 50
	historyMaxLimit     = 500
	// historyBlobByteCap caps BlobAt's returned content at the STORED (pre-
	// textconv) blob size. The daemon's API client caps response bodies at
	// 1 MiB and memory files are KB-scale, so this is a generous ceiling.
	historyBlobByteCap = 256 << 10
)

var (
	// ErrBlobTooLarge means BlobAt's target exceeds historyBlobByteCap.
	ErrBlobTooLarge = errors.New("blob exceeds the API size cap")
	// ErrBlobBinary means BlobAt's target, after the checkout's own
	// textconv decrypt wiring ran, is not valid UTF-8 text (or contains a
	// NUL byte) — never a sign of raw ciphertext leaking, since textconv
	// already ran before this check.
	ErrBlobBinary = errors.New("blob is not valid UTF-8 text")
	// ErrBadHistoryInput means folder, path, or rev failed shape validation
	// in validateHistoryInputs before any git subprocess ran. The daemon
	// (Task 2) maps it to a 400 via errors.Is — the caller named something
	// malformed, not a server failure — rather than pattern-matching
	// message text.
	ErrBadHistoryInput = errors.New("history: invalid input")
	// ErrHistoryNotFound means a syntactically-valid rev or folder/path did
	// not resolve to a real git object: an unknown rev, or a path never
	// tracked at that rev. BlobAt detects this with a `rev-parse --verify
	// --quiet` existence probe (exit code as data via gitx.RunStatus, never
	// git's stderr text — see BlobAt's own doc comment). The daemon maps
	// this to a 400 via errors.Is, same as ErrBadHistoryInput: the caller
	// named something that does not exist, which is honestly their mistake,
	// not a server failure — distinct from any LATER git failure once an
	// object is confirmed to exist, which IS a server failure.
	ErrHistoryNotFound = errors.New("history: not found")
)

// revPattern is the only shape BlobAt accepts for rev: a full or
// abbreviated lowercase-hex commit hash — exactly the shape every rev a
// History reply emits already has. Never a symbolic ref (HEAD, a branch
// name) and never anything that could be mistaken for a git option.
var revPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// HistoryVersion is one commit touching the queried folder/path, newest first.
type HistoryVersion struct {
	Rev     string    // full commit hash
	Subject string    // verbatim commit subject
	Host    string    // parsed from a capture subject; "" otherwise
	Stamp   time.Time // parsed from a capture subject; zero otherwise
	Paths   []string  // folder-relative changed paths; populated only in folder-wide mode
	Live    bool      // path mode only: this rev's blob for path equals HEAD's
}

// History lists commits touching folder (path == "", folder-wide mode) or
// folder/path (path-mode), newest first, capped at limit. Before running
// `git log`, it probes HEAD itself via `rev-parse --verify --quiet`
// (gitx.RunStatus, exit code as data — never git's stderr text, the same
// pattern markLive already uses for a single ref). A non-zero exit means an
// unborn HEAD — a checkout with zero commits — which is an honestly empty
// history, not a failure, so History returns (nil, nil) rather than ever
// running `git log` against a ref that cannot resolve. Once HEAD is
// confirmed born, it runs exactly one `git log` subprocess and parses its
// output once; path mode follows up with the Live resolution (one
// `rev-parse` per returned version, plus one for HEAD) since content
// identity cannot be read off the log line itself. Any failure from this
// point on (the log itself, or markLive's resolves) is a genuine
// infrastructure error and returns as-is — never repackaged, so the daemon
// (Task 2) can tell it apart from an ordinary empty/not-found outcome.
//
// Folder-wide versions carry their changed Paths and never set Live
// (Live is a path-mode question: "is THIS path's content live", which is
// meaningless without a path). Path-mode versions carry no Paths — the
// pathspec already narrows every record to that one file, so the field
// would be redundant — and do set Live.
func (e *Engine) History(ctx context.Context, folder, path string, limit int) ([]HistoryVersion, error) {
	if err := validateHistoryInputs(folder, path, ""); err != nil {
		return nil, err
	}
	limit = clampHistoryLimit(limit)

	headResult, err := gitx.RunStatus(ctx, e.checkout, "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		return nil, err
	}
	if headResult.ExitCode != 0 {
		return nil, nil // unborn HEAD (zero commits yet) — an empty history is data, not a failure
	}

	pathspec := folder
	if path != "" {
		pathspec = folder + "/" + path
	}
	// --no-renames keeps a renamed-away file's earlier commits keyed to
	// their ORIGINAL name in --name-only's list, matching what folder-wide
	// callers expect from "the changed paths at the time". -z with
	// --format's own %x01/%x00 delimiters (never valid in a hash and never
	// chosen as a field boundary by us) makes the whole reply one
	// unambiguously-parseable blob regardless of what bytes a hostile
	// subject or filename contains.
	result, err := gitx.Run(ctx, e.checkout, "log", "--no-renames",
		fmt.Sprintf("--max-count=%d", limit), "-z", "--name-only",
		"--format=%x01%H%x00%s", "--", pathspec)
	if err != nil {
		return nil, err
	}
	versions := parseHistoryRecords(result.Stdout, folder)

	if path != "" {
		// Path mode's pathspec is one file, so --name-only's per-record list
		// is always that same (redundant) single entry; Paths is a
		// folder-wide-mode field by contract (see the struct doc comment),
		// so clear it here rather than teach the mode-agnostic parser about
		// query mode.
		for i := range versions {
			versions[i].Paths = nil
		}
		if err := e.markLive(ctx, pathspec, versions); err != nil {
			return nil, err
		}
	}
	return versions, nil
}

// clampHistoryLimit enforces the [1, historyMaxLimit] range the API
// contract promises: an unset (zero, or negative — never trust a caller
// not to pass one) limit becomes the default page size, and anything past
// the ceiling is capped rather than rejected — asking for "too many" gets
// the most this call allows, not an error.
func clampHistoryLimit(limit int) int {
	switch {
	case limit <= 0:
		return historyDefaultLimit
	case limit > historyMaxLimit:
		return historyMaxLimit
	default:
		return limit
	}
}

// markLive resolves HEAD's blob OID for pathspec once, then flags every
// version whose OWN <rev>:pathspec blob matches it — content identity, not
// "is this rev HEAD". After a restore that byte-copies an old version's
// content back as a new capture commit, both the new head and the original
// version it restored read Live: both are honestly "this content is live".
//
// Neither resolve failing is an error: HEAD's failing means the path is
// deleted right now (nothing is live, full stop); one version's failing
// means that particular commit deleted the path (that entry alone stays
// not-live). Both are ordinary, expected outcomes of browsing history, not
// exceptional ones.
func (e *Engine) markLive(ctx context.Context, pathspec string, versions []HistoryVersion) error {
	headResult, err := gitx.RunStatus(ctx, e.checkout, "rev-parse", "--verify", "--quiet", "HEAD:"+pathspec)
	if err != nil {
		return err
	}
	if headResult.ExitCode != 0 {
		return nil // path deleted at HEAD — nothing is live
	}
	headOID := strings.TrimSpace(headResult.Stdout)
	for i := range versions {
		result, err := gitx.RunStatus(ctx, e.checkout, "rev-parse", "--verify", "--quiet", versions[i].Rev+":"+pathspec)
		if err != nil {
			return err
		}
		if result.ExitCode == 0 && strings.TrimSpace(result.Stdout) == headOID {
			versions[i].Live = true
		}
	}
	return nil
}

// BlobAt returns the plaintext content of folder/path as it stood at rev.
// `cat-file --textconv` IS the checkout's own decrypt wiring (spec §5's
// filter.agentbrain, wired to diff.agentbrain.textconv by
// gitx.InstallFilters) — BlobAt never implements a decrypt path of its
// own, so its output is only ever as trustworthy as that wiring already is.
//
// Guard order matters, and draws a hard line between "the caller asked for
// something that doesn't exist" and "git itself failed". First, a
// `rev-parse --verify --quiet` existence probe resolves blobRef via
// gitx.RunStatus — exit code as data, never git's stderr text (git's
// human-facing messages are not a stable interface: they shift across
// versions and locales, so branching on them is a latent bug) — and
// a non-zero exit is ErrHistoryNotFound: an ordinary, caller-facing outcome
// (an unknown rev, or a path never tracked at that rev), not a server
// failure. Only once existence is confirmed does the size probe run: it
// reads the STORED (pre-textconv) blob size before any content leaves git —
// cheaper than decrypting first, and a safe proxy for plaintext size
// because AES-SIV overhead is a small additive constant, never a
// multiplier, so an oversize ciphertext blob implies an oversize plaintext
// too. The UTF-8/NUL check runs only after a successful content fetch. Any
// failure from this point on (the size probe, cat-file --textconv, or its
// size parse) is a genuine infrastructure error on an object git already
// confirmed exists, and returns as-is — never repackaged — so the daemon
// (Task 2) maps it to a 500 rather than blaming the caller.
func (e *Engine) BlobAt(ctx context.Context, folder, path, rev string) ([]byte, error) {
	if rev == "" {
		return nil, fmt.Errorf("history: rev is required")
	}
	if err := validateHistoryInputs(folder, path, rev); err != nil {
		return nil, err
	}
	blobRef := fmt.Sprintf("%s:%s/%s", rev, folder, path)

	resolveResult, err := gitx.RunStatus(ctx, e.checkout, "rev-parse", "--verify", "--quiet", blobRef)
	if err != nil {
		return nil, err
	}
	if resolveResult.ExitCode != 0 {
		return nil, fmt.Errorf("%w: %s/%s@%s", ErrHistoryNotFound, folder, path, rev)
	}

	sizeResult, err := gitx.Run(ctx, e.checkout, "cat-file", "-s", blobRef)
	if err != nil {
		return nil, err
	}
	trimmedSize := strings.TrimSpace(sizeResult.Stdout)
	size, err := strconv.Atoi(trimmedSize)
	if err != nil {
		return nil, fmt.Errorf("history: parse blob size %q: %w", trimmedSize, err)
	}
	if size > historyBlobByteCap {
		return nil, fmt.Errorf("history: %w", ErrBlobTooLarge)
	}

	contentResult, err := gitx.Run(ctx, e.checkout, "cat-file", "--textconv", blobRef)
	if err != nil {
		return nil, err
	}
	if !utf8.ValidString(contentResult.Stdout) || strings.IndexByte(contentResult.Stdout, 0) >= 0 {
		return nil, fmt.Errorf("history: %w", ErrBlobBinary)
	}
	return []byte(contentResult.Stdout), nil
}

// validateHistoryInputs fails closed before any git subprocess sees an
// argument: folder and path are user-influenced wire inputs (Task 2), and
// pathspecs beginning with "-" or containing ".." must never reach git.
// rev is checked only when non-empty: History has no rev of its own (it
// calls in with ""); BlobAt requires one and rejects an empty rev itself
// before ever reaching here.
func validateHistoryInputs(folder, path, rev string) error {
	// repo.ValidateFolderName's reservedFolders set (repo/names.go) rejects
	// GlobalFolder ("_global") outright — a name-contract collision guard
	// against a user picking that folder name for a real project. But
	// GlobalFolder is ALSO the real, on-disk checkout directory global-scope
	// providers mirror into (repo.Layout.UnitDir), so history/blob lookups
	// must accept the one name that guard exists to reject for project
	// registration.
	if folder != repo.GlobalFolder {
		if err := repo.ValidateFolderName(folder); err != nil {
			return fmt.Errorf("%w: %w", ErrBadHistoryInput, err)
		}
	}
	if path != "" {
		if err := repo.ValidateRelPath(path); err != nil {
			return fmt.Errorf("%w: %w", ErrBadHistoryInput, err)
		}
	}
	if rev != "" && !revPattern.MatchString(rev) {
		return fmt.Errorf("%w: invalid rev %q", ErrBadHistoryInput, rev)
	}
	return nil
}

// parseCaptureSubject extracts (host, stamp) from the engine's own
// capture-subject convention (commit.go); ok=false leaves the caller
// rendering the subject verbatim — foreign commits are data, not errors.
// The convention is `memory: <host> <folder> <timestamp>`: exactly four
// space-separated fields, the first literally "memory:", the last parsed
// as time.RFC3339. Integrate merges and hand-made or hostile subjects fail
// one of these checks and render as ok=false — never a panic, never a
// partial parse. Manifest commits ("memory: <host> manifest <stamp>")
// WOULD parse — same shape, folder position reading "manifest" — but they
// never reach this parser through History: they touch only .agent-brain/
// paths, so the folder pathspec filters them out of every log query.
func parseCaptureSubject(subject string) (host string, stamp time.Time, ok bool) {
	fields := strings.Split(subject, " ")
	if len(fields) != 4 || fields[0] != "memory:" {
		return "", time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, fields[3])
	if err != nil {
		return "", time.Time{}, false
	}
	return fields[1], parsed, true
}

// parseHistoryRecords splits `--format=%x01%H%x00%s --name-only -z` output.
// Each record is hash NUL subject, optionally followed by NUL-separated
// changed paths; when paths are present, git inserts a literal newline —
// the ordinary blank-line separator between a commit message and its
// name-only list, which -z does not convert — right before the first one,
// stripped here. A commit git's default history simplification renders
// path-less (typically a merge) yields a record with no path fields at
// all: zero Paths, tolerated as data, not an error.
//
// A hostile subject or filename containing \x01 forges a fake record
// boundary, but it can only garble THAT ONE record's own fields (a
// misplaced hash, subject, or path confined to that single entry) — every
// other record's fields come straight from git's own %H/%s/--name-only
// output and are never reachable from another record's bytes. Every field
// access below is length-guarded first, so even a maximally garbled
// fragment (as short as a single field) yields a partial HistoryVersion,
// never an out-of-range panic.
func parseHistoryRecords(raw, folder string) []HistoryVersion {
	if raw == "" {
		return nil
	}
	prefix := folder + "/"
	records := strings.Split(raw, "\x01")
	versions := make([]HistoryVersion, 0, len(records))
	for _, record := range records {
		if record == "" {
			continue // the leading empty split before git's own first \x01
		}
		fields := strings.Split(record, "\x00")
		version := HistoryVersion{Rev: fields[0]}
		if len(fields) > 1 {
			version.Subject = fields[1]
			version.Host, version.Stamp, _ = parseCaptureSubject(fields[1])
		}
		if len(fields) > 2 {
			for i, changed := range fields[2:] {
				if i == 0 {
					changed = strings.TrimPrefix(changed, "\n")
				}
				if changed == "" {
					continue
				}
				version.Paths = append(version.Paths, strings.TrimPrefix(changed, prefix))
			}
		}
		versions = append(versions, version)
	}
	return versions
}
