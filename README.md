# FileSync

Two-way file synchronization between your own machines on a LAN —
a small Syncthing, in one static Go binary. **No cloud
intermediary, no account, no telemetry.** Peers authenticate each
other with per-device certificates; every connection is mutual
TLS 1.3; nothing syncs to a device you have not explicitly paired
with (6-digit code + fingerprint confirmation on both sides).

**Status: phase 1.** LAN only — static addresses plus mDNS
discovery. Relay/NAT traversal is planned for phase 2.

## How it works

```
 machine A                                    machine B
┌──────────────────────────────┐            ┌──────────────────────────────┐
│ sync dir                     │            │ sync dir                     │
│   │ watcher (fsnotify)       │            │   │ watcher                  │
│   ▼                          │   mutual   │   ▼                          │
│ scanner ──► commit (vector)  │  TLS 1.3   │ scanner ──► commit (vector)  │
│   │                          │ ◄────────► │   │                          │
│ store (SQLite WAL)           │  index +   │ store (SQLite WAL)           │
│   │                          │  chunks    │   │                          │
│ engine: planner (vectors)    │  (gzip)    │ engine: planner (vectors)    │
│   staging: .filesync/partial │            │   staging: .filesync/partial │
│   history: .filesync/{trash, │            │   history: .filesync/{trash, │
│            versions}         │            │            versions}         │
└──────────────────────────────┘            └──────────────────────────────┘
```

One sync round, step by step:

1. You edit a file. The watcher notices (with a 30-second rescan
   as a safety net).
2. The scanner hashes the file in fixed 1 MiB chunks; the commit
   step stamps the store entry with a **version vector** bumped
   for this device.
3. On the next round with a peer (one connection per peer carries
   both directions), the two sides exchange their full file
   indexes.
4. A pure **planner** compares version vectors — never wall
   clocks — and decides per file: pull, delete, rename, merge
   vectors, or resolve a conflict.
5. Pulls fetch only the chunks that differ, into
   `.filesync/partial/` staging; an interrupted transfer resumes
   from its journal, sending only the missing chunks. Chunk
   payloads are gzip-compressed when that helps (protocol v2).
6. A completed file is hash-verified end to end, the previous
   version is archived to history, and the new file is installed
   with an atomic write.

Conflicts (both sides changed the same file while apart) are
never resolved by overwriting: the loser is kept next to the
winner as `name (conflict <device> <timestamp>)<ext>`, and both
sides converge on the same outcome deterministically.

## Build from source

You need Go 1.27+.

```sh
git clone <this repo> && cd filesync
go build -o filesync ./cmd/filesync
go test ./...                                   # unit + integration
go test -tags=e2e ./internal/e2e/... -timeout 20m   # full e2e + workload
```

## Install as a service

Build the binary first, then pick your OS.

### Linux (systemd user unit)

```sh
mkdir -p ~/.local/bin && cp filesync ~/.local/bin/
cp deploy/systemd/filesync.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now filesync
```

### Windows

**Recommended: the one-file installer.** Download `FileSync-Setup.exe`
from a GitHub Release and double-click it. It asks for admin rights,
installs `filesync.exe`, runs `init`, registers the auto-start service,
opens the Windows Firewall for FileSync (inbound, Private networks
only: the sync port TCP 22000 and mDNS discovery UDP 5353), and only
reports success after the service has been verified RUNNING. When the
install finishes, **your browser opens the management page by itself**
— no further step is needed. It also adds a **FileSync** shortcut with
the FileSync logo to both your **Desktop** and the Start Menu (they
open the web management UI), and a standard entry in Windows Settings
> Apps, where you can uninstall it again (uninstalling removes the
service, both shortcuts, the firewall rules and the registry entry;
your config and synced data are kept).

Upgrades are data-safe: running a newer `FileSync-Setup.exe` just
replaces the binary and re-registers the service. The installer never
writes to or deletes your config dir (`%APPDATA%\filesync`, which
holds your device identity) or your sync folder, and `init` is
idempotent, so your device ID survives upgrades.

**Version 0.3.0 notes:** 0.3.0 is the "install and it just
works" release. It keeps the 0.2.1 service fix (in 0.2.0 the daemon
did not speak the Windows service protocol, so the FileSync service
could wedge in START_PENDING forever; the daemon is now a real
Windows service that reports RUNNING only once it is actually
listening, and the installer recognises a wedged pending-state
service from an older install, terminates exactly that service's
own process, and then upgrades normally) and adds the auto-opening
installer, the logo, and the redesigned web UI described below.
If you installed an older version, just run the 0.3.0
`FileSync-Setup.exe` over it — config and data are kept as with
every upgrade.

About icons: the FileSync logo appears on the Desktop and Start
Menu shortcuts, in Settings > Apps, as the web page's favicon and
in the page header. The `.exe` files themselves keep Go's default
icon — embedding an icon into the binaries would need extra
build tooling this release deliberately avoids.

Note: the installer is not code-signed yet, so Windows SmartScreen
may ask once on first run — choose *More info > Run anyway*.

**Fallback (manual scripts):** the release zip also carries
`filesync.exe` plus `install-service.ps1`, `uninstall-service.ps1`,
`cai-dat-filesync.bat` and `go-cai-dat-filesync.bat` (sources in
`deploy/windows/`). Put them in one folder and double-click
`cai-dat-filesync.bat`, or from an **elevated** PowerShell:

```powershell
mkdir "$env:LOCALAPPDATA\Programs\filesync" -Force
copy filesync.exe "$env:LOCALAPPDATA\Programs\filesync\"
.\deploy\windows\install-service.ps1
# remove later with: .\deploy\windows\uninstall-service.ps1
```

The PowerShell scripts are UTF-8 **with BOM** and print full
Vietnamese messages (Windows PowerShell 5.1+ reads the BOM and
decodes them correctly); the `.bat` launchers stay plain ASCII
by design, because cmd.exe parses batch text in the OEM
codepage. The setup and `filesync uninstall-service` consoles
switch themselves to UTF-8 (code page 65001) before printing.

### macOS (launchd)

```sh
sudo cp filesync /usr/local/bin/
mkdir -p ~/Library/Logs/filesync
sed -i '' "s/YOURNAME/$USER/g" deploy/launchd/io.filesync.daemon.plist
cp deploy/launchd/io.filesync.daemon.plist ~/Library/LaunchAgents/
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/io.filesync.daemon.plist
```

## First run: init, serve, pair

On **each** machine:

```sh
filesync init     # creates config, device identity, sync dir
filesync serve    # run the daemon (or use the service above)
filesync status   # daemon state, peers, queue, errors
```

Pairing (do it once per pair of machines). On machine B:

```sh
filesync pair
# pairing code: 483 920  (shown grouped; type it without spaces)
# expires in 5 minutes
```

On machine A (B's address is shown by B's `filesync status`, or
use the mDNS-discovered/static address you configured):

```sh
filesync pair --to 192.168.1.20:22000 --code 483920
```

Both terminals now show the **other device's fingerprint**. Compare
them with what the other machine displays (`filesync status` /
its pair prompt), and only then type `y` on **both** sides.
Pairing completes when both have confirmed; the peer then appears
in:

```sh
filesync devices            # list paired devices
filesync devices revoke <device-id-or-unique-prefix>
```

`filesync pair` talks to the running daemon through a mailbox
file; when no daemon is running it runs a minimal one-shot
pairing runtime in the foreground instead — same protocol, same
checks.

Other commands: `filesync pause` / `filesync resume` (works with
the daemon up or down), `filesync conflicts`,
`filesync conflicts resolve …` (below), `filesync history <path>`
and `filesync restore <path>` (below), `filesync version`.
Most read commands accept `--json`; every command accepts
`--config-dir <dir>` (default: the per-user config dir).

## Web UI

Prefer clicking to typing? While the daemon runs it also serves a
management page on this machine:

```sh
filesync ui     # prints the URL and opens it in your browser
```

Default address: `http://127.0.0.1:22080`.

**First run is a pairing wizard.** While no device is paired yet,
the page opens with a plain-language pairing wizard instead of the
management tabs: *show this machine's code* (with this machine's
address printed big, ready to read out) or *enter the other
machine's code*. Machines running FileSync on the same network
also appear under **"Máy ở gần đây" (nearby machines)** — click one
to fill in its address instead of typing it. The fingerprint
confirmation step stays manual on both machines, exactly like the
CLI — nothing is ever approved automatically. Once pairing
completes on both sides, the page switches to the main screen by
itself.

**The main screen starts with a drag-and-drop box.** Drop files
(or whole folders) onto it — or use the pick buttons — and they
are streamed into this machine's sync folder, then sync to the
other machine as usual. Dropping is exactly like copying the file
into the sync folder with Explorer: a same-name file overwrites
the copy on *this* machine (the receiving side still archives the
old version into its version history when the overwrite arrives),
and if syncing is paused the file waits in the folder until you
resume. Below the drop box, the **"Đang chuyển" (transferring)**
panel shows live progress in both directions — what is being sent
and what is being received, with percentages — and errors are
explained in plain language with the technical detail folded away
under "Chi tiết kỹ thuật".

From the other tabs you can also see paired devices and revoke
them, resolve conflicts, browse version history and restore, and
pause/resume syncing.

The URL contains a secret access token — **do not share it**.
Anyone with the token (or the `ui-token` file in the config dir)
controls the daemon. Settings, in `config.json`: turn the UI off
with `"ui_enabled": false`, or move it with `"ui_port": <port>`.
To rotate the token: stop the daemon, delete the `ui-token` file
in the config dir, start the daemon again.

## Handling conflicts by hand

When both sides edit a file while disconnected, the winner (by
deterministic rule: larger version-vector author, then content
hash — never the clock) keeps the original name. The loser is
preserved next to it:

```
report.txt
report (conflict 7KX2ABCD 20241011T093015Z).txt
```

Nothing was overwritten. To resolve:

```sh
filesync conflicts                       # list all conflict copies
filesync conflicts resolve report.txt --keep mine     # keep canonical
filesync conflicts resolve report.txt --keep theirs   # keep the copy
```

Whichever you discard is **not deleted**: it goes to
`.filesync/trash` (deletions) and `.filesync/versions` (previous
versions, 5 per file by default), inside the sync dir. Restore a
trashed file by moving it back. For archived versions, use:

```sh
filesync history report.txt                    # list versions, newest first
filesync restore report.txt                    # restore the newest version
filesync restore report.txt --version <name>   # restore a specific version
```

Restoring archives the current content first, so a restore is
itself undoable (restore again to go back) — and the restored
file is picked up as a normal edit and synced to your peers.

## Configuration

Config file: `<config-dir>/config.json` (written by `init`,
mode 0600; the config dir itself is 0700).

| Field | Default | Meaning |
|---|---|---|
| `device_name` | host name | shown to peers during pairing |
| `listen_addr` | `:22000` | TCP listen address for peers |
| `sync_dir` | `~/FileSync` | the folder that is synchronized |
| `bandwidth_limit_kbps` | `0` (off) | outbound rate cap, KiB/s |
| `parallelism` | `4` | max concurrent file pulls per round |
| `version_history_keep` | `5` | archived versions kept per file |
| `version_history_quota_bytes` | `1 GiB` | total history size cap |
| `max_path_length` | `240` | longest accepted relative path |
| `rescan_interval_seconds` | `30` | periodic full rescan (safety net) |
| `ui_enabled` | `true` | serve the web management UI |
| `ui_port` | `22080` | loopback port of the web UI (0 = default) |

Environment: `FILESYNC_DISABLE_MDNS=1` turns off mDNS discovery
(static peer addresses still work; used by tests/CI).

Peers are remembered in the store with the addresses seen at
pairing time; add a static address by re-pairing or by editing
via `filesync devices` output — in phase 1 the acceptor does not
learn a dial-back address for the initiator (its source port is
ephemeral), so at least one side of each pair needs a reachable
listen address.

## Known limitations (phase 1, honestly)

- **The web UI listens on 127.0.0.1 only** — it works solely in a
  browser on the machine running the daemon; there is no
  remote/LAN access to it.
- **Fixed 1 MiB chunks, no content-defined chunking** (ADR-002):
  inserting one byte into a large file shifts every following
  chunk and re-sends them. Appends and in-place edits only
  re-send the chunks that actually changed.
- **Chunk payloads ARE gzip-compressed** on the wire when smaller
  (protocol v2, job 004a). An earlier design note (job 004 spec
  D4) dropped compression; it was superseded before the engine
  was built.
- **mDNS on a real LAN is unverified** in the development
  environment (no multicast available there; the discovery test
  is gated behind `FILESYNC_TEST_MDNS=1`). Static addresses
  always work.
- **Restarting the daemon mid-pairing loses the pairing window**
  (codes and pending confirmations live in RAM only — fail-safe
  by design): just run `filesync pair` again. No data is lost.
- The pairing **code travels in clear text inside
  `pair-mailbox-result.json`** (mode 0600 inside the 0700 config
  dir) between daemon and CLI on the same machine; the CLI
  deletes it right after displaying. The code itself never goes
  over the network (the wire carries an HMAC proof).
- A tiny check→rename/hash race from job 002 remains (a file
  changing in the exact microsecond it is being installed can
  need an extra round to converge; no data loss — closing it
  needs descriptor-based no-follow APIs).
- macOS service packaging is **cross-compiled and code-reviewed,
  not run on real hardware** in the development environment. The
  Windows installer and service were run on a real machine
  (which is how the 0.2.0 START_PENDING service bug, fixed in
  0.2.1, was found); locked-file classification remains
  cross-compiled only. On Windows, the single-instance check
  cannot distinguish PID reuse.
- The **full index is exchanged every round** (D11): trees far
  beyond the verified 10k files will pay index bandwidth per
  round. One round's index is also capped (1M entries / 256 MiB
  of payload, accumulated across batches) as an anti-DoS bound —
  a peer streaming more than that has its round aborted. The
  in-flight disk-space budget (D12) is per-process advisory only.
- A single index entry carries the file's whole chunk list
  (~90 bytes of JSON per 1 MiB chunk): past roughly 30–45 GiB in
  one file, the entry alone can exceed the 4 MiB index message
  cap, and such a file cannot sync in phase 1.
- Repeated conflict generations on the same name get `-2`, `-3`
  suffixes (D3). `.syncignore` is per-machine policy and is never
  synced itself.
- **Receiving is fsync-bound on slow disks**: the pull path
  spends ~7 fsyncs per received file (transfer journal,
  fingerprint key, finalize, atomic write). On storage with slow
  fsync, the first convergence of a large tree is limited by the
  disk's fsync latency, not by CPU or network. Batching these
  into one combined "complete transfer" store operation is a
  phase-2 optimization.
- **Large files are not versioned on overwrite**: a file larger
  than `version_history_quota_bytes` (default 1 GiB) that a peer
  overwrites via sync does NOT keep its old copy in version
  history — otherwise one 20 GiB file could pin ~100 GiB of
  versions (5 kept copies) despite the quota. The file itself
  syncs normally; the number of skipped archives and the most
  recent one are visible in `status` (`skipped_version_archives`,
  `last_version_skip`). To keep old copies of large files, raise
  `version_history_quota_bytes` in config.json (0 = unlimited)
  and restart the daemon. Deleting a large file still moves it
  to trash as usual — this policy only covers overwrites.
- **Post-TTL resurrection is accepted behavior**: tombstones are
  purged after 30 days on every node; a node returning after
  that with a stale live entry will re-share it as new. If you
  keep a machine offline for more than 30 days, expect deleted
  files it still holds to come back — check before reconnecting.

## Repository layout

- `cmd/filesync` — the binary entrypoint (~30 lines).
- `internal/{config,identity,validate,logx,vclock,ignore,chunk}`
  — core primitives (job 001).
- `internal/{store,safefile,history,index}` — SQLite store,
  atomic filesystem ops, version history + trash, scanner/watcher
  (job 002).
- `internal/{proto,transport,pairing}` — wire protocol v2,
  mutual-TLS transport + mDNS, code pairing (jobs 003, 004a).
- `internal/{sync,daemon,cli}` — sync engine, daemon, CLI
  (job 004).
- `deploy/` — systemd unit, launchd plist, Windows service
  scripts. `scripts/smoke-test.py` — identity-checked post-install
  smoke test (also the official app-identity marker: status.json
  + device_id).
- `docs/adr/` — architecture decision records;
  `docs/domain.md` — domain model and invariants.
