package ghx

import (
	"errors"
	"fmt"
	"strings"
)

// FailureClass buckets a failed gh invocation by what a caller must do about
// it. It exists so one classifier feeds every gh-dependent surface — the hub's
// update-check, its doctor probe, init — rather than each re-deriving "is this
// the dead token?" from a formatted string. The zero value is FailureOther, the
// fail-closed default, so an unrecognized failure is never mistaken for a
// benign or an actionable one.
type FailureClass int

const (
	// FailureOther is any failure without a positive signature below: a
	// repository-not-found, an HTTP 5xx, a parse error, anything unknown. Being
	// the zero value, a caller that forgets to handle a class treats it as
	// "unknown — surface it," never as auth-invalid or offline.
	FailureOther FailureClass = iota
	// FailureAuthInvalid means gh's stored credential is present but rejected —
	// an expired or revoked OAuth token. Only a human can re-mint it through
	// GitHub's device/browser flow, so no daemon may retry it away and the hub
	// arms its sticky attention state on this class alone.
	FailureAuthInvalid
	// FailureOffline means the network path to GitHub is unreachable — a
	// transient condition that clears on its own and is never an auth problem,
	// so it must not arm the auth attention (the same fail-closed discipline
	// engine.fetchFailureIsOffline applies to git fetch stderr).
	FailureOffline
)

// ErrAuthInvalid is the sentinel a gh invocation wraps when its stderr carries a
// positive auth-invalid signature, so a caller can detect the dead token with
// errors.Is rather than string-matching a formatted message (the hub's
// update-check detector, internal/cli/dashboard). Its text names the exact and
// only remedy: an interactive re-auth no automated path can perform.
var ErrAuthInvalid = errors.New("gh authentication is invalid — run `gh auth login -h github.com`")

// Classify buckets a gh invocation's stderr by matching positive, lowercase
// substrings — auth-invalid first (the actionable class), then offline, then
// the fail-closed FailureOther default. It mirrors engine.fetchFailureIsOffline
// exactly: only known signatures qualify, everything else surfaces as unknown,
// because mislabeling a rejected token "offline" (or a dead network "auth") both
// strand the user — one behind a benign banner, the other at a re-auth prompt
// that cannot help. LC_ALL=C is not asserted here: gh emits these diagnostics in
// English regardless of locale (unlike git, whose engine wrapper pins it), and
// the match is case-insensitive besides.
func Classify(stderr string) FailureClass {
	lowered := strings.ToLower(stderr)
	for _, signature := range authInvalidSignatures {
		if strings.Contains(lowered, signature) {
			return FailureAuthInvalid
		}
	}
	for _, signature := range offlineSignatures {
		if strings.Contains(lowered, signature) {
			return FailureOffline
		}
	}
	return FailureOther
}

// authInvalidSignatures are lowercase substrings that positively identify a
// rejected gh credential. All are real gh/GitHub output:
//   - "the token in keyring is invalid" and "failed to log in to github.com
//     account" are `gh auth status`'s own lines on an expired keyring token —
//     the hub's doctor probe surface, observed live on the user's machine.
//   - "bad credentials" is GitHub's canonical HTTP 401 body, surfaced verbatim
//     by any API-backed gh call (`gh release list`, `gh api`): the signature
//     that lets the hub's update-check — not only the doctor probe — detect the
//     dead token (TestClientLogin already fixtures this exact 401).
//
// A not-found, a 5xx, or a network error never matches — by design.
var authInvalidSignatures = []string{
	"the token in keyring is invalid",
	"failed to log in to github.com account",
	"bad credentials",
}

// offlineSignatures are lowercase substrings that positively identify network
// unreachability across gh's Go net/http surface and the git it shells out to —
// the same transport-unreachable class engine.offlineFetchSignatures recognizes
// for git fetch stderr, spelled for gh's own errors (DNS, connect, and TLS
// timeouts). Auth and not-found failures never match.
var offlineSignatures = []string{
	"could not resolve host",               // git https / ssh DNS
	"no such host",                         // Go net dial DNS
	"temporary failure in name resolution", // glibc EAI_AGAIN
	"server misbehaving",                   // Go net DNS SERVFAIL
	"network is unreachable",               // kernel ENETUNREACH
	"no route to host",                     // kernel EHOSTUNREACH
	"connection refused",                   // ECONNREFUSED
	"connection timed out",                 // ETIMEDOUT (connect)
	"operation timed out",                  // macOS ETIMEDOUT spelling
	"i/o timeout",                          // Go net deadline
	"tls handshake timeout",                // Go net/http TLS stall
}

// classifyFailure turns a non-zero gh invocation into an error that names the
// operation and gh's own stderr, and — when the stderr carries a positive
// auth-invalid signature — wraps ErrAuthInvalid so a caller detects the dead
// token with errors.Is instead of re-parsing this message. Non-auth failures
// keep the plain "op: stderr" shape every existing caller and test expects, so
// only the auth-invalid path changes what callers see.
func classifyFailure(op string, result Result) error {
	stderr := strings.TrimSpace(result.Stderr)
	if Classify(result.Stderr) == FailureAuthInvalid {
		return fmt.Errorf("%s: %s: %w", op, stderr, ErrAuthInvalid)
	}
	return fmt.Errorf("%s: %s", op, stderr)
}
