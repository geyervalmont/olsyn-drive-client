package shell

import (
	"context"
	"log/slog"
)

// libraryLogger is the logger handed to Wails. Wails logs every asset it
// serves at Info ("[AssetFileServerFS] Handling request"), which buried
// ui.log on the first Windows run; it gets Warn and above unless the app runs
// with --debug, when it gets everything.
func libraryLogger(log *slog.Logger) *slog.Logger {
	if log.Enabled(context.Background(), slog.LevelDebug) {
		return log.With("from", "wails")
	}
	return slog.New(minLevel{h: log.Handler(), min: slog.LevelWarn}).With("from", "wails")
}

// minLevel drops records below min before they reach h.
type minLevel struct {
	h   slog.Handler
	min slog.Level
}

func (m minLevel) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= m.min && m.h.Enabled(ctx, l)
}

func (m minLevel) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < m.min {
		return nil
	}
	return m.h.Handle(ctx, r)
}

func (m minLevel) WithAttrs(attrs []slog.Attr) slog.Handler {
	return minLevel{h: m.h.WithAttrs(attrs), min: m.min}
}

func (m minLevel) WithGroup(name string) slog.Handler {
	return minLevel{h: m.h.WithGroup(name), min: m.min}
}
