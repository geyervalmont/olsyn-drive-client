package fakedrive

import (
	"crypto/rand"
	"html/template"
	"net/http"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
)

// userCode is a code like "BCDF-GHJK" (consonants only, as the real drive).
func userCode() string {
	const letters = "BCDFGHJKLMNPQRSTVWXZ"
	var b [8]byte
	_, _ = rand.Read(b[:])
	out := make([]byte, 0, 9)
	for i, c := range b {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, letters[int(c)%len(letters)])
	}
	return string(out)
}

var linkTmpl = template.Must(template.New("link").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Fake drive: sign in a computer</title>
<style>body{font:16px system-ui,sans-serif;max-width:34rem;margin:3rem auto;padding:0 1rem}
code{font-size:1.6rem;letter-spacing:.1em}button{font:inherit;padding:.5rem 1.2rem;margin-right:.5rem}
.note{color:#555}</style></head><body>
<h1>Sign in a computer</h1>
<p class="note">This is the fake drive for testing. No real account is involved.</p>
{{if .Done}}<p><strong>{{.Done}}</strong> You can close this page.</p>
{{else if .Code}}<p>Check that this code matches the one the app shows:</p>
<p><code>{{.Code}}</code></p>
<form method="post" action="/link"><input type="hidden" name="code" value="{{.Code}}">
<button name="decision" value="approve">Approve</button>
<button name="decision" value="deny">Deny</button></form>
{{else}}<p>There is no sign-in waiting for that code. Start signing in from the app, then open the link it gives.</p>{{end}}
</body></html>
`))

type linkView struct {
	Code string
	Done string
}

// linkPage shows the code a sign-in is waiting on, with Approve and Deny.
func (s *Server) linkPage(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	s.mu.Lock()
	l := s.pendingLinkLocked(code)
	s.mu.Unlock()
	v := linkView{}
	if l != nil {
		v.Code = l.userCode
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = linkTmpl.Execute(w, v)
}

// linkDecide approves or denies the sign-in with the posted code.
func (s *Server) linkDecide(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		fail(w, http.StatusBadRequest, driveapi.CodeInvalid, "bad form", nil)
		return
	}
	s.mu.Lock()
	l := s.pendingLinkLocked(r.PostForm.Get("code"))
	v := linkView{}
	switch {
	case l == nil:
	case r.PostForm.Get("decision") == "approve":
		l.approved = true
		v.Done = "Approved. The app signs in within a few seconds."
	default:
		l.denied = true
		v.Done = "Denied. The app will say so."
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = linkTmpl.Execute(w, v)
}

func (s *Server) pendingLinkLocked(code string) *link {
	if code == "" {
		return nil
	}
	for _, l := range s.links {
		if l.userCode == code && !l.claimed && !l.approved && !l.denied && time.Now().Before(l.expires) {
			return l
		}
	}
	return nil
}
