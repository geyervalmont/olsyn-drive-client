package agent

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
)

type harness struct {
	t       *testing.T
	agent   *Agent
	client  *Client
	drive   *fakeDrive
	fs      *fakeFS
	mounter *fakeMounter
	system  *fakeSystem
	creds   *platform.MemoryCredentialStore
	// store replaces creds as the agent's store when set.
	store  platform.CredentialStore
	paths  platform.Paths
	cancel context.CancelFunc
	done   chan Exit
}

// newHarness runs an agent on a real unix socket in a short temp dir (the
// socket path limit on macOS is 104 bytes). prepare may seed files and fakes.
func newHarness(t *testing.T, prepare func(h *harness)) *harness {
	t.Helper()
	dir, err := os.MkdirTemp("", "oda-")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		t: t, drive: newFakeDrive(), fs: newFakeFS(), mounter: &fakeMounter{},
		system: &fakeSystem{driver: platform.DriverInfo{Name: "winfsp", Needed: true, Installed: true, Version: "2.2.25089"}},
		creds:  platform.NewMemoryCredentialStore(), paths: platform.Under(dir), done: make(chan Exit, 1),
	}
	if err := h.paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	if prepare != nil {
		prepare(h)
	}
	ep, err := platform.Endpoint(h.paths)
	if err != nil {
		t.Fatal(err)
	}
	l, err := platform.Listen(ep)
	if err != nil {
		t.Fatal(err)
	}
	var store platform.CredentialStore = h.creds
	if h.store != nil {
		store = h.store
	}
	a, err := New(Options{
		Version: "1.0.0", Platform: "windows-x64", Paths: h.paths, Listener: l,
		DriveFor: func(string) Drive { return h.drive }, FS: h.fs, Mounter: h.mounter, Creds: store, System: h.system,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		DeviceName:        "TEST-PC",
		HeartbeatInterval: 30 * time.Millisecond,
		MinLinkInterval:   time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.agent = a
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		exit, _ := a.Run(ctx)
		h.done <- exit
	}()
	h.client = NewClient(func(ctx context.Context) (net.Conn, error) { return platform.Dial(ctx, ep) })
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			t.Error("agent did not stop")
		}
		os.RemoveAll(dir)
	})
	return h
}

func (h *harness) waitState(what string, cond func(State) bool) State {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last State
	for time.Now().Before(deadline) {
		s, err := h.client.State(context.Background())
		if err == nil {
			last = s
			if cond(s) {
				return s
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s; last state: connection=%s reason=%+v signin=%+v mount=%+v",
		what, last.Connection, last.Reason, last.SignIn, last.Mount)
	return last
}

func (h *harness) post(path string, body, out any) error {
	return h.client.Do(context.Background(), http.MethodPost, path, body, out)
}

// seedAccount makes the agent start already signed in.
func seedAccount(h *harness) {
	acc := Account{Server: "https://drive.olsyn.com", UserID: "user-1", Name: "Test Person", Email: "person@example.test", DeviceID: "dev-1"}
	data, _ := json.Marshal(acc)
	os.WriteFile(filepath.Join(h.paths.Config, "account.json"), data, 0o600)
	h.creds.Set("Olsyn Drive/drive.olsyn.com", []byte("odt_synthetic_token_value"))
}

// collectEvents subscribes and records events until the test ends.
type collector struct {
	mu     sync.Mutex
	events []StreamEvent
}

func (c *collector) all() []StreamEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]StreamEvent(nil), c.events...)
}

func (h *harness) collect() *collector {
	c := &collector{}
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	ready := make(chan struct{})
	var once sync.Once
	go h.client.Events(ctx, "", func(ev StreamEvent) error {
		c.mu.Lock()
		c.events = append(c.events, ev)
		c.mu.Unlock()
		once.Do(func() { close(ready) })
		return nil
	})
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		h.t.Fatal("no snapshot event")
	}
	return c
}

func TestSignInHappyPath(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.drive.polls = []error{ErrAuthorizationPending, ErrAuthorizationPending, nil}
	})
	h.waitState("signed out", func(s State) bool { return s.Connection == ConnSignedOut && s.Ready })
	ev := h.collect()

	var si SignIn
	if err := h.post("/v1/signin", map[string]bool{"open_browser": true}, &si); err != nil {
		t.Fatal(err)
	}
	if si.State != "waiting" || si.UserCode != "BCDF-GHJK" || si.VerificationURL == "" {
		t.Fatalf("signin = %+v", si)
	}
	// first run: signed in, but the drive waits for the person to choose
	// its letter
	s := h.waitState("connected, letter not chosen", func(s State) bool {
		return s.Connection == ConnConnected && s.Ready && s.Account != nil
	})
	if s.Mount.State != MountUnmounted || s.Mount.LetterConfirmed {
		t.Fatalf("mounted before the letter was chosen: %+v", s.Mount)
	}
	// W: is a remembered network mapping on this PC: not offered, X: suggested
	h.system.mu.Lock()
	h.system.lettersInUse = map[string]string{"D:": platform.LetterVolume, "W:": platform.LetterNetwork}
	h.system.mu.Unlock()
	var set SettingsResponse
	if err := h.client.Do(context.Background(), http.MethodGet, "/v1/settings", nil, &set); err != nil {
		t.Fatal(err)
	}
	if set.Choices.SuggestedLetter != "X:" || set.Choices.LettersInUse["W:"] != platform.LetterNetwork || slices.Contains(set.Choices.DriveLetters, "W:") {
		t.Fatalf("choices = %+v", set.Choices)
	}
	var ae *APIError
	if err := h.post("/v1/mount", map[string]string{"letter": "W:"}, nil); !errors.As(err, &ae) || ae.Code != "letter_taken" {
		t.Fatalf("mount on a taken letter: %v", err)
	}
	if err := h.post("/v1/mount", map[string]string{"letter": "x"}, nil); err != nil {
		t.Fatal(err)
	}
	s = h.waitState("mounted on the chosen letter", func(s State) bool {
		return s.Mount.State == MountMounted && s.Mount.Letter != nil && *s.Mount.Letter == "X:" && s.Mount.LetterConfirmed
	})
	if saved, _ := os.ReadFile(filepath.Join(h.paths.State, "agent-state.json")); !strings.Contains(string(saved), `"letter_confirmed":true`) {
		t.Fatalf("confirmation not kept: %s", saved)
	}
	if s.Account == nil || s.Account.Email != "person@example.test" || s.Account.DeviceID != "dev-1" {
		t.Fatalf("account = %+v", s.Account)
	}
	if s.DeviceName != "TEST-PC" {
		t.Fatalf("device name = %q", s.DeviceName)
	}
	tok, err := h.creds.Get("Olsyn Drive/drive.olsyn.com")
	if err != nil || string(tok) != "odt_synthetic_token_value" {
		t.Fatalf("token in credential store = %q, %v", tok, err)
	}
	accFile, _ := os.ReadFile(filepath.Join(h.paths.Config, "account.json"))
	stateJSON, _ := json.Marshal(s)
	for name, data := range map[string][]byte{"account.json": accFile, "state": stateJSON} {
		if strings.Contains(string(data), "odt_") || strings.Contains(string(data), "odl_") {
			t.Fatalf("%s carries a secret: %s", name, data)
		}
	}
	h.system.mu.Lock()
	urls := h.system.urls
	h.system.mu.Unlock()
	if len(urls) != 1 || !strings.Contains(urls[0], "BCDF-GHJK") {
		t.Fatalf("browser opened %v", urls)
	}
	if h.fs.session.Token != "odt_synthetic_token_value" {
		t.Fatal("file system did not get the session token")
	}
	// The stream saw the link approved.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range ev.all() {
			if e.Type == "signin" && strings.Contains(string(e.Data), `"approved"`) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no signin approved event")
}

func TestSignInExpiry(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.drive.link.ExpiresIn = 150 * time.Millisecond
		h.drive.sticky = ErrAuthorizationPending
	})
	if err := h.post("/v1/signin", map[string]bool{"open_browser": false}, nil); err != nil {
		t.Fatal(err)
	}
	s := h.waitState("expired", func(s State) bool { return s.SignIn != nil && s.SignIn.State == "expired" })
	if s.Connection != ConnSignedOut || s.Reason == nil || s.Reason.Code != "link_expired" {
		t.Fatalf("state = %s %+v", s.Connection, s.Reason)
	}
	// The server saying 410 ends it the same way.
	h.drive.mu.Lock()
	h.drive.link.ExpiresIn = 5 * time.Second
	h.drive.sticky = ErrLinkExpired
	h.drive.mu.Unlock()
	if err := h.post("/v1/signin", map[string]bool{"open_browser": false}, nil); err != nil {
		t.Fatal(err)
	}
	h.waitState("expired by server", func(s State) bool {
		return s.SignIn != nil && s.SignIn.State == "expired" && s.Connection == ConnSignedOut
	})
}

func TestSignInDenied(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.drive.polls = []error{ErrAuthorizationPending}
		h.drive.sticky = ErrLinkDenied
	})
	if err := h.post("/v1/signin", map[string]bool{"open_browser": false}, nil); err != nil {
		t.Fatal(err)
	}
	s := h.waitState("denied", func(s State) bool { return s.SignIn != nil && s.SignIn.State == "denied" })
	if s.Connection != ConnSignedOut || s.Reason.Code != "link_denied" || s.SignIn.Error == nil {
		t.Fatalf("state = %s %+v %+v", s.Connection, s.Reason, s.SignIn)
	}
	if _, err := h.creds.Get("Olsyn Drive/drive.olsyn.com"); !errors.Is(err, platform.ErrNotFound) {
		t.Fatal("a token was stored after a denied link")
	}
}

// lockedStore is Credential Manager in an SSH session: nothing can be written.
type lockedStore struct {
	*platform.MemoryCredentialStore
}

func (lockedStore) Set(string, []byte) error {
	return errors.New("write credential: A specified logon session does not exist")
}
func (s lockedStore) CheckWritable() error { return s.Set("probe", nil) }

// A sign-in that could not be kept is refused before any code is shown, so
// "olsyn-drive login" over SSH ends at once with a clear message.
func TestSignInRefusedWhenTheTokenCannotBeKept(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.store = lockedStore{h.creds} })
	err := h.post("/v1/signin", map[string]bool{"open_browser": false}, nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 503 || ae.Code != "credential_store" || !strings.Contains(ae.Message, "SSH") {
		t.Fatalf("signin error = %v", err)
	}
	s := h.waitState("signed out", func(s State) bool { return s.Connection == ConnSignedOut })
	if s.SignIn != nil {
		t.Fatalf("a link was started: %+v", s.SignIn)
	}
}

func TestSignInCancel(t *testing.T) {
	h := newHarness(t, func(h *harness) { h.drive.sticky = ErrAuthorizationPending })
	if err := h.post("/v1/signin", map[string]bool{"open_browser": false}, nil); err != nil {
		t.Fatal(err)
	}
	var again SignIn
	if err := h.post("/v1/signin", map[string]bool{"open_browser": false}, &again); err != nil || again.State != "waiting" {
		t.Fatalf("second signin while linking: %+v %v", again, err)
	}
	var si SignIn
	if err := h.post("/v1/signin/cancel", nil, &si); err != nil || si.State != "cancelled" {
		t.Fatalf("cancel: %+v %v", si, err)
	}
	h.waitState("signed out", func(s State) bool { return s.Connection == ConnSignedOut })
}

func TestRevokedTokenSignsOut(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		seedAccount(h)
		h.drive.hbErr = func(n int) error {
			if n >= 3 {
				return ErrUnauthenticated
			}
			return nil
		}
	})
	ev := h.collect()
	s := h.waitState("revoked", func(s State) bool { return s.Reason != nil && s.Reason.Code == "token_revoked" })
	if s.Connection != ConnSignedOut || s.Account != nil || s.Mount.State != MountUnmounted {
		t.Fatalf("state = %s account=%v mount=%+v", s.Connection, s.Account, s.Mount)
	}
	if _, err := h.creds.Get("Olsyn Drive/drive.olsyn.com"); !errors.Is(err, platform.ErrNotFound) {
		t.Fatal("token kept after revocation")
	}
	if h.mounter.isMounted() {
		t.Fatal("still mounted")
	}
	if h.fs.cleared {
		t.Fatal("cache cleared on revocation")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range ev.all() {
			if e.Type == "notification" && strings.Contains(string(e.Data), `"signed_out"`) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no signed_out notification")
}

func TestServerRestartingAndOffline(t *testing.T) {
	var mu sync.Mutex
	mode := "ok"
	h := newHarness(t, func(h *harness) {
		seedAccount(h)
		h.drive.hbErr = func(int) error {
			mu.Lock()
			defer mu.Unlock()
			switch mode {
			case "restarting":
				return &RetryAfterError{Code: "service_unavailable", After: time.Second}
			case "offline":
				return errors.New("dial tcp: connection refused")
			}
			return nil
		}
	})
	h.waitState("connected", func(s State) bool { return s.Connection == ConnConnected })
	mu.Lock()
	mode = "restarting"
	mu.Unlock()
	h.waitState("server restarting", func(s State) bool { return s.Connection == ConnServerRestarting })
	mu.Lock()
	mode = "offline"
	mu.Unlock()
	h.waitState("offline", func(s State) bool { return s.Connection == ConnOffline })
	mu.Lock()
	mode = "ok"
	mu.Unlock()
	h.waitState("connected again", func(s State) bool { return s.Connection == ConnConnected })
}

func TestEventsCoalescing(t *testing.T) {
	h := newHarness(t, seedAccount)
	h.waitState("connected", func(s State) bool { return s.Connection == ConnConnected })
	ev := h.collect()
	base := Transfer{ID: "t_1", Direction: "up", Origin: "mount", State: TransferActive, Name: "a.bin", Size: 1000}
	start := time.Now()
	for i := 1; i <= 200; i++ {
		tr := base
		tr.BytesDone = int64(i * 5)
		h.fs.events <- FSEvent{Transfer: &tr}
		time.Sleep(time.Millisecond)
	}
	elapsed := time.Since(start)
	done := base
	done.State, done.BytesDone = TransferDone, 1000
	h.fs.events <- FSEvent{Transfer: &done}

	var got []Transfer
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got = got[:0]
		for _, e := range ev.all() {
			if e.Type == "transfer" {
				var tr Transfer
				json.Unmarshal(e.Data, &tr)
				got = append(got, tr)
			}
		}
		if len(got) > 0 && got[len(got)-1].State == TransferDone {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(got) == 0 || got[len(got)-1].State != TransferDone {
		t.Fatalf("done event missing; got %d transfer events", len(got))
	}
	// 200 updates over ~elapsed: at most 4 per second plus the first (a new
	// transfer is sent at once), the final state, and one tick of slack.
	limit := int(elapsed/coalesceEvery) + 3
	if len(got) > limit {
		t.Fatalf("%d transfer events for 200 updates in %s (limit %d)", len(got), elapsed, limit)
	}
	for i := 1; i < len(got); i++ {
		if got[i].BytesDone < got[i-1].BytesDone {
			t.Fatalf("progress went backwards: %d then %d", got[i-1].BytesDone, got[i].BytesDone)
		}
	}
}

func TestHubSlowSubscriberGetsLaggedSnapshot(t *testing.T) {
	hub := NewHub("r1")
	sub, err := hub.Subscribe("")
	if err != nil {
		t.Fatal(err)
	}
	need, lagged, _, _ := sub.Next(context.Background(), time.Second)
	if !need || lagged {
		t.Fatalf("first Next: need=%v lagged=%v", need, lagged)
	}
	// Coalescible events of one key never pile up.
	for i := range 1000 {
		hub.Throttle("transfer", "t1", i)
		hub.Flush()
	}
	_, _, events, _ := sub.Next(context.Background(), time.Second)
	if len(events) != 1 || string(events[0].Data) != "999" {
		t.Fatalf("coalesced queue = %d events, last %s", len(events), events[len(events)-1].Data)
	}
	// Non-mergeable events overflow the queue: the backlog is dropped for a snapshot.
	for i := range subscriberQueue + 10 {
		hub.Publish("held", i)
	}
	need, lagged, events, _ = sub.Next(context.Background(), time.Second)
	if !need || !lagged || len(events) != 0 {
		t.Fatalf("after overflow: need=%v lagged=%v events=%d", need, lagged, len(events))
	}
	// Resume by id replays only what came after.
	sub2, _ := hub.Subscribe(hub.CurrentID())
	hub.Publish("state", "x")
	need, _, events, _ = sub2.Next(context.Background(), time.Second)
	if need || len(events) != 1 || events[0].Type != "state" {
		t.Fatalf("resume: need=%v events=%v", need, events)
	}
	if _, err := hub.Subscribe("other-run-5"); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	h := newHarness(t, seedAccount)
	h.waitState("connected", func(s State) bool { return s.Connection == ConnConnected && s.Mount.State == MountMounted })
	var got SettingsResponse
	if err := h.client.Do(context.Background(), http.MethodGet, "/v1/settings", nil, &got); err != nil {
		t.Fatal(err)
	}
	if got.Settings.DriveLetter != "W:" || got.Settings.UpdateChannel != "stable" || len(got.Choices.DriveLetters) == 0 {
		t.Fatalf("settings = %+v", got)
	}
	patch := map[string]any{"bandwidth": map[string]any{"up_bps": 5000000}, "drive_letter": "x", "start_at_login": false}
	if err := h.client.Do(context.Background(), http.MethodPatch, "/v1/settings", patch, &got); err != nil {
		t.Fatal(err)
	}
	if got.Settings.Bandwidth.UpBPS != 5000000 || got.Settings.DriveLetter != "X:" || got.Settings.StartAtLogin {
		t.Fatalf("patched = %+v", got.Settings)
	}
	h.fs.mu.Lock()
	bw := h.fs.bandwidth
	h.fs.mu.Unlock()
	if bw.UpBPS != 5000000 {
		t.Fatal("bandwidth not applied")
	}
	if h.system.autostart == nil || *h.system.autostart {
		t.Fatal("autostart not turned off")
	}
	h.waitState("remounted on X:", func(s State) bool {
		return s.Mount.State == MountMounted && s.Mount.Letter != nil && *s.Mount.Letter == "X:"
	})
	data, _ := os.ReadFile(filepath.Join(h.paths.Config, "settings.json"))
	if !strings.Contains(string(data), `"X:"`) {
		t.Fatalf("not saved: %s", data)
	}
	err := h.client.Do(context.Background(), http.MethodPatch, "/v1/settings", map[string]any{"update_channel": "nightly"}, nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 422 || ae.Code != "invalid_setting" || ae.Body["field"] != "update_channel" {
		t.Fatalf("invalid patch: %v", err)
	}
	err = h.client.Do(context.Background(), http.MethodPatch, "/v1/settings", map[string]any{"server_url": "https://other.example.test"}, nil)
	if !errors.As(err, &ae) || ae.Code != "already_signed_in" {
		t.Fatalf("server change while signed in: %v", err)
	}
}

func TestSignOutWithUnsentData(t *testing.T) {
	h := newHarness(t, seedAccount)
	h.waitState("connected", func(s State) bool { return s.Connection == ConnConnected && s.Mount.State == MountMounted })
	h.fs.mu.Lock()
	h.fs.unsent = Unsent{Files: 2, Bytes: 12_400_000, Held: 1}
	h.fs.mu.Unlock()

	err := h.post("/v1/signout", map[string]bool{"clear_cache": true}, nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 409 || ae.Code != "unsent_data" {
		t.Fatalf("expected unsent_data, got %v", err)
	}
	unsent, _ := ae.Body["unsent"].(map[string]any)
	if unsent["files"] != float64(2) || unsent["held"] != float64(1) {
		t.Fatalf("unsent detail = %v", ae.Body)
	}
	s := h.waitState("still connected", func(s State) bool { return s.Connection == ConnConnected })
	if s.Account == nil || !h.mounter.isMounted() {
		t.Fatal("refused sign-out changed something")
	}

	var res SignOutResult
	if err := h.post("/v1/signout", map[string]bool{"clear_cache": true, "discard_unsent_confirmed": true}, &res); err != nil {
		t.Fatal(err)
	}
	if !res.SignedOut || !res.Unlinked || !res.CacheCleared {
		t.Fatalf("result = %+v", res)
	}
	s = h.waitState("signed out", func(s State) bool { return s.Connection == ConnSignedOut })
	if s.Reason == nil || s.Reason.Code != "signed_out" {
		t.Fatalf("reason = %+v", s.Reason)
	}
	if _, err := h.creds.Get("Olsyn Drive/drive.olsyn.com"); !errors.Is(err, platform.ErrNotFound) {
		t.Fatal("token kept after sign-out")
	}
	if len(h.drive.unlinked) != 1 || h.drive.unlinked[0] != "dev-1" {
		t.Fatalf("unlinked = %v", h.drive.unlinked)
	}
	if h.mounter.isMounted() {
		t.Fatal("still mounted")
	}
	if err := h.post("/v1/signout", nil, nil); !errors.As(err, &ae) || ae.Code != "not_signed_in" {
		t.Fatalf("second sign-out: %v", err)
	}
}

func TestSignOutKeepsUnsentWithoutClearing(t *testing.T) {
	h := newHarness(t, seedAccount)
	h.waitState("connected", func(s State) bool { return s.Connection == ConnConnected })
	h.fs.mu.Lock()
	h.fs.unsent = Unsent{Files: 3, Bytes: 100}
	h.fs.mu.Unlock()
	var res SignOutResult
	if err := h.post("/v1/signout", map[string]bool{"clear_cache": false}, &res); err != nil {
		t.Fatal(err)
	}
	if res.CacheCleared || res.UnsentKept == nil || res.UnsentKept.Files != 3 || h.fs.cleared {
		t.Fatalf("result = %+v cleared=%v", res, h.fs.cleared)
	}
}

func TestOriginRefusedAndUnknownEndpoint(t *testing.T) {
	h := newHarness(t, nil)
	req, _ := http.NewRequest(http.MethodGet, "http://olsyn-drive/v1/state", nil)
	req.Header.Set("Origin", "https://evil.example.test")
	resp, err := h.client.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var ae *APIError
	if err := h.client.Do(context.Background(), http.MethodGet, "/v1/nope", nil, nil); !errors.As(err, &ae) || ae.Code != "not_found" {
		t.Fatalf("unknown endpoint: %v", err)
	}
	if err := h.client.Do(context.Background(), http.MethodDelete, "/v1/state", nil, nil); !errors.As(err, &ae) || ae.Status != 405 || ae.Code != "method_not_allowed" {
		t.Fatalf("wrong method: %v", err)
	}
}

func TestPauseResumeAndQuit(t *testing.T) {
	h := newHarness(t, seedAccount)
	h.waitState("connected", func(s State) bool { return s.Connection == ConnConnected && s.Mount.State == MountMounted })
	if err := h.post("/v1/pause", map[string]int{"minutes": 30}, nil); err != nil {
		t.Fatal(err)
	}
	s := h.waitState("paused", func(s State) bool { return s.Connection == ConnPaused })
	if s.PausedUntil == nil {
		t.Fatal("no paused_until")
	}
	if err := h.post("/v1/resume", nil, nil); err != nil {
		t.Fatal(err)
	}
	h.waitState("resumed", func(s State) bool { return s.Connection == ConnConnected })

	var ae *APIError
	if err := h.post("/v1/quit", map[string]bool{"unmount": false}, nil); !errors.As(err, &ae) || ae.Code != "mounted" {
		t.Fatalf("quit while mounted: %v", err)
	}
	if err := h.post("/v1/quit", map[string]bool{"unmount": true}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case exit := <-h.done:
		if exit.Update != nil {
			t.Fatal("quit reported an update")
		}
		h.done <- exit // for cleanup
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not quit")
	}
	if h.mounter.isMounted() {
		t.Fatal("still mounted after quit")
	}
}

func TestDiagnosticsRedacts(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		seedAccount(h)
		h.fs.transfers = []Transfer{{ID: "t_9", State: TransferHeld, Name: "Secret Project Plan.rvt",
			LocalPath: `W:\Clients\Secret Project\Secret Project Plan.rvt`, Held: &Held{Code: "sealed"}}}
	})
	h.waitState("connected", func(s State) bool { return s.Connection == ConnConnected })
	log := `{"time":"2026-11-24T00:00:00Z","level":"INFO","msg":"uploaded","path":"/Users/person/Secret Project/plan.rvt"}` + "\n" +
		`plain line Authorization: Bearer odt_synthetic_token_value for person@example.test at C:\Users\person\Secret Project` + "\n"
	os.WriteFile(filepath.Join(h.paths.Logs, "agent.log"), []byte(log), 0o600)

	var out struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	if err := h.post("/v1/diagnostics", nil, &out); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(out.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var all strings.Builder
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
		rc, _ := f.Open()
		data, _ := io.ReadAll(rc)
		rc.Close()
		all.Write(data)
	}
	for _, want := range []string{"state.json", "settings.json", "transfers.json", "system.json", "logs/agent.log"} {
		if !names[want] {
			t.Errorf("bundle lacks %s", want)
		}
	}
	text := all.String()
	for _, secret := range []string{"odt_synthetic", "person@example.test", "Test Person", "Secret Project", "TEST-PC", `\Users\person`, "/Users/person"} {
		if strings.Contains(text, secret) {
			t.Errorf("bundle leaks %q", secret)
		}
	}
	if !strings.Contains(text, "<name:") || !strings.Contains(text, "sealed") {
		t.Error("bundle lost its useful structure")
	}
}

func TestStartsSignedOutWhenTokenMissing(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		seedAccount(h)
		h.creds.Delete("Olsyn Drive/drive.olsyn.com")
	})
	s := h.waitState("signed out", func(s State) bool { return s.Connection == ConnSignedOut && s.Ready })
	if s.Reason == nil || s.Reason.Code != "token_missing" {
		t.Fatalf("reason = %+v", s.Reason)
	}
}

// The install reports its steps, then a restart of Windows is needed: the
// driver is shown as restart_needed and the mount waits for it.
func TestDriverInstallProgressThenRestartNeeded(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(h *harness) {
		seedAccount(h)
		h.system.driver = platform.DriverInfo{Name: "winfsp", Needed: true, Others: []string{"dokany"}}
		h.system.install = func(progress func(string)) error {
			progress(platform.DriverStepPreparing)
			progress(platform.DriverStepApproval)
			<-release
			progress(platform.DriverStepInstalling)
			h.system.mu.Lock()
			h.system.driver = platform.DriverInfo{Name: "winfsp", Needed: true, Installed: true, Version: "2.2.26323"}
			h.system.mu.Unlock()
			return platform.ErrRestartNeeded
		}
	})
	s := h.waitState("driver missing", func(s State) bool { return s.Driver.State == "missing" && s.Ready })
	if len(s.Driver.Others) != 1 || s.Driver.Others[0] != "dokany" {
		t.Fatalf("others = %v", s.Driver.Others)
	}
	if err := h.post("/v1/driver/install", nil, nil); err != nil {
		t.Fatal(err)
	}
	h.waitState("approval", func(s State) bool {
		return s.Driver.State == "installing" && s.Driver.Progress == platform.DriverStepApproval
	})
	close(release)
	s = h.waitState("restart needed", func(s State) bool { return s.Driver.State == "restart_needed" })
	if s.Driver.Error == nil || s.Driver.Error.Code != "restart_needed" || s.Driver.Progress != "" {
		t.Fatalf("driver = %+v", s.Driver)
	}
	if !slices.ContainsFunc(s.Issues, func(i Issue) bool { return i.Code == "driver_restart_needed" }) {
		t.Fatalf("issues = %+v", s.Issues)
	}
	var ae *APIError
	if err := h.post("/v1/mount", map[string]string{"letter": "W:"}, nil); !errors.As(err, &ae) || ae.Code != "driver_restart_needed" {
		t.Fatalf("mount before restart: %v", err)
	}
	if err := h.post("/v1/driver/install", nil, nil); !errors.As(err, &ae) || ae.Code != "driver_restart_needed" {
		t.Fatalf("install again: %v", err)
	}
}

// A declined prompt leaves the driver missing with a "declined" error.
func TestDriverInstallDeclined(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		seedAccount(h)
		h.system.driver = platform.DriverInfo{Name: "winfsp", Needed: true}
	})
	h.waitState("driver missing", func(s State) bool { return s.Driver.State == "missing" && s.Ready })
	if err := h.post("/v1/driver/install", nil, nil); err != nil {
		t.Fatal(err)
	}
	s := h.waitState("declined", func(s State) bool { return s.Driver.Error != nil })
	if s.Driver.State != "missing" || s.Driver.Error.Code != "declined" {
		t.Fatalf("driver = %+v", s.Driver)
	}
}

func TestDriverMissingBlocksMount(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		seedAccount(h)
		h.system.driver = platform.DriverInfo{Name: "winfsp", Needed: true}
	})
	s := h.waitState("attention", func(s State) bool { return s.Connection == ConnAttention && s.Ready })
	if s.Driver.State != "missing" || s.Mount.State != MountUnmounted || s.Issues[0].Code != "driver_missing" {
		t.Fatalf("driver=%+v mount=%+v issues=%+v", s.Driver, s.Mount, s.Issues)
	}
	var ae *APIError
	if err := h.post("/v1/mount", nil, nil); !errors.As(err, &ae) || ae.Code != "driver_missing" {
		t.Fatalf("mount without driver: %v", err)
	}
}
