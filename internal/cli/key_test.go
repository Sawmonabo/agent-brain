package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/keys"
)

// TestKeyExportIsPipeClean pins the export contract: stdout carries EXACTLY
// the armored keyset plus a trailing newline (so a shell pipeline gets clean
// bytes), while the password-manager reminder — which must never end up
// piped into a file — goes to stderr.
func TestKeyExportIsPipeClean(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", dir)
	keysetPath := filepath.Join(dir, "keyset.json")
	if err := keys.Generate(keysetPath); err != nil {
		t.Fatal(err)
	}
	want, err := keys.Export(keysetPath)
	if err != nil {
		t.Fatal(err)
	}

	root := Root()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"key", "export"})
	if err := root.Execute(); err != nil {
		t.Fatalf("key export: %v\nstderr: %s", err, stderr.String())
	}
	if got := stdout.String(); got != want+"\n" {
		t.Fatalf("key export stdout = %q, want %q (armored + newline, nothing else)", got, want+"\n")
	}
	if !strings.Contains(stderr.String(), "password manager") {
		t.Fatalf("key export stderr missing the recovery reminder: %q", stderr.String())
	}
}

// TestKeyImportRoundtrip proves an exported keyset can be piped into import
// on a clean machine (empty config dir) and the result loads as a valid
// primitive.
func TestKeyImportRoundtrip(t *testing.T) {
	srcDir := t.TempDir()
	srcKeyset := filepath.Join(srcDir, "keyset.json")
	if err := keys.Generate(srcKeyset); err != nil {
		t.Fatal(err)
	}
	armored, err := keys.Export(srcKeyset)
	if err != nil {
		t.Fatal(err)
	}

	dstDir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", dstDir)

	root := Root()
	root.SetIn(strings.NewReader(armored + "\n")) // trailing newline must be trimmed, not treated as payload
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"key", "import"})
	if err := root.Execute(); err != nil {
		t.Fatalf("key import: %v\n%s", err, out.String())
	}

	if _, err := keys.Primitive(filepath.Join(dstDir, "keyset.json")); err != nil {
		t.Fatalf("imported keyset does not load as a primitive: %v", err)
	}
}

// TestKeyImportRefusesClobberWithoutForce proves import never silently
// destroys an existing keyset: it refuses, names --force in the error, and
// leaves the on-disk keyset byte-for-byte untouched.
func TestKeyImportRefusesClobberWithoutForce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", dir)
	keysetPath := filepath.Join(dir, "keyset.json")
	if err := keys.Generate(keysetPath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(keysetPath)
	if err != nil {
		t.Fatal(err)
	}

	armored, err := keys.Export(keysetPath)
	if err != nil {
		t.Fatal(err)
	}

	root := Root()
	root.SetIn(strings.NewReader(armored))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"key", "import"})
	err = root.Execute()
	if err == nil {
		t.Fatal("key import onto an existing keyset without --force succeeded; must refuse")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("refusal must name --force as the fix: %v", err)
	}
	if !strings.Contains(err.Error(), keysetPath) {
		t.Fatalf("refusal must name the existing keyset path: %v", err)
	}

	after, err := os.ReadFile(keysetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a refused import must not modify the existing keyset")
	}
}

// TestKeyImportForceBacksUpAndReplaces proves --force never destroys key
// material outright: it renames the old keyset to a .bak-<unixts> sibling,
// and the freshly imported keyset decrypts what the OLD keyset encrypted
// (i.e. it is really the replacement key, not a fresh/blank one).
func TestKeyImportForceBacksUpAndReplaces(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", dir)
	keysetPath := filepath.Join(dir, "keyset.json")
	if err := keys.Generate(keysetPath); err != nil {
		t.Fatal(err)
	}

	otherDir := t.TempDir()
	otherKeyset := filepath.Join(otherDir, "keyset.json")
	if err := keys.Generate(otherKeyset); err != nil {
		t.Fatal(err)
	}
	armored, err := keys.Export(otherKeyset)
	if err != nil {
		t.Fatal(err)
	}

	root := Root()
	root.SetIn(strings.NewReader(armored))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"key", "import", "--force"})
	if err := root.Execute(); err != nil {
		t.Fatalf("key import --force: %v\n%s", err, out.String())
	}

	matches, err := filepath.Glob(keysetPath + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("want exactly one keyset.json.bak-<unixts> file, got %v", matches)
	}

	// The replaced keyset must decrypt what the OLD (otherKeyset) key sealed —
	// proving --force actually installed the new key material, not a no-op.
	newPrimitive, err := keys.Primitive(keysetPath)
	if err != nil {
		t.Fatalf("post-force keyset does not load: %v", err)
	}
	oldPrimitive, err := keys.Primitive(otherKeyset)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := oldPrimitive.EncryptDeterministically([]byte("probe"), nil)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := newPrimitive.DecryptDeterministically(sealed, nil)
	if err != nil || string(opened) != "probe" {
		t.Fatalf("imported keyset cannot decrypt what the source keyset encrypted: %v", err)
	}
}

// TestKeyImportForceValidatesBeforeTouchingExistingKeyset proves --force
// validates the incoming armored text BEFORE it disturbs the existing
// keyset: garbage input must fail without renaming the existing keyset
// away, without leaving a .bak file, and without leaving a scratch file
// behind.
func TestKeyImportForceValidatesBeforeTouchingExistingKeyset(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", dir)
	keysetPath := filepath.Join(dir, "keyset.json")
	if err := keys.Generate(keysetPath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(keysetPath)
	if err != nil {
		t.Fatal(err)
	}

	root := Root()
	root.SetIn(strings.NewReader("not a valid armored keyset"))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"key", "import", "--force"})
	if err := root.Execute(); err == nil {
		t.Fatal("key import --force with garbage input must fail")
	}

	after, err := os.ReadFile(keysetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a failed --force import must not disturb the existing keyset")
	}
	if matches, _ := filepath.Glob(keysetPath + ".bak-*"); len(matches) != 0 {
		t.Fatalf("a failed --force import must not create a backup file either: %v", matches)
	}
	if leftovers, _ := filepath.Glob(keysetPath + ".importing-*"); len(leftovers) != 0 {
		t.Fatalf("a failed --force import must not leave a scratch file behind: %v", leftovers)
	}
}

// startFakeDaemonForRotate serves a ready daemon that records every
// /v0/reencrypt hit and answers it with resp. It points the CLI at itself via
// AGENT_BRAIN_RUNTIME_DIR (t.Setenv ⇒ no t.Parallel), the same shape the other
// CLI fake daemons use.
// startFakeRotateDaemon serves /v0/status as "ready" and routes /v0/reencrypt to
// reencrypt (after counting the hit), returning the reencrypt hit counter. It
// points AGENT_BRAIN_RUNTIME_DIR at this socket so newAPIClient dials it.
func startFakeRotateDaemon(t *testing.T, reencrypt http.HandlerFunc) func() int {
	t.Helper()
	dir, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", dir)

	var mu sync.Mutex
	hits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v0/status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.StatusResponse{State: "ready"})
	})
	mux.HandleFunc("/v0/reencrypt", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		reencrypt(w, r)
	})
	listener, err := net.Listen("unix", filepath.Join(dir, "agent-brain.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return hits
	}
}

// startFakeDaemonForRotate serves a canned successful reencrypt response.
func startFakeDaemonForRotate(t *testing.T, resp api.ReencryptResponse) func() int {
	t.Helper()
	return startFakeRotateDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(resp)
	})
}

// TestKeyRotateRefusesWhenDaemonDown pins the design refusal: with no daemon to
// run the immediate re-encrypt, `key rotate` must fail — naming `service start`
// — WITHOUT touching the keyset (a bare rotation would leave the repo
// mixed-primary indefinitely).
func TestKeyRotateRefusesWhenDaemonDown(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", configDir)
	keysetPath := filepath.Join(configDir, "keyset.json")
	if err := keys.Generate(keysetPath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(keysetPath)
	if err != nil {
		t.Fatal(err)
	}

	// A runtime dir with no socket in it: the client dials and finds nothing.
	runtimeDir, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", runtimeDir)

	root := Root()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader("")) // no TTY; the refusal fires before any prompt anyway
	root.SetArgs([]string{"key", "rotate"})
	err = root.Execute()
	if err == nil {
		t.Fatal("key rotate with the daemon down succeeded; it must refuse")
	}
	if !strings.Contains(err.Error(), "service start") {
		t.Fatalf("refusal must name `service start` as the fix: %v", err)
	}

	after, err := os.ReadFile(keysetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a refused rotate (daemon down) must not modify the keyset")
	}
}

// TestKeyRotateAbortsWithoutTouchingKeyset pins the EOF/decline path: when the
// confirmation is declined (exactly what an EOF'd stdin yields — the prefill is
// ABORT), rotate must change nothing and never call the daemon.
func TestKeyRotateAbortsWithoutTouchingKeyset(t *testing.T) {
	reencryptHits := startFakeDaemonForRotate(t, api.ReencryptResponse{})
	configDir := t.TempDir()
	keysetPath := filepath.Join(configDir, "keyset.json")
	if err := keys.Generate(keysetPath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(keysetPath)
	if err != nil {
		t.Fatal(err)
	}

	client, err := newAPIClient()
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	declined := func() (bool, error) { return false, nil } // the EOF outcome
	if err := runKeyRotate(context.Background(), client, keysetPath, &out, &errOut, false, declined); err != nil {
		t.Fatalf("runKeyRotate (declined): %v", err)
	}
	if !strings.Contains(out.String(), "aborted") {
		t.Fatalf("declined rotate must say it aborted; got %q", out.String())
	}
	after, err := os.ReadFile(keysetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a declined rotate must not modify the keyset")
	}
	if got := reencryptHits(); got != 0 {
		t.Fatalf("a declined rotate called the daemon %d times; want 0", got)
	}
}

// TestKeyRotateHappyPathRotatesAndReencrypts pins the full flow: rotate the
// keyset (old key retained, primary switched — proven via the public
// primitive), print the new armored export, call the daemon's re-encrypt, and
// report the file count.
func TestKeyRotateHappyPathRotatesAndReencrypts(t *testing.T) {
	reencryptHits := startFakeDaemonForRotate(t, api.ReencryptResponse{Files: 3, Pushed: true})
	configDir := t.TempDir()
	keysetPath := filepath.Join(configDir, "keyset.json")
	if err := keys.Generate(keysetPath); err != nil {
		t.Fatal(err)
	}

	// Seal a probe under the pre-rotation key so we can prove the old key is
	// retained (rotation, not a fresh keyset) and the primary switched.
	oldPrimitive, err := keys.Primitive(keysetPath)
	if err != nil {
		t.Fatal(err)
	}
	const probe = "recovery-probe"
	oldSealed, err := oldPrimitive.EncryptDeterministically([]byte(probe), nil)
	if err != nil {
		t.Fatal(err)
	}

	client, err := newAPIClient()
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	mustNotPrompt := func() (bool, error) {
		t.Fatal("--yes must skip the confirmation prompt")
		return false, nil
	}
	if err := runKeyRotate(context.Background(), client, keysetPath, &out, &errOut, true, mustNotPrompt); err != nil {
		t.Fatalf("runKeyRotate (happy path): %v", err)
	}

	// The daemon re-encrypt was invoked exactly once, and its count surfaced.
	if got := reencryptHits(); got != 1 {
		t.Fatalf("daemon re-encrypt called %d times; want 1", got)
	}
	if !strings.Contains(out.String(), "re-encrypted 3 files") {
		t.Fatalf("output missing the re-encrypt summary: %q", out.String())
	}

	// The new armored keyset was printed to stdout, and the password-manager
	// reminder to stderr (the `key export` split).
	newArmored, err := keys.Export(keysetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), newArmored) {
		t.Fatal("rotate did not print the new armored keyset to stdout")
	}
	if !strings.Contains(errOut.String(), "password manager") {
		t.Fatalf("rotate did not print the recovery reminder to stderr: %q", errOut.String())
	}

	// Proof it was a real rotation: the new keyset still opens what the old key
	// sealed (old key retained), yet seals the same plaintext differently.
	newPrimitive, err := keys.Primitive(keysetPath)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := newPrimitive.DecryptDeterministically(oldSealed, nil)
	if err != nil || string(opened) != probe {
		t.Fatalf("post-rotate keyset cannot open pre-rotate ciphertext: err=%v opened=%q", err, opened)
	}
	newSealed, err := newPrimitive.EncryptDeterministically([]byte(probe), nil)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(oldSealed, newSealed) {
		t.Fatal("primary did not switch: identical plaintext sealed identically after rotate")
	}
}

// TestKeyRotateReencryptFailureNamesReRotate pins the F2 fix: once the keyset is
// rotated but the daemon's re-encrypt fails, the repo is mixed-primary, and the
// error must direct the user to re-run `agent-brain key rotate` (which reseals) —
// NOT to `agent-brain sync`, which only touches changed blobs and would leave the
// repo mixed-primary under the old key. Security-relevant: the (possibly
// compromised) old key still opens every un-resealed blob on the wire.
func TestKeyRotateReencryptFailureNamesReRotate(t *testing.T) {
	reencryptHits := startFakeRotateDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "reencrypt boom", http.StatusInternalServerError)
	})
	configDir := t.TempDir()
	keysetPath := filepath.Join(configDir, "keyset.json")
	if err := keys.Generate(keysetPath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(keysetPath)
	if err != nil {
		t.Fatal(err)
	}

	client, err := newAPIClient()
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	mustNotPrompt := func() (bool, error) {
		t.Fatal("--yes must skip the confirmation prompt")
		return false, nil
	}
	err = runKeyRotate(context.Background(), client, keysetPath, &out, &errOut, true, mustNotPrompt)
	if err == nil {
		t.Fatal("a failed daemon re-encrypt must surface as an error")
	}

	// The daemon WAS asked to re-encrypt, so the keyset really did rotate first.
	if got := reencryptHits(); got != 1 {
		t.Fatalf("daemon re-encrypt called %d times; want 1", got)
	}
	// The corrected recovery: re-run `key rotate`, and NOT the old `sync` misdirection.
	if !strings.Contains(err.Error(), "re-run `agent-brain key rotate`") {
		t.Fatalf("re-encrypt-failure error must direct the user to re-run `agent-brain key rotate`: %v", err)
	}
	if strings.Contains(err.Error(), "will re-encrypt on its next cycle") {
		t.Fatalf("error still carries the old misdirection to `agent-brain sync`: %v", err)
	}
	// The wrapped daemon failure is preserved for diagnosis.
	if !strings.Contains(err.Error(), "reencrypt boom") {
		t.Fatalf("error dropped the wrapped daemon failure: %v", err)
	}
	// The hazard the message warns about is real: the keyset already rotated.
	after, err := os.ReadFile(keysetPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, after) {
		t.Fatal("keyset unchanged after a re-encrypt failure; the test no longer exercises the mixed-primary hazard")
	}
}
