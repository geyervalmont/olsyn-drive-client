// Package frontend embeds the built front end (npm run build writes dist/).
// The Go files here are the only Go code in this folder.
package frontend

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Assets returns the built front end rooted at index.html, or an error when
// it was not built before compiling.
func Assets() (fs.FS, error) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, fmt.Errorf("front-end assets: %w", err)
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, errors.New("the front end was not built into this program (run make frontend, then build again)")
	}
	return sub, nil
}
