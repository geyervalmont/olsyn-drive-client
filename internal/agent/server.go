package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
	"github.com/geyervalmont/olsyn-drive-client/internal/settings"
)

const maxBody = 1 << 20

// handler is the IPC HTTP API (docs/ipc.md).
func (a *Agent) handler() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, methods map[string]http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			h, ok := methods[r.Method]
			if !ok {
				allowed := make([]string, 0, len(methods))
				for m := range methods {
					allowed = append(allowed, m)
				}
				slices.Sort(allowed)
				w.Header().Set("Allow", strings.Join(allowed, ", "))
				writeError(w, newAPIError(http.StatusMethodNotAllowed, "method_not_allowed", "Use "+strings.Join(allowed, " or ")+"."))
				return
			}
			h(w, r)
		})
	}
	get := func(h http.HandlerFunc) map[string]http.HandlerFunc {
		return map[string]http.HandlerFunc{http.MethodGet: h}
	}
	post := func(h http.HandlerFunc) map[string]http.HandlerFunc {
		return map[string]http.HandlerFunc{http.MethodPost: h}
	}

	route("/v1/state", get(a.hState))
	route("/v1/signin", post(a.hSignIn))
	route("/v1/signin/cancel", post(a.hSignInCancel))
	route("/v1/signout", post(a.hSignOut))
	route("/v1/transfers", get(a.hTransfers))
	route("/v1/transfers/{id}/retry", post(a.hTransferRetry))
	route("/v1/transfers/{id}/cancel", post(a.hTransferCancel))
	route("/v1/held/{id}/discard", post(a.hHeldDiscard))
	route("/v1/held/{id}/save-copy", post(a.hHeldSaveCopy))
	route("/v1/held/{id}/reupload", post(a.hHeldReupload))
	route("/v1/pause", post(a.hPause))
	route("/v1/resume", post(a.hResume))
	route("/v1/send", post(a.hSend))
	route("/v1/settings", map[string]http.HandlerFunc{http.MethodGet: a.hSettingsGet, http.MethodPatch: a.hSettingsPatch})
	route("/v1/mount", post(a.hMount))
	route("/v1/unmount", post(a.hUnmount))
	route("/v1/update", get(a.hUpdate))
	route("/v1/update/check", post(a.hUpdateCheck))
	route("/v1/update/apply", post(a.hUpdateApply))
	route("/v1/driver/install", post(a.hDriverInstall))
	route("/v1/driver/check", post(a.hDriverCheck))
	route("/v1/diagnostics", post(a.hDiagnostics))
	route("/v1/open", post(a.hOpen))
	route("/v1/quit", post(a.hQuit))
	route("/v1/cache", get(a.hCache))
	route("/v1/cache/free", post(a.hCacheFree))
	route("/v1/drive/folders", get(a.hDriveFolders))
	route("/v1/activity", get(a.hActivity))
	route("/v1/batches", get(a.hBatches))
	route("/v1/batches/{id}/cancel", post(a.hBatchCancel))
	route("/v1/licences", get(a.hLicences))
	route("/v1/events", get(a.hEvents))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, newAPIError(http.StatusNotFound, "not_found", "No such endpoint."))
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Olsyn-Drive-Agent", a.opts.Version)
		w.Header().Set("Olsyn-Drive-Api", strconv.Itoa(APIVersion))
		if r.Header.Get("Origin") != "" {
			writeError(w, newAPIError(http.StatusForbidden, "forbidden_origin", "Requests from web pages are not accepted."))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		mux.ServeHTTP(w, r)
	})
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, e *apiError) {
	body := map[string]any{}
	for k, v := range e.Extra {
		body[k] = v
	}
	body["error"] = e.Code
	body["message"] = e.Message
	writeJSON(w, e.Status, body)
}

// fail maps any error onto the IPC error format.
func (a *Agent) fail(w http.ResponseWriter, err error) {
	var ae *apiError
	var fe *settings.FieldError
	switch {
	case errors.As(err, &ae):
		writeError(w, ae)
	case errors.As(err, &fe):
		e := newAPIError(422, "invalid_setting", fe.Message)
		e.Extra = map[string]any{"field": fe.Field}
		writeError(w, e)
	case errors.Is(err, ErrNotFound):
		writeError(w, newAPIError(404, "not_found", "Not found."))
	case errors.Is(err, ErrNotRetryable):
		writeError(w, newAPIError(409, "not_retryable", "This transfer cannot be retried."))
	case errors.Is(err, ErrNotCancellable):
		writeError(w, newAPIError(409, "not_cancellable", "This transfer cannot be cancelled."))
	case errors.Is(err, ErrNotHeld):
		writeError(w, newAPIError(409, "not_held", "This item is not held."))
	case errors.Is(err, ErrReadOnly):
		writeError(w, newAPIError(403, "read_only", "This computer was linked read-only."))
	case errors.Is(err, ErrLocalNotFound):
		writeError(w, newAPIError(404, "local_not_found", "That file or folder was not found on this computer."))
	case errors.Is(err, ErrDriveFolderNotFound):
		writeError(w, newAPIError(404, "drive_folder_not_found", "That folder was not found on the drive."))
	case errors.Is(err, ErrDriveUnreachable):
		writeError(w, newAPIError(503, "drive_unreachable", "The drive can't be reached right now. Check the connection and try again."))
	case errors.Is(err, ErrDriveFolderReadOnly):
		writeError(w, newAPIError(403, "drive_folder_read_only", "Files can't be added to that drive folder."))
	case errors.Is(err, context.Canceled):
		writeError(w, newAPIError(503, "stopping", "The agent is stopping."))
	default:
		a.log.Error("ipc request failed", "err", err)
		writeError(w, newAPIError(500, "internal", "Something went wrong in the agent. A diagnostics bundle will help."))
	}
}

// decode reads an optional JSON body into v.
func decode(r *http.Request, v any) error {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return newAPIError(400, "bad_request", "The request body could not be read.")
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return newAPIError(400, "bad_request", "The request body is not valid JSON for this endpoint.")
	}
	return nil
}

func invalid(field, msg string) *apiError {
	e := newAPIError(422, "invalid", msg)
	e.Extra = map[string]any{"field": field}
	return e
}

// --- handlers ---

func (a *Agent) hState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.State())
}

func (a *Agent) hSignIn(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OpenBrowser *bool `json:"open_browser"`
	}
	if err := decode(r, &body); err != nil {
		a.fail(w, err)
		return
	}
	open := body.OpenBrowser == nil || *body.OpenBrowser
	s, err := a.SignIn(r.Context(), open)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 202, s)
}

func (a *Agent) hSignInCancel(w http.ResponseWriter, r *http.Request) {
	s, err := a.CancelSignIn()
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 200, s)
}

func (a *Agent) hSignOut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClearCache             bool `json:"clear_cache"`
		DiscardUnsentConfirmed bool `json:"discard_unsent_confirmed"`
	}
	if err := decode(r, &body); err != nil {
		a.fail(w, err)
		return
	}
	res, err := a.SignOut(r.Context(), body.ClearCache, body.DiscardUnsentConfirmed)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 200, res)
}

var transferStates = []string{TransferActive, TransferWaiting, TransferHeld, TransferDone, TransferSkipped, TransferFailed, TransferCancelled}

func (a *Agent) hTransfers(w http.ResponseWriter, r *http.Request) {
	q := TransferQuery{Cursor: r.URL.Query().Get("cursor"), Limit: 100}
	for _, v := range r.URL.Query()["state"] {
		for _, s := range strings.Split(v, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if !slices.Contains(transferStates, s) {
				a.fail(w, newAPIError(400, "bad_request", "Unknown transfer state "+strconv.Quote(s)+"."))
				return
			}
			q.States = append(q.States, s)
		}
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 500 {
			a.fail(w, newAPIError(400, "bad_request", "limit must be 1 to 500."))
			return
		}
		q.Limit = n
	}
	page, err := a.opts.FS.Transfers(r.Context(), q)
	if err != nil {
		a.fail(w, err)
		return
	}
	if page.Items == nil {
		page.Items = []Transfer{}
	}
	writeJSON(w, 200, page)
}

func (a *Agent) hTransferRetry(w http.ResponseWriter, r *http.Request) {
	t, err := a.opts.FS.Retry(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 200, t)
}

func (a *Agent) hTransferCancel(w http.ResponseWriter, r *http.Request) {
	t, err := a.opts.FS.Cancel(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 200, t)
}

func (a *Agent) hBatchCancel(w http.ResponseWriter, r *http.Request) {
	b, err := a.opts.FS.CancelBatch(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 200, b)
}

func (a *Agent) hHeldDiscard(w http.ResponseWriter, r *http.Request) {
	if err := a.opts.FS.DiscardHeld(r.Context(), r.PathValue("id")); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"discarded": true})
}

func (a *Agent) hHeldReupload(w http.ResponseWriter, r *http.Request) {
	if err := a.opts.FS.ReuploadHeld(r.Context(), r.PathValue("id")); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"queued": true})
}

func (a *Agent) hHeldSaveCopy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DestDir string `json:"dest_dir"`
	}
	if err := decode(r, &body); err != nil {
		a.fail(w, err)
		return
	}
	dest := body.DestDir
	if dest == "" {
		d, err := a.opts.System.DownloadsDir()
		if err != nil {
			a.fail(w, fmt.Errorf("find downloads folder: %w", err))
			return
		}
		dest = d
	}
	if !filepath.IsAbs(dest) {
		a.fail(w, invalid("dest_dir", "dest_dir must be an absolute folder path."))
		return
	}
	if fi, err := os.Stat(dest); err != nil || !fi.IsDir() {
		a.fail(w, ErrLocalNotFound)
		return
	}
	path, err := a.opts.FS.SaveHeldCopy(r.Context(), r.PathValue("id"), dest)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"path": path})
}

func (a *Agent) hPause(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Minutes int `json:"minutes"`
	}
	if err := decode(r, &body); err != nil {
		a.fail(w, err)
		return
	}
	if body.Minutes < 0 || body.Minutes > 7*24*60 {
		a.fail(w, invalid("minutes", "minutes must be 0 to 10080."))
		return
	}
	if err := a.Pause(r.Context(), body.Minutes); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 200, a.State())
}

func (a *Agent) hResume(w http.ResponseWriter, r *http.Request) {
	if err := a.Resume(r.Context()); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 200, a.State())
}

func (a *Agent) hSend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LocalPath     string   `json:"local_path"`
		LocalPaths    []string `json:"local_paths"`
		DriveFolder   string   `json:"drive_folder"`
		DriveFolderID string   `json:"drive_folder_id"`
		OnConflict    string   `json:"on_conflict"`
		IncludeOnline bool     `json:"include_online_only"`
	}
	if err := decode(r, &body); err != nil {
		a.fail(w, err)
		return
	}
	if err := a.requireRunning(); err != nil {
		a.fail(w, err)
		return
	}
	paths, err := sendPaths(body.LocalPath, body.LocalPaths)
	if err != nil {
		a.fail(w, err)
		return
	}
	if body.DriveFolder == "" && body.DriveFolderID == "" {
		a.fail(w, invalid("drive_folder", "Give drive_folder or drive_folder_id."))
		return
	}
	switch body.OnConflict {
	case "", "hold", "new_version", "keep_both":
	default:
		a.fail(w, invalid("on_conflict", "on_conflict is hold, new_version or keep_both."))
		return
	}
	a.mu.Lock()
	ro := a.account != nil && a.account.ReadOnly
	a.mu.Unlock()
	if ro {
		a.fail(w, ErrReadOnly)
		return
	}
	res, err := a.opts.FS.Send(r.Context(), SendRequest{LocalPaths: paths, DriveFolder: body.DriveFolder, DriveFolderID: body.DriveFolderID,
		OnConflict: body.OnConflict, IncludeOnlineOnly: body.IncludeOnline})
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 202, res)
}

func (a *Agent) hSettingsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.settingsResponse())
}

func (a *Agent) hSettingsPatch(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		a.fail(w, newAPIError(400, "bad_request", "The request body could not be read."))
		return
	}
	res, err := a.PatchSettings(r.Context(), data)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (a *Agent) hMount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Letter string `json:"letter"`
	}
	if err := decode(r, &body); err != nil {
		a.fail(w, err)
		return
	}
	m, already, err := a.Mount(r.Context(), body.Letter)
	if err != nil {
		a.fail(w, err)
		return
	}
	status := 202
	if already {
		status = 200
	}
	writeJSON(w, status, map[string]Mount{"mount": m})
}

func (a *Agent) hUnmount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Force bool `json:"force"`
	}
	if err := decode(r, &body); err != nil {
		a.fail(w, err)
		return
	}
	m, err := a.Unmount(r.Context(), body.Force)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 202, map[string]Mount{"mount": m})
}

func (a *Agent) hUpdate(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.State().Update)
}

func (a *Agent) hUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if a.opts.Updater == nil {
		a.fail(w, newAPIError(409, "not_supported", "Updates are turned off in this build."))
		return
	}
	writeJSON(w, 200, a.CheckUpdate(r.Context()))
}

func (a *Agent) hUpdateApply(w http.ResponseWriter, r *http.Request) {
	var body struct {
		When string `json:"when"`
	}
	if err := decode(r, &body); err != nil {
		a.fail(w, err)
		return
	}
	switch body.When {
	case "":
		body.When = "idle"
	case "idle", "now":
	default:
		a.fail(w, invalid("when", "when must be idle or now."))
		return
	}
	u, err := a.ApplyUpdate(r.Context(), body.When)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 202, u)
}

func (a *Agent) hDriverInstall(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	if a.driver.State == "not_needed" {
		a.mu.Unlock()
		a.fail(w, newAPIError(409, "not_supported", "This computer needs no driver."))
		return
	}
	if a.driver.State == "restart_needed" {
		a.mu.Unlock()
		a.fail(w, newAPIError(409, "driver_restart_needed", restartNeededMessage))
		return
	}
	if a.driver.State != "installing" {
		a.driver.State, a.driver.Error, a.driver.Progress, a.driver.Download = "installing", nil, platform.DriverStepDownloading, nil
		runCtx := a.ctx
		go a.installDriver(runCtx)
	}
	d := a.driver
	a.mu.Unlock()
	a.emitState()
	writeJSON(w, 202, d)
}

func (a *Agent) installDriver(ctx context.Context) {
	err := a.opts.System.InstallDriver(ctx, func(p platform.DriverProgress) {
		a.mu.Lock()
		if a.driver.State == "installing" {
			a.driver.Progress, a.driver.Download = p.Step, nil
			if p.Step == platform.DriverStepDownloading {
				a.driver.Download = &Progress{BytesDone: p.Done, BytesTotal: p.Total}
			}
		}
		a.mu.Unlock()
		a.emitState()
	})
	a.mu.Lock()
	a.driver.State, a.driver.Progress, a.driver.Download = "", "", nil
	if errors.Is(err, platform.ErrRestartNeeded) {
		a.driverRestart = true
	}
	a.refreshDriverLocked()
	switch {
	case err == nil, errors.Is(err, platform.ErrRestartNeeded):
	case errors.Is(err, platform.ErrDeclined):
		a.driver.Error = &Problem{"declined", "The administrator prompt was declined."}
	case errors.Is(err, platform.ErrDriverUnavailable):
		a.driver.Error = &Problem{"download_failed", driverUnavailableMessage}
	default:
		a.driver.Error = &Problem{"install_failed", "The driver could not be installed: " + err.Error()}
	}
	wantMount := a.mountWanted && a.letterConfirmed && a.driver.State == "ok" && a.mount.State != MountMounted && a.phase == phaseRunning
	a.mu.Unlock()
	switch {
	case errors.Is(err, platform.ErrRestartNeeded):
		a.log.Info("driver installed; Windows needs a restart before it can be used")
	case err != nil:
		a.log.Warn("driver install failed", "err", err)
	}
	a.emitState()
	if wantMount {
		a.doMount(ctx)
	}
}

// driverUnavailableMessage is the driver error when its installer could not
// be downloaded or failed its checks.
const driverUnavailableMessage = "WinFsp could not be downloaded from its publisher on this PC (no connection, or the download is blocked), so nothing was installed. You can install WinFsp yourself from its download page, then choose Check again."

// hDriverCheck detects the driver again, for a person who installed it by
// hand. While an install runs it only reports.
func (a *Agent) hDriverCheck(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	if a.driver.State != "installing" {
		prev := a.driver.Error
		a.refreshDriverLocked()
		if (a.driver.State == "missing" || a.driver.State == "outdated") && prev != nil && prev.Code == "download_failed" {
			a.driver.Error = prev // still not there: the way forward is unchanged
		}
	}
	d := a.driver
	wantMount := a.mountWanted && a.letterConfirmed && a.driver.State == "ok" && a.mount.State != MountMounted && a.phase == phaseRunning
	a.mu.Unlock()
	a.emitState()
	if wantMount {
		go a.doMount(a.ctx)
	}
	writeJSON(w, 200, d)
}

func (a *Agent) hDiagnostics(w http.ResponseWriter, r *http.Request) {
	path, size, err := a.Diagnostics(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"path": path, "size": size})
}

func (a *Agent) hOpen(w http.ResponseWriter, r *http.Request) {
	var body OpenAction
	if err := decode(r, &body); err != nil {
		a.fail(w, err)
		return
	}
	ctx := r.Context()
	var err error
	switch body.Target {
	case "drive":
		a.mu.Lock()
		m := a.mount
		a.mu.Unlock()
		if m.State != MountMounted {
			a.fail(w, newAPIError(409, "not_mounted", "The drive is not connected."))
			return
		}
		local := drivePathToLocal(m.Path, body.Path)
		if fi, statErr := os.Stat(local); statErr == nil && !fi.IsDir() {
			local = filepath.Dir(local)
		}
		err = a.opts.System.OpenFolder(ctx, local)
	case "web":
		var u string
		u, err = a.opts.FS.WebURL(ctx, body.Path)
		if err == nil {
			err = a.opts.System.OpenURL(ctx, u)
		}
	case "folder":
		if !filepath.IsAbs(body.Path) {
			a.fail(w, invalid("path", "path must be an absolute folder path."))
			return
		}
		if fi, statErr := os.Stat(body.Path); statErr != nil || !fi.IsDir() {
			a.fail(w, ErrLocalNotFound)
			return
		}
		err = a.opts.System.OpenFolder(ctx, body.Path)
	case "logs":
		err = a.opts.System.OpenFolder(ctx, a.opts.Paths.Logs)
	case "ui":
		err = a.openUI(ctx, body.Path)
	default:
		a.fail(w, invalid("target", "target must be drive, web, folder, logs or ui."))
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Agent) hQuit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Unmount bool `json:"unmount"`
	}
	if err := decode(r, &body); err != nil {
		a.fail(w, err)
		return
	}
	a.mu.Lock()
	mounted := a.mount.State == MountMounted || a.mount.State == MountMounting
	a.mu.Unlock()
	if mounted && !body.Unmount {
		a.fail(w, newAPIError(409, "mounted", "Quitting disconnects the drive; confirm with unmount: true."))
		return
	}
	writeJSON(w, 202, map[string]bool{"quitting": true})
	a.log.Info("quit requested over ipc")
	a.requestExit(Exit{})
}

func (a *Agent) hEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		a.fail(w, errors.New("streaming not supported"))
		return
	}
	sub, err := a.hub.Subscribe(r.Header.Get("Last-Event-ID"))
	if err != nil {
		writeError(w, newAPIError(503, "busy", "Too many event streams are open."))
		return
	}
	defer a.hub.Unsubscribe(sub)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	ctx := r.Context()
	for {
		need, lagged, events, err := sub.Next(ctx, 15*time.Second)
		if err != nil {
			return
		}
		var b strings.Builder
		if !need && len(events) == 0 {
			b.WriteString(": ping\n\n")
		}
		if need {
			id := a.hub.CurrentID()
			data, _ := json.Marshal(a.snapshot(ctx, lagged))
			b.WriteString(formatSSE(id, "snapshot", data))
		}
		for _, ev := range events {
			b.WriteString(formatSSE(ev.ID, ev.Type, ev.Data))
		}
		if _, err := io.WriteString(w, b.String()); err != nil {
			return
		}
		fl.Flush()
	}
}

func (a *Agent) snapshot(ctx context.Context, lagged bool) Snapshot {
	s := Snapshot{State: a.State(), Active: []Transfer{}, Held: []Transfer{}, Lagged: lagged}
	if p, err := a.opts.FS.Transfers(ctx, TransferQuery{States: []string{TransferActive, TransferWaiting}, Limit: 50}); err == nil && p.Items != nil {
		s.Active = p.Items
	}
	if p, err := a.opts.FS.Transfers(ctx, TransferQuery{States: []string{TransferHeld}, Limit: 500}); err == nil && p.Items != nil {
		s.Held = p.Items
	}
	return s
}

// --- settings ---

func (a *Agent) settingsResponse() SettingsResponse {
	a.mu.Lock()
	s, d := a.settings, a.defaults
	pending := slices.Clone(a.pendingRestart)
	a.mu.Unlock()
	inUse, err := a.opts.System.DriveLettersInUse()
	if err != nil {
		a.log.Warn("could not list drive letters", "err", err)
	}
	letters, suggested := []string{}, ""
	if inUse != nil {
		a.mu.Lock()
		mounted := a.mount.State == MountMounted && a.mount.Letter != nil
		ours := ""
		if mounted {
			ours = *a.mount.Letter
		}
		a.mu.Unlock()
		delete(inUse, ours) // our own drive holds it
		letters = platform.FreeLetters(inUse)
		if ours != "" && !slices.Contains(letters, ours) {
			letters = append([]string{ours}, letters...)
		}
		suggested = platform.SuggestLetter(s.DriveLetter, inUse)
	} else {
		inUse = map[string]string{}
	}
	if pending == nil {
		pending = []string{}
	}
	return SettingsResponse{
		Settings: s, Defaults: d, PendingRestart: pending,
		Choices: SettingsChoices{DriveLetters: letters, LettersInUse: inUse, SuggestedLetter: suggested,
			UpdateChannels: []string{"stable", "beta"}},
	}
}

// PatchSettings applies a JSON merge patch, saves, and applies the effects.
func (a *Agent) PatchSettings(ctx context.Context, patch []byte) (SettingsResponse, error) {
	inUse, lerr := a.opts.System.DriveLettersInUse()
	if lerr != nil {
		a.log.Warn("could not list drive letters", "err", lerr)
	}
	a.mu.Lock()
	old := a.settings
	next, err := settings.Patch(old, patch)
	if err != nil {
		a.mu.Unlock()
		return SettingsResponse{}, err
	}
	changed := settings.Changed(old, next)
	has := func(prefix string) bool {
		return slices.ContainsFunc(changed, func(c string) bool { return c == prefix || strings.HasPrefix(c, prefix+".") })
	}
	// start_at_login sent unchanged while its entry is missing: the person
	// chose to turn it back on (the issue's action).
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(patch, &fields)
	_, sentAutostart := fields["start_at_login"]
	reapplyAutostart := has("start_at_login") || (sentAutostart && a.autostartMissing)
	if has("server_url") && a.opts.Server != "" {
		a.mu.Unlock()
		return SettingsResponse{}, &settings.FieldError{Field: "server_url", Message: "This build connects only to " + a.opts.Server + "."}
	}
	if has("server_url") && a.phase != phaseSignedOut {
		a.mu.Unlock()
		return SettingsResponse{}, newAPIError(409, "already_signed_in", "Sign out before changing the server.")
	}
	mounted := a.mount.State == MountMounted
	if kind := inUse[next.DriveLetter]; has("drive_letter") && kind != "" {
		a.mu.Unlock()
		return SettingsResponse{}, &settings.FieldError{Field: "drive_letter", Message: next.DriveLetter + " is already used on this PC by " + letterUser(kind) + "."}
	}
	if has("drive_letter") && mounted && a.opts.Mounter.OpenFiles() > 0 {
		a.mu.Unlock()
		return SettingsResponse{}, newAPIError(409, "busy", "Close the files open on the drive before changing its letter.")
	}
	if err := settings.Save(a.settingsPath, next); err != nil {
		a.mu.Unlock()
		return SettingsResponse{}, err
	}
	a.settings = next
	for _, f := range []string{"cache_dir", "device_name"} {
		if has(f) && a.phase != phaseSignedOut && !slices.Contains(a.pendingRestart, f) {
			a.pendingRestart = append(a.pendingRestart, f)
		}
	}
	if !mounted {
		a.mount.Letter = letterPtr(next.DriveLetter)
	}
	a.update.Channel = next.UpdateChannel
	runCtx := a.ctx
	a.mu.Unlock()

	if has("bandwidth") {
		a.opts.FS.SetBandwidth(next.Bandwidth)
	}
	if has("cache_cap_bytes") {
		a.opts.FS.SetCacheCap(next.CacheCapBytes)
	}
	if has("cache_release") {
		a.opts.FS.SetCacheRelease(next.CacheRelease)
	}
	if reapplyAutostart {
		if err := a.opts.System.SetAutostart(next.StartAtLogin); err != nil {
			a.log.Warn("could not change start at login", "err", err)
		}
		a.mu.Lock()
		a.checkAutostartLocked()
		a.mu.Unlock()
	}
	if has("update_channel") && a.opts.Updater != nil {
		go a.CheckUpdate(runCtx)
	}
	if has("drive_letter") && mounted {
		go a.remount(runCtx)
	}
	a.log.Info("settings changed", "fields", changed)
	a.hub.Publish("settings", next)
	a.emitState()
	return a.settingsResponse(), nil
}

func (a *Agent) remount(ctx context.Context) {
	if err := a.opts.Mounter.Unmount(ctx, false); err != nil {
		a.log.Warn("unmount for letter change failed", "err", err)
		return
	}
	a.mu.Lock()
	a.mount = Mount{State: MountUnmounted, Letter: letterPtr(a.settings.DriveLetter)}
	a.mu.Unlock()
	a.doMount(ctx)
}
