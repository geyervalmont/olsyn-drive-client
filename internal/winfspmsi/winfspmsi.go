// Package winfspmsi names the WinFsp installer (MSI) the Windows program
// needs when the WinFsp driver is missing, and downloads it from the WinFsp
// project's own GitHub release, checking its size and SHA-256 against the
// pins below. The program does not carry WinFsp: it links to it. The
// downloaded file is then checked again, with its Authenticode signature,
// before it runs (platform.InstallDriver).
//
// WinFsp is (c) Bill Zissimopoulos and is used under the WinFsp FLOSS
// exception (see NOTICE). The file is the publisher's, byte for byte.
package winfspmsi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const (
	// Version of the pinned installer.
	Version = "2.2.26323"
	// File is the release asset name.
	File = "winfsp-" + Version + ".msi"
	// ReleaseTag is the WinFsp project's release the installer comes from;
	// SourceURL is that release's complete source code.
	ReleaseTag = "v2.2"
	SourceURL  = "https://github.com/winfsp/winfsp/tree/" + ReleaseTag
	// URL is the official download of the installer: the release asset on
	// the WinFsp project's GitHub releases.
	URL = "https://github.com/winfsp/winfsp/releases/download/" + ReleaseTag + "/" + File
	// DownloadPage is where a person can get WinFsp's signed installer
	// themselves when the download from this program does not work.
	DownloadPage = "https://winfsp.dev/rel/"
	// SHA256 pins the release asset (checked after the download, and again
	// before it is run elevated).
	SHA256 = "36ce01f9eedbbe6842b7fb4bedc79d7d4f71983e8aceb6877b46a07b331c7e19"
	// Size of the release asset in bytes.
	Size = 2220032
	// MinVersion is the oldest installed WinFsp (DLL file version) the
	// client works with: an installed WinFsp at least this new is used as
	// it is and the installer is not run.
	MinVersion = "2.0"
)

// ErrUnavailable means the installer could not be downloaded intact: no
// network, a download blocked on the way, or a file that is not the pinned
// one. The person can install WinFsp from DownloadPage instead.
var ErrUnavailable = errors.New("the WinFsp installer could not be downloaded")

// Fetcher downloads the pinned installer. The zero value fetches URL with
// http.DefaultClient and checks Size and SHA256; tests set the fields.
type Fetcher struct {
	HTTP   *http.Client
	URL    string
	Size   int64
	SHA256 string
}

// progressStep is how often (in bytes) Fetch reports progress.
const progressStep = 128 << 10

// Fetch downloads the installer to dst, which must not exist, and checks its
// size and SHA-256. progress (may be nil) hears bytes done of the total as
// the download goes. On any failure dst is removed and the error wraps
// ErrUnavailable.
func (f Fetcher) Fetch(ctx context.Context, dst string, progress func(done, total int64)) error {
	if progress == nil {
		progress = func(int64, int64) {}
	}
	client, url, size, sum := f.HTTP, f.URL, f.Size, f.SHA256
	if client == nil {
		client = http.DefaultClient
	}
	if url == "" {
		url = URL
	}
	if size == 0 {
		size = Size
	}
	if sum == "" {
		sum = SHA256
	}
	err := fetch(ctx, client, url, dst, size, sum, progress)
	if err != nil {
		os.Remove(dst)
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}

func fetch(ctx context.Context, client *http.Client, url, dst string, size int64, sum string, progress func(done, total int64)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build the download request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", File, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: the server answered %s", File, resp.Status)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != size {
		return fmt.Errorf("download %s: the server offers %d bytes, not the expected %d", File, resp.ContentLength, size)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	h := sha256.New()
	pw := &progressWriter{total: size, report: progress}
	// One byte more than expected is read so that a longer file is noticed.
	n, err := io.Copy(io.MultiWriter(out, h, pw), io.LimitReader(resp.Body, size+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", File, err)
	}
	progress(n, size)
	if n != size {
		return fmt.Errorf("the downloaded %s has %d bytes, not the expected %d", File, n, size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, sum) {
		return fmt.Errorf("the downloaded %s has SHA-256 %s, not the expected %s", File, got, sum)
	}
	return nil
}

type progressWriter struct {
	done, total, reported int64
	report                func(done, total int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if p.done-p.reported >= progressStep {
		p.reported = p.done
		p.report(p.done, p.total)
	}
	return len(b), nil
}
