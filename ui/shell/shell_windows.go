package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
	"github.com/wailsapp/wails/v3/pkg/w32"

	"github.com/geyervalmont/olsyn-drive-client/ui/internal/agentconn"
	"github.com/geyervalmont/olsyn-drive-client/ui/internal/bridge"
	"github.com/geyervalmont/olsyn-drive-client/ui/internal/tray"
	"github.com/geyervalmont/olsyn-drive-client/ui/internal/winstate"
)

const (
	popoverWidth  = 380
	popoverHeight = 520
	// The main window opens at this size, centred, the first time; after
	// that where the person left it (winstate).
	mainWidth     = 980
	mainHeight    = 680
	mainMinWidth  = 760
	mainMinHeight = 520
)

// Window colours from ui/frontend/src/styles/tokens.css (--bg, --ink,
// --rule), so the frame and the page agree in both themes and a window never
// flashes the wrong colour while it loads.
var (
	lightBg, lightInk, lightRule = application.NewRGB(0xf5, 0xf3, 0xef), [3]uint8{0x21, 0x2e, 0x45}, [3]uint8{0xe6, 0xe2, 0xda}
	darkBg, darkInk, darkRule    = application.NewRGB(0x10, 0x16, 0x1f), [3]uint8{0xec, 0xee, 0xf2}, [3]uint8{0x26, 0x32, 0x47}
)

// titleBarTheme colours the title bar like the page: it follows the Windows
// app theme (light or dark), which is also what the page's
// prefers-color-scheme follows.
func titleBarTheme() application.ThemeSettings {
	theme := func(bg application.RGBA, ink, rule [3]uint8) *application.WindowTheme {
		return &application.WindowTheme{
			TitleBarColour:  application.NewRGBPtr(bg.Red, bg.Green, bg.Blue),
			TitleTextColour: application.NewRGBPtr(ink[0], ink[1], ink[2]),
			BorderColour:    application.NewRGBPtr(rule[0], rule[1], rule[2]),
		}
	}
	return application.ThemeSettings{
		DarkModeActive:    theme(darkBg, darkInk, darkRule),
		DarkModeInactive:  theme(darkBg, darkInk, darkRule),
		LightModeActive:   theme(lightBg, lightInk, lightRule),
		LightModeInactive: theme(lightBg, lightInk, lightRule),
	}
}

// background is the window colour for the current Windows app theme.
func background() application.RGBA {
	if w32.IsCurrentlyDarkMode() {
		return darkBg
	}
	return lightBg
}

// Run starts the desktop UI and blocks until the person quits it or ctx ends.
// Quitting the UI leaves the agent running: the drive keeps working.
func Run(ctx context.Context, opts Options) error {
	if opts.Assets == nil {
		return errors.New("shell: no front-end assets")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	dialer, err := agentconn.DefaultDialer()
	if err != nil {
		return fmt.Errorf("find the agent: %w", err)
	}
	agent := &agentconn.Agent{
		HTTP:      agentconn.NewHTTPClient(dialer),
		UserAgent: "olsyn-drive-ui/" + opts.Version,
		Log:       log,
	}
	if starter, err := agentconn.FindAgent(); err == nil {
		agent.Starter = starter
	} else {
		log.Warn("cannot start the agent if it stops", "err", err)
	}

	s := &shell{ctx: ctx, log: log, agent: agent, stateFile: opts.StateFile}
	s.bridge = bridge.New(ctx, agent, s, log)
	s.notifier = notifications.New()

	// The .exe's icon resource (ui/winres) is the window icon; this PNG is
	// the fallback for a build made without it (plain go build).
	icon, err := tray.AppIcon(256)
	if err != nil {
		return fmt.Errorf("draw the app icon: %w", err)
	}
	s.app = application.New(application.Options{
		Icon:        icon,
		Name:        "Olsyn Drive",
		Description: "Your Olsyn Drive on this PC",
		Logger:      libraryLogger(log),
		Services:    []application.Service{application.NewService(s.notifier)},
		Assets: application.AssetOptions{
			Handler:    application.AssetFileServerFS(opts.Assets),
			Middleware: s.bridge.Middleware,
		},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID:               AppID,
			OnSecondInstanceLaunch: s.secondInstance,
		},
		Windows: application.WindowsOptions{
			// Closing the main window hides it; the tray keeps running.
			DisableQuitOnLastWindowClosed: true,
		},
	})

	startRoute := ""
	if link, ok := bridge.LinkFromArgs(opts.Args); ok {
		startRoute = s.routeForLink(link)
	}
	s.createWindows(startRoute)
	s.createTray()

	s.notifier.OnNotificationResponse(s.notificationClicked)
	s.app.Event.OnApplicationEvent(events.Common.ApplicationLaunchedWithUrl, func(e *application.ApplicationEvent) {
		if route := s.routeForLink(e.Context().URL()); route != "" {
			s.showMain(route)
		}
	})
	s.app.OnShutdown(s.saveMainBounds)
	s.app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		s.restoreMainBounds()
		go s.watch()
		if !opts.Background || startRoute != "" {
			s.showMain(startRoute)
		}
	})

	go func() {
		<-ctx.Done()
		s.app.Quit()
	}()
	err = s.app.Run()
	s.bridge.CloseAll()
	if err != nil {
		return fmt.Errorf("desktop app: %w", err)
	}
	return nil
}

type shell struct {
	ctx      context.Context
	log      *slog.Logger
	agent    *agentconn.Agent
	bridge   *bridge.Bridge
	app      *application.App
	notifier *notifications.NotificationService

	main    *application.WebviewWindow
	popover *application.WebviewWindow
	tray    *application.SystemTray

	stateFile string

	mu       sync.Mutex
	lastView tray.View
	viewSet  bool
	paused   bool
	pauseItm *application.MenuItem
}

func pageURL(view, route string) string {
	u := "/?transport=bridge&view=" + view
	if route != "" {
		u += "#" + route
	}
	return u
}

func (s *shell) createWindows(startRoute string) {
	bg := background()

	s.main = s.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             "main",
		Title:            "Olsyn Drive",
		Width:            mainWidth,
		Height:           mainHeight,
		MinWidth:         mainMinWidth,
		MinHeight:        mainMinHeight,
		InitialPosition:  application.WindowCentered,
		URL:              pageURL("main", startRoute),
		Hidden:           true,
		EnableFileDrop:   true,
		BackgroundColour: bg,
		Windows: application.WindowsWindow{
			Theme:       application.SystemDefault,
			CustomTheme: titleBarTheme(),
		},
	})
	// Closing hides the window; quitting is in the tray menu.
	s.main.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		e.Cancel()
		s.saveMainBounds()
		s.main.Hide()
	})
	s.main.OnWindowEvent(events.Common.WindowFilesDropped, func(e *application.WindowEvent) {
		paths := e.Context().DroppedFiles()
		s.log.Info("files dropped on the main window", "count", len(paths))
		s.bridge.SendDrop("main", paths)
	})

	s.popover = s.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             "popover",
		Title:            "Olsyn Drive",
		Width:            popoverWidth,
		Height:           popoverHeight,
		URL:              pageURL("popover", ""),
		Hidden:           true,
		Frameless:        true,
		AlwaysOnTop:      true,
		DisableResize:    true,
		HideOnFocusLost:  true,
		HideOnEscape:     true,
		EnableFileDrop:   true,
		BackgroundColour: bg,
		Windows: application.WindowsWindow{
			HiddenOnTaskbar: true,
			Theme:           application.SystemDefault,
		},
	})
	// Dropping files on the popover opens the send dialog in the main window.
	s.popover.OnWindowEvent(events.Common.WindowFilesDropped, func(e *application.WindowEvent) {
		paths := e.Context().DroppedFiles()
		s.popover.Hide()
		s.showMain("transfers/active")
		s.bridge.SendDrop("main", paths)
	})
}

// restoreMainBounds puts the main window where it was last time, if that is
// still on a screen; otherwise it stays centred at its default size.
func (s *shell) restoreMainBounds() {
	if s.stateFile == "" {
		return
	}
	st, ok, err := winstate.Load(s.stateFile)
	if err != nil {
		s.log.Warn("ignored the saved window place", "err", err)
		return
	}
	if !ok {
		return
	}
	var areas []winstate.Rect
	for _, sc := range s.app.Screen.GetAll() {
		wa := sc.WorkArea
		areas = append(areas, winstate.Rect{X: wa.X, Y: wa.Y, Width: wa.Width, Height: wa.Height})
	}
	b, ok := winstate.Place(st, areas, mainMinWidth, mainMinHeight)
	if !ok {
		return
	}
	s.main.SetBounds(application.Rect{X: b.X, Y: b.Y, Width: b.Width, Height: b.Height})
	if st.Maximised {
		s.main.Maximise()
	}
}

// saveMainBounds remembers the main window's place (not while minimised or
// maximised: then the last normal bounds are kept and only the flag changes).
func (s *shell) saveMainBounds() {
	if s.stateFile == "" || s.main == nil || s.main.IsMinimised() {
		return
	}
	st, _, _ := winstate.Load(s.stateFile)
	st.Maximised = s.main.IsMaximised()
	if !st.Maximised {
		b := s.main.Bounds()
		if b.Width <= 0 || b.Height <= 0 {
			return
		}
		st.Bounds = winstate.Rect{X: b.X, Y: b.Y, Width: b.Width, Height: b.Height}
	}
	if err := winstate.Save(s.stateFile, st); err != nil {
		s.log.Warn("could not save the window place", "err", err)
	}
}

func (s *shell) createTray() {
	s.tray = s.app.SystemTray.New()
	s.applyView(tray.Down())
	s.tray.AttachWindow(s.popover).WindowOffset(8)

	menu := application.NewMenu()
	menu.Add("Open Olsyn Drive").OnClick(func(*application.Context) { s.showMain("") })
	menu.Add("Open the drive in File Explorer").OnClick(func(*application.Context) {
		s.openTarget(agentconn.OpenTarget{Target: "drive", Path: "/"})
	})
	menu.Add("Open Olsyn Drive on the web").OnClick(func(*application.Context) {
		s.openTarget(agentconn.OpenTarget{Target: "web"})
	})
	menu.AddSeparator()
	s.pauseItm = menu.Add("Pause transfers").OnClick(func(*application.Context) { s.togglePause() })
	menu.Add("Settings").OnClick(func(*application.Context) { s.showMain("settings") })
	menu.AddSeparator()
	// Quit asks first in the main window (it explains that the drive keeps working).
	menu.Add("Quit Olsyn Drive").OnClick(func(*application.Context) { s.showMain("quit") })
	s.tray.SetMenu(menu)
}

// watch follows the agent for the tray icon, tooltip and toasts.
func (s *shell) watch() {
	s.agent.Watch(s.ctx, agentconn.WatchHandlers{
		State: func(st agentconn.State) {
			s.mu.Lock()
			s.paused = st.Paused
			s.mu.Unlock()
			s.applyView(tray.FromState(st))
		},
		Notification: s.notify,
		Down: func(err error) {
			s.log.Warn("agent not answering", "err", err)
			s.applyView(tray.Down())
		},
	})
}

// applyView redraws the tray only when what it shows has changed.
func (s *shell) applyView(v tray.View) {
	s.mu.Lock()
	same := s.viewSet && v.Kind == s.lastView.Kind && tray.Bucket(v.Progress) == tray.Bucket(s.lastView.Progress)
	tipSame := s.viewSet && v.Tooltip == s.lastView.Tooltip
	s.lastView, s.viewSet = v, true
	paused := s.paused
	s.mu.Unlock()

	if !same {
		light, err1 := tray.Icon(v, tray.LightTaskbar, 32)
		dark, err2 := tray.Icon(v, tray.DarkTaskbar, 32)
		if err := errors.Join(err1, err2); err != nil {
			s.log.Error("draw tray icon", "err", err)
		} else {
			s.tray.SetIcon(light)
			s.tray.SetDarkModeIcon(dark)
		}
	}
	if !tipSame {
		s.tray.SetTooltip(v.Tooltip)
	}
	if s.pauseItm != nil {
		label := "Pause transfers"
		if paused {
			label = "Resume transfers"
		}
		s.pauseItm.SetLabel(label)
	}
}

func (s *shell) togglePause() {
	s.mu.Lock()
	paused := s.paused
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	var err error
	if paused {
		err = s.agent.Resume(ctx)
	} else {
		err = s.agent.Pause(ctx)
	}
	if err != nil {
		s.log.Warn("pause or resume failed", "err", err)
		s.showMain("transfers/active")
	}
}

func (s *shell) notify(n agentconn.Notification) {
	data := map[string]any{"kind": n.Kind}
	if n.Action != nil && len(n.Action.Open) > 0 {
		data["open"] = string(n.Action.Open)
	}
	err := s.notifier.SendNotification(notifications.NotificationOptions{
		ID:    n.ID,
		Title: n.Title,
		Body:  n.Body,
		Data:  data,
	})
	if err != nil {
		s.log.Warn("show notification", "kind", n.Kind, "err", err)
	}
}

func (s *shell) notificationClicked(result notifications.NotificationResult) {
	if result.Error != nil {
		s.log.Warn("notification response", "err", result.Error)
		return
	}
	n := agentconn.Notification{ID: result.Response.ID}
	if kind, ok := result.Response.UserInfo["kind"].(string); ok {
		n.Kind = kind
	}
	if open, ok := result.Response.UserInfo["open"].(string); ok && open != "" {
		n.Action = &struct {
			Label string          `json:"label"`
			Open  json.RawMessage `json:"open"`
		}{Open: json.RawMessage(open)}
	}
	click := tray.ClickFor(n)
	if click.Open != nil {
		s.openTarget(*click.Open)
		return
	}
	s.showMain(click.Route)
}

func (s *shell) openTarget(t agentconn.OpenTarget) {
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	if err := s.agent.Open(ctx, t); err != nil {
		s.log.Warn("open failed", "target", t.Target, "err", err)
		s.showMain("")
	}
}

// showMain brings the main window forward, optionally at a route.
func (s *shell) showMain(route string) {
	if s.popover != nil {
		s.popover.Hide()
	}
	if route != "" {
		s.bridge.SendRoute("main", route)
	}
	s.main.Show()
	s.main.Restore()
	s.main.Focus()
}

func (s *shell) secondInstance(data application.SecondInstanceData) {
	route := ""
	if link, ok := bridge.LinkFromArgs(data.Args); ok {
		route = s.routeForLink(link)
		if route == "" {
			return // an "open" link was handed to the agent
		}
	}
	s.showMain(route)
}

// routeForLink handles an olsyn-drive:// link: it returns the UI route to
// show, or "" after handing an "open" link to the agent.
func (s *shell) routeForLink(link string) string {
	action, err := bridge.ParseLink(link)
	if err != nil {
		s.log.Warn("ignored a link", "err", err)
		return ""
	}
	if action.Kind == "open" {
		go s.openTarget(agentconn.OpenTarget{Target: "drive", Path: action.DrivePath})
		return ""
	}
	return action.Route
}

// Host implementation for the bridge.

func (s *shell) window(name string) (*application.WebviewWindow, error) {
	switch name {
	case "main":
		return s.main, nil
	case "popover":
		return s.popover, nil
	}
	return nil, bridge.ErrNoWindow
}

// Push hands a message to a page. json.Marshal escapes <, >, & and U+2028/9,
// so the JSON is safe to embed as a JavaScript expression.
func (s *shell) Push(window string, message []byte) {
	w, err := s.window(window)
	if err != nil || w == nil {
		return
	}
	w.ExecJS("window.__olsynBridge && window.__olsynBridge.receive(" + string(message) + ")")
}

func (s *shell) PickFolder(ctx context.Context, title string) (string, error) {
	type result struct {
		path string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		dlg := s.app.Dialog.OpenFile().
			CanChooseDirectories(true).
			CanChooseFiles(false).
			CanCreateDirectories(true).
			SetTitle(title)
		if s.main != nil && s.main.IsVisible() {
			dlg = dlg.AttachToWindow(s.main)
		}
		p, err := dlg.PromptForSingleSelection()
		done <- result{p, err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-done:
		if r.err != nil {
			return "", fmt.Errorf("folder picker: %w", r.err)
		}
		return r.path, nil
	}
}

func (s *shell) WindowAction(window, action, route string) error {
	switch action {
	case "show-main":
		s.showMain(route)
	case "hide-popover":
		s.popover.Hide()
	case "quit-ui":
		s.app.Quit()
	default:
		return fmt.Errorf("unknown window action %q", action)
	}
	return nil
}

func (s *shell) OpenURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return errors.New("only https links can be opened")
	}
	if err := s.app.Browser.OpenURL(u.String()); err != nil {
		return fmt.Errorf("open browser: %w", err)
	}
	return nil
}

var _ bridge.Host = (*shell)(nil)
var _ http.Handler = (*bridge.Bridge)(nil)
