// Package ghx_test holds the Client-level, black-box tests: they exercise
// only ghx's exported surface, driven by ghxtest.Fake. This file must be an
// external test package (ghx_test, not ghx) because ghxtest itself imports
// ghx — an internal (package ghx) test file importing ghxtest would be a
// import cycle (see ghx_test.go, which covers the execRunner internals that
// need unexported access instead).
package ghx_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/ghx"
	"github.com/Sawmonabo/agent-brain/internal/ghx/ghxtest"
)

func TestClientAuthOK(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		result  ghx.Result
		wantErr []string // substrings the error message must contain; nil means AuthOK must return nil
	}{
		{
			name:   "authenticated",
			result: ghx.Result{ExitCode: 0},
		},
		{
			name:    "not logged in",
			result:  ghx.Result{ExitCode: 1, Stderr: "You are not logged in to any GitHub hosts.\n"},
			wantErr: []string{"You are not logged in to any GitHub hosts.", "gh auth login"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fake := ghxtest.New(t, ghxtest.Call{Args: []string{"auth", "status"}, Result: test.result})
			client := ghx.NewClientWithRunner(fake, "/usr/bin/gh")

			err := client.AuthOK(context.Background())

			if test.wantErr == nil {
				if err != nil {
					t.Fatalf("AuthOK() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("AuthOK() = nil, want error")
			}
			for _, substr := range test.wantErr {
				if !strings.Contains(err.Error(), substr) {
					t.Errorf("AuthOK() error = %q, want substring %q", err, substr)
				}
			}
		})
	}
}

// TestClientAuthOKPropagatesRunnerError pins the OTHER failure shape every
// Client method must handle distinctly from gh's own non-zero exit: a Runner
// that fails outright (a killed process, a spawn failure, a canceled
// context — never a clean gh invocation) must come back from the Client
// unchanged, not be swallowed or reworded into a "gh said no" message.
func TestClientAuthOKPropagatesRunnerError(t *testing.T) {
	t.Parallel()
	runnerErr := errors.New("spawn gh [auth status]: exec: \"gh\": file does not exist")
	fake := ghxtest.New(t, ghxtest.Call{Args: []string{"auth", "status"}, Err: runnerErr})
	client := ghx.NewClientWithRunner(fake, "/usr/bin/gh")

	err := client.AuthOK(context.Background())
	if !errors.Is(err, runnerErr) {
		t.Errorf("AuthOK() error = %v, want the Runner error propagated unchanged: %v", err, runnerErr)
	}
}

func TestClientLogin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		result    ghx.Result
		wantLogin string
		wantErr   bool
	}{
		{
			name:      "success",
			result:    ghx.Result{Stdout: "Sawmonabo\n", ExitCode: 0},
			wantLogin: "Sawmonabo",
		},
		{
			name:    "empty stdout",
			result:  ghx.Result{Stdout: "", ExitCode: 0},
			wantErr: true,
		},
		{
			name:    "nonzero exit",
			result:  ghx.Result{ExitCode: 1, Stderr: "HTTP 401: Bad credentials"},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fake := ghxtest.New(t, ghxtest.Call{Args: []string{"api", "user", "--jq", ".login"}, Result: test.result})
			client := ghx.NewClientWithRunner(fake, "/usr/bin/gh")

			login, err := client.Login(context.Background())

			if test.wantErr {
				if err == nil {
					t.Fatalf("Login() = %q, nil, want error", login)
				}
				return
			}
			if err != nil {
				t.Fatalf("Login() error = %v", err)
			}
			if login != test.wantLogin {
				t.Errorf("Login() = %q, want %q", login, test.wantLogin)
			}
		})
	}
}

func TestClientRepoExists(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		result     ghx.Result
		wantExists bool
		wantErr    bool
	}{
		{
			name:       "exists",
			result:     ghx.Result{ExitCode: 0},
			wantExists: true,
		},
		{
			name:       "not found",
			result:     ghx.Result{ExitCode: 1, Stderr: "GraphQL: Could not resolve to a Repository with the name 'o/r'. (repository)\n"},
			wantExists: false,
		},
		{
			// A network failure must NOT read as "repo missing" — init would
			// otherwise try to create a repo over one that may already exist.
			name:    "network failure surfaces as error, not false",
			result:  ghx.Result{ExitCode: 1, Stderr: "connect: network is unreachable"},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fake := ghxtest.New(t, ghxtest.Call{Args: []string{"repo", "view", "o/r", "--json", "name"}, Result: test.result})
			client := ghx.NewClientWithRunner(fake, "/usr/bin/gh")

			exists, err := client.RepoExists(context.Background(), "o", "r")

			if test.wantErr {
				if err == nil {
					t.Fatalf("RepoExists() = %v, nil, want error", exists)
				}
				if exists {
					t.Error("RepoExists() = true alongside an error; want false")
				}
				return
			}
			if err != nil {
				t.Fatalf("RepoExists() error = %v", err)
			}
			if exists != test.wantExists {
				t.Errorf("RepoExists() = %v, want %v", exists, test.wantExists)
			}
		})
	}
}

func TestClientCreateRepo(t *testing.T) {
	t.Parallel()
	fake := ghxtest.New(t, ghxtest.Call{
		Args:   []string{"repo", "create", "agent-brain-memories", "--private", "--description", "agent-brain encrypted memory sync"},
		Result: ghx.Result{Stdout: "https://github.com/sawmonabo/agent-brain-memories\n"},
	})
	client := ghx.NewClientWithRunner(fake, "/usr/bin/gh")

	url, err := client.CreateRepo(context.Background(), "agent-brain-memories", "agent-brain encrypted memory sync")
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://github.com/sawmonabo/agent-brain-memories"; url != want {
		t.Errorf("CreateRepo() = %q, want %q", url, want)
	}
}

func TestClientCreateRepoError(t *testing.T) {
	t.Parallel()
	fake := ghxtest.New(t, ghxtest.Call{
		Args:   []string{"repo", "create", "agent-brain-memories", "--private", "--description", "d"},
		Result: ghx.Result{ExitCode: 1, Stderr: "HTTP 422: Repository creation failed."},
	})
	client := ghx.NewClientWithRunner(fake, "/usr/bin/gh")

	if _, err := client.CreateRepo(context.Background(), "agent-brain-memories", "d"); err == nil {
		t.Fatal("CreateRepo() = nil error for nonzero exit, want error")
	}
}

func TestClientClone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		gitArgs  []string
		wantArgs []string
	}{
		{
			name:     "passthrough git args",
			gitArgs:  []string{"--no-checkout"},
			wantArgs: []string{"repo", "clone", "o/r", "/tmp/dest", "--", "--no-checkout"},
		},
		{
			name:     "no git args, no trailing separator",
			gitArgs:  nil,
			wantArgs: []string{"repo", "clone", "o/r", "/tmp/dest"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fake := ghxtest.New(t, ghxtest.Call{Args: test.wantArgs, Result: ghx.Result{ExitCode: 0}})
			client := ghx.NewClientWithRunner(fake, "/usr/bin/gh")

			if err := client.Clone(context.Background(), "o/r", "/tmp/dest", test.gitArgs...); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestClientCloneError(t *testing.T) {
	t.Parallel()
	fake := ghxtest.New(t, ghxtest.Call{
		Args:   []string{"repo", "clone", "o/r", "/tmp/dest"},
		Result: ghx.Result{ExitCode: 1, Stderr: "HTTP 404: Not Found"},
	})
	client := ghx.NewClientWithRunner(fake, "/usr/bin/gh")

	if err := client.Clone(context.Background(), "o/r", "/tmp/dest"); err == nil {
		t.Fatal("Clone() = nil error for nonzero exit, want error")
	}
}

func TestNewClient(t *testing.T) {
	dir := t.TempDir()
	ghPath := filepath.Join(dir, "gh")
	if err := os.WriteFile(ghPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("found on PATH", func(t *testing.T) {
		t.Setenv("PATH", dir)
		client, err := ghx.NewClient()
		if err != nil {
			t.Fatal(err)
		}
		if client.BinaryPath() != ghPath {
			t.Errorf("BinaryPath() = %q, want %q", client.BinaryPath(), ghPath)
		}
	})
	t.Run("missing from PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if _, err := ghx.NewClient(); !errors.Is(err, ghx.ErrMissing) {
			t.Errorf("NewClient() error = %v, want ErrMissing", err)
		}
	})
}

// TestClientListReleases pins the exact gh invocation and the JSON
// translation for `update`'s release resolution (spec §7).
func TestClientListReleases(t *testing.T) {
	t.Parallel()
	fake := ghxtest.New(t, ghxtest.Call{
		Args: []string{
			"release", "list", "--repo", "owner/agent-brain",
			"--limit", "50", "--json", "tagName,isPrerelease,isDraft",
		},
		Result: ghx.Result{Stdout: `[
			{"tagName":"v2.0.0","isPrerelease":false,"isDraft":false},
			{"tagName":"v2.0.0-rc.2","isPrerelease":true,"isDraft":false}
		]`},
	})
	client := ghx.NewClientWithRunner(fake, "/usr/bin/gh")

	releases, err := client.ListReleases(context.Background(), "owner/agent-brain", 50)
	if err != nil {
		t.Fatalf("ListReleases: %v", err)
	}
	want := []ghx.ReleaseInfo{
		{TagName: "v2.0.0"},
		{TagName: "v2.0.0-rc.2", IsPrerelease: true},
	}
	if diff := cmp.Diff(want, releases); diff != "" {
		t.Fatalf("ListReleases mismatch (-want +got):\n%s", diff)
	}
}

// TestClientListReleasesErrors proves both failure shapes surface: a
// non-zero gh exit carries gh's stderr, and unparseable stdout is an
// explicit parse error rather than a silent empty slice.
func TestClientListReleasesErrors(t *testing.T) {
	t.Parallel()
	t.Run("non-zero exit", func(t *testing.T) {
		t.Parallel()
		fake := ghxtest.New(t, ghxtest.Call{
			Args: []string{
				"release", "list", "--repo", "owner/agent-brain",
				"--limit", "50", "--json", "tagName,isPrerelease,isDraft",
			},
			Result: ghx.Result{ExitCode: 1, Stderr: "HTTP 404: Not Found\n"},
		})
		client := ghx.NewClientWithRunner(fake, "/usr/bin/gh")
		_, err := client.ListReleases(context.Background(), "owner/agent-brain", 50)
		if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
			t.Fatalf("ListReleases error = %v, want gh's stderr surfaced", err)
		}
	})
	t.Run("unparseable json", func(t *testing.T) {
		t.Parallel()
		fake := ghxtest.New(t, ghxtest.Call{
			Args: []string{
				"release", "list", "--repo", "owner/agent-brain",
				"--limit", "50", "--json", "tagName,isPrerelease,isDraft",
			},
			Result: ghx.Result{Stdout: "gh: unexpected banner text"},
		})
		client := ghx.NewClientWithRunner(fake, "/usr/bin/gh")
		_, err := client.ListReleases(context.Background(), "owner/agent-brain", 50)
		if err == nil || !strings.Contains(err.Error(), "parse json") {
			t.Fatalf("ListReleases error = %v, want a parse error", err)
		}
	})
}

// TestClientDownloadReleaseAssets pins the exact gh invocation — one
// --pattern flag per requested asset — and the stderr-carrying failure.
func TestClientDownloadReleaseAssets(t *testing.T) {
	t.Parallel()
	t.Run("success", func(t *testing.T) {
		t.Parallel()
		fake := ghxtest.New(t, ghxtest.Call{
			Args: []string{
				"release", "download", "v2.1.0", "--repo", "owner/agent-brain", "--dir", "/tmp/dl",
				"--pattern", "agent-brain_2.1.0_darwin_arm64.tar.gz",
				"--pattern", "agent-brain_2.1.0_checksums.txt",
			},
			Result: ghx.Result{ExitCode: 0},
		})
		client := ghx.NewClientWithRunner(fake, "/usr/bin/gh")
		err := client.DownloadReleaseAssets(context.Background(), "owner/agent-brain", "v2.1.0", "/tmp/dl",
			"agent-brain_2.1.0_darwin_arm64.tar.gz", "agent-brain_2.1.0_checksums.txt")
		if err != nil {
			t.Fatalf("DownloadReleaseAssets: %v", err)
		}
	})
	t.Run("failure carries stderr", func(t *testing.T) {
		t.Parallel()
		fake := ghxtest.New(t, ghxtest.Call{
			Args: []string{
				"release", "download", "v2.1.0", "--repo", "owner/agent-brain", "--dir", "/tmp/dl",
				"--pattern", "agent-brain_2.1.0_darwin_arm64.tar.gz",
			},
			Result: ghx.Result{ExitCode: 1, Stderr: "no assets match the file pattern\n"},
		})
		client := ghx.NewClientWithRunner(fake, "/usr/bin/gh")
		err := client.DownloadReleaseAssets(context.Background(), "owner/agent-brain", "v2.1.0", "/tmp/dl",
			"agent-brain_2.1.0_darwin_arm64.tar.gz")
		if err == nil || !strings.Contains(err.Error(), "no assets match") {
			t.Fatalf("DownloadReleaseAssets error = %v, want gh's stderr surfaced", err)
		}
	})
}
