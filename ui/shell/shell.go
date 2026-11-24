// Package shell is the Olsyn Drive desktop UI: a tray icon with a popover, a
// main window, native notifications and the olsyn-drive:// link handler,
// built on Wails v3. The page it shows is ui/frontend, served from Assets.
//
// Only Windows is supported for now; on other systems Run returns an error
// (the pure parts underneath, in ui/internal, build and test everywhere).
package shell

import (
	"io/fs"
	"log/slog"
)

// Options configure Run.
type Options struct {
	// Assets is the built front end (ui/frontend/dist), rooted at index.html.
	Assets fs.FS
	// Background starts with only the tray icon (used for start at login).
	Background bool
	// Args are the command-line arguments after the program name; an
	// olsyn-drive:// link among them is handled at start.
	Args []string
	// Version is shown in the User-Agent sent to the agent.
	Version string
	// Logger receives the shell's log lines. Nil discards them. Wails gets
	// only its warnings and errors unless Logger is enabled for debug.
	Logger *slog.Logger
	// StateFile is where the main window's size and place are kept between
	// runs; empty keeps nothing.
	StateFile string
}

// AppID names the single-instance lock and the notification sender.
const AppID = "com.olsyn.drive"
