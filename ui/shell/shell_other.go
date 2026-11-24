//go:build !windows

package shell

import (
	"context"
	"errors"
)

// Run reports that the desktop shell is Windows-only for now.
func Run(ctx context.Context, opts Options) error {
	return errors.New("the Olsyn Drive desktop app runs on Windows for now; use the front end's mock (npm run dev) elsewhere")
}
