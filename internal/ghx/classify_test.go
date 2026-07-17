package ghx_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/ghx"
	"github.com/Sawmonabo/agent-brain/internal/ghx/ghxtest"
)

// TestClassify is the classifier corpus: real gh/GitHub stderr lines map to the
// class a caller acts on. Auth-invalid arms the hub's attention; offline never
// does (a transient network path, not a dead token); everything unrecognized
// stays FailureOther, so an unknown failure is never mistaken for either — the
// same fail-closed discipline engine.fetchFailureIsOffline applies to git.
func TestClassify(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		stderr string
		want   ghx.FailureClass
	}{
		// Auth-invalid — the two `gh auth status` lines the doctor probe emits on
		// an expired keyring token, observed live on the user's machine.
		{
			name:   "keyring token invalid",
			stderr: "github.com\n  X Failed to log in to github.com account Sawmonabo (keyring)\n  - The token in keyring is invalid.\n  - To re-authenticate, run: gh auth login -h github.com\n",
			want:   ghx.FailureAuthInvalid,
		},
		{
			name:   "failed to log in line alone",
			stderr: "Failed to log in to github.com account Sawmonabo (keyring)\n",
			want:   ghx.FailureAuthInvalid,
		},
		// Auth-invalid — GitHub's HTTP 401 body, surfaced verbatim by any
		// API-backed gh call (release list, api user): the signature that lets
		// the hub's update-check detect the dead token, not just the doctor probe.
		{
			name:   "http 401 bad credentials from release list",
			stderr: "HTTP 401: Bad credentials (https://api.github.com/repos/Sawmonabo/agent-brain/releases?per_page=100)\n",
			want:   ghx.FailureAuthInvalid,
		},
		{
			name:   "mixed case still matches",
			stderr: "The Token In Keyring Is Invalid.",
			want:   ghx.FailureAuthInvalid,
		},
		// Offline — gh's Go net/http and git shell-out transport errors.
		{
			name:   "dns lookup failure",
			stderr: "Get \"https://api.github.com/repos/o/r/releases\": dial tcp: lookup api.github.com: no such host\n",
			want:   ghx.FailureOffline,
		},
		{
			name:   "network unreachable",
			stderr: "dial tcp 140.82.113.5:443: connect: network is unreachable\n",
			want:   ghx.FailureOffline,
		},
		{
			name:   "tls handshake timeout",
			stderr: "Get \"https://api.github.com\": net/http: TLS handshake timeout\n",
			want:   ghx.FailureOffline,
		},
		{
			name:   "could not resolve host from git",
			stderr: "fatal: unable to access 'https://github.com/o/r/': Could not resolve host: github.com\n",
			want:   ghx.FailureOffline,
		},
		// Other — recognized-but-unrelated failures must never read as either.
		{
			name:   "repository not found",
			stderr: "GraphQL: Could not resolve to a Repository with the name 'o/r'. (repository)\n",
			want:   ghx.FailureOther,
		},
		{
			name:   "http 404",
			stderr: "HTTP 404: Not Found (https://api.github.com/repos/o/r/releases)\n",
			want:   ghx.FailureOther,
		},
		{
			name:   "http 500",
			stderr: "HTTP 500: Internal Server Error\n",
			want:   ghx.FailureOther,
		},
		{
			name:   "empty stderr",
			stderr: "",
			want:   ghx.FailureOther,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := ghx.Classify(test.stderr); got != test.want {
				t.Errorf("Classify(%q) = %v, want %v", test.stderr, got, test.want)
			}
		})
	}
}

// TestClassifyZeroValueIsOther pins the fail-closed default: FailureOther is the
// zero value, so a caller that forgets a case treats an unknown failure as
// "surface it," never as auth-invalid or offline.
func TestClassifyZeroValueIsOther(t *testing.T) {
	t.Parallel()
	var zero ghx.FailureClass
	if zero != ghx.FailureOther {
		t.Errorf("zero FailureClass = %v, want FailureOther", zero)
	}
}

// TestListReleasesWrapsAuthInvalid proves the seam a caller relies on: a
// release-list failure whose stderr is an auth-invalid signature returns an
// error that errors.Is(ErrAuthInvalid) — the hub's update-check detector — while
// a non-auth failure (a 404) does NOT, so the attention arms only on a real dead
// token, never on any release-list error.
func TestListReleasesWrapsAuthInvalid(t *testing.T) {
	t.Parallel()
	listArgs := []string{
		"release", "list", "--repo", "owner/agent-brain",
		"--limit", "50", "--json", "tagName,isPrerelease,isDraft",
	}
	t.Run("auth-invalid wraps ErrAuthInvalid", func(t *testing.T) {
		t.Parallel()
		fake := ghxtest.New(t, ghxtest.Call{
			Args:   listArgs,
			Result: ghx.Result{ExitCode: 1, Stderr: "HTTP 401: Bad credentials\n"},
		})
		client := ghx.NewClientWithRunner(fake, "/usr/bin/gh")
		_, err := client.ListReleases(context.Background(), "owner/agent-brain", 50)
		if !errors.Is(err, ghx.ErrAuthInvalid) {
			t.Fatalf("ListReleases error = %v, want errors.Is(ErrAuthInvalid)", err)
		}
	})
	t.Run("non-auth failure does not wrap", func(t *testing.T) {
		t.Parallel()
		fake := ghxtest.New(t, ghxtest.Call{
			Args:   listArgs,
			Result: ghx.Result{ExitCode: 1, Stderr: "HTTP 404: Not Found\n"},
		})
		client := ghx.NewClientWithRunner(fake, "/usr/bin/gh")
		_, err := client.ListReleases(context.Background(), "owner/agent-brain", 50)
		if err == nil {
			t.Fatal("ListReleases with a 404 returned nil, want an error")
		}
		if errors.Is(err, ghx.ErrAuthInvalid) {
			t.Fatalf("ListReleases 404 error = %v, want NOT errors.Is(ErrAuthInvalid)", err)
		}
	})
}
