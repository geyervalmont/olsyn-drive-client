package tray

import (
	"encoding/json"

	"github.com/geyervalmont/olsyn-drive-client/ui/internal/agentconn"
)

// Click is what clicking a toast does: show a UI route, or ask the agent to
// open something (POST /v1/open).
type Click struct {
	Route string
	Open  *agentconn.OpenTarget
}

// ClickFor decides what clicking notification n does. The action the agent
// supplied wins; otherwise the kind picks a sensible screen.
func ClickFor(n agentconn.Notification) Click {
	if n.Action != nil && len(n.Action.Open) > 0 {
		var t agentconn.OpenTarget
		if json.Unmarshal(n.Action.Open, &t) == nil && t.Target != "" {
			if t.Target == "ui" {
				if t.Path == "" {
					return Click{Route: "transfers/active"}
				}
				return Click{Route: t.Path}
			}
			return Click{Open: &t}
		}
	}
	switch n.Kind {
	case "held", "conflict":
		return Click{Route: "transfers/held"}
	case "signed_out":
		return Click{Route: "signin"}
	case "update_applied", "update_failed":
		return Click{Route: "about"}
	case "finished":
		return Click{Route: "transfers/done"}
	default:
		return Click{Route: "activity"}
	}
}
