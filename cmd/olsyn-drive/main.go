// Command olsyn-drive is the Olsyn Drive desktop client: the per-user agent
// that owns the mount, plus the CLI verbs that talk to it over local IPC
// (docs/ipc.md).
//
// Build with the version stamped in:
//
//	go build -ldflags "-X main.version=1.4.2" ./cmd/olsyn-drive
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/geyervalmont/olsyn-drive-client/internal/agent"
	"github.com/geyervalmont/olsyn-drive-client/internal/notices"
	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
)

// Set at build time with -ldflags -X. They are the only package-level values.
var (
	version = "dev"
)

// Exit codes.
const (
	exitOK         = 0
	exitError      = 1
	exitUsage      = 2
	exitNoAgent    = 3
	exitNeedsInput = 4
)

func main() {
	if len(os.Args) > 1 && !isUILaunch(os.Args[1:]) {
		attachConsole() // a GUI-subsystem build prints command output in the console it was started from
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

const usage = `Olsyn Drive

Usage: olsyn-drive <command> [options]

  (no command)          open the desktop app (builds that include it)
  ui [--background]     the same; --background starts with only the tray icon
  agent                 run the background agent (started at login)
  status [--json]       show what the drive is doing
  login [--no-browser]  sign this computer in through the browser
  logout [--clear-cache] [--yes]
                        sign out (and optionally delete the local cache)
  pause [--minutes N]   pause transfers
  resume                resume transfers
  send <path>... --to <drive folder> [--wait] [--on-conflict hold|new-version|keep-both]
                        [--include-online-only]
                        upload files and folders to the drive (they stay on this PC)
  open <screen>         bring the window to the front on a screen: transfers
                        [active|waiting|held|done|skipped], activity, settings,
                        account, about, or popover (the tray popover)
  diagnose              write a diagnostics bundle for support
  quit                  stop the agent (it unmounts the drive)
  version               print the version
  licences              print the open-source notices
  install --user        install for this user (no administrator needed)
  uninstall [--keep-cache] [--yes]
                        remove this program and its data (--keep-cache keeps
                        settings, cache and files not yet uploaded)

Environment: OLSYN_DRIVE_HOME puts every file under one folder (portable or test runs).
`

// env is what every command needs.
type env struct {
	ctx      context.Context
	paths    platform.Paths
	endpoint string
	stdin    io.Reader
	stdout   io.Writer
	stderr   io.Writer
}

func newEnv(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (*env, error) {
	var paths platform.Paths
	if home := os.Getenv("OLSYN_DRIVE_HOME"); home != "" {
		paths = platform.Under(home)
	} else {
		p, err := platform.DefaultPaths()
		if err != nil {
			return nil, err
		}
		paths = p
	}
	ep, err := platform.Endpoint(paths)
	if err != nil {
		return nil, err
	}
	return &env{ctx: ctx, paths: paths, endpoint: ep, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

func (e *env) client() *agent.Client {
	return agent.NewClient(func(ctx context.Context) (net.Conn, error) { return platform.Dial(ctx, e.endpoint) })
}

func (e *env) printf(format string, a ...any) { fmt.Fprintf(e.stdout, format, a...) }

func (e *env) errorf(format string, a ...any) { fmt.Fprintf(e.stderr, format, a...) }

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 && !hasUI {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	e, err := newEnv(ctx, stdin, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "olsyn-drive: %v\n", err)
		return exitError
	}
	if len(args) == 0 {
		// A double-click: from Downloads this offers to install (or update
		// the installed copy) and hands over to it.
		if code, done := offerInstall(e); done {
			return code
		}
	}
	if isUILaunch(args) {
		if len(args) > 0 && args[0] == "ui" {
			args = args[1:]
		}
		return runUI(e, args)
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "agent":
		return cmdAgent(e, rest)
	case "status":
		return cmdStatus(e, rest)
	case "login":
		return cmdLogin(e, rest)
	case "logout":
		return cmdLogout(e, rest)
	case "pause":
		return cmdPause(e, rest)
	case "resume":
		return cmdResume(e, rest)
	case "send":
		return cmdSend(e, rest)
	case "diagnose":
		return cmdDiagnose(e, rest)
	case "quit":
		if err := stopAgent(e); err != nil {
			return e.report(err)
		}
		e.printf("The agent is stopped (changes not yet uploaded stay queued for its next start).\n")
		return exitOK
	case "version", "--version", "-v":
		return cmdVersion(e, rest)
	case "install":
		return cmdInstall(e, rest)
	case "uninstall":
		return cmdUninstall(e, rest)
	case "open":
		return cmdOpen(e, rest)
	case "open-url":
		return cmdOpenURL(e, rest)
	case "licences", "licenses":
		fmt.Fprint(stdout, notices.Text())
		return exitOK
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	fmt.Fprintf(stderr, "olsyn-drive: unknown command %q\n\n%s", verb, usage)
	return exitUsage
}

// isUILaunch reports whether the arguments open the desktop app: none,
// "ui", "--background", or an olsyn-drive:// link (from the browser or a
// notification).
func isUILaunch(args []string) bool {
	if len(args) == 0 {
		return true
	}
	return args[0] == "ui" || args[0] == "--background" || strings.HasPrefix(args[0], "olsyn-drive://")
}

// report prints an error the way the person should read it and returns an exit code.
func (e *env) report(err error) int {
	var ae *agent.APIError
	switch {
	case errors.As(err, &ae):
		e.errorf("olsyn-drive: %s\n", ae.Error())
	case isNoAgent(err):
		e.errorf("olsyn-drive: the agent is not running (start it with \"olsyn-drive agent\", or sign in again)\n")
		return exitNoAgent
	default:
		e.errorf("olsyn-drive: %v\n", err)
	}
	return exitError
}

func isNoAgent(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) || errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}
