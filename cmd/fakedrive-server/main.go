//go:build devbuild

// Command fakedrive-server runs the fake Olsyn drive on this computer for
// testing the desktop client by hand (see docs/testing-windows.md). It is
// built only with -tags devbuild, listens only on a loopback address, and
// holds synthetic data only.
//
//	fakedrive-server [--addr 127.0.0.1:8787] [--faults 127.0.0.1:8788] [--auto-approve] [--data DIR]
//	                 [--release-exe olsyn-drive.exe [--release-version 1.0.0-dev] [--install-scripts scripts/install]]
//
// With --release-exe it also serves a signed release of that exe and the
// install scripts (release.go), to try the scripts by hand.
//
// Fault controls, on the --faults address:
//
//	POST /deploy?seconds=N   answer 503 + Retry-After for N seconds, as during a deploy
//	POST /offline?on=1|0     drop every connection (API and storage), or stop doing so
//	POST /revoke             revoke every token: the client must sign in again
//	POST /approve            approve every waiting sign-in (instead of the link page)
//	POST /corrupt?n=N        the next N completed uploads "arrive damaged" (checksum mismatch)
//	POST /checksum-pass      run the drive's checksum pass on claimed large files now
//	GET  /stats              what the drive holds and how often each thing happened
//	GET  /tree?path=/F       every file and folder below /F: path, kind, id, size, versions, sha256
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/fakedrive"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "fakedrive-server:", err)
		os.Exit(1)
	}
}

type config struct {
	addr, faults, data string
	autoApprove        bool
	checksums          string
	bigFolder          int
	virtualSize        int64
	releaseExe         string
	releaseVersion     string
	installScripts     string
}

func parse(args []string, stderr io.Writer) (config, error) {
	var c config
	fs := flag.NewFlagSet("fakedrive-server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.addr, "addr", "127.0.0.1:8787", "loopback address the drive API listens on")
	fs.StringVar(&c.faults, "faults", "127.0.0.1:8788", "loopback address of the fault controls")
	fs.StringVar(&c.data, "data", "", "keep uploaded content in files under this folder (default: in memory)")
	fs.BoolVar(&c.autoApprove, "auto-approve", false, "approve every sign-in at once (no link page step)")
	fs.StringVar(&c.checksums, "checksums", "verify", "large uploads: verify (claimed, verified 5 s later), never (stay claimed), none (a drive without checksum states)")
	fs.IntVar(&c.bigFolder, "big-folder", 12000, "how many files the large folder holds")
	gib := fs.Float64("virtual-gib", 5, "size of the generated large file, in GiB")
	fs.StringVar(&c.releaseExe, "release-exe", "", "serve this exe as the stable release's Windows installer (signed with a key made at start)")
	fs.StringVar(&c.releaseVersion, "release-version", "1.0.0-dev", "version of the served release")
	fs.StringVar(&c.installScripts, "install-scripts", "", "folder with install.ps1 and install.cmd, served at /install.ps1 and /install.cmd with this server's address filled in")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	c.virtualSize = int64(*gib * (1 << 30))
	for _, a := range []string{c.addr, c.faults} {
		if err := loopback(a); err != nil {
			return c, err
		}
	}
	switch c.checksums {
	case "verify", "never", "none":
	default:
		return c, fmt.Errorf("-checksums must be verify, never or none, not %q", c.checksums)
	}
	if c.bigFolder < 0 || c.virtualSize < 0 {
		return c, errors.New("sizes must not be negative")
	}
	return c, nil
}

// loopback refuses any address other people could reach.
func loopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("address %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("address %q is not a loopback address: the fake drive listens on this computer only", addr)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	c, err := parse(args, stderr)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	if c.data != "" {
		if err := os.MkdirAll(c.data, 0o700); err != nil {
			return fmt.Errorf("make the data folder: %w", err)
		}
	}
	// Listen on the faults port first, so a port in use fails before seeding.
	fl, err := net.Listen("tcp", c.faults)
	if err != nil {
		return fmt.Errorf("listen for fault controls: %w", err)
	}
	defer fl.Close()
	if l, err := net.Listen("tcp", c.addr); err != nil {
		return fmt.Errorf("listen for the drive: %w", err)
	} else {
		_ = l.Close()
	}
	fd := fakedrive.New(fakedrive.Options{Addr: c.addr, DataDir: c.data, AutoApprove: c.autoApprove, PartSize: 16 << 20, SingleMax: 32 << 20,
		ChecksumPassDelay: 5 * time.Second, NeverVerify: c.checksums == "never", NoChecksumState: c.checksums == "none"})
	defer fd.Close()
	start := time.Now()
	tree := seed(fd, c.bigFolder, c.virtualSize)
	log.Info("seeded", "folders", tree.folders, "files", tree.files, "took", time.Since(start).Round(time.Millisecond))

	var rel *devRelease
	if c.releaseExe != "" {
		if rel, err = newDevRelease(fd, c.releaseExe, c.releaseVersion, c.installScripts); err != nil {
			return err
		}
	}

	srv := &http.Server{Handler: faults(fd, log), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(fl) }()
	defer srv.Close()

	fmt.Fprintf(stdout, `Fake drive at %s (synthetic data only)
Fault controls at http://%s (POST /deploy?seconds=N, /offline?on=1|0, /revoke, /approve, /corrupt?n=N, /checksum-pass; GET /stats)
Sign-ins: %s
Point a dev build of the client at it:
  olsyn-drive agent --server %s
Ctrl+C stops it.
`, fd.URL(), c.faults, approveHint(c.autoApprove), fd.URL())
	if rel != nil {
		fmt.Fprintf(stdout, `Release %s (stable) of %s, signed with key %q.
Install it (in a normal PowerShell window):
  irm %s/install.ps1 | iex
A dev build has no release keys; to have it check this manifest's signature, set first:
  $env:OLSYN_DRIVE_DEV_RELEASE_KEYS = "%s=%s"
or let it install unchecked: & ([scriptblock]::Create((irm %s/install.ps1))) -AllowUnverified
`, c.releaseVersion, c.releaseExe, rel.keyID, fd.URL(), rel.keyID, rel.publicKey, fd.URL())
	}
	<-ctx.Done()
	log.Info("stopping")
	return nil
}

func approveHint(auto bool) string {
	if auto {
		return "approved at once (--auto-approve)"
	}
	return "approve on the link page the client opens, or POST /approve"
}

// faults serves the fault controls.
func faults(fd *fakedrive.Server, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, msg string) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, msg)
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		ok(w, "POST /deploy?seconds=N, POST /offline?on=1|0, POST /revoke, POST /approve, POST /corrupt?n=N, POST /checksum-pass, GET /stats, GET /tree?path=/F")
	})
	mux.HandleFunc("POST /deploy", func(w http.ResponseWriter, r *http.Request) {
		secs, err := strconv.Atoi(r.URL.Query().Get("seconds"))
		if err != nil || secs < 0 || secs > 3600 {
			http.Error(w, "seconds must be 0 to 3600", http.StatusBadRequest)
			return
		}
		fd.Unavailable(time.Duration(secs) * time.Second)
		log.Info("fault: deploy", "seconds", secs)
		ok(w, fmt.Sprintf("answering 503 for %d seconds", secs))
	})
	mux.HandleFunc("POST /offline", func(w http.ResponseWriter, r *http.Request) {
		var on bool
		switch r.URL.Query().Get("on") {
		case "1", "true":
			on = true
		case "0", "false":
		default:
			http.Error(w, "on must be 1 or 0", http.StatusBadRequest)
			return
		}
		fd.SetOffline(on)
		log.Info("fault: offline", "on", on)
		ok(w, map[bool]string{true: "offline: every connection is dropped", false: "online again"}[on])
	})
	mux.HandleFunc("POST /revoke", func(w http.ResponseWriter, r *http.Request) {
		fd.RevokeAll()
		log.Info("fault: every token revoked")
		ok(w, "every token revoked")
	})
	mux.HandleFunc("POST /approve", func(w http.ResponseWriter, r *http.Request) {
		fd.ApproveLinks()
		log.Info("sign-ins approved")
		ok(w, "waiting sign-ins approved")
	})
	mux.HandleFunc("POST /corrupt", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(r.URL.Query().Get("n"))
		if err != nil || n < 0 || n > 1000 {
			http.Error(w, "n must be 0 to 1000", http.StatusBadRequest)
			return
		}
		fd.CorruptNextUploads(n)
		log.Info("fault: corrupt uploads", "n", n)
		ok(w, fmt.Sprintf("the next %d completed uploads arrive damaged", n))
	})
	mux.HandleFunc("POST /checksum-pass", func(w http.ResponseWriter, r *http.Request) {
		ok(w, fmt.Sprintf("checked %d versions", fd.RunChecksumPass()))
	})
	mux.HandleFunc("GET /tree", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		if p == "" {
			p = "/"
		}
		entries, found := fd.Tree(p)
		if !found {
			http.Error(w, "no folder at "+p, http.StatusNotFound)
			return
		}
		if entries == nil {
			entries = []fakedrive.TreeEntry{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entries)
	})
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(fd.Stats())
	})
	return mux
}
