package agent

import (
	"context"
	"errors"
	"maps"
	"sync"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/platform"
	"github.com/geyervalmont/olsyn-drive-client/internal/settings"
)

// fakeDrive answers the device link and heartbeats from a script.
type fakeDrive struct {
	mu          sync.Mutex
	link        Link
	linkErr     error
	polls       []error // one per poll, in order; a nil entry approves
	sticky      error   // returned once polls run out; nil approves
	approveWith LinkResult
	pollCount   int
	hbErr       func(n int) error
	hbCount     int
	unlinked    []string
	lastHB      Heartbeat
}

func newFakeDrive() *fakeDrive {
	return &fakeDrive{
		link: Link{DeviceCode: "odl_synthetic_device_code", UserCode: "BCDF-GHJK",
			VerificationURL: "https://drive.example.test/link?code=BCDF-GHJK", Interval: time.Millisecond, ExpiresIn: 5 * time.Second},
		approveWith: LinkResult{Token: "odt_synthetic_token_value", DeviceID: "dev-1",
			User: User{ID: "user-1", Name: "Test Person", Email: "person@example.test"}},
	}
}

func (d *fakeDrive) LinkStart(ctx context.Context, req LinkRequest) (Link, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.link, d.linkErr
}

func (d *fakeDrive) LinkPoll(ctx context.Context, code string) (LinkResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if code != d.link.DeviceCode {
		return LinkResult{}, errors.New("wrong device code")
	}
	d.pollCount++
	if len(d.polls) > 0 {
		err := d.polls[0]
		d.polls = d.polls[1:]
		if err != nil {
			return LinkResult{}, err
		}
		return d.approveWith, nil
	}
	if d.sticky != nil {
		return LinkResult{}, d.sticky
	}
	return d.approveWith, nil
}

func (d *fakeDrive) Heartbeat(ctx context.Context, token string, hb Heartbeat) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.hbCount++
	d.lastHB = hb
	if d.hbErr != nil {
		return d.hbErr(d.hbCount)
	}
	return nil
}

func (d *fakeDrive) Unlink(ctx context.Context, token, deviceID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.unlinked = append(d.unlinked, deviceID)
	return nil
}

// fakeFS is an in-memory file system core.
type fakeFS struct {
	mu        sync.Mutex
	started   bool
	stops     int
	status    FSStatus
	events    chan FSEvent
	transfers []Transfer
	unsent    Unsent
	cleared   bool
	paused    bool
	bandwidth settings.Bandwidth
	session   Session
	sent      []SendRequest
}

func newFakeFS() *fakeFS {
	return &fakeFS{status: FSStatus{Connectivity: Online}, events: make(chan FSEvent, 1024)}
}

func (f *fakeFS) Start(ctx context.Context, s Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started, f.session = true, s
	return nil
}

func (f *fakeFS) Stop(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = false
	f.stops++
	return nil
}

func (f *fakeFS) Status() FSStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeFS) Events() <-chan FSEvent { return f.events }

func (f *fakeFS) Transfers(ctx context.Context, q TransferQuery) (TransferPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var p TransferPage
	for _, t := range f.transfers {
		if len(q.States) == 0 || contains(q.States, t.State) {
			p.Items = append(p.Items, t)
		}
	}
	return p, nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func (f *fakeFS) Retry(ctx context.Context, id string) (Transfer, error) {
	return Transfer{}, ErrNotRetryable
}
func (f *fakeFS) Cancel(ctx context.Context, id string) (Transfer, error) {
	return Transfer{}, ErrNotFound
}
func (f *fakeFS) DiscardHeld(ctx context.Context, id string) error  { return ErrNotHeld }
func (f *fakeFS) ReuploadHeld(ctx context.Context, id string) error { return ErrNotHeld }
func (f *fakeFS) SaveHeldCopy(ctx context.Context, id, dest string) (string, error) {
	return "", ErrNotHeld
}

func (f *fakeFS) Pause(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = true
	return nil
}

func (f *fakeFS) Resume(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = false
	return nil
}

func (f *fakeFS) Send(ctx context.Context, req SendRequest) (SendResult, error) {
	f.mu.Lock()
	f.sent = append(f.sent, req)
	f.mu.Unlock()
	return SendResult{BatchID: "b_1", Files: len(req.LocalPaths), Bytes: 10}, nil
}

func (f *fakeFS) Batches() []Batch { return nil }

func (f *fakeFS) CacheUsage(ctx context.Context) (CacheUsage, error) {
	return CacheUsage{UsedBytes: 5 << 20, UnsentBytes: 1 << 20, UnsentFiles: 2}, nil
}

func (f *fakeFS) DriveFolders(ctx context.Context, parent, parentID, cursor string) (DriveFolders, error) {
	if parent == "/missing" {
		return DriveFolders{}, ErrDriveFolderNotFound
	}
	return DriveFolders{Parent: parent, CanWrite: true, Folders: []DriveFolder{{ID: "f1", Name: "Projects", Path: "/Projects", CanWrite: true}}}, nil
}

func (f *fakeFS) SetCacheCap(int64) {}

func (f *fakeFS) SetBandwidth(b settings.Bandwidth) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bandwidth = b
}

func (f *fakeFS) Unsent(ctx context.Context) (Unsent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unsent, nil
}

func (f *fakeFS) ClearCache(ctx context.Context, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = true
	f.unsent = Unsent{}
	return nil
}

func (f *fakeFS) WebURL(ctx context.Context, p string) (string, error) {
	return "https://drive.example.test/files" + p, nil
}

// fakeMounter pretends to mount.
type fakeMounter struct {
	mu        sync.Mutex
	mounted   bool
	letter    string
	openFiles int
	mounts    int
	failWith  error
}

func (m *fakeMounter) Mount(ctx context.Context, letter string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWith != nil {
		return "", m.failWith
	}
	m.mounted, m.letter = true, letter
	m.mounts++
	return letter + "\\", nil
}

func (m *fakeMounter) Unmount(ctx context.Context, force bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mounted = false
	return nil
}

func (m *fakeMounter) OpenFiles() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.openFiles
}

func (m *fakeMounter) isMounted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mounted
}

// fakeSystem records what the agent asks of the OS.
type fakeSystem struct {
	mu        sync.Mutex
	urls      []string
	folders   []string
	autostart *bool
	driver    platform.DriverInfo
	routes    []string
	// lettersInUse overrides the default (only D: taken)
	lettersInUse map[string]string
	// install runs for InstallDriver; nil declines the prompt.
	install func(progress func(string)) error
}

func (s *fakeSystem) OpenURL(ctx context.Context, u string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.urls = append(s.urls, u)
	return nil
}

func (s *fakeSystem) OpenFolder(ctx context.Context, p string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.folders = append(s.folders, p)
	return nil
}

func (s *fakeSystem) SetAutostart(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.autostart = &on
	return nil
}

func (s *fakeSystem) DetectDriver() (platform.DriverInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.driver, nil
}

func (s *fakeSystem) InstallDriver(ctx context.Context, progress func(string)) error {
	s.mu.Lock()
	install := s.install
	s.mu.Unlock()
	if install == nil {
		return platform.ErrDeclined
	}
	return install(progress)
}
func (s *fakeSystem) DriveLettersInUse() (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lettersInUse != nil {
		return maps.Clone(s.lettersInUse), nil
	}
	return map[string]string{"D:": platform.LetterVolume}, nil
}
func (s *fakeSystem) DownloadsDir() (string, error)        { return "/nonexistent", nil }
func (s *fakeSystem) FreeDiskBytes(string) (uint64, error) { return 100 << 30, nil }

func (s *fakeSystem) ShowUI(ctx context.Context, route string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes = append(s.routes, route)
	return nil
}
