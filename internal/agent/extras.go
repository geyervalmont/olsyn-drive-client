package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/settings"
)

// SendRequest is a POST /v1/send: local files and folders into one drive
// folder (by path, or by id).
type SendRequest struct {
	LocalPaths    []string
	DriveFolder   string
	DriveFolderID string
	// OnConflict is what happens when the drive has a different file of
	// the same name: "hold" (default), "new_version" or "keep_both".
	OnConflict string
	// IncludeOnlineOnly reads cloud online-only files (downloads them).
	IncludeOnlineOnly bool
}

// Batch is one send's progress (the "batch" event, GET /v1/batches).
type Batch struct {
	ID           string     `json:"batch_id"`
	State        string     `json:"state"` // scanning, sending, done, failed, cancelled
	DriveFolder  string     `json:"drive_folder"`
	LocalPaths   []string   `json:"local_paths"`
	Files        int        `json:"files"`
	FilesDone    int        `json:"files_done"`
	FilesSkipped int        `json:"files_skipped"`
	FilesFailed  int        `json:"files_failed"`
	FilesHeld    int        `json:"files_held"`
	NotLocal     int        `json:"files_not_local"`
	Bytes        int64      `json:"bytes"`
	BytesDone    int64      `json:"bytes_done"`
	BPS          int64      `json:"bps"`
	ETASeconds   *int64     `json:"eta_seconds"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
	Error        *Problem   `json:"error"`
}

// Batch states.
const (
	BatchScanning  = "scanning"
	BatchSending   = "sending"
	BatchDone      = "done"
	BatchFailed    = "failed"
	BatchCancelled = "cancelled"
)

// CacheUsage is GET /v1/cache.
type CacheUsage struct {
	Dir         string `json:"dir"`
	UsedBytes   int64  `json:"used_bytes"`
	CapBytes    int64  `json:"cap_bytes"`
	UnsentBytes int64  `json:"unsent_bytes"`
	UnsentFiles int    `json:"unsent_files"`
	// UploadedBytes: sent from this PC, cleared once the drive has
	// confirmed it (after a short grace period).
	UploadedBytes int64 `json:"uploaded_bytes"`
	// OpenedBytes: drive files opened on this PC, kept so they open fast;
	// the oldest go when the cache is over its cap.
	OpenedBytes   int64 `json:"opened_bytes"`
	DiskFreeBytes int64 `json:"disk_free_bytes"`
}

// CacheState is the cache part of State: the same split as CacheUsage.
type CacheState struct {
	UsedBytes     int64 `json:"used_bytes"`
	CapBytes      int64 `json:"cap_bytes"`
	UnsentBytes   int64 `json:"unsent_bytes"`
	UploadedBytes int64 `json:"uploaded_bytes"`
	OpenedBytes   int64 `json:"opened_bytes"`
}

// CacheFreed is POST /v1/cache/free.
type CacheFreed struct {
	Files int        `json:"freed_files"`
	Bytes int64      `json:"freed_bytes"`
	Cache CacheUsage `json:"cache"`
}

// DriveFolder is one entry of GET /v1/drive/folders.
type DriveFolder struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	CanWrite bool   `json:"can_write"`
	// Why is why the folder cannot be written: "read_only", "managed",
	// "sealed" or "" when it can.
	Why string `json:"why"`
}

// DriveFolders is GET /v1/drive/folders.
type DriveFolders struct {
	Parent     string        `json:"parent"`
	ParentID   string        `json:"parent_id"`
	CanWrite   bool          `json:"can_write"`
	Folders    []DriveFolder `json:"folders"`
	NextCursor *string       `json:"next_cursor"`
}

// ActivityPage is GET /v1/activity.
type ActivityPage struct {
	Items      []Notification `json:"items"`
	NextCursor *string        `json:"next_cursor"`
}

// activityLog keeps the last activityMax notifications, newest last, in a
// file under the state folder. It holds file names, as the journal does,
// and no content.
type activityLog struct {
	path string
	mu   sync.Mutex
	list []Notification
}

const activityMax = 500

func openActivity(path string) *activityLog {
	l := &activityLog{path: path}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &l.list)
	}
	return l
}

func (l *activityLog) add(n Notification) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list = append(l.list, n)
	if over := len(l.list) - activityMax; over > 0 {
		l.list = slices.Delete(l.list, 0, over)
	}
	l.saveLocked()
}

func (l *activityLog) clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list = nil
	l.saveLocked()
}

func (l *activityLog) saveLocked() {
	if l.path == "" {
		return
	}
	data, err := json.Marshal(l.list)
	if err == nil {
		_ = settings.WriteFileAtomic(l.path, data, 0o600)
	}
}

// page returns newest first; the cursor is an offset.
func (l *activityLog) page(cursor string, limit int) (ActivityPage, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	start := 0
	if cursor != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(cursor, "a_"))
		if err != nil || n < 0 {
			return ActivityPage{}, invalid("cursor", "cursor is not one this agent gave.")
		}
		start = n
	}
	out := ActivityPage{Items: []Notification{}}
	for i := len(l.list) - 1 - start; i >= 0 && len(out.Items) < limit; i-- {
		out.Items = append(out.Items, l.list[i])
	}
	if next := start + len(out.Items); next < len(l.list) {
		c := "a_" + strconv.Itoa(next)
		out.NextCursor = &c
	}
	return out, nil
}

// lastID returns the highest notification number seen, so ids stay unique
// across restarts.
func (l *activityLog) lastID() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	high := 0
	for _, n := range l.list {
		if v, err := strconv.Atoi(strings.TrimPrefix(n.ID, "n_")); err == nil && v > high {
			high = v
		}
	}
	return high
}

// --- handlers ---

func (a *Agent) hCache(w http.ResponseWriter, r *http.Request) {
	if err := a.requireRunning(); err != nil {
		a.fail(w, err)
		return
	}
	u, err := a.cacheUsage(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 200, u)
}

// cacheUsage is GET /v1/cache's answer.
func (a *Agent) cacheUsage(ctx context.Context) (CacheUsage, error) {
	u, err := a.opts.FS.CacheUsage(ctx)
	if err != nil {
		return CacheUsage{}, err
	}
	a.mu.Lock()
	u.CapBytes = a.settings.CacheCapBytes
	dir := a.settings.CacheDir
	a.mu.Unlock()
	if dir == "" {
		dir = a.opts.Paths.Cache
	}
	u.Dir = dir
	if free, ferr := a.opts.System.FreeDiskBytes(dir); ferr == nil {
		u.DiskFreeBytes = int64(free)
	}
	return u, nil
}

func (a *Agent) hCacheFree(w http.ResponseWriter, r *http.Request) {
	if err := a.requireRunning(); err != nil {
		a.fail(w, err)
		return
	}
	freed, err := a.opts.FS.FreeCache(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	u, err := a.cacheUsage(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	freed.Cache = u
	a.log.Info("cache freed on request", "files", freed.Files, "bytes", freed.Bytes)
	a.emitState()
	writeJSON(w, 200, freed)
}

func (a *Agent) hDriveFolders(w http.ResponseWriter, r *http.Request) {
	if err := a.requireRunning(); err != nil {
		a.fail(w, err)
		return
	}
	q := r.URL.Query()
	parent, parentID := q.Get("parent"), q.Get("parent_id")
	if parent == "" && parentID == "" {
		parent = "/"
	}
	if parent != "" && !strings.HasPrefix(parent, "/") {
		a.fail(w, invalid("parent", "parent must be a drive path starting with /."))
		return
	}
	res, err := a.opts.FS.DriveFolders(r.Context(), parent, parentID, q.Get("cursor"))
	if err != nil {
		a.fail(w, err)
		return
	}
	a.mu.Lock()
	ro := a.account != nil && a.account.ReadOnly
	a.mu.Unlock()
	if ro { // a read-only device writes nowhere
		res.CanWrite = false
		for i := range res.Folders {
			res.Folders[i].CanWrite, res.Folders[i].Why = false, "read_only"
		}
	}
	writeJSON(w, 200, res)
}

func (a *Agent) hActivity(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			a.fail(w, invalid("limit", "limit must be 1 to 200."))
			return
		}
		limit = n
	}
	p, err := a.activity.page(r.URL.Query().Get("cursor"), limit)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 200, p)
}

func (a *Agent) hBatches(w http.ResponseWriter, r *http.Request) {
	items := a.opts.FS.Batches()
	if items == nil {
		items = []Batch{}
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (a *Agent) hLicences(w http.ResponseWriter, r *http.Request) {
	if a.opts.Licences == "" {
		a.fail(w, newAPIError(404, "not_found", "This build carries no licence notices."))
		return
	}
	writeJSON(w, 200, map[string]string{"text": a.opts.Licences})
}

// uiRoutes are the routes the desktop app knows (its links use the same).
var uiRoutes = []string{"transfers", "activity", "settings", "account", "about", "signin"}

func validUIRoute(route string) bool {
	head, _, _ := strings.Cut(route, "/")
	return slices.Contains(uiRoutes, head) && !strings.ContainsAny(route, "?#\\") && len(route) <= 200
}

// openUI shows the desktop app at a route.
func (a *Agent) openUI(ctx context.Context, route string) error {
	route = strings.Trim(route, "/")
	if route == "" {
		route = "transfers/active"
	}
	if !validUIRoute(route) {
		return invalid("path", "path must be a window route such as transfers/held.")
	}
	if err := a.opts.System.ShowUI(ctx, route); err != nil {
		if errors.Is(err, ErrNoUI) {
			return newAPIError(409, "no_ui", "This build has no desktop window; use the command line.")
		}
		return fmt.Errorf("show the window: %w", err)
	}
	return nil
}

// sendPaths validates a send's local paths.
func sendPaths(single string, many []string) ([]string, error) {
	paths := slices.Clone(many)
	if single != "" {
		paths = append([]string{single}, paths...)
	}
	if len(paths) == 0 {
		return nil, invalid("local_paths", "Give local_paths (or local_path).")
	}
	if len(paths) > 1000 {
		return nil, invalid("local_paths", "Send at most 1,000 items at once.")
	}
	seen := map[string]bool{}
	out := paths[:0]
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			return nil, invalid("local_paths", "Every local path must be absolute.")
		}
		p = filepath.Clean(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		if _, err := os.Stat(p); err != nil {
			e := newAPIError(404, "local_not_found", "Not found on this PC: "+filepath.Base(p))
			e.Extra = map[string]any{"local_path": p}
			return nil, e
		}
		out = append(out, p)
	}
	return out, nil
}
