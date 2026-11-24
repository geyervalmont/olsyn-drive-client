# Olsyn Drive desktop client

Work in progress. The desktop client for the Olsyn drive: it shows the drive as a normal drive on a PC (W: on
Windows), signs in through the browser, uploads and downloads reliably in the background, and shows what it is doing.
Windows 11 first; macOS and Linux follow.

- Design: [docs/design.md](docs/design.md)
- Agent and interface contract: [docs/ipc.md](docs/ipc.md)
- Testing on Windows: [docs/testing-windows.md](docs/testing-windows.md)
- Contributing: [CONTRIBUTING.md](CONTRIBUTING.md); reporting a vulnerability: [SECURITY.md](SECURITY.md)

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
