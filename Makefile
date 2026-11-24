# Builds the Olsyn Drive client with make, Go and Node only (no PowerShell).
# It cross-compiles from macOS or Linux; on Windows run it from Git Bash or
# MSYS2. Every Go build is CGO_ENABLED=0.
#
#   make windows        dist/windows-amd64/olsyn-drive.exe (desktop app + agent + CLI)
#   make windows-arm64  dist/windows-arm64/olsyn-drive.exe
#   make dev            dev builds: olsyn-drive.exe that may use another server,
#                       and fakedrive-server.exe (dist/windows-amd64/dev/)
#   make host-dev       the same two for this computer (dist/host-dev/), without the desktop app
#   make vet test       go vet on the four targets; tests (two packages at a time)
#   make notices        regenerate the open-source notices
#   make fp-info        file name, size, SHA-256 and version of the release exe, for a
#                       false-positive report (EXE=path for another file)
#   make clean

GO      ?= go
NPM     ?= npm
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

export CGO_ENABLED := 0

LDFLAGS := -s -w -X main.version=$(VERSION)
BUILD   := $(GO) build -trimpath

FRONTEND      := ui/frontend
FRONTEND_DIST := $(FRONTEND)/dist/index.html
FRONTEND_SRC  := $(shell find $(FRONTEND)/src -type f -not -name '*.test.*') $(FRONTEND)/index.html \
                 $(FRONTEND)/vite.config.ts $(FRONTEND)/package.json

.PHONY: all windows windows-arm64 dev host-dev frontend notices fp-info vet test clean FORCE

all: windows

windows: dist/windows-amd64/olsyn-drive.exe
windows-arm64: dist/windows-arm64/olsyn-drive.exe
dev: dist/windows-amd64/dev/olsyn-drive.exe dist/windows-amd64/dev/fakedrive-server.exe
host-dev: dist/host-dev/olsyn-drive dist/host-dev/fakedrive-server

# The front end is built once and embedded; it is rebuilt only when its
# sources change.
frontend: $(FRONTEND_DIST)

$(FRONTEND)/node_modules/.package-lock.json: $(FRONTEND)/package-lock.json
	cd $(FRONTEND) && $(NPM) ci

$(FRONTEND_DIST): $(FRONTEND)/node_modules/.package-lock.json $(FRONTEND_SRC)
	cd $(FRONTEND) && $(NPM) run build

# The program does not carry WinFsp. When the driver is missing it downloads
# WinFsp's own signed installer from the WinFsp project's GitHub release,
# pinned by size and SHA-256 in internal/winfspmsi, and checks it (and its
# Authenticode signature) before it runs.

# One exe: no arguments opens the desktop app; a command runs headless. It
# is a GUI-subsystem program that attaches to the console it was started
# from, so commands still print there.
ARCHS := amd64 arm64

# The icon, manifest and version information go in as a .syso that
# ui/winres writes for the arch being built (gitignored, rewritten each time
# so the version is current).
WINRES := $(GO) run ./ui/winres -out cmd/olsyn-drive

$(ARCHS:%=dist/windows-%/olsyn-drive.exe): dist/windows-%/olsyn-drive.exe: $(FRONTEND_DIST) FORCE
	$(WINRES) -arch $* -version $(VERSION)
	GOOS=windows GOARCH=$* $(BUILD) -tags desktopui -ldflags "$(LDFLAGS) -H=windowsgui" -o $@ ./cmd/olsyn-drive

$(ARCHS:%=dist/windows-%/dev/olsyn-drive.exe): dist/windows-%/dev/olsyn-drive.exe: $(FRONTEND_DIST) FORCE
	$(WINRES) -arch $* -version $(VERSION)-dev
	GOOS=windows GOARCH=$* $(BUILD) -tags "desktopui devbuild" -ldflags "$(LDFLAGS)-dev -H=windowsgui" -o $@ ./cmd/olsyn-drive

$(ARCHS:%=dist/windows-%/dev/fakedrive-server.exe): dist/windows-%/dev/fakedrive-server.exe: FORCE
	GOOS=windows GOARCH=$* $(BUILD) -tags devbuild -ldflags "$(LDFLAGS)" -o $@ ./cmd/fakedrive-server

dist/host-dev/olsyn-drive: FORCE
	$(BUILD) -tags devbuild -ldflags "$(LDFLAGS)-dev" -o $@ ./cmd/olsyn-drive

dist/host-dev/fakedrive-server: FORCE
	$(BUILD) -tags devbuild -ldflags "$(LDFLAGS)" -o $@ ./cmd/fakedrive-server

TARGETS := windows/amd64 windows/arm64 darwin/arm64 linux/amd64

vet:
	@for t in $(TARGETS); do \
		echo "vet $$t"; \
		GOOS=$${t%/*} GOARCH=$${t#*/} $(GO) vet ./... || exit 1; \
		GOOS=$${t%/*} GOARCH=$${t#*/} $(GO) vet -tags "desktopui devbuild" ./... || exit 1; \
	done

test:
	$(GO) test -p 2 ./...
	$(GO) test -p 2 -tags devbuild ./cmd/fakedrive-server
	cd $(FRONTEND) && $(NPM) test

notices:
	$(GO) run ./internal/notices/gen

# Reads the exe as built (it does not rebuild it, so the hash is the one of
# the file you send); docs/false-positive-submission.md says where it goes.
EXE ?= dist/windows-amd64/olsyn-drive.exe
fp-info:
	@test -f "$(EXE)" || { echo "$(EXE) not found: run make first, or pass EXE=path"; exit 1; }
	@$(GO) run ./ui/winres -inspect "$(EXE)"

clean:
	rm -rf dist cmd/olsyn-drive/rsrc_windows_*.syso

FORCE:
