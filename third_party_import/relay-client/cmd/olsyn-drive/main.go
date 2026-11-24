// Command olsyn-drive is the drive's desktop client: sign in once, send
// big folders straight to the drive (`send`), or keep drop folders that
// upload themselves (`relay`). See `olsyn-drive help`.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"olsyn-drive/internal/auth"
	"olsyn-drive/internal/driveapi"
	"olsyn-drive/internal/spool"
	"olsyn-drive/internal/state"
	"olsyn-drive/internal/status"
)

// Set at build time: -ldflags "-X main.version=1.2.3 -X main.defaultDrive=https://drive.example.com"
var (
	version      = "dev"
	defaultDrive = ""
)

const usage = `olsyn-drive: the drive's desktop client

  olsyn-drive login [--drive URL]       sign this computer in (approve in your browser)
  olsyn-drive login --token-stdin       a shared relay: store a token an admin made
  olsyn-drive logout                    forget the sign-in on this computer
  olsyn-drive relay [--spool DIR]       keep drop folders: files put there are sent,
                                        checked, then removed from the folder
  olsyn-drive status                    what the background client is doing
  olsyn-drive install --user [--yes]    start the drop-folder client when you sign in
  olsyn-drive uninstall --user [--yes]  stop doing that
  olsyn-drive version

Exit codes: 0 everything is in the drive; 1 some files were held, skipped or
failed (listed); 2 a usage or sign-in problem; 130 stopped with Ctrl-C.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	var err error
	code := 0
	switch cmd {
	case "login":
		err = cmdLogin(rest, stdout)
	case "logout":
		err = cmdLogout(stdout)
	case "send":
		fmt.Fprintln(stderr, "send moved into the main client: olsyn-drive send <path>... --to <drive folder>")
		return 2
	case "relay":
		err = cmdRelay(rest, stderr)
	case "status":
		err = cmdStatus(stdout)
	case "install", "uninstall":
		err = cmdInstall(cmd, rest, stdout)
	case "mount", "update":
		fmt.Fprintf(stderr, "%s isn't built yet; it's planned (see the design notes).\n", cmd)
		return 2
	case "version", "--version":
		fmt.Fprintf(stdout, "olsyn-drive %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "olsyn-drive:", err)
		if code == 0 {
			code = 2
		}
	}
	return code
}

// ---- configuration ----

// Config is olsyn-drive.json in the client's folder; environment
// variables override it. It holds no secrets.
type Config struct {
	Drive       string  `json:"drive"`
	Spool       string  `json:"spool"`
	StatusAddr  string  `json:"status_addr"`
	MaxSpoolGB  float64 `json:"max_spool_gb"`
	FreeFloorGB float64 `json:"free_floor_gb"`
	Files       int     `json:"files"`
	// SettleSeconds: how long a spooled file must stay unchanged before
	// it's sent (default 20). Raise it for slow copies over a network.
	SettleSeconds int `json:"settle_seconds"`
	// CAFile: extra certificate authorities (PEM) to trust for the drive.
	CAFile string `json:"ca_file"`
}

func loadConfig() (Config, string, error) {
	dir, err := auth.Dir()
	if err != nil {
		return Config{}, "", err
	}
	var c Config
	b, err := os.ReadFile(filepath.Join(dir, "olsyn-drive.json"))
	if err == nil {
		if err := json.Unmarshal(b, &c); err != nil {
			return c, dir, fmt.Errorf("olsyn-drive.json: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return c, dir, err
	}
	if v := os.Getenv("OLSYN_DRIVE_URL"); v != "" {
		c.Drive = v
	}
	if v := os.Getenv("OLSYN_DRIVE_SPOOL"); v != "" {
		c.Spool = v
	}
	if v := os.Getenv("OLSYN_DRIVE_CA_FILE"); v != "" {
		c.CAFile = v
	}
	if c.StatusAddr == "" {
		c.StatusAddr = status.DefaultAddr
	}
	return c, dir, nil
}

func agent() string {
	return "olsyn-drive/" + version + " (" + runtime.GOOS + "/" + runtime.GOARCH + ")"
}

func machineName() string {
	h, _ := os.Hostname()
	if h == "" {
		h = "this computer"
	}
	return h
}

// signedIn loads the sign-in and a client for its drive.
func signedIn() (*auth.Login, *driveapi.Client, string, error) {
	_, dir, err := loadConfig()
	if err != nil {
		return nil, nil, "", err
	}
	l, tok, err := auth.Store{Dir: dir}.Load()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, dir, fmt.Errorf("this computer isn't signed in: run olsyn-drive login")
	}
	if err != nil {
		return nil, nil, dir, err
	}
	api, err := newClient(l.Drive, tok)
	return l, api, dir, err
}

func newClient(base, tok string) (*driveapi.Client, error) {
	cfg, _, err := loadConfig()
	if err != nil {
		return nil, err
	}
	api, err := driveapi.New(base, tok, agent())
	if err == nil && cfg.CAFile != "" {
		err = api.TrustCA(cfg.CAFile)
	}
	return api, err
}

func refuseRoot(allow bool) error {
	if runtime.GOOS != "windows" && os.Geteuid() == 0 && !allow {
		return fmt.Errorf("don't run olsyn-drive as root: run it as the user (or a dedicated account) that owns the files; --allow-root overrides")
	}
	return nil
}

// ---- login / logout ----

func cmdLogin(args []string, out io.Writer) error {
	fl := flag.NewFlagSet("login", flag.ContinueOnError)
	drive := fl.String("drive", "", "the drive's address, e.g. https://drive.example.com")
	noBrowser := fl.Bool("no-browser", false, "don't open the browser; just print the link")
	tokenStdin := fl.Bool("token-stdin", false, "a shared relay: read the token an admin made from standard input")
	if err := fl.Parse(args); err != nil {
		return err
	}
	cfg, dir, err := loadConfig()
	if err != nil {
		return err
	}
	base := firstOf(*drive, cfg.Drive, defaultDrive)
	if base == "" {
		return fmt.Errorf("which drive? pass --drive https://drive.example.com (or set OLSYN_DRIVE_URL)")
	}
	api, err := newClient(base, "")
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *tokenStdin {
		return loginWithToken(ctx, base, dir, os.Stdin, out)
	}
	l, err := auth.SignIn(ctx, api, auth.Store{Dir: dir}, machineName(), version, func(code, link string) {
		fmt.Fprintf(out, "To sign this computer in, open\n\n    %s\n\nand check the code shown there is %s, then approve it.\nWaiting…\n", link, code)
		if !*noBrowser {
			openBrowser(link)
		}
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Signed in as %s. This computer shows as %q in your devices; remove it there to sign it out everywhere.\n", l.Person, l.NodeName)
	return nil
}

// loginWithToken stores a token an admin made for a shared relay, after
// checking with the drive that it is a relay node's.
func loginWithToken(ctx context.Context, base, dir string, in io.Reader, out io.Writer) error {
	b, err := io.ReadAll(io.LimitReader(in, 4096))
	if err != nil {
		return err
	}
	tok := strings.TrimSpace(string(b))
	if !strings.HasPrefix(tok, "odt_") {
		return fmt.Errorf("that isn't a drive token (they start with odt_)")
	}
	api, err := newClient(base, tok)
	if err != nil {
		return err
	}
	self, err := api.Self(ctx)
	if err != nil {
		if s := driveapi.StatusOf(err); s == 401 || s == 403 {
			return fmt.Errorf("the drive doesn't accept that token as a relay node's")
		}
		return err
	}
	l := &auth.Login{Drive: api.Base.String(), NodeID: self.ID, NodeName: self.Name, At: time.Now().UTC()}
	if self.Person != nil {
		l.Person, l.PersonID = self.Person.Name, self.Person.ID
	}
	if err := (auth.Store{Dir: dir}).Save(l, tok); err != nil {
		return err
	}
	fmt.Fprintf(out, "This computer is now the relay node %q (%s).\n", self.Name, self.Mode)
	return nil
}

func firstOf(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// openBrowser opens a link that came from the drive. It must be http(s)
// and is passed as one argument, never through a shell.
func openBrowser(link string) {
	if !strings.HasPrefix(link, "https://") && !strings.HasPrefix(link, "http://") {
		return
	}
	var c *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", link)
	case "darwin":
		c = exec.Command("open", link)
	default:
		c = exec.Command("xdg-open", link)
	}
	_ = c.Start()
}

func cmdLogout(out io.Writer) error {
	_, dir, err := loadConfig()
	if err != nil {
		return err
	}
	if err := (auth.Store{Dir: dir}).Forget(); err != nil {
		return err
	}
	fmt.Fprintln(out, "Signed out on this computer. To revoke it everywhere, remove it from your devices in the drive.")
	return nil
}

// ---- relay ----

func cmdRelay(args []string, stderr io.Writer) error {
	fl := flag.NewFlagSet("relay", flag.ContinueOnError)
	spoolDir := fl.String("spool", "", "the drop folders' root")
	allowRoot := fl.Bool("allow-root", false, "run as root anyway")
	background := fl.Bool("background", false, "detach from the console (used by install on Windows)")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if err := refuseRoot(*allowRoot); err != nil {
		return err
	}
	if *background && detach(os.Args[1:]) {
		return nil
	}
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	l, api, dir, err := signedIn()
	if err != nil {
		return err
	}
	log, closeLog := logger(dir, stderr)
	defer closeLog()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	self, err := api.Self(ctx)
	if err != nil {
		return fmt.Errorf("asking the drive about this computer: %w", err)
	}
	root := firstOf(*spoolDir, cfg.Spool)
	if root == "" {
		if self.Mode != "personal" {
			return fmt.Errorf("a shared relay needs --spool (or \"spool\" in olsyn-drive.json)")
		}
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, "Olsyn Drive Uploads")
	}
	store, err := state.Open(filepath.Join(dir, "state", "spool"))
	if err != nil {
		return err
	}
	sp := spool.New(spool.Config{Root: root, MaxBytes: int64(cfg.MaxSpoolGB * (1 << 30)), FreeFloor: int64(cfg.FreeFloorGB * (1 << 30)),
		Files: cfg.Files, Version: version, Machine: machineName(), Settle: time.Duration(cfg.SettleSeconds) * time.Second}, api, store, log)
	started := time.Now()
	snap := func() status.Snapshot {
		st := sp.Stats()
		s := status.Snapshot{Mode: "relay", Node: l.NodeName, Person: l.Person, Drive: l.Drive, Version: version, Spool: root,
			Started: started, Waiting: st.Waiting, Queued: st.Queued, Held: st.Held, Failed: st.Failed, Unmapped: st.Unmapped,
			SpoolBytes: st.SpoolBytes, FreeBytes: st.FreeBytes, Full: st.Full, SentFiles: st.SentFiles, SentBytes: st.SentBytes,
			LastScan: st.LastScan, LastError: st.LastError}
		for _, p := range st.HeldReasons {
			s.Problems = append(s.Problems, status.Problem{Path: p.Path, Reason: p.Reason})
		}
		if cur, err := api.Self(context.Background()); err == nil {
			for _, d := range cur.Drops {
				s.Drops = append(s.Drops, status.Drop{Path: firstOf(d.Path, d.Label), Folder: d.FolderName, Policy: d.OnConflict})
			}
		}
		return s
	}
	go func() {
		if err := status.Serve(ctx, cfg.StatusAddr, snap); err != nil {
			log.Warn("status page", "err", err.Error())
		}
	}()
	log.Info("relay started", "node", l.NodeName, "mode", self.Mode, "spool", root, "version", version)
	return sp.Run(ctx)
}

// logger writes JSON lines to stderr and to olsyn-drive.log in the
// client's folder (kept under 10 MB: the old one becomes .1).
func logger(dir string, stderr io.Writer) (*slog.Logger, func()) {
	p := filepath.Join(dir, "olsyn-drive.log")
	if st, err := os.Stat(p); err == nil && st.Size() > 10<<20 {
		os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return slog.New(slog.NewJSONHandler(stderr, nil)), func() {}
	}
	return slog.New(slog.NewJSONHandler(io.MultiWriter(stderr, f), nil)), func() { f.Close() }
}

// ---- status ----

func cmdStatus(out io.Writer) error {
	cfg, dir, err := loadConfig()
	if err != nil {
		return err
	}
	if l, _, err := (auth.Store{Dir: dir}).Load(); err == nil {
		fmt.Fprintf(out, "Signed in to %s as %s (%s).\n", l.Drive, l.Person, l.NodeName)
	} else {
		fmt.Fprintln(out, "Not signed in: run olsyn-drive login.")
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + cfg.StatusAddr + "/status")
	if err != nil {
		fmt.Fprintln(out, "The drop-folder client isn't running.")
		return nil
	}
	defer resp.Body.Close()
	var s status.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return err
	}
	fmt.Fprintf(out, "Drop folders under %s: %d arriving, %d queued, %d held, %d failed; %d files (%s) delivered since %s.\n",
		s.Spool, s.Waiting, s.Queued, s.Held, s.Failed, s.SentFiles, status.Bytes(s.SentBytes), s.Started.Local().Format("2 Jan 15:04"))
	if s.Full {
		fmt.Fprintln(out, "The spool is FULL: new files are refused until it drains.")
	}
	for _, p := range s.Problems {
		fmt.Fprintf(out, "  held: %s: %s\n", p.Path, p.Reason)
	}
	fmt.Fprintf(out, "Details: http://%s/\n", cfg.StatusAddr)
	return nil
}

// ---- helpers shared by send ----

var uuidRE = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// folderID takes a drive folder link (…/n/<id>, …/folders/<id>, any link
// whose last id is the folder) or a bare id.
func folderID(s string) (string, error) {
	ids := uuidRE.FindAllString(s, -1)
	if len(ids) == 0 {
		return "", fmt.Errorf("%q isn't a drive folder link or id (copy the folder's link from the drive)", s)
	}
	return strings.ToLower(ids[len(ids)-1]), nil
}
