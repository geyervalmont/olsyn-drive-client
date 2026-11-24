// Command verify checks a downloaded WinFsp installer against the size and
// SHA-256 pinned in package winfspmsi (used by make before embedding it).
//
//	go run ./internal/winfspmsi/verify build/winfsp-2.2.26323.msi
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/geyervalmont/olsyn-drive-client/internal/winfspmsi"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: verify <winfsp msi>")
		os.Exit(2)
	}
	if err := verify(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		os.Exit(1)
	}
}

func verify(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != winfspmsi.SHA256 || n != winfspmsi.Size {
		return fmt.Errorf("%s is %d bytes with SHA-256 %s; want %d bytes, %s", path, n, got, winfspmsi.Size, winfspmsi.SHA256)
	}
	fmt.Printf("%s: %d bytes, SHA-256 %s (WinFsp %s): ok\n", path, n, winfspmsi.SHA256, winfspmsi.Version)
	return nil
}
