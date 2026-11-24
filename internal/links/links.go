// Package links names the desktop app's screens and reads olsyn-drive://
// links, so the command line ("olsyn-drive open <screen>"), the agent
// (POST /v1/open with target "ui"), links from the browser and notification
// clicks all reach a screen the same way: the agent starts the app with
// ScreenLink(screen), a running app takes the link (single instance) and
// shows that screen.
package links

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// Scheme is the URL scheme the installer registers.
const Scheme = "olsyn-drive"

// DefaultScreen is what a link or command without a screen shows.
const DefaultScreen = "transfers/active"

// Popover is the screen name of the tray popover (not a page of the main window).
const Popover = "popover"

// Screens are the screen names people and scripts use. "transfers" takes a
// filter after a slash; "signin" is the sign-in step (signed out) and is
// used by notifications.
var Screens = []string{"transfers", "activity", "settings", "account", "about", Popover, "signin"}

// TransferFilters are the tabs of the transfers screen.
var TransferFilters = []string{"active", "waiting", "held", "done", "skipped", "failed"}

// ErrUnknownScreen is returned for a screen name the app does not have.
var ErrUnknownScreen = errors.New("unknown screen")

// Screen checks and normalises a screen name: "" is DefaultScreen,
// "transfers" is "transfers/active", "Transfers/Held" is "transfers/held".
func Screen(name string) (string, error) {
	s := strings.ToLower(strings.Trim(strings.TrimSpace(name), "/"))
	if s == "" {
		return DefaultScreen, nil
	}
	if len(s) > 64 || strings.ContainsAny(s, "?#\\ ") {
		return "", fmt.Errorf("%q: %w", name, ErrUnknownScreen)
	}
	head, sub, hasSub := strings.Cut(s, "/")
	if !slices.Contains(Screens, head) {
		return "", fmt.Errorf("%q: %w (one of %s)", name, ErrUnknownScreen, strings.Join(Screens, ", "))
	}
	if head == "transfers" {
		if !hasSub || sub == "" {
			return DefaultScreen, nil
		}
		if !slices.Contains(TransferFilters, sub) {
			return "", fmt.Errorf("transfers filter %q: %w (one of %s)", sub, ErrUnknownScreen, strings.Join(TransferFilters, ", "))
		}
		return "transfers/" + sub, nil
	}
	if hasSub {
		return "", fmt.Errorf("%q: %w (only transfers takes a filter)", name, ErrUnknownScreen)
	}
	return head, nil
}

// ScreenLink is the link that shows a screen, e.g. olsyn-drive://open/transfers/held.
// The screen must already be normalised by Screen.
func ScreenLink(screen string) string {
	return Scheme + "://open/" + screen
}

// Kind is what a link asks for.
type Kind string

const (
	KindScreen Kind = "screen" // a screen of the desktop app
	KindDrive  Kind = "drive"  // a path on the drive, in File Explorer
	KindWeb    Kind = "web"    // a path on the drive, in the web app
)

// Action is a parsed link.
type Action struct {
	Kind   Kind
	Screen string // KindScreen: normalised screen name
	Path   string // KindDrive, KindWeb: drive path such as /Projects/Tower ("" = the top)
}

// Parse reads an olsyn-drive:// link:
//
//	olsyn-drive://                          the main window (transfers)
//	olsyn-drive://open/<screen>             a screen: transfers[/active|waiting|held|done|skipped], activity,
//	                                        settings, account, about, popover
//	olsyn-drive://show/<screen>             the same (links from earlier versions)
//	olsyn-drive://open?path=/Projects/Tower a folder of the drive in File Explorer
//	olsyn-drive://drive/Projects/Tower      the same
//	olsyn-drive://open/Projects/Tower       the same (links from earlier versions: a path whose
//	                                        first part is not a screen name, in lower case)
//	olsyn-drive://web?path=/Projects/Tower  the same folder in the drive's web app
func Parse(raw string) (Action, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Action{}, fmt.Errorf("read link: %w", err)
	}
	if !strings.EqualFold(u.Scheme, Scheme) {
		return Action{}, fmt.Errorf("not an %s link", Scheme)
	}
	host := strings.ToLower(u.Host)
	if host == "" && u.Opaque != "" { // olsyn-drive:open/settings
		host, _, _ = strings.Cut(strings.TrimPrefix(u.Opaque, "//"), "/")
	}
	rest, err := url.PathUnescape(strings.Trim(u.EscapedPath(), "/"))
	if err != nil {
		return Action{}, fmt.Errorf("read link path: %w", err)
	}
	query := u.Query().Get("path")
	switch host {
	case "", "show":
		return screenAction(rest)
	case "open":
		if rest == "" && query != "" {
			return driveAction(KindDrive, query)
		}
		if head, _, _ := strings.Cut(rest, "/"); head != "" && !slices.Contains(Screens, head) {
			return driveAction(KindDrive, rest)
		}
		return screenAction(rest)
	case "drive":
		if query != "" {
			return driveAction(KindDrive, query)
		}
		return driveAction(KindDrive, rest)
	case "web":
		if query != "" {
			return driveAction(KindWeb, query)
		}
		return driveAction(KindWeb, rest)
	}
	return Action{}, fmt.Errorf("unknown %s link %q", Scheme, host)
}

func screenAction(name string) (Action, error) {
	s, err := Screen(name)
	if err != nil {
		return Action{}, err
	}
	return Action{Kind: KindScreen, Screen: s}, nil
}

func driveAction(kind Kind, p string) (Action, error) {
	p = "/" + strings.Trim(p, "/")
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return Action{}, errors.New("bad drive path in link")
		}
	}
	if p == "/" && kind == KindWeb {
		p = ""
	}
	return Action{Kind: kind, Path: p}, nil
}

// FromArgs finds an olsyn-drive:// link among command-line arguments.
func FromArgs(args []string) (string, bool) {
	for _, a := range args {
		if strings.HasPrefix(strings.ToLower(a), Scheme+"://") || strings.HasPrefix(strings.ToLower(a), Scheme+":") {
			return a, true
		}
	}
	return "", false
}
