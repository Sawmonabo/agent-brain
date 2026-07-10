package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// --- command-level tests: real exec path via a PATH-shimmed fake gitleaks
// (the same technique as this package's withFakeChezmoiOnPath/fakeGhOnPath,
// migrate_test.go/doctor_test.go) ---

// withFakeGitleaksOnPath installs a fake `gitleaks` on PATH that always
// prints stdout and exits with exitCode, regardless of its arguments — this
// command only ever invokes gitleaks one documented way (scanGitleaksArgs),
// so the fake doesn't need to branch on argv. It also appends the
// directory argument (argv[2]: `gitleaks dir <dir> ...`) to a log file,
// whose path it returns, so a caller that cares WHICH unit(s) were actually
// scanned (TestScanCommandProjectFlagFiltersToOneUnit) can assert on
// invocation scope rather than only on (possibly identical) canned output.
func withFakeGitleaksOnPath(t *testing.T, exitCode int, stdout string) (invocationLog string) {
	t.Helper()
	dir := t.TempDir()
	invocationLog = filepath.Join(dir, "invocations.log")
	script := filepath.Join(dir, "gitleaks")
	content := fmt.Sprintf("#!/bin/sh\necho \"$2\" >> %s\ncat <<'GITLEAKS_FAKE_EOF'\n%s\nGITLEAKS_FAKE_EOF\nexit %d\n", invocationLog, stdout, exitCode)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return invocationLog
}

// scanTestPaths points AGENT_BRAIN_CONFIG_DIR/AGENT_BRAIN_DATA_DIR at a
// fresh temp dir and returns the resolved config.Paths. Unlike doctor/init
// tests, scan needs no keyset and no checkout: it only reads the local
// registry (repo.LocalRegistry) and shells out to gitleaks directly over
// each unit's plaintext dir — it never talks to the daemon.
func scanTestPaths(t *testing.T) config.Paths {
	t.Helper()
	base := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", filepath.Join(base, "cfg"))
	t.Setenv("AGENT_BRAIN_DATA_DIR", filepath.Join(base, "data"))
	paths, err := config.DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return paths
}

// enrollUnits saves a local registry at paths' LocalRegistryFile — the
// same file scan's RunE and doctor's checkRegistryLocal both read.
func enrollUnits(t *testing.T, paths config.Paths, units ...repo.Unit) {
	t.Helper()
	registry := repo.NewLocalRegistry()
	for _, unit := range units {
		if err := registry.Enroll(unit); err != nil {
			t.Fatal(err)
		}
	}
	if err := registry.Save(paths.LocalRegistryFile()); err != nil {
		t.Fatal(err)
	}
}

func TestScanCommandFindingReportsTableRowAndExitsNonZero(t *testing.T) {
	paths := scanTestPaths(t)
	localDir := t.TempDir()
	enrollUnits(t, paths, repo.Unit{Provider: "claude", Folder: "myproj", LocalDir: localDir})
	withFakeGitleaksOnPath(t, 1, fmt.Sprintf(`[{"RuleID":"generic-api-key","Description":"Detected a Generic API Key","StartLine":3,"File":"%s/config.json","Secret":"s3cr3t","Match":"api_key: s3cr3t"}]`, localDir))

	out, err := runCmd(t, nil, "scan")
	if err == nil {
		t.Fatalf("scan with a finding succeeded; want a non-zero exit\noutput:\n%s", out)
	}
	if !strings.Contains(string(out), "generic-api-key") {
		t.Fatalf("scan output missing rule id:\n%s", out)
	}
	if !strings.Contains(string(out), "myproj") {
		t.Fatalf("scan output missing folder:\n%s", out)
	}
	if strings.Contains(string(out), "s3cr3t") {
		t.Fatalf("scan table output must not echo the raw secret text:\n%s", out)
	}
}

func TestScanCommandCleanReportsNoFindingsAndExitsZero(t *testing.T) {
	paths := scanTestPaths(t)
	enrollUnits(t, paths, repo.Unit{Provider: "claude", Folder: "myproj", LocalDir: t.TempDir()})
	withFakeGitleaksOnPath(t, 0, `[]`)

	out, err := runCmd(t, nil, "scan")
	if err != nil {
		t.Fatalf("clean scan failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(string(out), "no findings") {
		t.Fatalf("scan output missing \"no findings\":\n%s", out)
	}
}

func TestScanCommandGitleaksMissingNamesInstallHint(t *testing.T) {
	paths := scanTestPaths(t)
	enrollUnits(t, paths, repo.Unit{Provider: "claude", Folder: "myproj", LocalDir: t.TempDir()})
	t.Setenv("PATH", t.TempDir()) // empty dir: no gitleaks anywhere on PATH

	_, err := runCmd(t, nil, "scan")
	if err == nil {
		t.Fatal("scan without gitleaks installed succeeded; want an error naming the install fix")
	}
	if !strings.Contains(err.Error(), "brew install gitleaks") {
		t.Fatalf("error does not name the install fix: %v", err)
	}
}

func TestScanCommandProjectFlagFiltersToOneUnit(t *testing.T) {
	paths := scanTestPaths(t)
	dirA := t.TempDir()
	dirB := t.TempDir()
	enrollUnits(
		t, paths,
		repo.Unit{Provider: "claude", Folder: "project-a", LocalDir: dirA},
		repo.Unit{Provider: "claude", Folder: "project-b", LocalDir: dirB},
	)
	invocationLog := withFakeGitleaksOnPath(t, 0, `[]`)

	out, err := runCmd(t, nil, "scan", "--project", "project-a")
	if err != nil {
		t.Fatalf("scan --project failed: %v\noutput:\n%s", err, out)
	}
	data, readErr := os.ReadFile(invocationLog)
	if readErr != nil {
		t.Fatal(readErr)
	}
	invoked := strings.TrimSpace(string(data))
	if invoked != dirA {
		t.Fatalf("scan --project project-a invoked gitleaks on %q, want exactly %q (project-b must not be scanned)", invoked, dirA)
	}
}

func TestScanCommandProjectFlagUnknownFolderErrors(t *testing.T) {
	paths := scanTestPaths(t)
	enrollUnits(t, paths, repo.Unit{Provider: "claude", Folder: "myproj", LocalDir: t.TempDir()})
	withFakeGitleaksOnPath(t, 0, `[]`)

	_, err := runCmd(t, nil, "scan", "--project", "no-such-folder")
	if err == nil {
		t.Fatal("scan --project with an unenrolled folder succeeded; want an error")
	}
}

func TestScanCommandNoProjectsEnrolled(t *testing.T) {
	scanTestPaths(t)
	out, err := runCmd(t, nil, "scan")
	if err != nil {
		t.Fatalf("scan with nothing enrolled failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(string(out), "no projects enrolled") {
		t.Fatalf("scan output missing the empty-state message:\n%s", out)
	}
}

func TestScanCommandJSONOutputRoundTrips(t *testing.T) {
	paths := scanTestPaths(t)
	localDir := t.TempDir()
	enrollUnits(t, paths, repo.Unit{Provider: "claude", Folder: "myproj", LocalDir: localDir})
	withFakeGitleaksOnPath(t, 1, fmt.Sprintf(`[{"RuleID":"generic-api-key","Description":"d","StartLine":3,"File":"%s/x.json","Secret":"s","Match":"m"}]`, localDir))

	out, err := runCmd(t, nil, "scan", "--json")
	if err == nil {
		t.Fatalf("scan --json with a finding succeeded; want a non-zero exit\noutput:\n%s", out)
	}
	var findings []scanFinding
	if jsonErr := json.Unmarshal(out, &findings); jsonErr != nil {
		t.Fatalf("scan --json did not produce valid JSON: %v\noutput:\n%s", jsonErr, out)
	}
	if len(findings) != 1 || findings[0].Finding.RuleID != "generic-api-key" || findings[0].Folder != "myproj" {
		t.Fatalf("scan --json findings = %+v, want one generic-api-key finding in folder myproj", findings)
	}
}

func TestScanCommandJSONCleanIsEmptyArrayNotNull(t *testing.T) {
	paths := scanTestPaths(t)
	enrollUnits(t, paths, repo.Unit{Provider: "claude", Folder: "myproj", LocalDir: t.TempDir()})
	withFakeGitleaksOnPath(t, 0, `[]`)

	out, err := runCmd(t, nil, "scan", "--json")
	if err != nil {
		t.Fatalf("clean scan --json failed: %v\noutput:\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "[]" {
		t.Fatalf("scan --json clean output = %q, want []", strings.TrimSpace(string(out)))
	}
}

// --- unit-level tests: a hand-written fake gitleaksRunner, no subprocess,
// no PATH — pure coverage of scanUnit/scanUnits' merge/sort/error logic. ---

// fakeGitleaksRunner is a hand-written gitleaksRunner keyed by the dir
// argument (scanGitleaksArgs always shapes args as ["dir", dir,
// "--no-banner", ...], so args[1] is the dir this package's own scanUnit
// passed in).
type fakeGitleaksRunner struct {
	results map[string]gitleaksResult
}

func (f *fakeGitleaksRunner) Run(_ context.Context, args ...string) (gitleaksResult, error) {
	dir := args[1]
	result, ok := f.results[dir]
	if !ok {
		return gitleaksResult{}, fmt.Errorf("fakeGitleaksRunner: no canned result for %s", dir)
	}
	return result, nil
}

func TestScanUnitsMergesAndSortsAcrossUnits(t *testing.T) {
	t.Parallel()
	runner := &fakeGitleaksRunner{results: map[string]gitleaksResult{
		"/a": {ExitCode: 1, Stdout: `[{"RuleID":"r2","File":"/a/two.txt","StartLine":5}]`},
		"/b": {ExitCode: 1, Stdout: `[{"RuleID":"r1","File":"/b/one.txt","StartLine":1}]`},
	}}
	units := []repo.Unit{
		{Provider: "claude", Folder: "zzz-folder", LocalDir: "/a"},
		{Provider: "claude", Folder: "aaa-folder", LocalDir: "/b"},
	}
	findings, err := scanUnits(context.Background(), runner, units)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2", len(findings))
	}
	if findings[0].Folder != "aaa-folder" || findings[1].Folder != "zzz-folder" {
		t.Fatalf("findings not sorted by folder: %+v", findings)
	}
}

func TestScanUnitsPropagatesRunnerError(t *testing.T) {
	t.Parallel()
	runner := &fakeGitleaksRunner{results: map[string]gitleaksResult{}} // no canned results: every call errors
	units := []repo.Unit{{Provider: "claude", Folder: "f", LocalDir: "/nope"}}
	if _, err := scanUnits(context.Background(), runner, units); err == nil {
		t.Fatal("scanUnits with a failing runner succeeded; want an error")
	}
}

func TestScanUnitRejectsUnexpectedExitCode(t *testing.T) {
	t.Parallel()
	runner := &fakeGitleaksRunner{results: map[string]gitleaksResult{
		"/a": {ExitCode: 2, Stderr: "bad config"},
	}}
	if _, err := scanUnit(context.Background(), runner, repo.Unit{LocalDir: "/a"}); err == nil {
		t.Fatal("scanUnit with exit code 2 succeeded; want an error")
	}
}

func TestFilterUnitsByFolder(t *testing.T) {
	t.Parallel()
	units := []repo.Unit{
		{Provider: "claude", Folder: "a"},
		{Provider: "codex", Folder: "b"},
		{Provider: "claude", Folder: "b"},
	}
	got := filterUnitsByFolder(units, "b")
	if len(got) != 2 {
		t.Fatalf("filterUnitsByFolder(%q) = %d units, want 2", "b", len(got))
	}
}
