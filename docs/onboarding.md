# Onboarding a machine

The under-5-minute new-machine runbook (spec §13), expanded per OS. The shape is the
same everywhere:

```
install → agent-brain init → (agent-brain migrate if bash-era state) → verify
```

`init` walks a first-run wizard: detect/authenticate `gh`, create-or-clone the private
`agent-brain-memories` repo, generate the keyset (first machine) or import it (every
machine after), wire the git filters, install the login service, and open the
enrollment picker. The **first machine ever** generates the keyset and stores an
armored copy in your password manager; **every machine after** imports that copy —
never transfer the keyset file through a repo.

## Prerequisites (all platforms)

- **git** and the **GitHub CLI** `gh` ≥ 2.40, authenticated: `gh auth status`. `init`
  borrows the gh token at provision time only and never runs `gh auth setup-git`
  (ADR 08).
- The **armored keyset export** from your password manager (for a joining machine).
  Reprint it anytime on an existing machine with `agent-brain key export`.

### Installing the binary

Once the repo is public, Homebrew is the path on macOS/Linux:

```bash
brew install sawmonabo/tap/agent-brain
```

**While the repo is private** (current posture, 2026-07-10 — Homebrew fetches release
assets anonymously and cannot read a private repo, so the tap is not yet live), use an
authenticated release download or `go install`:

```bash
# Release archive (owner/collaborator gh auth); swap the OS_ARCH pattern per platform:
mkdir -p ~/.local/bin
gh release download <tag> -R Sawmonabo/agent-brain -p '*<os>_<arch>*' -O - \
  | tar -xz -C ~/.local/bin agent-brain
#   macOS Apple Silicon: *darwin_arm64*   ·   Intel: *darwin_amd64*
#   Linux/WSL2 x86-64:   *linux_amd64*    ·   arm64: *linux_arm64*

# …or build from source (needs owner git access; set GOPRIVATE if the module is private):
go install github.com/Sawmonabo/agent-brain/cmd/agent-brain@latest
```

Confirm the binary runs and is on `PATH`:

```bash
agent-brain --version
```

On Apple Silicon a release binary that prints its version rather than being
`killed: 9` is also the proof that CI's mandatory ad-hoc signing worked (spec §13).

## macOS

The daemon runs as a per-user **LaunchAgent**, installed by `init` via
kardianos/service.

```bash
# 1. Install (brew once public, or the interim download above).
# 2. First-run wizard — imports the keyset on a joining machine:
agent-brain init
#    Joining an existing fleet: at the keyset step choose "import" and paste the
#    armored export from your password manager. First machine ever: choose
#    "generate" and save the printed export to your password manager immediately.
# 3. If this machine has bash-era memories under ~/.agent-brain:
agent-brain migrate
# 4. Verify (below).
```

`init --non-interactive` with `--import-key` (reads the armored keyset from stdin),
`--enroll all|none`, and `--skip-service` scripts the whole flow.

## Linux

The daemon runs as a **`systemd --user`** unit, installed by `agent-brain service
install` (invoked by `init`). The unit runs `agent-brain daemon run`.

```bash
agent-brain init          # same wizard; installs the systemd --user service
agent-brain migrate       # only if ~/.agent-brain bash-era state exists here
systemctl --user status agent-brain    # or: agent-brain service status
```

For the service to survive logout on a headless box, the user session needs lingering
(`loginctl enable-linger $USER`) — `service install` handles this on WSL2 automatically
(below); on a normal Linux desktop with an active graphical session it is usually
unnecessary.

## WSL2

WSL2 runs the **Linux binary**, with three platform-specific caveats. Everything in
this section is scheduled to be **live-verified on the real WSL2 machine in the fleet
cutover (Task 10)** and corrected here if reality disagrees.

**1. systemd must be enabled.** `systemctl --user` only works when WSL's systemd
integration is on. Put this in `/etc/wsl.conf`:

```ini
[boot]
systemd=true
```

then restart the distro from Windows (`wsl --shutdown`, reopen). Confirm:

```bash
systemctl --user is-system-running    # expect: running (or: degraded — still usable)
```

**2. Lingering keeps the daemon alive across logouts.** A `systemd --user` unit is
killed when its last login session ends unless the user is *lingering*. Since Task 3c,
`agent-brain service install` runs `loginctl enable-linger $USER` for you on WSL2.
Verify it took, and re-run it by hand if not:

```bash
loginctl show-user "$USER" --property=Linger   # expect: Linger=yes
loginctl enable-linger "$USER"                 # manual fallback if it shows Linger=no
```

A linger-enable failure is a warning, not a fatal install error — the unit still works
for the current session; the tick/poll backstop covers gaps.

**3. If `systemctl --user` misbehaves, check the user D-Bus.** Historically WSL2 could
start a user session without a user D-Bus, breaking `systemctl --user`
(microsoft/WSL#8842). If unit commands act up, check the address is set:

```bash
echo "$DBUS_SESSION_BUS_ADDRESS"   # empty → the #8842 gotcha; start/repair the user bus
```

**Inherent limit — lingering cannot keep the VM booted.** Lingering keeps the unit
alive across *logouts*, but it cannot keep the WSL2 utility VM running: the VM halts
when the last WSL session closes, and the daemon stops with it. Residency therefore
holds only while a session/VM is up; when you reopen WSL, the daemon's start-up
recovery scan and polling backstop catch up on anything missed. (The idle-posture
decision for WSL2 is settled during Task 10 and recorded in the ledger.)

## Verify (all platforms)

```bash
agent-brain doctor      # every check ok (a "legacy leftovers" warning is expected
                        # until the bash-era retirement checklist runs — spec §10)
agent-brain status      # daemon ready; last cycle reported
agent-brain dashboard   # live TUI: Projects healthy, Activity sane (interactive TTY only)
```

Two-machine proof (the point of v2): write a memory in an enrolled project on one
machine, then watch it arrive on another (dashboard **Activity** or `agent-brain
service logs`), and vice versa.

## Upgrading (all platforms)

```bash
agent-brain update --check              # report whether a newer release exists
agent-brain update                      # download, verify, swap, restart the service
agent-brain update --prerelease         # admit release candidates (needed until v2.0.0 tags)
agent-brain update v2.0.0-rc.2          # pin an exact release (rollback too — warned, then honored)
agent-brain update --select             # pick from the release list (interactive terminal only)
```

The update runs through the same authenticated `gh` as the install, so it works
while the repo is private. It verifies the release's sha256 checksums, sanity-runs
the new binary, swaps atomically, and confirms the daemon came back on the new
version (ADR 18). Implicit resolution never downgrades; naming an older version
does, after a warning — run `agent-brain doctor` afterwards, since state written
by the newer version may not load. Homebrew-managed installs are refused by
design — use `brew upgrade agent-brain` there. Dev builds (`go build`, version
`dev`) are also refused so a working tree's binary never overwrites itself.

## Retiring bash-era state

After a verified `migrate`, follow the spec §10 retirement checklist (remove the
SessionStart healthcheck hook, delete `~/.local/bin/ab-claude`, strip
`autoMemoryDirectory` from per-project `.claude/settings.local.json`, remove
`~/.config/agent-brain/chezmoi.toml`, delete `~/.agent-brain/`). The age key stays
archived until the history scrub (ADR 13) completes fleet-wide.
