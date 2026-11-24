# Olsyn Drive desktop client

Work in progress. The desktop client for the Olsyn drive: it shows the drive as a normal drive on a PC (W: on
Windows), signs in through the browser, uploads and downloads reliably in the background, and shows what it is doing.
Windows 11 first; macOS and Linux follow.

- Design: [docs/design.md](docs/design.md)
- Agent and interface contract: [docs/ipc.md](docs/ipc.md)
- Testing on Windows: [docs/testing-windows.md](docs/testing-windows.md)

## Build

    make        # dist/windows-amd64/olsyn-drive.exe (needs Go and Node)
    make dev    # development build plus a fake drive server for testing
    make test

## Licence

Apache-2.0, see [LICENSE](LICENSE).

On Windows the drive letter is provided by WinFsp, which is installed separately:
WinFsp - Windows File System Proxy, Copyright (C) Bill Zissimopoulos, https://github.com/winfsp/winfsp
