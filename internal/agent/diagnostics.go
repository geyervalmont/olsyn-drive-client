package agent

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

// maxLogBytes is how much log (newest first) goes into a bundle.
const maxLogBytes = 20 << 20

// Diagnostics writes a zip of state, settings, transfer counts, system facts
// and recent logs, with secrets and names redacted, and returns its path.
func (a *Agent) Diagnostics(ctx context.Context) (string, int64, error) {
	salt := make([]byte, 16)
	rand.Read(salt)
	rd := redactor{salt: salt}

	dir := filepath.Join(a.opts.Paths.Logs, "diagnostics")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, fmt.Errorf("create diagnostics folder: %w", err)
	}
	name := "olsyn-drive-diagnostics-" + time.Now().UTC().Format("20060102T150405Z") + ".zip"
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, fmt.Errorf("create diagnostics bundle: %w", err)
	}
	zw := zip.NewWriter(f)
	addJSON := func(name string, v any) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		redacted, err := rd.json(data)
		if err != nil {
			return err
		}
		_, err = w.Write(redacted)
		return err
	}

	a.mu.Lock()
	s := a.settings
	a.mu.Unlock()
	transfers := map[string]any{}
	if p, err := a.opts.FS.Transfers(ctx, TransferQuery{States: []string{TransferHeld, TransferFailed}, Limit: 200}); err == nil {
		problems := []map[string]any{}
		for _, t := range p.Items {
			item := map[string]any{"id": t.ID, "state": t.State, "direction": t.Direction, "size": t.Size, "attempts": t.Attempts, "name": t.Name}
			if t.Error != nil {
				item["error_code"] = t.Error.Code
			}
			if t.Held != nil {
				item["held_code"] = t.Held.Code
			}
			problems = append(problems, item)
		}
		transfers["counts"] = p.Counts
		transfers["problems"] = problems
	}
	free, _ := a.opts.System.FreeDiskBytes(s.CacheDir)
	system := map[string]any{
		"os": runtime.GOOS, "arch": runtime.GOARCH, "go": runtime.Version(),
		"version": a.opts.Version, "platform": a.opts.Platform, "cache_free_bytes": free,
		"agent_run": a.run, "started_at": a.start, "bundle_at": time.Now().UTC(),
	}
	err = addJSON("state.json", a.State())
	if err == nil {
		err = addJSON("settings.json", s)
	}
	if err == nil {
		err = addJSON("transfers.json", transfers)
	}
	if err == nil {
		err = addJSON("system.json", system)
	}
	if err == nil {
		err = a.addLogs(zw, rd)
	}
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return "", 0, fmt.Errorf("write diagnostics bundle: %w", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", 0, fmt.Errorf("stat diagnostics bundle: %w", err)
	}
	a.log.Info("diagnostics bundle written", "bytes", fi.Size())
	return path, fi.Size(), nil
}

// addLogs adds the newest log files, up to maxLogBytes, each line redacted.
func (a *Agent) addLogs(zw *zip.Writer, rd redactor) error {
	entries, err := os.ReadDir(a.opts.Paths.Logs)
	if err != nil {
		return nil // no logs yet
	}
	type logFile struct {
		name string
		mod  time.Time
		size int64
	}
	var files []logFile
	for _, e := range entries {
		if e.IsDir() || !strings.Contains(e.Name(), ".log") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, logFile{e.Name(), fi.ModTime(), fi.Size()})
	}
	slices.SortFunc(files, func(x, y logFile) int { return y.mod.Compare(x.mod) })
	budget := int64(maxLogBytes)
	for _, lf := range files {
		if budget <= 0 {
			break
		}
		src, err := os.Open(filepath.Join(a.opts.Paths.Logs, lf.name))
		if err != nil {
			continue
		}
		if lf.size > budget {
			src.Seek(lf.size-budget, io.SeekStart)
		}
		w, err := zw.Create("logs/" + lf.name)
		if err != nil {
			src.Close()
			return err
		}
		sc := bufio.NewScanner(io.LimitReader(src, budget))
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := rd.line(sc.Bytes())
			w.Write(line)
			w.Write([]byte{'\n'})
		}
		budget -= lf.size
		src.Close()
	}
	return nil
}

// redactor removes secrets and personal data. Names become <name:xxxxxxxx>,
// an HMAC with a per-bundle salt, so one name matches itself across the
// bundle but cannot be looked up from outside it.
type redactor struct {
	salt []byte
}

var (
	secretKeys = map[string]bool{"token": true, "device_code": true, "authorization": true, "secret": true, "password": true, "cookie": true}
	nameKeys   = map[string]bool{"name": true, "email": true, "device_name": true, "user_id": true, "device_id": true, "user_code": true, "machine": true}
	pathKeys   = map[string]bool{"path": true, "local_path": true, "drive_path": true, "conflict_copy": true, "cache_dir": true, "dest_dir": true, "file": true, "dir": true, "mount_path": true, "drive_folder": true}

	tokenRE  = regexp.MustCompile(`\b(odt|odl|odp)_[A-Za-z0-9_\-]+`)
	bearerRE = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-~+/=]+`)
	emailRE  = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	winPath  = regexp.MustCompile(`[A-Za-z]:\\[^"\r\n\t]*`)
	unixPath = regexp.MustCompile(`/(?:Users|home|Volumes|mnt)/[^"\s]*`)
)

func (r redactor) hash(s string) string {
	m := hmac.New(sha256.New, r.salt)
	m.Write([]byte(strings.ToLower(s)))
	return hex.EncodeToString(m.Sum(nil))[:8]
}

func (r redactor) name(s string) string {
	if s == "" {
		return s
	}
	return "<name:" + r.hash(s) + ">"
}

// path hashes every component but keeps separators, a drive letter and the
// structure, so "W:\Projects\a.rvt" stays readable as a shape.
func (r redactor) path(s string) string {
	var b strings.Builder
	start := 0
	flush := func(end int) {
		part := s[start:end]
		switch {
		case part == "":
		case len(part) == 2 && part[1] == ':':
			b.WriteString(part)
		default:
			b.WriteString(r.name(part))
		}
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '/' || s[i] == '\\' {
			flush(i)
			b.WriteByte(s[i])
			start = i + 1
		}
	}
	flush(len(s))
	return b.String()
}

func (r redactor) text(s string) string {
	s = tokenRE.ReplaceAllString(s, "<token>")
	s = bearerRE.ReplaceAllString(s, "Bearer <token>")
	s = emailRE.ReplaceAllStringFunc(s, r.name)
	s = winPath.ReplaceAllStringFunc(s, r.path)
	s = unixPath.ReplaceAllStringFunc(s, r.path)
	return s
}

// json redacts a JSON document by key, then every remaining string by pattern.
func (r redactor) json(data []byte) ([]byte, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.MarshalIndent(r.value("", v), "", "  ")
}

func (r redactor) value(key string, v any) any {
	k := strings.ToLower(key)
	switch x := v.(type) {
	case map[string]any:
		for kk, vv := range x {
			x[kk] = r.value(kk, vv)
		}
		return x
	case []any:
		for i := range x {
			x[i] = r.value(key, x[i])
		}
		return x
	case string:
		switch {
		case secretKeys[k]:
			return "<redacted>"
		case pathKeys[k]:
			return r.path(x)
		case nameKeys[k]:
			return r.name(x)
		}
		return r.text(x)
	}
	return v
}

// line redacts one log line: JSON lines (slog) by key, others by pattern.
func (r redactor) line(b []byte) []byte {
	t := bytes.TrimSpace(b)
	if len(t) > 0 && t[0] == '{' {
		if out, err := r.json(t); err == nil {
			var compact bytes.Buffer
			if json.Compact(&compact, out) == nil {
				return compact.Bytes()
			}
		}
	}
	return []byte(r.text(string(b)))
}
