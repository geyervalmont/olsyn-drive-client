package fakedrive

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
)

// rateWindow counts calls in fixed windows, as a per-person limit does.
type rateWindow struct {
	start time.Time
	n     int
}

// rateLimitedLocked counts one call against the named limit and, when it
// is over limit calls in the current window, answers 429 rate_limited with
// Retry-After (whole seconds until the window ends) and reports true.
// limit <= 0 means no limit.
func (s *Server) rateLimitedLocked(w http.ResponseWriter, name string, limit int) bool {
	if limit <= 0 {
		return false
	}
	now := time.Now()
	rw := s.rates[name]
	if rw == nil || now.Sub(rw.start) >= s.opts.RateWindow {
		rw = &rateWindow{start: now}
		s.rates[name] = rw
	}
	rw.n++
	if rw.n <= limit {
		return false
	}
	s.counts[name+".429"]++
	left := rw.start.Add(s.opts.RateWindow).Sub(now)
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(left.Seconds())))))
	fail(w, http.StatusTooManyRequests, driveapi.CodeRateLimited, "too many requests", nil)
	return true
}

// SetRateLimits changes the per-window limits of pre-checks and upload
// starts (see Options.CheckRate, UploadStartRate, RateWindow); counting
// starts again.
func (s *Server) SetRateLimits(check, uploadStart int, window time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.CheckRate, s.opts.UploadStartRate = check, uploadStart
	if window > 0 {
		s.opts.RateWindow = window
	}
	clear(s.rates)
}

// modifiedAtOK reports whether the drive accepts t as a client modified
// time: from the Windows FILETIME epoch to ten minutes past now.
func modifiedAtOK(t time.Time) bool {
	return !t.Before(time.Date(1601, 1, 1, 0, 0, 0, 0, time.UTC)) && !t.After(time.Now().Add(10*time.Minute))
}
