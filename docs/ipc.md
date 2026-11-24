# Agent IPC: contract between the agent and the UI / CLI

Status: v1, 2026-11-24. Source of truth for `internal/agent` (server) and `ui/`, `cmd/olsyn-drive` (clients).

The agent (`olsyn-drive agent`) serves HTTP/1.1 and Server-Sent Events on a local endpoint that only the
signed-in OS user can open. The UI renders only what this API says; it keeps no state of its own that the
agent does not also have. Everything it shows changes live from `GET /v1/events`.

## Transport

| OS | Endpoint | Access |
|---|---|---|
| Windows | named pipe `\\.\pipe\olsyn-drive-<user SID>` | DACL `O:<SID>D:P(A;;GA;;;<SID>)`: that user only, first pipe instance (a squatter makes the agent fail to start rather than letting it share the name); the client checks that the pipe's server process runs as the same SID |
| macOS | unix socket `~/Library/Application Support/Olsyn Drive/run/agent.sock` | socket 0600 in a 0700 directory |
| Linux | unix socket `$XDG_RUNTIME_DIR/olsyn-drive/agent.sock` (fallback `~/.local/state/olsyn-drive/run/agent.sock`) | socket 0600 in a 0700 directory |

- No TCP port, no token: the endpoint's access control is the authentication.
- Clients send `Host: olsyn-drive` and may send `User-Agent: olsyn-drive-ui/<version>`.
- A request carrying an `Origin` header is refused (403 `forbidden_origin`): browsers never get a path in.
- Request and response bodies are JSON (`Content-Type: application/json`), UTF-8, at most 1 MiB per request.
  An empty body on a POST is the same as `{}`.
- Times are RFC 3339 UTC with milliseconds (`2026-11-24T03:15:07.123Z`). Sizes and rates are integers in bytes
  and bytes per second. Durations are integer seconds.
- Every response has `Olsyn-Drive-Agent: <agent version>` and `Olsyn-Drive-Api: 1`.
- Only one agent runs per OS user (single-instance lock). If the endpoint does not answer, the UI starts
  `olsyn-drive agent` and retries for up to 10 s.

## Versioning

- The path prefix is the major version: `/v1/`.
- Within v1 the agent only adds: new fields, new endpoints, new event types, new enum values, new error codes.
  Clients ignore fields and event types they do not know and treat an unknown enum value as its
  nearest neutral meaning (an unknown `connection` is shown as `attention`; an unknown transfer state is
  listed but offers no actions).
- Removing or renaming anything, or changing a meaning, is v2. The agent then serves v1 and v2 side by side for
  at least one release. `GET /v1/state` lists what it serves in `api_versions`.
- The UI and the agent ship in the same binary, so a mismatch only happens during an update; the UI reconnects
  and re-reads state when `agent_run` changes.

## Errors

Every error is the drive server's shape, so one parser serves both:

```json
{"error": "unsent_data", "message": "3 files (12.4 MB) have not reached the drive yet.", "unsent": {"files": 3, "bytes": 12400000}}
```

`error` is a stable code (`[a-z_]+`), `message` is a sentence a person can read (the UI may show it as is),
other keys are extra detail named per code below.

| Status | Codes |
|---|---|
| 400 | `bad_request` (not JSON, wrong type, unknown query value) |
| 403 | `forbidden_origin`, `read_only` (device was linked read-only) |
| 404 | `not_found` (unknown transfer / held id, path), `drive_folder_not_found`, `local_not_found` |
| 405 | `method_not_allowed` |
| 409 | `already_signed_in`, `not_signed_in`, `unsent_data`, `mounted`, `not_mounted`, `busy`, `driver_missing`, `driver_outdated`, `update_required`, `no_update`, `not_retryable`, `not_cancellable`, `not_held`, `linking`, `not_supported` (this OS) |
| 422 | `invalid_setting` (with `field`), `invalid` (with `field`) |
| 502 | `drive_error` (the drive answered with an error; `drive_error_code` holds its code) |
| 503 | `offline` (drive not reachable), `server_restarting` (with `retry_after`), `stopping` (agent is shutting down) |
| 500 | `internal` |

## Types

### State

`GET /v1/state` → 200 `State`. Also the payload of the `state` event.

```json
{
  "api_version": 1,
  "api_versions": [1],
  "agent_run": "r8f3k2",
  "version": "1.4.2",
  "platform": "windows-x64",
  "ready": true,
  "connection": "working",
  "reason": null,
  "issues": [
    {"code": "held_files", "message": "2 files could not be saved to the drive.", "action": "show_held"}
  ],
  "account": {
    "server": "https://drive.olsyn.com",
    "user_id": "6f1c…",
    "name": "Ada Example",
    "email": "ada@example.com",
    "device_id": "0b7e…",
    "read_only": false,
    "linked_at": "2026-11-24T03:15:07.123Z"
  },
  "device_name": "ADA-PC",
  "signin": null,
  "mount": {"state": "mounted", "letter": "W:", "path": "W:\\", "error": null},
  "paused": false,
  "paused_until": null,
  "transfers": {
    "active": 2, "waiting": 14, "held": 2, "files_left": 16,
    "bytes_total": 734003200, "bytes_done": 123731968,
    "up_bps": 10485760, "down_bps": 0, "eta_seconds": 58
  },
  "update": {"state": "up_to_date", "current": "1.4.2", "available": null, "notes": null,
             "required": false, "progress": null, "error": null, "checked_at": "2026-11-24T03:00:00.000Z"},
  "driver": {"state": "ok", "name": "winfsp", "version": "2.2.25089", "min_version": "2.0"},
  "started_at": "2026-11-24T02:59:58.000Z",
  "first_run": false,
  "unsent": {"files": 16, "bytes": 610271232, "held": 2},
  "cache": {"used_bytes": 3233808384, "cap_bytes": 10737418240, "unsent_bytes": 610271232,
            "uploaded_bytes": 1395864371, "opened_bytes": 1227672781}
}
```

- `connection`, highest first when several hold:
  `signed_out` (no account; `reason` says why) → `linking` (sign-in started, waiting for the browser) →
  `starting` (account present, index/mount coming up) → `offline` (drive not reachable) →
  `server_restarting` (drive answered 503/Retry-After or `access_unavailable`: wait, not an error) →
  `attention` (something needs the person: see `issues`) → `paused` → `working` (transfers active or waiting) →
  `connected`.
- `reason`: `null` or `{"code", "message"}` explaining the current `connection`. Codes:
  `signed_out` (the person signed out), `token_revoked` ("Signed out by the drive": device removed in the web
  app), `token_missing` (credential store lost it), `link_denied`, `link_expired`, `network`, `server_restarting`.
- `issues`: what makes the state `attention`; always an array. Codes and the `action` the UI offers:
  `held_files` (`show_held`), `mount_failed` (`mount`), `driver_missing` / `driver_outdated` (`install_driver`),
  `update_required` (`apply_update`), `update_failed` (`check_update`), `cache_full` (`show_settings`),
  `credential_store` (`sign_in`).
- `ready`: false until start-up has finished (sign-in state loaded and, when the drive was mounted before,
  the mount is back or has failed). Used by the update health check.
- `account`: `null` when signed out. `device_name` is this PC's name as the drive's device list shows it.
- `signin`: `null` or a `SignIn` while linking (and for the last outcome until the next sign-in).
- `mount.state`: `unmounted` | `mounting` | `mounted` | `unmounting` | `failed`. `error` is `null` or `{code, message}`
  (`letter_taken` when the letter is held by something else at mount time).
  `letter` is the configured letter (Windows) or `null`; `path` is where files are (`W:\`, a mount point).
  `letter_confirmed`: `false` from the first sign-in until the person chooses the letter (the UI's letter step, or
  `olsyn-drive login`). The agent never mounts before that: the first `POST /v1/mount` (with or without `letter`)
  confirms it, and from then on the agent mounts at every start. Absent (agents before 1.1) means `true`.
- `transfers`: the `Summary`, also sent alone as the `summary` event.
- `update`: an `Update`. `driver`: a `Driver`.
- `first_run`: `true` until this PC has been signed in once (the UI opens with its welcome step); stays `false`
  after a sign-out.
- `unsent`: changes not yet on the drive: `files` queued uploads, `held` held items, `bytes` of both. Zero while
  signed out (unsent data kept by a sign-out is counted again at the next sign-in to the same account).
- Agents before 1.1 send neither `first_run` nor `unsent`; treat absent as `false` and zero.
- `cache`: how much of this PC the drive's cache uses, split three ways (the same figures as `GET /v1/cache`):
  `unsent_bytes` waiting to upload (on this PC only), `uploaded_bytes` sent and cleared once the drive has
  confirmed it, `opened_bytes` drive files opened here and kept so they open fast. Refreshed at most once a
  second. Absent from agents before 1.2.

### SignIn

```json
{"state": "waiting", "user_code": "BCDF-GHJK", "verification_url": "https://drive.olsyn.com/link?code=BCDF-GHJK",
 "expires_at": "2026-11-24T03:25:07.000Z", "error": null}
```

`state`: `waiting` | `approved` | `denied` | `expired` | `cancelled` | `failed` (`error` = `{code, message}`).
The device code is the agent's secret and never crosses this API.

### Summary

```json
{"active": 2, "waiting": 14, "held": 2, "files_left": 16, "bytes_total": 734003200, "bytes_done": 123731968,
 "up_bps": 10485760, "down_bps": 0, "eta_seconds": 58}
```

Covers active + waiting work (bytes of done files drop out once the queue empties). `eta_seconds` is `null`
when unknown. `files_left` = active + waiting.

### Transfer

```json
{
  "id": "t_01JD7Q9V8C",
  "direction": "up",
  "origin": "mount",
  "state": "active",
  "name": "Level 3.rvt",
  "local_path": "W:\\Projects\\Tower\\Level 3.rvt",
  "drive_path": "/Projects/Tower/Level 3.rvt",
  "node_id": null,
  "size": 524288000,
  "bytes_done": 104857600,
  "bps": 10485760,
  "eta_seconds": 40,
  "attempts": 1,
  "queued_at": "2026-11-24T03:15:07.123Z",
  "started_at": "2026-11-24T03:15:09.000Z",
  "finished_at": null,
  "error": null,
  "held": null,
  "skipped_reason": null,
  "conflict_copy": null,
  "batch_id": null,
  "can": {"retry": false, "cancel": false}
}
```

- `direction`: `up` | `down`. `origin`: `mount` (a file saved on the drive letter) | `send` (POST /v1/send) |
  `read` (download for an open file).
- `state`: `active` | `waiting` (queued, paused, or waiting for the drive) | `held` (refused for good, see
  `held`) | `done` | `skipped` (already on the drive: `skipped_reason` `already_there`, `junk`) | `failed`
  (gave up after retries; `can.retry`) | `cancelled`.
- `error`: `null` or `{code, message, retry_at}` (the last error; a waiting transfer can carry one while it backs off).
- `held`: `null` or `{code, message, since, actions}`: `code` is the drive's refusal (`sealed`, `legal_hold`,
  `managed`, `quota_exceeded`, `invalid_name`, `forbidden`, `writes_paused` is never held) or
  `checksum_mismatch` (the drive received other bytes than this PC sent: refused at completion twice, or flagged
  later by the drive's checksum pass for a large file), `message` is plain words
  ("This folder is sealed, so new files can't be added."), `actions` ⊆ `["reupload", "save_copy", "discard"]`
  in that order of preference. `reupload` is offered only for `checksum_mismatch` while this PC still has the
  bytes; `save_copy` only while it has them.
- `conflict_copy`: when the drive's file changed under us, the drive path our copy was saved as.
- `can`: which of `POST /v1/transfers/{id}/retry|cancel` would succeed now. Mount uploads are never
  cancellable (that would lose a saved file); use discard on a held item instead.

### Update

```json
{"state": "ready", "channel": "stable", "current": "1.4.2", "available": "1.5.0", "notes": "Faster folder listings.",
 "published_at": "2026-11-20T00:00:00.000Z", "required": false, "driver_required": null,
 "progress": {"bytes_done": 18874368, "bytes_total": 18874368}, "error": null,
 "checked_at": "2026-11-24T03:00:00.000Z", "apply": "when_idle"}
```

- `state`: `idle` (never checked) | `checking` | `up_to_date` | `available` | `downloading` | `ready` (verified
  and staged) | `applying` (the agent is about to restart) | `failed` (`error` = `{code, message}`; codes
  `offline`, `bad_signature`, `replayed`, `hash_mismatch`, `rolled_back`, `no_artifact`).
- `required`: this version is below the release's `min_supported`; the drive will not mount until updated.
- `driver_required`: `null` or `{name, min_version}` when the new version needs a newer WinFsp than installed;
  the update waits until `POST /v1/driver/install` has run.
- `apply`: `when_idle` (applied once nothing is open on the drive and nothing is transferring, or at the next
  start) | `now` (the person asked).

### Driver

```json
{"state": "installing", "name": "winfsp", "version": null, "min_version": "2.0", "error": null,
 "progress": "approval", "others": ["dokany"]}
```

`state`: `ok` | `missing` | `outdated` | `installing` | `restart_needed` (installed, but Windows must sign
out and in, or restart, before it loads; the mount waits, 409 `driver_restart_needed`) | `not_needed` (this OS needs
no driver yet) | `unknown`. `min_version` is this build's minimum (WinFsp 2.0) or a release manifest's, whichever is
newer; an installed WinFsp at least that new is used as it is and never reinstalled.
`progress` (only while `installing`): `preparing` (extracting and checking the installer) → `approval` (the
Windows administrator prompt is up) → `installing`. `others`: other file system providers found in Windows'
network provider order (`dokany`); they coexist with WinFsp and are only reported.
`error.code` after an install: `declined` (the prompt was refused; state stays `missing`), `install_failed`,
`restart_needed`.

### Settings

```json
{
  "drive_letter": "W:",
  "cache_dir": "C:\\Users\\ada\\AppData\\Local\\Olsyn Drive\\cache",
  "cache_cap_bytes": 10737418240,
  "cache_release": {"after_upload_seconds": 300, "keep_opened_seconds": 1800},
  "bandwidth": {"up_bps": 0, "down_bps": 0},
  "start_at_login": true,
  "update_channel": "stable",
  "notifications": {"finished": true, "held": true, "signed_out": true, "update_applied": true},
  "server_url": "https://drive.olsyn.com",
  "device_name": ""
}
```

Bandwidth `0` = unlimited. `device_name` `""` = the OS computer name.

`cache_cap_bytes` defaults to 10% of the disk's free space, at most 10 GiB and at least 1 GiB. A settings file
written before version 2 whose cap was the old computed default (20% of free space, up to 50 GiB) moves to the new
default once; a size the person chose stays.

`cache_release` (agent 1.2): an uploaded file's content is cleared from this PC `after_upload_seconds` after the
drive has confirmed it (checksum `verified`, or `claimed` and verified later; default 300) unless it is open, was
read in the last `keep_opened_seconds` (default 1800) or is in a folder kept offline. Uploads the drive never
confirms are cleared only when the cache is over its cap, and never while they are the only copy. 0 = at once;
both 0..604800.

### Notification

```json
{"id": "n_17", "kind": "held", "title": "Couldn't save 2 files", "body": "The Tower folder is sealed.",
 "at": "2026-11-24T03:15:07.123Z", "action": {"label": "Show", "open": {"target": "ui", "path": "transfers/held"}}}
```

`kind`: `finished` | `held` | `conflict` | `signed_out` | `update_applied` | `update_failed` | `error`. The agent
filters by the person's notification settings before publishing; the UI shows them as OS toasts.
`action` is `null` or a label plus a `POST /v1/open` body (target `ui` = a UI route).

## Endpoints

### Sign-in

`POST /v1/signin` body `{"open_browser": true}` (default true: the agent opens `verification_url`)
→ 202 `SignIn` (state `waiting`). Calling it again while linking returns the same `SignIn`.
409 `already_signed_in`. 503 `offline` when the drive cannot be reached to start a link.
The agent polls the drive at the interval it asks for; outcome arrives as `signin` then `state` events. On
approval the token goes to the OS credential store and nowhere else, then `connection` goes
`starting` → `connected`.

`POST /v1/signin/cancel` → 200 `SignIn` (state `cancelled`). 409 `not_signed_in` with no link in progress.

`POST /v1/signout` body `{"clear_cache": false, "discard_unsent_confirmed": false}` → 200
`{"signed_out": true, "unlinked": true, "cache_cleared": false, "unsent_kept": {"files": 3, "bytes": 12400000}}`.
- Order: stop transfers, unmount, unlink this device on the drive, delete the token, then clear the cache if asked.
- `clear_cache: true` while files have not reached the drive (journal entries or held items) → 409 `unsent_data`
  with `unsent: {files, bytes, held}`, nothing changed. Repeat with `discard_unsent_confirmed: true` after the
  person has seen what will be lost.
- `clear_cache: false`: unsent files stay in the cache (`unsent_kept`) and are sent if the same account signs in on
  this PC again.
- `unlinked: false` (+ `unlink_error`) when the drive could not be told; the token is removed locally anyway.

### Transfers and held files

`GET /v1/transfers?state=active|waiting|held|done|skipped|failed&cursor=&limit=100` → 200
`{"items": [Transfer], "next_cursor": "c_…" | null, "counts": {"active": 2, "waiting": 14, "held": 2, "done": 310, "skipped": 4, "failed": 0}}`.
- `state` may be repeated or comma separated; absent = all. `limit` 1..500. Order: active and waiting in queue
  order, held newest first, done/skipped/failed by `finished_at` newest first. `cursor` is opaque.
- History: done/skipped/failed keep the last 1,000 and 7 days, whichever is fewer.

`POST /v1/transfers/{id}/retry` → 200 `Transfer`. 409 `not_retryable`.
`POST /v1/transfers/{id}/cancel` → 200 `Transfer`. 409 `not_cancellable` (mount uploads, finished transfers).

`POST /v1/held/{id}/discard` → 200 `{"discarded": true}`: the local copy is deleted. 409 `not_held`.
`POST /v1/held/{id}/reupload` → 200 `{"queued": true}`: the item leaves the held list (`held` event `removed`)
and is queued again as a `waiting` transfer that replaces the damaged version. 409 `not_held`; 409
`not_retryable` when `reupload` is not among the item's `actions`.
`POST /v1/held/{id}/save-copy` body `{"dest_dir": "C:\\Users\\ada\\Documents"}` (optional; default the OS
Downloads folder) → 200 `{"path": "C:\\Users\\ada\\Downloads\\Level 3.rvt"}`; the item leaves the held list.
A name clash gets " (2)". 404 `local_not_found` when `dest_dir` does not exist.

`POST /v1/pause` body `{"minutes": 60}` (optional; absent = until resumed; not kept across an agent restart)
→ 200 `State`. `POST /v1/resume` → 200 `State`. Pausing stops uploads and background downloads at the next part
boundary; reads of open files still fetch what they need.

`POST /v1/send` body `{"local_paths": ["D:\\Exports\\Tower", "D:\\Exports\\Plans.pdf"], "drive_folder": "/Projects/Tower"}`
(files and folders, at most 1,000; duplicates are dropped; `local_path` with one path is still accepted;
`drive_folder` is a drive path, or `drive_folder_id` instead; optional `"on_conflict": "hold" | "new_version" | "keep_both"`
for a name the drive already has with other content, default `hold`: the drive's file is left alone and this one is held
with code `name_conflict`; optional `"include_online_only": true` reads online-only files, making the sync client
download them) → 202 `{"batch_id": "b_9", "files": 120, "bytes": 3400000000}`.
The whole request is one batch: its transfers have `origin: send` and that `batch_id`, and its progress comes as
`batch` events. `files`/`bytes` are what was found so far (folders are counted while they upload; the `batch`
events carry the totals). A folder lands as a folder of the same name inside `drive_folder`. Sending never deletes
or changes anything on this PC; files a cloud-sync client (OneDrive) keeps online-only are not downloaded and are
counted in `files_not_local`. Files already on the drive with the same content are skipped.
404 `local_not_found` (with `local_path`) / `drive_folder_not_found`, 403 `read_only` (this device) /
`drive_folder_read_only` (no permission, managed or sealed), 409 `not_signed_in`, 422 `invalid` (relative path, too many),
503 `drive_unreachable` (the destination could not be looked up for 20 seconds: offline or the drive is restarting).
Send transfers have ids `b_<n>_<k>`. Held send files offer only `discard` (it drops the item from the list; the file on
this PC is never touched); failed ones can be retried; waiting and active ones cancelled. Sends are not `unsent` data:
nothing is lost by clearing the cache. An unfinished batch (the agent stopped or the PC restarted) runs again at the next
start; files that reached the drive before then count as skipped.

`GET /v1/batches` → 200 `{"items": [Batch]}`, the sends of this agent run and the last 7 days, newest first. A `Batch`:

```json
{"batch_id": "b_9", "state": "sending", "drive_folder": "/Projects/Tower", "local_paths": ["D:\\Exports\\Tower"],
 "files": 120, "files_done": 40, "files_skipped": 3, "files_failed": 0, "files_held": 0, "files_not_local": 1,
 "bytes": 3400000000, "bytes_done": 1200000000, "bps": 10485760, "eta_seconds": 210,
 "started_at": "2026-11-24T03:15:07.000Z", "finished_at": null, "error": null}
```

`state`: `scanning` (walking the folders) → `sending` → `done` (every file is on the drive, skipped, held or failed),
`failed` (`error` says why: the drive folder went away, the device was signed out) or `cancelled`. The batch's
files are ordinary transfers: retry, cancel and the held list work on them as on any other.

### Settings

`GET /v1/settings` → 200
`{"settings": Settings, "defaults": Settings, "choices": {"drive_letters": ["Z:", "Y:", "X:"], "letters_in_use": {"D:": "volume", "W:": "network"}, "suggested_letter": "X:", "update_channels": ["stable", "beta"]}, "pending_restart": []}`.
`choices.drive_letters` is the letters D:-Z: nothing holds, last first, plus the current one while the drive is
mounted there (empty off Windows). A letter is held by a volume (`volume`), a network mapping (`network`: connected,
or remembered but disconnected — `net use` lists it as Unavailable and Explorer still shows it under This PC;
read from WNetGetConnection and HKCU\Network) or a subst'd folder (`subst`); `letters_in_use` maps each held
letter to which. `suggested_letter` is the configured letter when free, else the next free one towards Z:, then
the nearest below it (`""` when none is free, or off Windows). `PATCH` of a held `drive_letter` → 422
`invalid_setting` (`field: "drive_letter"`).

`PATCH /v1/settings` body: a JSON merge patch (RFC 7396) of `Settings`, e.g. `{"bandwidth": {"up_bps": 5000000}}`
→ 200 same shape as GET. Validated as a whole; 422 `invalid_setting` with `field` (dotted) and nothing saved.
Effects: bandwidth, notifications and `cache_release` at once; `start_at_login` updates the OS entry at once; `drive_letter`
remounts when mounted (409 `busy` while files are open on the drive); `update_channel` triggers a check;
`cache_dir`, `server_url` and `device_name` apply at the next start and are listed in `pending_restart`
(`server_url` only while signed out, else 409 `already_signed_in`). A `settings` event follows every change.

### Mount

`POST /v1/mount` body `{"letter": "X:"}` (optional) → 202 `{"mount": Mount}`; progress by `state` events. With
`letter` (the person's choice in the letter step): it must be free (409 `letter_taken` with `letter` and `kind`;
422 `invalid` outside D:-Z:) and becomes the `drive_letter` setting (a `settings` event follows). Any mount request
sets `mount.letter_confirmed`. 409 `not_signed_in`, `driver_missing`, `driver_outdated`, `update_required`;
409 `mounted` when already mounted at another letter (change it with `PATCH /v1/settings`). Already mounted → 200.

First run, in order: sign in → (driver) → the UI's letter step shows `choices.drive_letters` with
`suggested_letter` selected and, when the configured letter is in `letters_in_use`, says what holds it → `POST
/v1/mount {"letter": …}` → `mounted`. `olsyn-drive login` does the same with `--letter` or the suggested letter
(`--no-mount` skips it).
`POST /v1/unmount` body `{"force": false}` → 202 `{"mount": Mount}`. 409 `busy` with `open_files` while files are open
on the drive, unless `force`. Unsaved data already written is kept and still uploads.
Whether the drive is mounted is remembered: the agent remounts at its next start only if it was mounted.

### Updates and driver

`GET /v1/update` → 200 `Update`.
`POST /v1/update/check` → 200 `Update` after the check (at most 30 s; the agent also checks every 4 hours).
`POST /v1/update/apply` body `{"when": "idle" | "now"}` (default `idle`) → 202 `Update`. 409 `no_update`,
`driver_outdated`. `now` interrupts transfers (they resume after the restart). The agent stops its endpoint,
the new version starts and must answer `GET /v1/state` with the new `version`, `ready: true` and, if the drive was
mounted, `mount.state: mounted` within 60 s; otherwise the old version is put back and started, and its
`update.state` is `failed` with code `rolled_back`. The UI simply reconnects (a new `agent_run`).

`POST /v1/driver/install` → 202 `Driver` (state `installing`, `progress` `preparing`). Extracts the WinFsp MSI
embedded in the program to a new temporary folder, checks its SHA-256 against the pinned value and its Authenticode
signature, and runs `msiexec` through ShellExecute `runas` (one UAC prompt). Progress and the outcome come as
`state` events: `driver.state` `ok`; `missing` with `error` `declined` when the person said no;
`restart_needed` when the installer asks for a restart (MSI 3010). 409 `not_supported` off Windows,
`driver_restart_needed` while a restart is pending.

### Other

`POST /v1/diagnostics` → 200 `{"path": "…\\olsyn-drive-diagnostics-20261124T031507Z.zip", "size": 482113}`. The zip
holds `state.json`, `settings.json`, `transfers.json` (counts and errors), `system.json` (OS, versions, free disk)
and the last 20 MiB of logs. Tokens, device codes, e-mail addresses, the account and device names and every
file or folder name are redacted (names become `<name:3f9a1c2b>`, a hash that matches across the bundle).

`POST /v1/open` body `{"target": "drive" | "web" | "folder" | "logs" | "ui", "path": "/Projects/Tower"}` → 204.
`drive`: the path on the mounted drive in the file manager (409 `not_mounted`). `web`: the same item in the drive's
web app (path optional). `folder`: a local folder (`path` absolute; 404 `local_not_found`). `logs`: the logs folder.
`ui`: the desktop window at a route (`path` such as `transfers/held`, `activity`, `settings`, `about`; default
`transfers/active`; 422 `invalid` for anything else). The agent starts the app with an
`olsyn-drive://show/<route>` link; a running app takes the link and shows the route. Notification actions use this
target. 409 `no_ui` from a build without the desktop app (the command-line-only builds).

`GET /v1/cache` → 200 `{"dir": "C:\\Users\\ada\\AppData\\Local\\Olsyn Drive\\Cache", "used_bytes": 3233808384,
"cap_bytes": 10737418240, "unsent_bytes": 610271232, "unsent_files": 18, "uploaded_bytes": 1395864371,
"opened_bytes": 1227672781, "disk_free_bytes": 227633266688}`.
`used_bytes` counts cached content including unsent; `unsent_bytes` is content on this PC only; `uploaded_bytes`
was sent and is cleared once the drive has confirmed it; `opened_bytes` is everything else (opened files, and
uploads the drive could not confirm). The last two are absent before agent 1.2. 409 `not_signed_in`.

`POST /v1/cache/free` (agent 1.2) → 200 `{"freed_files": 37, "freed_bytes": 2623537152, "cache": <GET /v1/cache>}`.
Clears every cached file that can go now: not open, not waiting to upload, not an upload the drive has yet to
confirm. The files stay on the drive and are fetched again when opened. A `state` event follows. 409
`not_signed_in`.

`GET /v1/drive/folders?parent=/Projects` (or `parent_id=`; default the top of the drive; `cursor=` for more) → 200
`{"parent": "/Projects", "parent_id": "…", "can_write": true, "folders": [{"id": "…", "name": "Tower", "path": "/Projects/Tower",
"can_write": false, "why": "sealed"}], "next_cursor": null}`. Folders only, in the drive's order, at most 500 per page.
`why` is `""` when files can be added, else `read_only` (no permission, or a read-only device), `managed` (a folder the
drive fills itself) or `sealed`. Asks the drive each time (no cache). 404 `drive_folder_not_found`, 409 `not_signed_in`,
502 `drive_error`.

`GET /v1/activity?cursor=&limit=50` → 200 `{"items": [Notification], "next_cursor": "a_50" | null}`, newest first,
`limit` 1..200. Every notification is kept here, including kinds turned off in `settings.notifications`; the last 500
are kept across restarts. A sign-out that clears the cache clears the activity too.

`GET /v1/licences` → 200 `{"text": "…"}`: the open-source notices (each component's licence and the WinFsp
attribution), for the About screen. The same text ships as NOTICE.txt.

`POST /v1/quit` body `{"unmount": true}` → 202 `{"quitting": true}`; the agent finishes writing what is open into the
journal, unmounts and exits. The drive is served by the agent, so quitting always disconnects it: while
mounted, `unmount` must be `true` (the person confirmed), else 409 `mounted`. Closing UI windows never calls this.

## Events: `GET /v1/events`

Server-Sent Events, `Content-Type: text/event-stream`. Each event:

```
id: r8f3k2-1042
event: transfer
data: {"id":"t_01JD7Q9V8C","state":"active","bytes_done":115343360,...}

```

`data` is one line of JSON. `id` is `<agent_run>-<seq>`; seq always increases along a stream, with gaps where
progress events were merged.

| event | data | when |
|---|---|---|
| `snapshot` | `{"state": State, "active": [Transfer], "held": [Transfer], "lagged": false}` | first event of every stream; again (with `lagged: true`) after the client fell behind |
| `state` | `State` | anything in State except the summary changed |
| `summary` | `Summary` | at most 4 per second while anything moves |
| `transfer` | `Transfer` | progress at most 4 per second per transfer; state changes at once |
| `held` | `{"op": "added" | "removed", "item": Transfer}` | an item joins or leaves the held list |
| `signin` | `SignIn` | each sign-in step |
| `update` | `Update` | each update step (download progress at most 4 per second) |
| `settings` | `Settings` | after a change |
| `notification` | `Notification` | something worth a toast |
| `batch` | `Batch` | progress of a send at most 4 per second per batch; state changes at once |

- `active` in the snapshot is the first 50 active/waiting transfers in queue order; `held` is all held items.
  With the snapshot the UI needs no separate GET to start, and never misses a change between a GET and the
  subscription.
- A comment line `: ping` every 15 s keeps the stream alive; a client that hears nothing for 45 s reconnects.
- Reconnecting with `Last-Event-ID`: if the id is from this `agent_run` and still in the agent's last 1,024
  events, the stream replays what came after it (no snapshot); otherwise it starts with a snapshot.
- Coalescing: for `transfer` and `summary` (and `update` progress) only the latest value matters. The agent emits
  at most one per transfer per 250 ms, and a slow client's queue replaces a not-yet-sent progress event in
  place rather than queueing another. A state change (`active` → `done`, …) is always delivered and never merged
  away.
- Slow subscribers never slow the agent: each stream has a queue of 256 events; if it overflows, the queue is
  dropped and the next event the client gets is a `snapshot` with `lagged: true`.
- At most 16 concurrent streams; the 17th gets 503 `busy`.

## Design notes for the UI

- Render from the snapshot, then apply events; never poll.
- Progress bars: interpolate between `transfer` events using `bps` for smooth motion; snap to each event.
- Keep the last `State`; on a stream error show the last state greyed out and reconnect with backoff
  (0.25 s, doubling to 5 s), starting the agent if the endpoint is gone.
- Every button that calls a POST should optimistically show the pending state; the `state` event that follows is the
  truth.
