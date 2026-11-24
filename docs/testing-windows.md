# Testing on Windows

How to accept a build of the Olsyn Drive client on Windows 11: by hand and with a script, first against the fake drive
on the same PC, then against a real drive. Everything here uses synthetic data.

## What you need

- Windows 11 (x64 or Arm64), signed in as an ordinary user. The client itself never needs administrator rights.
- WinFsp 2.2 from https://github.com/winfsp/winfsp/releases (the "Core" feature is enough). WinFsp - Windows File System
  Proxy, Copyright (C) Bill Zissimopoulos, https://github.com/winfsp/winfsp.
- The Microsoft Edge WebView2 Runtime (part of Windows 11; the desktop app does not start without it).
- The dev build, made on any computer with `make dev` (Go, Node and make; no PowerShell):
  - `dist/windows-amd64/dev/olsyn-drive.exe`: the desktop app, the agent and the commands in one program, allowed to
    use a server other than https://drive.olsyn.com;
  - `dist/windows-amd64/dev/fakedrive-server.exe`: the fake drive.

  On an Arm64 PC use `make dist/windows-arm64/dev/olsyn-drive.exe dist/windows-arm64/dev/fakedrive-server.exe`.
- `scripts\windows\acceptance.cmd` from this repository, for the scripted run (it needs the `curl.exe` that ships with
  Windows 11).

Copy the files to one folder, for example `C:\OlsynTest`. The rest of this page assumes that folder and `cmd.exe`.

## How the program behaves

- `olsyn-drive.exe` with no arguments opens the desktop app (tray icon, popover, main window). With a command
  (`agent`, `status`, `login`, `send`, ...) it runs without a window and prints to the console it was
  started from.
- It is a Windows (GUI) program. Typed at an interactive `cmd.exe` prompt, the prompt comes back before the output
  does and `%ERRORLEVEL%` is not set. Inside a `.cmd` script, cmd waits and sets `%ERRORLEVEL%`. At a prompt, use
  `start "" /wait /b olsyn-drive.exe status` when the exit code matters.
- Files (per user):

  | What | Where |
  |---|---|
  | Settings and account (no token) | `%LOCALAPPDATA%\Olsyn Drive\config` |
  | Cache | `%LOCALAPPDATA%\Olsyn Drive\cache` |
  | Agent log | `%LOCALAPPDATA%\Olsyn Drive\logs\agent.log`, rolled over at 10 MiB, 3 old files kept (`agent.log.1` newest) |
  | Desktop app log | `%LOCALAPPDATA%\Olsyn Drive\logs\ui.log`, 5 MiB, 2 old files |
  | Journal, history, sends | `%LOCALAPPDATA%\Olsyn Drive\state` |
  | Sign-in token | Windows Credential Manager only |

  `set OLSYN_DRIVE_HOME=C:\OlsynTest\home` puts all of them (except the token) under one folder, which keeps a test
  run apart from a real installation. The scripted run does this.
- Logs are JSON lines. They name files and folders but never hold a token or file content.

### Startup errors you may see

| Message | Meaning |
|---|---|
| `The WinFsp driver is needed to show the drive.` (status Issues, app banner) | WinFsp is not installed. Commands and send still work. |
| `the agent is already running` | A second `agent` was started; the first keeps running (exit 0). |
| `the agent is not running` (exit 3) | A command found no agent. Start `olsyn-drive.exe agent`, or open the app. |
| `this build connects only to https://drive.olsyn.com` | `--server` or `OLSYN_DRIVE_SERVER` given to a release build. |
| `http is allowed only for a server on this computer` | A dev build was pointed at plain http on another machine. |
| A message box "Olsyn Drive could not start" | The desktop app failed (most often: no WebView2). The box names `ui.log`. |
| `fakedrive-server: listen for the drive: ...` | Port 8787 or 8788 is taken: pass `--addr` / `--faults`. |
| `... is not a loopback address` | The fake drive only listens on 127.0.0.1, ::1 or localhost. |

## The fake drive

```bat
C:OlsynTest> fakedrive-server.exe
Fake drive at http://127.0.0.1:8787 (synthetic data only)
Fault controls at http://127.0.0.1:8788 (POST /deploy?seconds=N, /offline?on=1|0, /revoke, /approve; GET /stats)
```

Options: `--auto-approve` (sign-ins need no click), `--data C:\OlsynTest\fd-data` (uploaded content goes to files
instead of memory; use it for anything over a few hundred MB), `--addr`, `--faults`, `--big-folder N`,
`--virtual-gib N`. Everything is in memory and is gone when it stops.

What it holds:

| Path | |
|---|---|
| `/Projects/Example Tower/{Drawings,Models,Photos}` | three small text files each |
| `/Projects/Sample Park/Reports/Weekly report.txt` | |
| `/Large folder` | 12,000 small files, `item-00001.txt` ... |
| `/Large file/Generated 5 GiB.bin` | 5 GiB made up on read (takes no memory) |
| `/Managed outputs` | writes refused with `managed` (423); its file shows as managed |
| `/Issued for construction` | sealed: writes refused (403) |

Fault controls (Windows 11 has `curl.exe`):

| Command | Effect |
|---|---|
| `curl -s -X POST "http://127.0.0.1:8788/deploy?seconds=30"` | API answers 503 + Retry-After for 30 s, as during a deploy (storage keeps working) |
| `curl -s -X POST "http://127.0.0.1:8788/offline?on=1"` | every connection is dropped, API and storage; `on=0` ends it |
| `curl -s -X POST http://127.0.0.1:8788/revoke` | every token revoked: the client must sign in again |
| `curl -s -X POST http://127.0.0.1:8788/approve` | approves waiting sign-ins (instead of the link page) |
| `curl -s http://127.0.0.1:8788/stats` | files, folders, bytes, pending uploads, signed-in devices and request counts |

## Manual acceptance against the fake drive

Use two console windows. Before starting, install WinFsp 2.2 (step 1 checks the message without it first if you want).

| # | Do | Pass when |
|---|---|---|
| 1 | Without WinFsp: `olsyn-drive.exe agent --server http://127.0.0.1:8787` in window 2, then `start "" /wait /b olsyn-drive.exe status`. | Issues lists "The WinFsp driver is needed to show the drive." Then install WinFsp and stop the agent with `olsyn-drive.exe quit`. |
| 2 | Window 1: `fakedrive-server.exe --data C:\OlsynTest\fd-data`. Window 2: `start "" olsyn-drive.exe agent --server http://127.0.0.1:8787`, then `olsyn-drive.exe`. | The app opens on its first-run screen. `logs\agent.log` starts with "agent starting", `dev_build` true and the server. |
| 3 | Sign in from the app. The browser opens the link page; check the code matches and press Approve. | Signed in within 5 s of Approve. `W:` appears in Explorer within 10 s. |
| 4 | Open `W:\Projects\Example Tower\Drawings`, then `W:\Large folder`. | The small folder lists at once; the 12,000 entries list in under 5 s the first time and at once the second time. Explorer does not hang. |
| 5 | `copy /b "W:\Large file\Generated 5 GiB.bin" NUL` | Finishes without error; note the time (MB/s). The agent's memory (Task Manager) stays under 300 MB; the cache stays under its cap. |
| 6 | Open `W:\Projects\Example Tower\Drawings\Drawings 01.txt` in Notepad, change it, save. | Save returns at once. Status shows the upload and then nothing waiting. `/stats` counts one more `upload.complete`. Reopening shows the change. |
| 7 | Copy a folder of 300 files (under 64 MiB in all) into `W:\Projects`. | The copy finishes at local speed; uploads drain; `/stats` files rises by 300. |
| 8 | Copy a file into `W:\Managed outputs` and into `W:\Issued for construction`. | Managed: the copy succeeds locally, the app's Transfers › Held shows it with the managed reason, and Discard / Save a copy work. Sealed: Explorer refuses the write (access denied) or it is held as sealed; nothing reaches the drive. |
| 9 | `olsyn-drive.exe send C:\OlsynTest\sample --to /Projects --wait` (from a `.cmd` or with `start /wait /b`), then the same again. | First: all sent, exit 0, the source is untouched. Second: all "already there", exit 0, no `part.put` added in `/stats`. |
| 10 | Send into `/Issued for construction`. | Refused at once: "Files can't be added to that drive folder." exit 1. |
| 11 | During step 7 (or a new copy): `/deploy?seconds=30`. | The copy into W: finishes locally; uploads wait and resume within 35 s of the start; no held or failed items. |
| 12 | `/offline?on=1` for 60 s, then `on=0`. | Cached folders still list; an uncached file fails with an error instead of hanging Explorer (note how long it takes); saves made meanwhile show as unsent and upload after `on=0`. A send started while offline waits up to 20 s, then says "The drive can't be reached right now." |
| 13 | `taskkill /im olsyn-drive.exe /f` while a large save is uploading, then start the agent again. | The upload resumes from where it was (`part.put` grows by the missing parts only); no data lost. |
| 14 | `/revoke`. | Within 30 s the app says signed out by the drive; unsent saves are kept; signing in again uploads them. |
| 15 | `findstr /s /i /m "odt_" "%LOCALAPPDATA%\Olsyn Drive\logs\*"` and the same for a unique word you typed into a saved file in step 6. | No match for either. |

## Scripted acceptance

`scripts\windows\acceptance.cmd` runs the steps that need no clicks against a fresh fake drive, with
`OLSYN_DRIVE_HOME` under `%TEMP%`, so it does not touch a real installation:

```bat
C:OlsynTest> scriptswindowsacceptance.cmd C:OlsynTest
```

The argument is the folder holding both exes. It starts the fake drive with `--auto-approve`, starts the agent, signs
in, checks the status, mounts and lists `W:` when WinFsp is there, writes a file through `W:`, sends a generated
folder (200 small files and one 32 MiB file) twice, then runs the faults (deploy, offline, a managed and a sealed
destination, revoke) and checks the logs for tokens and file content. Each check prints `PASS` or `FAIL`; the exit
code is the number of failures. It stops the agent and the fake drive and deletes its folder at the end.

Over SSH (a network logon) Windows Credential Manager cannot store anything, so a sign-in there is refused at once
with a message saying so. A dev build can keep the token in an owner-only file under the data folder instead:
`set OLSYN_DRIVE_TOKEN_STORE=file` before starting the agent. `acceptance.cmd` does this by itself when it sees
`SSH_CONNECTION`. Release builds have no such switch.

## Smoke test of the desktop app

`scripts\windows\ui-smoke.ps1` checks the agent side of the app's sign-in without clicking. Run it in the desktop
session (not over SSH), from Windows PowerShell 5.1:

```bat
powershell -ExecutionPolicy Bypass -File scripts\windows\ui-smoke.ps1 -Bin C:\OlsynTest
```

It starts the fake drive with a data folder and without `--auto-approve`, starts `olsyn-drive.exe` with no
arguments (the app, which starts the agent) with a fresh `OLSYN_DRIVE_HOME` and `OLSYN_DRIVE_SERVER`, then talks
HTTP/1.1 to the agent over its named pipe as the app does: `GET /v1/state`, `POST /v1/signin`, the fake drive's
`POST /approve`, waits for connected (and mounted when WinFsp is installed), `GET /v1/transfers`, and checks that
`ui.log` has no per-asset lines and no failed requests. Each step writes `PASS` or `FAIL` to
`%TEMP%\olsyn-ui-smoke\result.txt` (`-Result` to change it), with full-screen screenshots of the app next to it.
`-KeepRunning` leaves everything running to click around; `-Screenshot <path.png>` only takes a screenshot.

Requests the window makes that fail inside the WebView (before reaching the app) are logged in `ui.log` as
`page request failed`, and requests the agent refuses as `agent refused a request` (method, path, status and
error code; never a body). Run the app with `olsyn-drive.exe ui --debug` to see Wails' own lines as well.

## Robustness checks

`scripts\windows\robustness.ps1` runs the agent of a dev build (no desktop app) against the fake drive and checks
what must survive real use. Run it in the desktop session from Windows PowerShell 5.1, with about 4 GB free:

```bat
powershell -ExecutionPolicy Bypass -File scripts\windows\robustness.ps1 -Bin C:\OlsynTest
```

Without `-Server` it starts `fakedrive-server.exe --auto-approve` itself; with `-Server` (and `-Faults`, default the
next port) it uses one already running. It signs in, installs WinFsp if needed (approve the prompt), mounts, and
then: (a) copies 500 files and 1 GiB, kills the agent mid-upload with `taskkill /f`, restarts it and checks with
the fake drive's `part.put.multipart` counter that the large file resumed from its finished parts; (b) takes the
fake drive offline for 60 s during a copy and checks reads of uncached data fail or answer within 2 s, writes go
on into the cache, and the backlog drains; (c) checks sign-out with unsent files is refused without
confirmation; (d) quits and restarts with unsent files; (e) runs 1,000 creates, overwrites, renames, deletes and
new folders and compares the drive (`GET /tree` on the fault controls) with the mounted copy, file by file and
hash by hash; (f) reads the tail of the generated 5 GiB file and 1 GiB sequentially, printing the throughput;
(g) saves a document the way Word does while another process holds it open and checks the drive kept the same
file with one more version. `PASS`, `FAIL` and `NOTE` lines go to `%TEMP%\olsyn-robustness\result.txt`
(`-Result` to change it); the exit code is the number of failed checks.

## Against a real drive

Use the release build (`make windows`), which only connects to https://drive.olsyn.com, and a test account with a
folder you may write to. Repeat manual steps 2 to 9 and 13 to 15 without `--server`, inside that test folder only.
Do not run the fault controls: they exist only on the fake drive. Pass criteria are the same, except the timings, which
depend on the connection: note them in the report.

## Every screen, by name (for screenshots)

With the agent running, each command brings the window to the front on that screen (the same path notifications
and `olsyn-drive://open/<screen>` links take):

```
olsyn-drive open transfers active      (also: waiting, held, done, skipped; or transfers/held)
olsyn-drive open activity
olsyn-drive open settings
olsyn-drive open account
olsyn-drive open about
olsyn-drive open popover               (the tray popover)
start olsyn-drive://open/transfers/held
```

Signed out, the window shows the setup steps instead of any screen: that is deliberate (the window always shows what
the agent says).

## Install scripts (manual, against the fake drive)

Build with `make dev`, copy `dist/windows-amd64/dev/` and `scripts/install/` to the test PC, then in one window:

```
fakedrive-server.exe --release-exe olsyn-drive.exe --release-version 1.0.0-dev --install-scripts scripts\install
```

It serves a stable release of that exe (both windows-x64 and windows-arm64, signed with a key it makes at start), the
artifact behind a 302 to storage as the drive does, and `/install.ps1` and `/install.cmd` with its own address filled
in; it prints the commands below with that address and key.

1. In a normal PowerShell 5.1 window: `$env:OLSYN_DRIVE_DEV_RELEASE_KEYS = "dev=<key it printed>"`, then
   `irm http://127.0.0.1:8787/install.ps1 | iex`. Expect: "Looking for the latest", the download line, "Checked: size
   and SHA-256 match", "Note: this build is not signed", then the pre-release wizard (steps 1-3, see 9 below;
   answer Y or C), "Checked again: SHA-256 unchanged", "Release manifest verified", "installed", the app opens
   on its Welcome screen. `$LASTEXITCODE` is 0 and the window stays open. No folder `olsyn-drive-install-*` is left in
   `%TEMP%`.
2. Run it again: "already installed" (exit 0), or with `-Force` a reinstall that stops and restarts the running copy.
3. Without the key variable (new window): the installer refuses ("has no release keys … --allow-unverified"), exit 1.
   With `& ([scriptblock]::Create((irm http://127.0.0.1:8787/install.ps1))) -AllowUnverified` it warns and installs.
4. A wrong key (`$env:OLSYN_DRIVE_DEV_RELEASE_KEYS = "dev=<another key>"`): refused, signature not valid, exit 1.
5. Change the exe on disk after starting the server: "does not match the SHA-256", exit 1, nothing installed.
6. Repeat 1 in PowerShell 7 (`pwsh`), and with `install.cmd` downloaded from `http://127.0.0.1:8787/install.cmd`
   (from cmd.exe; `echo %ERRORLEVEL%` is 0).
7. From an administrator PowerShell window with a signed build: refused, exit 1, with the reason. With an unsigned
   build the wizard runs instead (9), and the app is opened with normal rights (Task Manager > Details, "Elevated"
   column: No).
8. `powershell -File scripts\install\install.ps1 -Server http://127.0.0.1:9` (nothing listening): "could not read …",
   exit 1.
9. Pre-release wizard, normal window: `powershell -File scripts\install\install.ps1 -PreRelease -Installer olsyn-drive.exe`.
   Expect step 1 (two lines), step 2 creates `%LOCALAPPDATA%\Programs\Olsyn Drive`, Ctrl+V pastes its path, Windows
   Security opens on Exclusions; after Enter, step 3 says it cannot read the list and asks Y/N; N offers R/C/Q, Q
   ends with exit 3 and nothing installed. With Y: the file's size and SHA-256 are printed, it installs, the app
   opens, and steps 4-5 print the Protection history and "Remove later" text. Nothing in Windows Security changed
   unless you changed it.
10. The same from an administrator window: without the exclusion, step 3 says Windows Security does not list the
    folder; add it (also try with a trailing `\` or other letter case) and R: "Checked: Windows Security lists the
    Olsyn Drive folder". An exclusion of `%LOCALAPPDATA%\Programs` instead: "a larger folder ... so this works".
11. `-Sha256 <wrong 64 hex digits>`: refused, exit 1. `-DryRun`: walks steps 1-5, creates nothing, downloads
    nothing, exit 0. `-SelfTest`: "Self-test passed", exit 0 (also in `pwsh`).
12. With another antivirus active (or Defender turned off by policy): step 2 names it and asks you to allow the
    folder there; it does not open Windows Security.
