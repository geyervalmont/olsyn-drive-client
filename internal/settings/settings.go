// Package settings is the agent's versioned settings file: defaults,
// validation, JSON merge patches and atomic writes.
package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"unicode"
)

const (
	// CurrentVersion is the version this code writes.
	CurrentVersion = 1
	// GiB is 2^30 bytes.
	GiB int64 = 1 << 30
	// MaxDefaultCacheCap caps the default cache size.
	MaxDefaultCacheCap = 50 * GiB
	// MinCacheCap is the smallest cache allowed.
	MinCacheCap = 1 * GiB
	// DefaultServerURL is the public product URL; any drive server can be configured.
	DefaultServerURL = "https://drive.olsyn.com"
	// DefaultDriveLetter is the Windows drive letter used unless the person picks another.
	DefaultDriveLetter = "W:"
)

// ErrNewerVersion means the file was written by a newer client.
var ErrNewerVersion = errors.New("settings file was written by a newer version")

// Bandwidth limits in bytes per second; 0 means unlimited.
type Bandwidth struct {
	UpBPS   int64 `json:"up_bps"`
	DownBPS int64 `json:"down_bps"`
}

// CacheRelease says how soon a file uploaded from this PC stops taking space
// here, once the drive has confirmed it. Seconds; 0 means at once.
type CacheRelease struct {
	// AfterUploadSeconds is the grace period after an upload (default 300).
	AfterUploadSeconds int64 `json:"after_upload_seconds"`
	// KeepOpenedSeconds keeps a file an application read within this long
	// (default 1800).
	KeepOpenedSeconds int64 `json:"keep_opened_seconds"`
}

// Default release timings.
const (
	DefaultAfterUploadSeconds = 5 * 60
	DefaultKeepOpenedSeconds  = 30 * 60
	maxReleaseSeconds         = 7 * 24 * 60 * 60
)

// Notifications says which notifications the person wants.
type Notifications struct {
	Finished      bool `json:"finished"`
	Held          bool `json:"held"`
	SignedOut     bool `json:"signed_out"`
	UpdateApplied bool `json:"update_applied"`
}

// Settings are the person's choices. The JSON names are the IPC contract's.
type Settings struct {
	DriveLetter   string        `json:"drive_letter"`
	CacheDir      string        `json:"cache_dir"`
	CacheCapBytes int64         `json:"cache_cap_bytes"`
	CacheRelease  CacheRelease  `json:"cache_release"`
	Bandwidth     Bandwidth     `json:"bandwidth"`
	StartAtLogin  bool          `json:"start_at_login"`
	UpdateChannel string        `json:"update_channel"`
	Notifications Notifications `json:"notifications"`
	ServerURL     string        `json:"server_url"`
	DeviceName    string        `json:"device_name"`
}

// Env is what defaults depend on.
type Env struct {
	CacheDir      string // default cache folder
	FreeDiskBytes uint64 // free space where the cache goes
}

// DefaultCacheCap is min(50 GiB, 20% of free disk), but never below MinCacheCap.
func DefaultCacheCap(free uint64) int64 {
	c := int64(free / 5)
	if free/5 > uint64(MaxDefaultCacheCap) {
		c = MaxDefaultCacheCap
	}
	return max(c, MinCacheCap)
}

// Defaults returns the settings of a fresh install.
func Defaults(env Env) Settings {
	return Settings{
		DriveLetter:   DefaultDriveLetter,
		CacheDir:      env.CacheDir,
		CacheCapBytes: DefaultCacheCap(env.FreeDiskBytes),
		CacheRelease:  CacheRelease{AfterUploadSeconds: DefaultAfterUploadSeconds, KeepOpenedSeconds: DefaultKeepOpenedSeconds},
		StartAtLogin:  true,
		UpdateChannel: "stable",
		Notifications: Notifications{Finished: true, Held: true, SignedOut: true, UpdateApplied: true},
		ServerURL:     DefaultServerURL,
	}
}

// FieldError names the setting that failed validation, dotted as in JSON.
type FieldError struct {
	Field   string
	Message string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Message }

// NormalizeDriveLetter turns "w", "W" or "w:" into "W:".
func NormalizeDriveLetter(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) == 1 {
		s += ":"
	}
	return s
}

// Validate checks every field and returns the first *FieldError.
func (s Settings) Validate() error {
	if l := s.DriveLetter; len(l) != 2 || l[1] != ':' || l[0] < 'D' || l[0] > 'Z' {
		return &FieldError{"drive_letter", "must be a letter from D: to Z:"}
	}
	if s.CacheDir == "" || !filepath.IsAbs(s.CacheDir) {
		return &FieldError{"cache_dir", "must be an absolute folder path"}
	}
	if s.CacheCapBytes < MinCacheCap {
		return &FieldError{"cache_cap_bytes", fmt.Sprintf("must be at least %d bytes (1 GiB)", MinCacheCap)}
	}
	if v := s.CacheRelease.AfterUploadSeconds; v < 0 || v > maxReleaseSeconds {
		return &FieldError{"cache_release.after_upload_seconds", "must be from 0 to 604800 seconds (7 days)"}
	}
	if v := s.CacheRelease.KeepOpenedSeconds; v < 0 || v > maxReleaseSeconds {
		return &FieldError{"cache_release.keep_opened_seconds", "must be from 0 to 604800 seconds (7 days)"}
	}
	if s.Bandwidth.UpBPS < 0 {
		return &FieldError{"bandwidth.up_bps", "must be 0 (unlimited) or more"}
	}
	if s.Bandwidth.DownBPS < 0 {
		return &FieldError{"bandwidth.down_bps", "must be 0 (unlimited) or more"}
	}
	if s.UpdateChannel != "stable" && s.UpdateChannel != "beta" {
		return &FieldError{"update_channel", "must be stable or beta"}
	}
	if err := validateServerURL(s.ServerURL); err != nil {
		return &FieldError{"server_url", err.Error()}
	}
	if len(s.DeviceName) > 64 {
		return &FieldError{"device_name", "must be at most 64 characters"}
	}
	for _, r := range s.DeviceName {
		if unicode.IsControl(r) {
			return &FieldError{"device_name", "must not contain control characters"}
		}
	}
	return nil
}

// validateServerURL accepts https URLs, and http only to a loopback address
// (a local drive server during development and tests).
func validateServerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("must be an absolute https URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("must not carry credentials, a query or a fragment")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
		return errors.New("http is allowed only for a server on this computer; use https")
	default:
		return errors.New("must be an https URL")
	}
}

// fileFormat is the JSON on disk: the version next to the settings.
type fileFormat struct {
	Version int `json:"version"`
	Settings
}

// Load reads the settings file. A missing file gives the defaults and
// created=true. Fields missing from the file keep their defaults.
func Load(path string, env Env) (s Settings, created bool, err error) {
	s = Defaults(env)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, true, nil
	}
	if err != nil {
		return s, false, fmt.Errorf("read settings: %w", err)
	}
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return s, false, fmt.Errorf("parse settings %s: %w", path, err)
	}
	if head.Version > CurrentVersion {
		return s, false, fmt.Errorf("%s has version %d: %w", path, head.Version, ErrNewerVersion)
	}
	// Version 0 (no field) and 1 share a format; later versions migrate here.
	f := fileFormat{Settings: s}
	if err := json.Unmarshal(data, &f); err != nil {
		return s, false, fmt.Errorf("parse settings %s: %w", path, err)
	}
	f.Settings.DriveLetter = NormalizeDriveLetter(f.Settings.DriveLetter)
	if err := f.Settings.Validate(); err != nil {
		return s, false, fmt.Errorf("settings %s: %w", path, err)
	}
	return f.Settings, false, nil
}

// Save validates s and writes it atomically: a temporary file in the same
// folder, flushed, then renamed over the old file.
func Save(path string, s Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(fileFormat{Version: CurrentVersion, Settings: s}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	return WriteFileAtomic(path, append(data, '\n'), 0o600)
}

// WriteFileAtomic writes data to path through a synced temporary file and a rename.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Chmod(perm); err != nil && runtime.GOOS != "windows" {
		f.Close()
		return fmt.Errorf("chmod %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	if runtime.GOOS != "windows" {
		if d, err := os.Open(dir); err == nil {
			d.Sync()
			d.Close()
		}
	}
	return nil
}

// Patch applies a JSON merge patch (RFC 7396) to s and validates the result.
// Unknown fields are refused with a *FieldError.
func Patch(s Settings, patch []byte) (Settings, error) {
	cur, err := json.Marshal(s)
	if err != nil {
		return s, fmt.Errorf("encode settings: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(cur, &doc); err != nil {
		return s, fmt.Errorf("decode settings: %w", err)
	}
	var p any
	if err := json.Unmarshal(patch, &p); err != nil {
		return s, &FieldError{"", "patch is not valid JSON"}
	}
	pm, ok := p.(map[string]any)
	if !ok {
		return s, &FieldError{"", "patch must be a JSON object"}
	}
	if err := checkKnown(doc, pm, ""); err != nil {
		return s, err
	}
	merged := mergePatch(doc, pm)
	out, err := json.Marshal(merged)
	if err != nil {
		return s, fmt.Errorf("encode merged settings: %w", err)
	}
	var next Settings
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&next); err != nil {
		return s, &FieldError{"", "a value has the wrong type: " + err.Error()}
	}
	next.DriveLetter = NormalizeDriveLetter(next.DriveLetter)
	if err := next.Validate(); err != nil {
		return s, err
	}
	return next, nil
}

func checkKnown(doc, patch map[string]any, prefix string) error {
	for k, v := range patch {
		cur, ok := doc[k]
		if !ok {
			return &FieldError{prefix + k, "unknown setting"}
		}
		if v == nil {
			return &FieldError{prefix + k, "cannot be removed"}
		}
		if sub, ok := v.(map[string]any); ok {
			cm, ok := cur.(map[string]any)
			if !ok {
				return &FieldError{prefix + k, "is not an object"}
			}
			if err := checkKnown(cm, sub, prefix+k+"."); err != nil {
				return err
			}
		}
	}
	return nil
}

func mergePatch(target map[string]any, patch map[string]any) map[string]any {
	for k, v := range patch {
		if v == nil {
			delete(target, k)
			continue
		}
		if pm, ok := v.(map[string]any); ok {
			tm, _ := target[k].(map[string]any)
			if tm == nil {
				tm = map[string]any{}
			}
			target[k] = mergePatch(tm, pm)
			continue
		}
		target[k] = v
	}
	return target
}

// Changed lists the dotted JSON names of fields that differ between a and b.
func Changed(a, b Settings) []string {
	var out []string
	diff(reflect.ValueOf(a), reflect.ValueOf(b), "", &out)
	return out
}

func diff(a, b reflect.Value, prefix string, out *[]string) {
	t := a.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if f.Type.Kind() == reflect.Struct {
			diff(a.Field(i), b.Field(i), prefix+name+".", out)
			continue
		}
		if !reflect.DeepEqual(a.Field(i).Interface(), b.Field(i).Interface()) {
			*out = append(*out, prefix+name)
		}
	}
}
