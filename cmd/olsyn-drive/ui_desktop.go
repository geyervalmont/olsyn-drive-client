//go:build desktopui

package main

import (
	"log/slog"
	"path/filepath"

	"github.com/geyervalmont/olsyn-drive-client/ui/frontend"
	"github.com/geyervalmont/olsyn-drive-client/ui/shell"
)

// hasUI is true: this program is also the desktop app (tray, popover and
// window), started with no arguments, "ui", or an olsyn-drive:// link.
const hasUI = true

// runUI runs the desktop app until it quits. It logs to <data>/logs/ui.log.
func runUI(e *env, args []string) int {
	fs := newFlags(e, "ui")
	background := fs.Bool("background", false, "start with only the tray icon (used at sign-in)")
	debug := fs.Bool("debug", false, "log debug detail")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	logFile, err := openRotating(filepath.Join(e.paths.Logs, "ui.log"), 5<<20, 2)
	if err != nil {
		return e.report(err)
	}
	defer logFile.Close()
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(logFile, &slog.HandlerOptions{Level: level}))
	assets, err := frontend.Assets()
	if err != nil {
		log.Error("cannot start the desktop app", "err", err)
		showError("Olsyn Drive could not start", err.Error())
		return e.report(err)
	}
	log.Info("desktop app starting", "version", version, "background", *background)
	opts := shell.Options{Assets: assets, Background: *background, Args: fs.Args(), Version: version, Logger: log,
		StateFile: filepath.Join(e.paths.State, "ui-window.json"), StateDir: e.paths.State}
	if err := shell.Run(e.ctx, opts); err != nil {
		log.Error("desktop app stopped", "err", err)
		// Started from Explorer or at sign-in there is no console to print to.
		showError("Olsyn Drive could not start", err.Error()+"\n\nDetails are in "+filepath.Join(e.paths.Logs, "ui.log"))
		return e.report(err)
	}
	return exitOK
}
