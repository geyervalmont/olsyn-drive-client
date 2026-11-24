package bridge

import (
	"fmt"
	"net/url"
	"strings"
)

// Scheme is the URL scheme the installer registers for this app.
const Scheme = "olsyn-drive"

// LinkAction is what an olsyn-drive:// link asks for.
type LinkAction struct {
	// Kind is "open" (show a drive path in File Explorer) or "show" (a UI route).
	Kind string
	// DrivePath for "open", e.g. /Projects/Tower; Route for "show", e.g. transfers/held.
	DrivePath string
	Route     string
}

// ParseLink reads olsyn-drive://open/<drive path> and olsyn-drive://show/<route>.
// olsyn-drive:// on its own shows the main window.
func ParseLink(raw string) (LinkAction, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return LinkAction{}, fmt.Errorf("read link: %w", err)
	}
	if !strings.EqualFold(u.Scheme, Scheme) {
		return LinkAction{}, fmt.Errorf("not an %s link", Scheme)
	}
	rest := strings.Trim(u.EscapedPath(), "/")
	path, err := url.PathUnescape(rest)
	if err != nil {
		return LinkAction{}, fmt.Errorf("read link path: %w", err)
	}
	switch strings.ToLower(u.Host) {
	case "", "show":
		route := strings.Trim(path, "/")
		if route == "" {
			route = "transfers/active"
		}
		if !validRoute(route) {
			return LinkAction{}, fmt.Errorf("unknown route %q", route)
		}
		return LinkAction{Kind: "show", Route: route}, nil
	case "open":
		p := "/" + strings.Trim(path, "/")
		if strings.Contains(p, "/../") || strings.HasSuffix(p, "/..") {
			return LinkAction{}, fmt.Errorf("bad drive path")
		}
		return LinkAction{Kind: "open", DrivePath: p}, nil
	default:
		return LinkAction{}, fmt.Errorf("unknown link action %q", u.Host)
	}
}

func validRoute(route string) bool {
	head, _, _ := strings.Cut(route, "/")
	switch head {
	case "transfers", "activity", "settings", "account", "about", "signin":
		return true
	}
	return false
}

// LinkFromArgs finds an olsyn-drive:// link among command-line arguments.
func LinkFromArgs(args []string) (string, bool) {
	for _, a := range args {
		if strings.HasPrefix(strings.ToLower(a), Scheme+"://") {
			return a, true
		}
	}
	return "", false
}
