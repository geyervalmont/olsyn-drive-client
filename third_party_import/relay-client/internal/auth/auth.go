// Package auth signs this computer in to the drive and keeps the token.
//
// Sign-in is the drive's device-link flow: the client shows a short code
// and a link, the person approves it in their browser (already signed in
// there), and the client collects a token. The client never sees a
// password. The token is stored for the signed-in user only: on Windows
// encrypted with DPAPI (user scope), elsewhere in a 0600 file inside a 0700
// folder. It is never logged and never put on a command line.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"olsyn-drive/internal/driveapi"
)

// Login is what a sign-in leaves on disk besides the token.
type Login struct {
	Drive    string    `json:"drive"`
	NodeID   string    `json:"node_id"`
	NodeName string    `json:"node_name"`
	DeviceID string    `json:"device_id,omitempty"`
	Person   string    `json:"person,omitempty"` // name, for messages
	PersonID string    `json:"person_id,omitempty"`
	At       time.Time `json:"at"`
}

// Dir is the client's private folder: %AppData%\Olsyn Drive on Windows,
// ~/Library/Application Support/Olsyn Drive on macOS, ~/.config/olsyn-drive
// on Linux. OLSYN_DRIVE_HOME overrides it (tests, a shared node's service
// account).
func Dir() (string, error) {
	if d := os.Getenv("OLSYN_DRIVE_HOME"); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "linux" {
		return filepath.Join(base, "olsyn-drive"), nil
	}
	return filepath.Join(base, "Olsyn Drive"), nil
}

// Store is the sign-in on disk.
type Store struct{ Dir string }

func (s Store) loginPath() string { return filepath.Join(s.Dir, "login.json") }

// Load returns the sign-in and its token; fs.ErrNotExist when there's none.
func (s Store) Load() (*Login, string, error) {
	b, err := os.ReadFile(s.loginPath())
	if err != nil {
		return nil, "", err
	}
	var l Login
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, "", fmt.Errorf("the sign-in file is damaged; run olsyn-drive login again")
	}
	tok, err := loadToken(s.Dir)
	if err != nil {
		return nil, "", err
	}
	if !strings.HasPrefix(tok, "odt_") {
		return nil, "", fmt.Errorf("the stored sign-in is unreadable; run olsyn-drive login again")
	}
	return &l, tok, nil
}

// Save writes the sign-in, the token first.
func (s Store) Save(l *Login, token string) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(s.Dir, 0o700)
	if err := saveToken(s.Dir, token); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(l, "", "  ")
	return writePrivate(s.loginPath(), b)
}

// Forget removes the sign-in from this computer. The drive keeps the
// token valid until the person removes the device in the web app (the
// client says so).
func (s Store) Forget() error {
	var errs []error
	for _, p := range []string{s.loginPath(), tokenPath(s.Dir)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func writePrivate(p string, b []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0o600)
	return os.Rename(tmp, p)
}

// defaultPoll is how often to ask when the drive names no interval.
var defaultPoll = 5 * time.Second

// Prompt is how Login talks to the person.
type Prompt func(code, link string)

// SignIn runs the device-link flow against drive and stores the result.
// It waits until the person approves, the code expires, or ctx ends.
func SignIn(ctx context.Context, api *driveapi.Client, store Store, machine, version string, prompt Prompt) (*Login, error) {
	st, err := api.EnrollStart(ctx, machine, version)
	if err != nil {
		if driveapi.StatusOf(err) == 404 {
			return nil, fmt.Errorf("this drive doesn't offer the desktop client yet (relays are switched off)")
		}
		return nil, err
	}
	prompt(st.UserCode, st.VerificationURL)
	every := time.Duration(st.Interval) * time.Second
	if every < time.Second {
		every = defaultPoll
	}
	deadline := time.Now().Add(time.Duration(st.ExpiresIn) * time.Second)
	if st.ExpiresIn <= 0 {
		deadline = time.Now().Add(10 * time.Minute)
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(every):
		}
		res, err := api.EnrollToken(ctx, st.DeviceCode)
		switch s := driveapi.StatusOf(err); {
		case err == nil:
			l := &Login{Drive: api.Base.String(), NodeID: res.NodeID, NodeName: res.NodeName, DeviceID: res.DeviceID,
				Person: res.Person.Name, PersonID: res.Person.ID, At: time.Now().UTC()}
			if err := store.Save(l, res.Token); err != nil {
				return nil, fmt.Errorf("signed in, but couldn't store the sign-in: %w", err)
			}
			return l, nil
		case s == 428:
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("the code expired before it was approved; run login again")
			}
		case s == 403:
			return nil, fmt.Errorf("the sign-in was declined in the browser")
		case s == 410 || s == 404:
			return nil, fmt.Errorf("the code expired or was already used; run login again")
		case driveapi.Retryable(err):
			// keep polling through a drive restart
		default:
			return nil, err
		}
	}
}
