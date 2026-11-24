# Contributing

Thank you for helping. Issues and pull requests are welcome.

- Read [docs/design.md](docs/design.md) first; the agent's API for the interface is [docs/ipc.md](docs/ipc.md).
- Go code builds with `CGO_ENABLED=0` for windows/amd64, windows/arm64, darwin/arm64 and linux/amd64:
  run `make vet` and `make test` before opening a pull request. OS-specific code goes in `_windows.go`,
  `_darwin.go` or `_linux.go` files, with stubs elsewhere.
- Plain, readable Go: errors wrapped with context, no global state, `context.Context` on anything that can block,
  logging with `log/slog`. Never log a token or file content.
- Tests use synthetic data only (the fake drive in `internal/fakedrive`); keep test files small.
- New dependencies: prefer the standard library. A third-party module must be a tagged release at least two
  weeks old with a permissive licence (MIT, BSD or Apache-2.0); run `make notices` after adding one.
- Windows behaviour (the mount, the installer, the desktop app) is checked by hand with
  [docs/testing-windows.md](docs/testing-windows.md) and the scripts in `scripts/windows`.
- Contributions are accepted under the Apache License 2.0 (see [LICENSE](LICENSE)).

Please report security problems privately, as described in [SECURITY.md](SECURITY.md), not in public issues.
