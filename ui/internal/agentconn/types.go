package agentconn

import "encoding/json"

// The parts of docs/ipc.md types the shell itself reads (tray icon, tooltip,
// toasts). The front end has the full types; unknown fields are ignored.

// State is the subset of GET /v1/state the shell uses.
type State struct {
	AgentRun   string   `json:"agent_run"`
	Version    string   `json:"version"`
	Connection string   `json:"connection"`
	Reason     *Reason  `json:"reason"`
	Account    *Account `json:"account"`
	Mount      Mount    `json:"mount"`
	Paused     bool     `json:"paused"`
	Transfers  Summary  `json:"transfers"`
	Issues     []Issue  `json:"issues"`
}

// Reason explains the current connection value.
type Reason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Account is who is signed in; nil when signed out.
type Account struct {
	Name   string `json:"name"`
	Server string `json:"server"`
}

// Mount is where the drive is.
type Mount struct {
	State  string  `json:"state"`
	Letter *string `json:"letter"`
	Path   *string `json:"path"`
}

// Summary is the transfer summary (also the "summary" event).
type Summary struct {
	Active     int    `json:"active"`
	Waiting    int    `json:"waiting"`
	Held       int    `json:"held"`
	FilesLeft  int    `json:"files_left"`
	BytesTotal int64  `json:"bytes_total"`
	BytesDone  int64  `json:"bytes_done"`
	UpBps      int64  `json:"up_bps"`
	DownBps    int64  `json:"down_bps"`
	EtaSeconds *int64 `json:"eta_seconds"`
}

// Issue is something needing the person.
type Issue struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Action  string `json:"action"`
}

// Notification is the payload of the "notification" event.
type Notification struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	At     string `json:"at"`
	Action *struct {
		Label string          `json:"label"`
		Open  json.RawMessage `json:"open"`
	} `json:"action"`
}

// OpenTarget is the body of POST /v1/open, also used in notification actions.
type OpenTarget struct {
	Target string `json:"target"`
	Path   string `json:"path,omitempty"`
}

// Snapshot is the first event of every stream.
type Snapshot struct {
	State State `json:"state"`
}
