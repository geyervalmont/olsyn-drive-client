// Package tray decides what the tray icon shows: which icon, the tooltip, and
// whether a state change deserves a toast. It is pure so it can be tested
// anywhere; the Wails shell only applies the result.
package tray

import (
	"fmt"
	"strings"

	"github.com/geyervalmont/olsyn-drive-client/internal/blocked"
	"github.com/geyervalmont/olsyn-drive-client/ui/internal/agentconn"
)

// Kind is one of the tray icon states.
type Kind string

const (
	Idle      Kind = "idle"      // connected, nothing moving
	Syncing   Kind = "syncing"   // transfers in progress (with a progress fraction)
	Paused    Kind = "paused"    // transfers paused by the person
	Offline   Kind = "offline"   // working from the cache, will catch up
	Attention Kind = "attention" // something needs the person (held files, sign in, driver)
	Stopped   Kind = "stopped"   // the agent is not answering
)

// View is what the tray should show.
type View struct {
	Kind Kind
	// Progress is 0..1 for Syncing, -1 when unknown.
	Progress float64
	// Tooltip is at most 127 characters (the Windows limit for tray tips).
	Tooltip string
}

// FromState maps the agent state to the tray view.
func FromState(s agentconn.State) View {
	t := s.Transfers
	switch {
	case s.Account == nil || s.Connection == "signed_out":
		return View{Kind: Attention, Progress: -1, Tooltip: "Olsyn Drive: sign in to use your drive"}
	case s.Connection == "token_revoked":
		return View{Kind: Attention, Progress: -1, Tooltip: "Olsyn Drive: sign in again to keep working"}
	case t.Held > 0:
		return View{Kind: Attention, Progress: -1, Tooltip: fmt.Sprintf("Olsyn Drive: %s could not be uploaded", files(t.Held))}
	case len(s.Issues) > 0:
		return View{Kind: Attention, Progress: -1, Tooltip: tip("Olsyn Drive: " + s.Issues[0].Message)}
	case s.Paused:
		return View{Kind: Paused, Progress: -1, Tooltip: pausedTip(t)}
	case s.Connection == "offline" || s.Connection == "connecting":
		msg := "Olsyn Drive: offline, changes wait on this PC"
		if t.FilesLeft > 0 {
			msg = fmt.Sprintf("Olsyn Drive: offline, %s waiting to upload", files(t.FilesLeft))
		}
		return View{Kind: Offline, Progress: -1, Tooltip: msg}
	case s.Connection == "restarting":
		return View{Kind: Offline, Progress: -1, Tooltip: "Olsyn Drive: the drive service is restarting"}
	case t.Active > 0 || t.FilesLeft > 0:
		return View{Kind: Syncing, Progress: fraction(t), Tooltip: syncingTip(t)}
	case s.Connection != "online":
		return View{Kind: Attention, Progress: -1, Tooltip: "Olsyn Drive needs attention"}
	default:
		return View{Kind: Idle, Progress: -1, Tooltip: idleTip(s)}
	}
}

// Down is the view when the agent does not answer.
func Down() View {
	return View{Kind: Stopped, Progress: -1, Tooltip: "Olsyn Drive isn't running. Click to start it."}
}

// Blocked is the view when security software removed or blocked the agent.
func Blocked(e *blocked.Error) View {
	return View{Kind: Stopped, Progress: -1, Tooltip: tip("Olsyn Drive: " + e.Title())}
}

func fraction(t agentconn.Summary) float64 {
	if t.BytesTotal <= 0 {
		return -1
	}
	f := float64(t.BytesDone) / float64(t.BytesTotal)
	return min(max(f, 0), 1)
}

func idleTip(s agentconn.State) string {
	if s.Mount.Letter != nil && *s.Mount.Letter != "" {
		return tip(fmt.Sprintf("Olsyn Drive (%s:): all changes are uploaded", strings.TrimSuffix(*s.Mount.Letter, ":")))
	}
	return "Olsyn Drive: all changes are uploaded"
}

func syncingTip(t agentconn.Summary) string {
	parts := []string{fmt.Sprintf("Olsyn Drive: uploading, %s left", files(max(t.FilesLeft, t.Active)))}
	if f := fraction(t); f >= 0 {
		parts = append(parts, fmt.Sprintf("%d%% done", int(f*100)))
	}
	if t.EtaSeconds != nil && *t.EtaSeconds > 0 {
		parts = append(parts, "about "+duration(*t.EtaSeconds))
	}
	return tip(strings.Join(parts, ", "))
}

func pausedTip(t agentconn.Summary) string {
	if t.FilesLeft > 0 {
		return tip(fmt.Sprintf("Olsyn Drive: paused with %s waiting to upload", files(t.FilesLeft)))
	}
	return "Olsyn Drive: paused"
}

func files(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}

func duration(sec int64) string {
	switch {
	case sec < 90:
		return "a minute"
	case sec < 3600:
		return fmt.Sprintf("%d minutes", (sec+30)/60)
	default:
		h := (sec + 1800) / 3600
		if h == 1 {
			return "an hour"
		}
		return fmt.Sprintf("%d hours", h)
	}
}

// tip shortens s to the Windows tray tooltip limit at a word boundary.
func tip(s string) string {
	const limit = 127
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	cut := string(r[:limit-1])
	if i := strings.LastIndexByte(cut, ' '); i > limit/2 {
		cut = cut[:i]
	}
	return cut + "…"
}

// Bucket rounds progress so the icon is redrawn at most 12 times per copy.
func Bucket(p float64) int {
	if p < 0 {
		return -1
	}
	return int(p * 12)
}
