# Olsyn Drive desktop client: design

Status: draft, 2026-11-24. Windows 11 first, then macOS, then Linux.
Words in quotes attributed to the product owner are theirs; every other choice here is the client session's.

## What it is

One program, `olsyn-drive`, that makes the Olsyn drive a normal drive on a PC (W: on Windows),
signs the person in through the browser, moves files reliably in both directions, keeps itself up to date,
and shows what it is doing. The product owner: "The drive executable for users should be more than a relay it should
manage the mount, authentication, make it easy. Auto update etc", and "make it really good, i want it to look
nice, be dynamic, proper ui/ux".

It is open source under the Apache License 2.0 (the product owner's choice, 2026-11-24) so that it can use
WinFsp under WinFsp's FLOSS exception. Consequences: this repository holds nothing proprietary, no secrets, no
customer names, no internal host names; the Windows program is this code only: it does not carry WinFsp, it links
to it and downloads WinFsp's own installer from the WinFsp project when needed (GPLv3 with the FLOSS exception,
source at the WinFsp project's v2.2 tag); the About screen, `olsyn-drive licences`, NOTICE and the README carry
the WinFsp notice. `relay/` is this project's own earlier
relay client, kept for reference as a separate module; it is not built into the program.

## Decided by measurement (spike, 2026-11-24, Windows 11 build 26300, WinFsp 2.2)

A cgo-free Go binary using github.com/winfsp/go-winfsp v1.0.6 mounted W: as a network drive from a
non-elevated desktop process in 0.5 s; a 12,000-entry folder listed in 59 ms; Explorer's copy engine wrote
3,001 files / 3.2 GB with correct hashes and file dates; a simulated outage failed operations in under
20 ms without hanging Explorer. Not yet proven: a true standard user, Memory Integrity on, sleep/resume,
Revit, and anything against the real server.

## Processes

One binary, two roles.

- **agent** (`olsyn-drive agent`): headless, one per signed-in OS user, starts at login. Owns the mount, the
  index, the cache, the transfer queue, sign-in state, and updates. Never needs admin.
- **ui** (`olsyn-drive` with no arguments, or from the tray): tray icon, popover, main window. Starts the agent
  if it is not running. Closing every window leaves the agent and W: alone. Quitting from the tray asks
  whether to also disconnect the drive.
- CLI verbs for support and scripts talk to the agent the same way the UI does:
  `status`, `login`, `logout`, `send <folder> --to <drive folder>`, `pause`, `resume`, `diagnose`, `version`.

IPC: HTTP/1.1 + Server-Sent Events over a per-user local endpoint. Windows: named pipe
`\\.\pipe\olsyn-drive-<user SID>` with an ACL for that user only. macOS/Linux: unix socket in the user's
runtime dir, mode 0600. No TCP port, no token. Contract: `docs/ipc.md` (source of truth for agent and UI).

## Packages (Go module `github.com/geyervalmont/olsyn-drive-client`, name to confirm)

| Package | Job | Platform-specific? |
|---|---|---|
| `internal/driveapi` | typed client for the drive's HTTP API: link/sign-in, children, changes, ranged download, uploads, pre-check, heartbeat, releases. Retries, backoff, error codes. | no |
| `internal/index` | local metadata index of the remote tree (one file DB). Fed by listings and the changes feed. | no |
| `internal/cache` | content cache: sparse files by version id, block map, size cap, LRU eviction, never evicts open or unsent data. | small (sparse files) |
| `internal/journal` | durable queue of local changes not yet on the server (create, write, rename, delete, mkdir, set time), with upload session ids so multipart uploads resume. | no |
| `internal/transfer` | the engine: parallel multipart upload, pre-check, resume, verify, bandwidth limit, pause; downloads by range. Shared by the mount and by `send`. | no |
| `internal/vfs` | the file system semantics on top of index + cache + journal + transfer: open/read/write/rename/delete/list, conflict rules, name mapping, offline rules. Has no OS calls; tested on any OS against a fake server. | no |
| `internal/mount` | adapters from an OS file system interface to `vfs`: `winfsp` (Windows), later `fuse` (macOS/Linux). | yes |
| `internal/agent` | lifecycle, settings, sign-in state, IPC server, notifications feed, heartbeat, updater schedule. | small |
| `internal/platform` | credential store, autostart, single instance, IPC transport, paths, notifications, URL scheme. | yes |
| `internal/update` | signed manifest check, staged download, swap, health check, rollback. | small |
| `ui/` | Wails v3 shell + React front end. Talks only to the agent's IPC. | shell only |

Rule: nothing under `vfs`, `index`, `cache`, `journal`, `transfer`, `driveapi` imports an OS-specific package.
A macOS or Linux client is then a mount adapter, `platform` implementations, an installer and UI glue.

## File system behaviour

- **Listing** comes from the index, never from the network on the request path. A folder not yet indexed is
  fetched once (bounded wait, 5 s), then kept fresh by the changes feed (SSE wake-up, poll every 30-60 s,
  re-list on `reset`/`resync`).
- **Reading** fetches only the ranges asked for (read-ahead for sequential access) into the cache. Files not
  cached are reported with the offline attribute so Explorer does not download them for thumbnails.
- **Writing** goes to the cache at local speed; closing the file queues it in the journal; the engine uploads
  in the background (new file: `conflict:"fail"`; changed file: `new_version` + `base_version_id`). Explorer's
  copy dialog therefore finishes when the data is safely on this PC; the UI shows what is still going up.
  When unsent data reaches the cache cap, writes slow down to upload speed instead of failing.
- **Modified times** are kept: sent as `modified_at` (millisecond precision).
- **Conflicts**: if the server's file changed since our base version, ours is uploaded beside it as
  "name (conflict from <PC>, <date>).ext" and the UI says so. Nothing is overwritten silently.
- **Refusals** that will never succeed (managed folder or file, sealed folder, no permission, legal hold,
  quota, invalid name) are not retried. The file stays in a "held" list with the server's reason in plain
  words, and an action: save a copy elsewhere, or discard. Local data is never deleted because of a refusal.
- **Offline / server restarting / deploy**: operations that need the server fail fast (under 2 s) or are served
  from the cache; queued uploads wait and resume from the last finished part. 503 with Retry-After,
  `access_unavailable` and `writes_paused` are "wait", not errors.
- **Names**: characters Windows forbids and reserved names (CON, NUL, trailing dot or space) that exist on the
  drive are shown with a reversible substitution; case-insensitive like the server.
- **Deletes** go to the drive's trash (restorable from the web app); the client never purges.
- **Locks** are local to the PC until the server has a lock every channel respects; documented limit
  (Revit central models stay off W: for now).
- Junk the OS creates (Thumbs.db, desktop.ini, ~$ lock files, .DS_Store) stays local and is never uploaded.

## Sign-in

The drive's device-link flow, as is: `POST /api/mount/v1/link` → open the returned page in the browser →
poll `/link/token`. The window shows the same short code the browser shows. The token goes to the OS
credential store (Windows Credential Manager) and nowhere else; it is never logged or shown. Sign-out
unlinks the device on the server, removes the token, unmounts, and offers to clear the cache (refusing while
unsent files exist unless the person confirms what will be lost). A token revoked from the web app's device
list shows as "signed out by the drive" within one heartbeat.

## Updates

The drive serves a manifest signed with Ed25519 (`GET /api/client/v1/releases/<channel>.json`); the public
key is compiled in. The agent checks every few hours, downloads to a staging folder, verifies signature,
size and SHA-256, and applies when nothing is open on the drive and nothing is transferring (or at the next
start): rename the running exe aside, move the new one in, restart, health check (IPC answers, mount comes
back) within 60 s, otherwise roll back and report. Channels stable/beta. The WinFsp driver is not updated by
this path: if a release needs a newer driver the client says so and offers the one elevated step.

## Install

A per-user install with no admin, from one downloaded exe. Opened from Downloads it offers "Install Olsyn
Drive", copies itself to `%LOCALAPPDATA%\Programs\Olsyn Drive`, registers a Start menu entry, start-at-login,
the `olsyn-drive://` scheme and an uninstall entry (Settings > Apps runs `uninstall --interactive`, which deletes
settings and cache unless the person asks to keep them, and never deletes unsent files without asking), then
opens the installed copy. A newer exe opened over an installed one offers to update it: the agent quits
cleanly (drive disconnected, queue kept on disk), the tray app is stopped, the file swapped, the new copy
started; unsent files carry on. The same or an older exe just opens the installed copy. Dev builds do the
same, replacing the installed copy whenever the files differ.

The single elevated step is the WinFsp driver. The program does not carry it (2026-11-24: an unsigned dev build
that carried the MSI was flagged by Microsoft Defender's machine-learning heuristic; whatever the cause, a smaller
program that only links to WinFsp is the honest shape). First run detects WinFsp (an installed one at least as new
as required is used as it is; a Dokany network provider is noted and left alone), explains it in one sentence and,
when needed, downloads WinFsp's official MSI from the WinFsp project's GitHub release (2.2, pinned by size and
SHA-256 in `internal/winfspmsi`) to a temporary folder with progress, checks size, hash and Authenticode signature,
and runs it with one UAC prompt, showing each step. Offline, blocked, or a file that fails a check: nothing runs,
and the step shows WinFsp's own download page with "Check again", so the person (or IT) installs WinFsp from its
signed installer. A declined prompt can be tried again; an install that needs Windows to sign out and in says so
and setup finishes after it.

Nothing is written at first launch from Downloads before the person answers the install question; self-install
and start-at-login happen only when they agree. A start-at-login entry that has gone missing is offered again in
the app, never re-added silently. The program never touches security settings: no exclusions, no attempts to evade
or disable protection, no packing or obfuscation. When security software removes or blocks it (the agent exe
missing, refused at start, or stopped repeatedly) the app and CLI say so in plain words and link to
`docs/pre-release-windows.md`; the data folder (`%LOCALAPPDATA%\Olsyn Drive`, journal and cache) is untouched, so
after a reinstall queued uploads carry on, and the installer, finding the data without the program, says it is
reinstalling and keeping the waiting files.

Release binaries are signed; until then an unsigned pre-release goes only to people who knowingly add a Defender
exclusion for the program folder by hand (docs/pre-release-windows.md); false positives are reported to Microsoft
(docs/false-positive-submission.md).

## Interface

Tray icon with state (connected, working, paused, attention, offline, signed out). Popover: state line,
current transfers with overall progress, the last few finished, quick actions (open W:, open the drive in the
browser, pause, settings). Window: Transfers (active / waiting / held with reasons / done / skipped as
already there), Activity, Settings (drive letter, cache size and place, bandwidth limit, start at login,
update channel, notifications), Account (who, this PC's name, sign out), About (version, licences,
diagnostics bundle). First run: welcome → sign in → driver step if needed → pick letter (W: by default) →
done, with the drive open in Explorer. Look: the drive web app's tokens (Hanken Grotesk, Lora for titles),
light and dark following the OS.

## Integration tiers

- **First release**: tray + window as above; drag a folder onto the window to send; Explorer right-click
  (classic menu) "Copy drive link" and "Open in drive"; `olsyn-drive://` and drive web links opening the local
  path; notifications (finished, held, signed out, update applied); diagnostics bundle; pause/resume;
  bandwidth limit.
- **Soon**: keep-offline folders; recent and pinned from the drive in the popover; search from the tray;
  version history and restore from the right-click menu; managed-folder badges in the window; notifications
  bell from the drive.
- **Later**: Windows 11 top-level context menu (needs a packaged shell extension); status icons in Explorer;
  thumbnails served from the drive; cross-PC lock indicators (needs the server lock).

## Testing

Unit tests against a fake drive server for `vfs`, `index`, `cache`, `journal`, `transfer` on any OS. An
end-to-end suite on a real Windows 11 machine against a local drive: Explorer copy of thousands of small
files plus a multi-GB file, a server restart mid-copy, a network drop, kill and restart of the agent,
sign-out, an update and a rollback. Synthetic data only.
