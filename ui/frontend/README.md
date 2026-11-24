# Olsyn Drive desktop UI

The window and tray popover that people see on their PC. It shows what the drive is doing (files sending, files the drive refused, sign-in, updates) and never talks to the server itself: everything goes through the local agent described in `docs/ipc.md`.

```
  React page (this folder)  ──fetch /agent/v1/...──▶  Wails shell (ui/)  ──named pipe / unix socket──▶  olsyn-drive agent
                            ◀─ events pushed in ────
```

The page runs in two ways, picked with `?transport=`:

| `?transport=` | Talks to | Used for |
| --- | --- | --- |
| `mock` (the default) | an in-page mock agent | design work, demos, tests |
| `bridge` | the Wails shell, which relays to the agent | the desktop app |

`HttpTransport` in `src/api/transport.ts` speaks the agent's HTTP and SSE directly; the shell's `/agent/` relay and the tests use the same wire format.

## Run it with the mock

```sh
npm ci
npm run dev
```

Open the printed address. A small bar at the bottom switches scenario and surface; it only exists in mock mode.

URL parameters:

- `?scenario=<name>` picks what the mock agent does (list below). Default `copy`.
- `?view=popover` shows the tray popover instead of the main window.
- `?speed=4` runs the mock four times faster (transfers, countdowns).
- `#transfers/held`, `#settings`, `#about` and so on open a screen directly.

### Scenarios

| Name | What happens |
| --- | --- |
| `copy` | A copy of 3,000 small files and a 3 GB video going up, with realistic speed and time left. |
| `first-run` | A new PC: welcome, sign in with a code, the driver step, pick a letter, done. |
| `signin` | Signed out on a PC that already has the driver: sign in and pick a letter. |
| `link-expired` | Sign-in where the code runs out before the browser approves it. |
| `restart` | A copy in progress when the drive restarts: uploads wait, then carry on. |
| `offline` | The drive cannot be reached; saved files wait on this PC. |
| `held` | Three files the drive refused: a managed folder, no permission, a name not allowed. |
| `conflict` | A file changed on the drive while this PC was saving it: a conflict copy was made. |
| `update` | An update is available; install it and watch it apply. |
| `driver-missing` | Signed in, but the file system driver is missing. |
| `signed-out` | Signed out by the drive (this PC was removed in the web app). |
| `big-list` | 12,000 files waiting while paused, to check the list stays smooth. |
| `empty` | Connected with nothing moving and no history. |
| `loading` | The agent takes a few seconds to answer. |
| `unreachable` | The agent is not running at first, then starts. |

All names, folders and files in the mock are made up.

## Check and build

```sh
npm test            # vitest, jsdom
npm run typecheck   # tsc --noEmit
npm run build       # typecheck, then dist/ for the shell to embed
```

The fonts (Hanken Grotesk and Lora, both SIL Open Font License) are bundled from `@fontsource-variable`; the page loads nothing from the network, and the built page carries a Content-Security-Policy that keeps it that way.

Dependencies are pinned to exact versions. They were resolved with `npm install --before=<date>` so every package is a release at least two weeks old; keep doing that when updating.

## Code map

- `src/api`: the agent's types, the client, the SSE parser and the transports (HTTP, bridge; the mock's is in `src/mock`).
- `src/state`: one store fed by the event stream, with reconnect and replay (`Last-Event-ID`).
- `src/mock`: the mock agent and its scenarios.
- `src/screens`: onboarding, the popover, the main window (Transfers, Activity, Settings, Account, About) and the banners.
- `src/ui`: small shared pieces (dialogs, tabs, the virtual list, the drive-letter mark).
- `src/lib/shell.ts`: what the page asks of the desktop shell (folder picker, windows, dropped files).

## The desktop shell

`ui/shell` (Wails v3) is linked into `olsyn-drive.exe` with the `desktopui` build tag, and `ui/frontend` (package
`frontend`) embeds `dist/`. From the repository root, on any OS:

```sh
make windows    # builds the page once, then dist/windows-amd64/olsyn-drive.exe
```

Started with no arguments (or `ui`, `--background` or an `olsyn-drive://` link) the exe is the desktop app; with a
command (`agent`, `status`, `send`, ...) it runs headless. The shell serves `/agent/v1/...` (relayed to the agent, Origin removed, 1 MiB request limit) and `/bridge/...` (event streams per window, folder picker, window actions, opening https links) on the page's own origin. It does not open a network port. `--background` starts with only the tray icon; an `olsyn-drive://show/<screen>` or `olsyn-drive://open/<drive path>` argument is handled at start and when a second copy is launched. If the agent does not answer, the shell starts `olsyn-drive agent` (itself, as the exe is one program), from `PATH`, or from `OLSYN_DRIVE_AGENT_EXE`.

## Agent calls added in 1.1

`GET /v1/cache`, `GET /v1/drive/folders?parent=`, `GET /v1/activity`, `GET /v1/batches`, `GET /v1/licences`, the `batch` event, `local_paths` on `POST /v1/send`, `ui` as an `/v1/open` target and `first_run` / `unsent` in `State` are in `docs/ipc.md`. The client degrades to hiding a feature when an older agent answers 404.

The licence lists in `src/licences.generated.ts` are written by `go run ./internal/notices/gen` from the repository root; do not edit them by hand.

