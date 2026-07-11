// Package gitxtest provides the one hermetic git-config posture every test
// process in this repo — and every git child a test spawns — runs under.
// Five entrypoints used to hand-roll this at three different strength
// levels; this package is the single implementation all of them now share.
//
// It is imported only by test code, never by product code, so it deliberately
// has no dependency on anything else in this repo (stdlib only) — no import
// cycle is possible from any package that adopts it. The `go tool deadcode
// -test ./...` CI gate treats every test binary as a root, so a non-_test
// package reachable only from tests is expected to be invisible to it and
// stays out of the zero-finding baseline.
package gitxtest

import (
	"os"
	"path/filepath"
)

// HermeticGitConfig is the git configuration every test process and every
// git child spawned by tests runs under. Merely pointing GIT_CONFIG_GLOBAL at
// /dev/null neutralizes the developer's config but leaves git's DEFAULT
// auto-gc/auto-maintenance enabled, which runs DETACHED and can outlive the
// test: a detached `gc --auto` runs update_server_info at exit and recreates
// .git/info/ while t.TempDir() teardown is deleting the tree. Tests therefore
// disable auto maintenance entirely (determinism); this is deliberately
// STRONGER than the production checkout posture (ADR 19,
// docs/decisions/19-adr-checkout-maintenance-posture.md: auto maintenance
// enabled but pinned to the foreground) — tests need no maintenance,
// production does.
const HermeticGitConfig = `[gc]
	auto = 0
	autoDetach = false
[maintenance]
	auto = false
`

// Setenv writes HermeticGitConfig to a fresh temp file and points this
// process's GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM at it, so every git
// invocation the process makes — and every child that inherits its
// environment — runs under the same hermetic posture. For TestMain use,
// BEFORE m.Run() and before any t.Parallel test starts: this uses os.Setenv
// rather than t.Setenv deliberately, since TestMain has no *testing.T and
// t.Setenv is incompatible with t.Parallel. Returns the written config's path
// (for Env, so a spawned child can be given the identical posture explicitly)
// and a cleanup to run after m.Run().
func Setenv() (configPath string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "gitxtest-config-*")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }

	configPath = filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(configPath, []byte(HermeticGitConfig), 0o600); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := os.Setenv("GIT_CONFIG_GLOBAL", configPath); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := os.Setenv("GIT_CONFIG_SYSTEM", os.DevNull); err != nil {
		cleanup()
		return "", nil, err
	}
	return configPath, cleanup, nil
}

// Env returns the GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM pair a CHILD process
// must carry to run under the same hermetic posture as the calling test
// process — e.g. a daemon spawned with an explicit cmd.Env that does not
// inherit os.Environ() — for appending to that cmd.Env.
func Env(configPath string) []string {
	return []string{
		"GIT_CONFIG_GLOBAL=" + configPath,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
	}
}
