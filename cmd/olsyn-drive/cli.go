package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/agent"
	"github.com/geyervalmont/olsyn-drive-client/internal/links"
	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
)

func newFlags(e *env, name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	return fs
}

func cmdVersion(e *env, args []string) int {
	e.printf("olsyn-drive %s (%s, ipc v%d)\n", version, platform.Name(), agent.APIVersion)
	return exitOK
}

func cmdStatus(e *env, args []string) int {
	fs := newFlags(e, "status")
	asJSON := fs.Bool("json", false, "print the agent's state as JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	defer cancel()
	st, err := e.client().State(ctx)
	if err != nil {
		if *asJSON && isNoAgent(err) {
			e.printf("{\"agent\":\"not_running\"}\n")
			return exitNoAgent
		}
		return e.report(err)
	}
	if *asJSON {
		enc := json.NewEncoder(e.stdout)
		enc.SetIndent("", "  ")
		enc.Encode(st)
		return exitOK
	}
	io.WriteString(e.stdout, formatStatus(st))
	return exitOK
}

var connectionWords = map[string]string{
	agent.ConnSignedOut:        "Signed out",
	agent.ConnLinking:          "Signing in (waiting for the browser)",
	agent.ConnStarting:         "Starting",
	agent.ConnOffline:          "Offline: the drive cannot be reached",
	agent.ConnServerRestarting: "The drive is restarting; waiting",
	agent.ConnAttention:        "Needs attention",
	agent.ConnPaused:           "Paused",
	agent.ConnWorking:          "Uploading",
	agent.ConnConnected:        "Connected. All changes are uploaded",
}

// formatStatus is the human summary of State.
func formatStatus(st agent.State) string {
	var b strings.Builder
	row := func(label, value string) { fmt.Fprintf(&b, "%-10s %s\n", label+":", value) }
	fmt.Fprintf(&b, "Olsyn Drive %s (%s)\n", st.Version, st.Platform)
	state := connectionWords[st.Connection]
	if state == "" {
		state = st.Connection
	}
	if st.Reason != nil && st.Connection == agent.ConnSignedOut {
		state += " (" + st.Reason.Message + ")"
	}
	row("State", state)
	if st.SignIn != nil && st.SignIn.State == "waiting" {
		row("Code", st.SignIn.UserCode+" at "+st.SignIn.VerificationURL)
	}
	if st.Account != nil {
		host := st.Account.Server
		if u, err := url.Parse(host); err == nil && u.Host != "" {
			host = u.Host
		}
		who := st.Account.Name
		if st.Account.Email != "" {
			who += " <" + st.Account.Email + ">"
		}
		if st.Account.ReadOnly {
			who += " (read-only)"
		}
		row("Account", strings.TrimSpace(who)+" on "+host)
	}
	row("Computer", st.DeviceName)
	drive := st.Mount.State
	if st.Mount.Letter != nil {
		drive = *st.Mount.Letter + " " + drive
	} else if st.Mount.Path != "" {
		drive = st.Mount.Path + " " + drive
	}
	if st.Mount.Error != nil {
		drive += ": " + st.Mount.Error.Message
	}
	row("Drive", drive)
	t := st.Transfers
	if t.Active+t.Waiting+t.Held > 0 {
		line := fmt.Sprintf("%d active, %d waiting, %d held", t.Active, t.Waiting, t.Held)
		if t.BytesTotal > 0 {
			line += fmt.Sprintf("; %s of %s", humanBytes(t.BytesDone), humanBytes(t.BytesTotal))
		}
		if t.UpBPS > 0 {
			line += fmt.Sprintf(", %s/s up", humanBytes(t.UpBPS))
		}
		if t.DownBPS > 0 {
			line += fmt.Sprintf(", %s/s down", humanBytes(t.DownBPS))
		}
		if t.ETASeconds != nil {
			line += ", about " + (time.Duration(*t.ETASeconds) * time.Second).String() + " left"
		}
		row("Transfers", line)
	} else {
		row("Transfers", "none")
	}
	if c := st.Cache; c.UsedBytes > 0 {
		row("This PC", fmt.Sprintf("%s kept (%s waiting to upload, %s just uploaded, %s recently opened); files live on the drive",
			humanBytes(c.UsedBytes), humanBytes(c.UnsentBytes), humanBytes(c.UploadedBytes), humanBytes(c.OpenedBytes)))
	}
	if st.Paused && st.PausedUntil != nil {
		row("Paused", "until "+st.PausedUntil.Local().Format("15:04"))
	}
	u := st.Update
	switch {
	case u.Available != nil:
		row("Update", fmt.Sprintf("%s available (%s)", *u.Available, u.State))
	case u.Error != nil:
		row("Update", u.Error.Message)
	default:
		row("Update", strings.ReplaceAll(u.State, "_", " "))
	}
	switch st.Driver.State {
	case "not_needed", "":
	case "ok":
		row("Driver", "WinFsp "+st.Driver.Version)
	default:
		row("Driver", "WinFsp "+st.Driver.State)
	}
	for i, is := range st.Issues {
		label := ""
		if i == 0 {
			label = "Issues:"
		}
		fmt.Fprintf(&b, "%-10s - %s\n", label, is.Message)
	}
	return b.String()
}

func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

func cmdLogin(e *env, args []string) int {
	fs := newFlags(e, "login")
	noBrowser := fs.Bool("no-browser", false, "print the link instead of opening the browser")
	letter := fs.String("letter", "", "drive letter to connect after signing in (default: W:, or the next free one)")
	noMount := fs.Bool("no-mount", false, "sign in only; choose the drive letter later in the app")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	c, err := ensureAgent(e)
	if err != nil {
		return e.report(err)
	}
	var si agent.SignIn
	if err := c.Do(e.ctx, http.MethodPost, "/v1/signin", map[string]bool{"open_browser": !*noBrowser}, &si); err != nil {
		return e.report(err)
	}
	e.printf("To sign in, open %s\nand check that it shows the code %s\n", si.VerificationURL, si.UserCode)
	if !*noBrowser {
		e.printf("(the browser should have opened it)\n")
	}
	e.printf("Waiting for approval... (Ctrl-C to cancel)\n")
	outcome, reason, err := waitSignIn(e.ctx, si.ExpiresAt, c.Events)
	if e.ctx.Err() != nil {
		c.Do(context.WithoutCancel(e.ctx), http.MethodPost, "/v1/signin/cancel", nil, nil)
		e.errorf("Sign-in cancelled.\n")
		return exitError
	}
	if err != nil {
		return e.report(err)
	}
	switch outcome {
	case "approved":
		st, _ := c.State(e.ctx)
		if st.Account != nil {
			who := cmp.Or(st.Account.Email, st.Account.Name, "your account")
			e.printf("Signed in as %s.\n", who)
		}
		if *noMount || st.Mount.LetterConfirmed {
			return exitOK
		}
		return connectLetter(e, c, *letter)
	case "denied":
		e.errorf("Sign-in was declined in the browser.\n")
	case "expired":
		e.errorf("The code expired. Run \"olsyn-drive login\" again.\n")
	case "timeout":
		e.errorf("Sign-in did not finish before the code expired. Run \"olsyn-drive login\" again.\n")
	default:
		if reason != "" {
			e.errorf("Sign-in did not finish: %s\n", reason)
		} else {
			e.errorf("Sign-in did not finish (%s).\n", outcome)
		}
	}
	return exitError
}

// connectLetter confirms the drive letter after a first sign-in from the
// command line (the agent does not mount before that): the one asked for,
// or the configured one when it is free, or the next free one.
func connectLetter(e *env, c *agent.Client, want string) int {
	var set agent.SettingsResponse
	if err := c.Do(e.ctx, http.MethodGet, "/v1/settings", nil, &set); err != nil {
		return e.report(err)
	}
	hasLetters := len(set.Choices.DriveLetters) > 0 || len(set.Choices.LettersInUse) > 0 // false off Windows
	if want == "" && hasLetters {
		want = set.Choices.SuggestedLetter
		if want == "" {
			e.errorf("Every drive letter is in use on this PC: free one, then run \"olsyn-drive login --letter X:\".\n")
			return exitError
		}
		if cfg := set.Settings.DriveLetter; want != "" && want != cfg {
			e.printf("%s is in use on this PC (%s), so the drive uses %s.\n", cfg, set.Choices.LettersInUse[cfg], want)
		}
	}
	var body any
	if want != "" {
		body = map[string]string{"letter": want}
	}
	if err := c.Do(e.ctx, http.MethodPost, "/v1/mount", body, nil); err != nil {
		var ae *agent.APIError
		if errors.As(err, &ae) && (ae.Code == "driver_missing" || ae.Code == "driver_outdated") {
			e.printf("The drive letter needs the WinFsp driver: open Olsyn Drive to install it and choose the letter.\n")
			return exitOK
		}
		return e.report(err)
	}
	if want != "" {
		e.printf("Connecting the drive as %s.\n", want)
	}
	return exitOK
}

// signInGrace is how long login waits past the code's expiry for the agent
// to report the outcome.
const signInGrace = 30 * time.Second

// waitSignIn follows the agent's events until the sign-in started by login
// ends: "approved", or the link's final state ("denied", "expired",
// "cancelled", "failed") with the agent's reason. It never waits past the
// code's expiry plus signInGrace ("timeout"), so a lost update cannot hang it.
func waitSignIn(ctx context.Context, expires time.Time, events func(context.Context, string, func(agent.StreamEvent) error) error) (outcome, reason string, err error) {
	waitCtx := ctx
	if !expires.IsZero() {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithDeadline(ctx, expires.Add(signInGrace))
		defer cancel()
	}
	ended := func(s *agent.SignIn) bool {
		if s == nil || s.State == "waiting" || s.State == "approved" {
			return false
		}
		outcome = s.State
		if s.Error != nil {
			reason = s.Error.Message
		}
		return true
	}
	err = events(waitCtx, "", func(ev agent.StreamEvent) error {
		var st *agent.State
		switch ev.Type {
		case "snapshot":
			var s agent.Snapshot
			if json.Unmarshal(ev.Data, &s) == nil {
				st = &s.State
			}
		case "state":
			var s agent.State
			if json.Unmarshal(ev.Data, &s) == nil {
				st = &s
			}
		case "signin":
			var s agent.SignIn
			if json.Unmarshal(ev.Data, &s) == nil && ended(&s) {
				return agent.ErrStopStream
			}
		}
		if st == nil {
			return nil
		}
		if ended(st.SignIn) {
			return agent.ErrStopStream
		}
		if st.Account != nil && st.Connection != agent.ConnLinking {
			outcome = "approved"
			return agent.ErrStopStream
		}
		return nil
	})
	if errors.Is(err, agent.ErrStopStream) {
		err = nil
	}
	if outcome == "" && ctx.Err() == nil && waitCtx.Err() != nil {
		return "timeout", "", nil
	}
	return outcome, reason, err
}

func cmdLogout(e *env, args []string) int {
	fs := newFlags(e, "logout")
	clearCache := fs.Bool("clear-cache", false, "also delete the local cache")
	yes := fs.Bool("yes", false, "confirm losing files that have not reached the drive")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	c := e.client()
	body := map[string]bool{"clear_cache": *clearCache, "discard_unsent_confirmed": *yes}
	var res agent.SignOutResult
	err := c.Do(e.ctx, http.MethodPost, "/v1/signout", body, &res)
	var ae *agent.APIError
	if errors.As(err, &ae) && ae.Code == "unsent_data" {
		e.errorf("%s Clearing the cache deletes them.\n", ae.Message)
		if !confirm(e, "Delete them and sign out? [y/N] ") {
			e.errorf("Nothing changed. Sign out without --clear-cache to keep them.\n")
			return exitNeedsInput
		}
		body["discard_unsent_confirmed"] = true
		err = c.Do(e.ctx, http.MethodPost, "/v1/signout", body, &res)
	}
	if err != nil {
		return e.report(err)
	}
	e.printf("Signed out.\n")
	if res.UnlinkError != nil {
		e.printf("%s\n", *res.UnlinkError)
	}
	if res.UnsentKept != nil {
		e.printf("%d files not yet uploaded stay on this PC and upload if you sign in again.\n", res.UnsentKept.Files)
	}
	if res.CacheCleared {
		e.printf("The local cache was deleted.\n")
	}
	return exitOK
}

// confirm asks on stdin when it is a terminal-like reader.
func confirm(e *env, prompt string) bool {
	if f, ok := e.stdin.(*os.File); ok {
		if fi, err := f.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			return false
		}
	}
	e.errorf("%s", prompt)
	line, _ := bufio.NewReader(e.stdin).ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "y") || strings.EqualFold(strings.TrimSpace(line), "yes")
}

func cmdPause(e *env, args []string) int {
	fs := newFlags(e, "pause")
	minutes := fs.Int("minutes", 0, "resume by itself after this many minutes (0 = until resumed)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	c, err := ensureAgent(e)
	if err != nil {
		return e.report(err)
	}
	if err := c.Do(e.ctx, http.MethodPost, "/v1/pause", map[string]int{"minutes": *minutes}, nil); err != nil {
		return e.report(err)
	}
	e.printf("Transfers paused.\n")
	return exitOK
}

func cmdResume(e *env, args []string) int {
	c, err := ensureAgent(e)
	if err != nil {
		return e.report(err)
	}
	if err := c.Do(e.ctx, http.MethodPost, "/v1/resume", nil, nil); err != nil {
		return e.report(err)
	}
	e.printf("Transfers resumed.\n")
	return exitOK
}

func cmdSend(e *env, args []string) int {
	fs := newFlags(e, "send")
	to := fs.String("to", "", "drive folder to send into, e.g. /Projects/Tower")
	wait := fs.Bool("wait", false, "wait until all changes are uploaded, showing progress")
	onConflict := fs.String("on-conflict", "hold", "when the drive has a different file of that name: hold, new-version or keep-both")
	online := fs.Bool("include-online-only", false, "read cloud online-only files (makes OneDrive download them)")
	// Paths may come before, between or after the flags.
	var paths []string
	for {
		if err := fs.Parse(args); err != nil {
			return exitUsage
		}
		if fs.NArg() == 0 {
			break
		}
		paths = append(paths, fs.Arg(0))
		args = fs.Args()[1:]
	}
	conflict := map[string]string{"hold": "hold", "new-version": "new_version", "keep-both": "keep_both"}[*onConflict]
	if len(paths) == 0 || *to == "" || conflict == "" {
		e.errorf("usage: olsyn-drive send <path>... --to <drive folder> [--wait] [--on-conflict hold|new-version|keep-both] [--include-online-only]\n")
		return exitUsage
	}
	for i, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return e.report(err)
		}
		paths[i] = abs
	}
	c, err := ensureAgent(e)
	if err != nil {
		return e.report(err)
	}
	// Subscribe first so no batch event is missed between the POST and the stream.
	type outcome struct {
		b   agent.Batch
		err error
	}
	done := make(chan outcome, 1)
	var batchID string
	var idMu sync.Mutex
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	if *wait {
		var once sync.Once
		go func() {
			err := c.Events(ctx, "", func(ev agent.StreamEvent) error {
				once.Do(func() { close(ready) })
				if ev.Type != "batch" {
					return nil
				}
				var b agent.Batch
				if json.Unmarshal(ev.Data, &b) != nil {
					return nil
				}
				idMu.Lock()
				mine := b.ID == batchID
				idMu.Unlock()
				if !mine {
					return nil
				}
				fmt.Fprintf(e.stdout, "\r%s of %s, %d of %d files   ", humanBytes(b.BytesDone), humanBytes(b.Bytes),
					b.FilesDone+b.FilesSkipped+b.FilesHeld+b.FilesFailed+b.NotLocal, b.Files)
				if b.FinishedAt != nil {
					done <- outcome{b: b}
					return agent.ErrStopStream
				}
				return nil
			})
			once.Do(func() { close(ready) })
			select {
			case done <- outcome{err: err}:
			default:
			}
		}()
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
		}
	}
	idMu.Lock()
	var res agent.SendResult
	body := map[string]any{"local_paths": paths, "drive_folder": *to, "on_conflict": conflict, "include_online_only": *online}
	err = c.Do(e.ctx, http.MethodPost, "/v1/send", body, &res)
	batchID = res.BatchID
	idMu.Unlock()
	if err != nil {
		return e.report(err)
	}
	e.printf("Sending %d files (%s) to %s. Your files stay where they are.\n", res.Files, humanBytes(res.Bytes), *to)
	if !*wait {
		return exitOK
	}
	o := <-done
	e.printf("\n")
	if o.err != nil && !errors.Is(o.err, agent.ErrStopStream) {
		return e.report(o.err)
	}
	b := o.b
	e.printf("%d uploaded, %d already there or left out, %d held, %d failed, %d online-only (not downloaded).\n",
		b.FilesDone, b.FilesSkipped, b.FilesHeld, b.FilesFailed, b.NotLocal)
	switch {
	case b.State == agent.BatchFailed:
		msg := "the send stopped"
		if b.Error != nil {
			msg = b.Error.Message
		}
		e.errorf("olsyn-drive: %s\n", msg)
		return exitError
	case b.FilesHeld+b.FilesFailed+b.NotLocal > 0:
		e.errorf("Some files were not uploaded: run \"olsyn-drive status\" or open the app's Transfers for the reasons.\n")
		return exitError
	}
	return exitOK
}

func cmdDiagnose(e *env, args []string) int {
	c, err := ensureAgent(e)
	if err != nil {
		return e.report(err)
	}
	var out struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	if err := c.Do(e.ctx, http.MethodPost, "/v1/diagnostics", nil, &out); err != nil {
		return e.report(err)
	}
	e.printf("Diagnostics bundle (%s, names and secrets removed):\n%s\n", humanBytes(out.Size), out.Path)
	return exitOK
}

// cmdOpenURL handles olsyn-drive:// links registered with the OS
// (internal/links): a screen opens the desktop app there (a running app takes
// the link), a drive or web link is handed to the agent.
func cmdOpenURL(e *env, args []string) int {
	if len(args) != 1 {
		return exitUsage
	}
	action, err := parseOpenURL(args[0])
	if err != nil {
		e.errorf("olsyn-drive: %v\n", err)
		return exitUsage
	}
	if action.Target == "ui" && hasUI {
		return runUI(e, []string{links.ScreenLink(action.Path)})
	}
	c, err := ensureAgent(e)
	if err != nil {
		return e.report(err)
	}
	if err := c.Do(e.ctx, http.MethodPost, "/v1/open", action, nil); err != nil {
		return e.report(err)
	}
	return exitOK
}

// parseOpenURL turns an olsyn-drive:// link into the agent's POST /v1/open body.
func parseOpenURL(raw string) (agent.OpenAction, error) {
	a, err := links.Parse(raw)
	if err != nil {
		return agent.OpenAction{}, err
	}
	switch a.Kind {
	case links.KindDrive:
		return agent.OpenAction{Target: "drive", Path: a.Path}, nil
	case links.KindWeb:
		return agent.OpenAction{Target: "web", Path: a.Path}, nil
	}
	return agent.OpenAction{Target: "ui", Path: a.Screen}, nil
}

// cmdOpen brings the desktop app to the front on a screen, through the
// agent's POST /v1/open (the same way notifications and links go):
//
//	olsyn-drive open transfers [active|waiting|held|done|skipped]   (or transfers/held, or --filter held)
//	olsyn-drive open activity | settings | account | about | popover
func cmdOpen(e *env, args []string) int {
	var names []string
	filter := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--filter" || a == "-filter":
			if i+1 >= len(args) {
				e.errorf("olsyn-drive: --filter needs a value\n")
				return exitUsage
			}
			i++
			filter = args[i]
		case strings.HasPrefix(a, "--filter=") || strings.HasPrefix(a, "-filter="):
			_, filter, _ = strings.Cut(a, "=")
		case strings.HasPrefix(a, "-"):
			e.errorf("olsyn-drive: open: unknown option %s\n", a)
			return exitUsage
		default:
			names = append(names, a)
		}
	}
	if len(names) == 0 || len(names) > 2 {
		e.errorf("usage: olsyn-drive open <screen>\n  screens: transfers [active|waiting|held|done|skipped], activity, settings, account, about, popover\n")
		return exitUsage
	}
	name := strings.Join(names, "/")
	if filter != "" {
		name += "/" + filter
	}
	screen, err := links.Screen(name)
	if err != nil {
		e.errorf("olsyn-drive: %v\n", err)
		return exitUsage
	}
	c, err := ensureAgent(e)
	if err != nil {
		return e.report(err)
	}
	if err := c.Do(e.ctx, http.MethodPost, "/v1/open", agent.OpenAction{Target: "ui", Path: screen}, nil); err != nil {
		return e.report(err)
	}
	e.printf("Showing %s.\n", screen)
	return exitOK
}
