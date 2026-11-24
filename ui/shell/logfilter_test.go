package shell

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestLibraryLogger(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	lib := libraryLogger(log)
	lib.Info("[AssetFileServerFS] Handling request", "url", "/assets/app.js")
	lib.Debug("Asset Request:")
	lib.With("k", "v").Info("also dropped")
	lib.Warn("webview2 problem", "err", "x")
	log.Info("shell line")
	out := buf.String()
	if strings.Contains(out, "Handling request") || strings.Contains(out, "Asset Request") || strings.Contains(out, "also dropped") {
		t.Fatalf("Wails info lines reached the log:\n%s", out)
	}
	if !strings.Contains(out, "webview2 problem") || !strings.Contains(out, "from=wails") || !strings.Contains(out, "shell line") {
		t.Fatalf("lines missing:\n%s", out)
	}

	buf.Reset()
	debug := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	libraryLogger(debug).Info("[AssetFileServerFS] Handling request")
	if !strings.Contains(buf.String(), "Handling request") {
		t.Fatal("--debug should keep Wails info lines")
	}
}
