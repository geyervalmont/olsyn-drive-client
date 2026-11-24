# Olsyn Drive desktop client

Work in progress. The desktop client for the Olsyn drive: it shows the drive as a normal drive on a PC (W: on
Windows), signs in through the browser, uploads and downloads reliably in the background, and shows what it is doing.
Windows 11 first; macOS and Linux follow.

- Design: [docs/design.md](docs/design.md)
- Agent and interface contract: [docs/ipc.md](docs/ipc.md)
- Testing on Windows: [docs/testing-windows.md](docs/testing-windows.md)
- Contributing: [CONTRIBUTING.md](CONTRIBUTING.md); reporting a vulnerability: [SECURITY.md](SECURITY.md)

## Install

On Windows 10 or 11, in a normal (not administrator) PowerShell window:

    irm https://drive.olsyn.com/install.ps1 | iex

or download and run `install.cmd` from the same place. Both run [scripts/install/install.ps1](scripts/install/install.ps1)
(Windows PowerShell 5.1 or PowerShell 7, default execution policy, the system proxy, TLS 1.2 or later). It installs
for the signed-in person only, never asks for administrator rights, and running it again updates an older copy.
Options when run as a file: `-Server <url>`, `-Channel stable|beta`, `-NoLaunch`, `-Force`. Exit codes: 0 installed
(or already up to date), 1 failed, 2 not supported on this PC. It sends nothing but the requests below.

What it does, in order:

1. reads `<server>/api/client/v1/releases/<channel>.json` and picks the installer for this PC (x64 or Arm);
2. downloads it to a temporary folder and checks its size and SHA-256 against the manifest;
3. checks its Authenticode signature when it has one, and says "this build is not signed" when it has none;
4. runs `olsyn-drive.exe install --user --quiet --manifest <the saved manifest>`, deletes the temporary folder and
   opens Olsyn Drive, where the person signs in.

The script does **not** check the manifest's Ed25519 signature, because Windows PowerShell 5.1 has no Ed25519.
The installer does: it verifies the manifest it is handed with the release keys compiled into it, and refuses to
install when the signature does not check out or when it is not the installer that signed manifest names (size and
SHA-256 of its own file). A development build has no release keys: it says so and installs only with
`--allow-unverified` (the script's `-AllowUnverified`). macOS and Linux are not available yet
([scripts/install/install.sh](scripts/install/install.sh) says so and exits 1).

While builds are unsigned, a person sees this:

- PowerShell prints "Note: this build is not signed." and carries on; the size, SHA-256 and the manifest's
  signature are still checked as above.
- A file fetched by the script carries no Mark-of-the-Web, so SmartScreen's "Windows protected your PC" prompt for
  downloaded files does not appear. The same exe downloaded with a browser does get that prompt ("More info" > "Run
  anyway").
- Where Smart App Control is on, Windows blocks unsigned programs however they arrive: the installer cannot run
  there until builds are signed. The script then fails with the installer's error and exit code 1.

## Build

    make        # dist/windows-amd64/olsyn-drive.exe (needs Go, Node and the gh CLI)
    make dev    # development build plus a fake drive server for testing
    make test

The Windows builds embed the unmodified WinFsp installer; `make winfsp-msi` (run by `make`) downloads it from
the WinFsp project's v2.2 release with `gh` into `build/` and checks its size and SHA-256. It is never committed.

`relay/` holds this project's own earlier relay client (a separate Go module under the same licence), kept for
reference; it is not part of the program.

## Licence

Apache-2.0, see [LICENSE](LICENSE) and [NOTICE](NOTICE).

On Windows the drive letter is provided by WinFsp. The Windows program carries WinFsp's installer, unmodified,
and runs it once with the person's approval:
WinFsp - Windows File System Proxy, Copyright (C) Bill Zissimopoulos, https://github.com/winfsp/winfsp.
WinFsp is licensed under the GPLv3 with the WinFsp FLOSS exception; the source of the included release is at
https://github.com/winfsp/winfsp/tree/v2.2. `olsyn-drive licences` and the About screen list every open-source
component linked into the program, with its licence.
