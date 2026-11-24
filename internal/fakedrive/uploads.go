package fakedrive

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
)

func (s *Server) partURLLocked(u *upload, number int) string {
	sig := randToken()
	s.sigs[sig] = time.Now().Add(time.Hour)
	return s.srv.URL + "/s3/" + u.id + "/" + strconv.Itoa(number) + "?X-Amz-Signature=" + sig
}

func (u *upload) partLen(number int) int64 {
	if u.method == driveapi.MethodSingle {
		return u.size
	}
	if number < u.partCount {
		return u.partSize
	}
	return u.size - u.partSize*int64(u.partCount-1)
}

func (u *upload) uploadedLocked() []driveapi.UploadedPart {
	out := []driveapi.UploadedPart{}
	for i := 1; i <= u.partCount; i++ {
		if p, ok := u.parts[i]; ok && p.seg.size == u.partLen(i) {
			out = append(out, driveapi.UploadedPart{Number: i, ETag: p.etag, Size: p.seg.size})
		}
	}
	return out
}

func (s *Server) ticketLocked(u *upload, resumed bool) driveapi.UploadTicket {
	t := driveapi.UploadTicket{UploadID: u.id, NodeID: u.nodeID, VersionID: u.versionID, Name: u.name, Method: u.method,
		Resumed: resumed, ExpiresAt: u.expires, Uploaded: u.uploadedLocked()}
	if u.method == driveapi.MethodSingle {
		t.URL = s.partURLLocked(u, 1)
		t.Headers = map[string]string{"Content-Type": u.mime, "If-None-Match": "*"}
		return t
	}
	t.PartSize, t.PartCount = u.partSize, u.partCount
	for i := 1; i <= u.partCount && len(t.Parts) < 32; i++ {
		if p, ok := u.parts[i]; ok && p.seg.size == u.partLen(i) {
			continue
		}
		t.Parts = append(t.Parts, driveapi.PartURL{Number: i, URL: s.partURLLocked(u, i)})
	}
	return t
}

func (s *Server) uploadStart(w http.ResponseWriter, r *http.Request) {
	s.count("upload.start")
	var req driveapi.StartUpload
	if !decode(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.nodes[req.ParentID]
	if p == nil || p.kind != driveapi.KindFolder || !s.liveLocked(p) {
		fail(w, http.StatusNotFound, driveapi.CodeNotFound, "no such folder", nil)
		return
	}
	if s.refuseLocked(w, p.id) {
		return
	}
	if !validName(req.Name) {
		fail(w, http.StatusUnprocessableEntity, driveapi.CodeInvalidName, "bad name", nil)
		return
	}
	if req.ModifiedAt != nil && req.ModifiedAt.After(time.Now().Add(10*time.Minute)) {
		fail(w, http.StatusUnprocessableEntity, driveapi.CodeInvalidModifiedAt, "in the future", map[string]any{"field": "modified_at"})
		return
	}
	if req.Fingerprint != "" {
		for _, u := range s.uploads {
			if u.parentID == req.ParentID && u.size == req.Size && u.fingerprint == req.Fingerprint {
				writeJSON(w, http.StatusCreated, s.ticketLocked(u, true))
				return
			}
		}
	}
	if s.quotaLocked(p.id, req.Size) {
		fail(w, http.StatusRequestEntityTooLarge, driveapi.CodeQuotaExceeded, "quota", nil)
		return
	}
	u := &upload{id: newID(), versionID: newID(), parentID: p.id, name: req.Name, size: req.Size, mime: req.Mime,
		conflict: req.Conflict, base: req.BaseVersionID, fingerprint: req.Fingerprint, parts: map[int]part{},
		created: time.Now().UTC(), expires: time.Now().Add(7 * 24 * time.Hour)}
	if u.mime == "" {
		u.mime = "application/octet-stream"
	}
	if req.ModifiedAt != nil {
		t := req.ModifiedAt.Time
		u.modifiedAt = &t
	}
	if existing := s.childByNameLocked(p.id, req.Name); existing != nil {
		switch req.Conflict {
		case driveapi.ConflictNewVersion:
			if existing.kind != driveapi.KindFile {
				fail(w, http.StatusConflict, driveapi.CodeNameConflict, "a folder has that name", nil)
				return
			}
			if req.BaseVersionID != "" && existing.current() != nil && existing.current().id != req.BaseVersionID {
				fail(w, http.StatusConflict, driveapi.CodeVersionConflict, "the file changed", nil)
				return
			}
			u.nodeID = existing.id
		case driveapi.ConflictRename:
			u.name = freeName(s, p.id, req.Name)
		default:
			fail(w, http.StatusConflict, driveapi.CodeNameConflict, "name taken", nil)
			return
		}
	}
	if u.nodeID == "" {
		u.nodeID = newID()
	}
	if req.Size <= s.opts.SingleMax {
		u.method, u.partCount = driveapi.MethodSingle, 1
	} else {
		u.method, u.partSize = driveapi.MethodMultipart, s.opts.PartSize
		u.partCount = int((req.Size + u.partSize - 1) / u.partSize)
	}
	s.uploads[u.id] = u
	writeJSON(w, http.StatusCreated, s.ticketLocked(u, false))
}

func (s *Server) storagePut(w http.ResponseWriter, r *http.Request) {
	s.count("part.put")
	number, _ := strconv.Atoi(r.PathValue("part"))
	sig := r.URL.Query().Get("X-Amz-Signature")
	s.mu.Lock()
	exp, okSig := s.sigs[sig]
	u := s.uploads[r.PathValue("upload")]
	drop := int64(-1)
	if s.faults.dropPuts > 0 {
		s.faults.dropPuts--
		drop = s.faults.dropPutAfter
	}
	if u != nil && u.method == driveapi.MethodMultipart {
		s.counts["part.put.multipart"]++ // large files' parts alone, for resume checks
	}
	s.mu.Unlock()
	storageErr := func(status int, code string) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "<Error><Code>"+code+"</Code><Message>fake storage</Message></Error>")
	}
	if !okSig || time.Now().After(exp) {
		storageErr(http.StatusForbidden, "AccessDenied")
		return
	}
	if u == nil {
		storageErr(http.StatusNotFound, driveapi.CodeNoSuchUpload)
		return
	}
	if drop >= 0 {
		_, _ = io.CopyN(io.Discard, r.Body, drop)
		hangUp(w)
		return
	}
	if number < 1 || number > u.partCount {
		storageErr(http.StatusBadRequest, "InvalidPart")
		return
	}
	dir := ""
	if s.opts.DataDir != "" {
		dir = filepath.Join(s.opts.DataDir, "parts", u.id)
	}
	seg, tag, err := storePart(dir, strconv.Itoa(number), r.Body, u.partLen(number))
	if err != nil {
		return // connection gone
	}
	if seg.size != r.ContentLength {
		storageErr(http.StatusBadRequest, "IncompleteBody")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uploads[u.id] == nil {
		storageErr(http.StatusNotFound, driveapi.CodeNoSuchUpload)
		return
	}
	if u.method == driveapi.MethodSingle && r.Header.Get("If-None-Match") == "*" {
		if _, exists := u.parts[1]; exists {
			storageErr(http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
	}
	u.parts[number] = part{seg: seg, etag: tag}
	w.Header().Set("ETag", tag)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) uploadParts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Numbers []int `json:"numbers"`
	}
	if !decode(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.uploads[r.PathValue("id")]
	if u == nil {
		fail(w, http.StatusNotFound, driveapi.CodeNotFound, "no such upload", nil)
		return
	}
	if s.refuseLocked(w, u.parentID) {
		return
	}
	if len(req.Numbers) == 0 || len(req.Numbers) > 100 {
		fail(w, http.StatusUnprocessableEntity, driveapi.CodeInvalid, "1..100 numbers", nil)
		return
	}
	out := []driveapi.PartURL{}
	for _, n := range req.Numbers {
		if n < 1 || n > u.partCount {
			fail(w, http.StatusUnprocessableEntity, driveapi.CodeInvalid, "bad part number", nil)
			return
		}
		out = append(out, driveapi.PartURL{Number: n, URL: s.partURLLocked(u, n)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"parts": out})
}

func (s *Server) uploadComplete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Parts      []map[string]any `json:"parts"`
		ModifiedAt *driveapi.Time   `json:"modified_at"`
		SHA256     string           `json:"sha256"`
	}
	if !decode(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")
	if nodeID, ok := s.completed[id]; ok {
		writeJSON(w, http.StatusOK, s.toAPILocked(s.nodes[nodeID]))
		return
	}
	u := s.uploads[id]
	if u == nil {
		fail(w, http.StatusNotFound, driveapi.CodeNotFound, "no such upload", nil)
		return
	}
	if s.refuseLocked(w, u.parentID) {
		return
	}
	var content blob
	if u.method == driveapi.MethodSingle {
		p, ok := u.parts[1]
		if !ok {
			fail(w, http.StatusUnprocessableEntity, driveapi.CodeInvalid, "the file hasn't been uploaded yet", nil)
			return
		}
		if p.seg.size != u.size {
			fail(w, http.StatusUnprocessableEntity, driveapi.CodeInvalid, "wrong size", nil)
			return
		}
		content.segs = []segment{p.seg}
	} else {
		for i := 1; i <= u.partCount; i++ {
			p, ok := u.parts[i]
			if !ok || p.seg.size != u.partLen(i) {
				fail(w, http.StatusConflict, driveapi.CodeUploadIncomplete, "part "+strconv.Itoa(i)+" is missing", nil)
				return
			}
			content.segs = append(content.segs, p.seg)
		}
	}
	// Small files are hashed here; large ones with a client hash are
	// published as claimed and checked by the checksum pass later.
	if req.SHA256 != "" {
		s.counts["upload.sha256_sent"]++
	}
	corrupt := s.faults.corruptUploads > 0
	if corrupt {
		s.faults.corruptUploads--
	}
	sha, state := "", ""
	if u.size <= s.opts.VerifyMax {
		sha = content.sha256()
		if corrupt {
			sha = strings.Repeat("0", 64) // what "storage" holds differs
		}
		if req.SHA256 != "" {
			if sha != req.SHA256 {
				s.counts["upload.checksum_mismatch"]++
				fail(w, http.StatusUnprocessableEntity, driveapi.CodeChecksumMismatch, "checksum differs", nil)
				return
			}
			state = driveapi.ChecksumVerified
		}
	} else if req.SHA256 != "" {
		state = driveapi.ChecksumClaimed
	}
	n := s.nodes[u.nodeID]
	if n == nil {
		// a new file: the name may have been taken meanwhile
		if other := s.childByNameLocked(u.parentID, u.name); other != nil {
			fail(w, http.StatusConflict, driveapi.CodeNameConflict, "name taken", nil)
			return
		}
		now := time.Now().UTC()
		n = &node{id: u.nodeID, parentID: u.parentID, kind: driveapi.KindFile, name: u.name, created: now, updated: now}
		s.nodes[n.id] = n
	} else if u.base != "" && n.current() != nil && n.current().id != u.base {
		fail(w, http.StatusConflict, driveapi.CodeVersionConflict, "the file changed", nil)
		return
	}
	mt := u.modifiedAt
	if req.ModifiedAt != nil {
		t := req.ModifiedAt.Time
		mt = &t
	}
	v := s.addBlobLocked(n, content, sha, u.fingerprint, mt)
	v.id = u.versionID
	v.checksumState = state
	if state == driveapi.ChecksumClaimed {
		v.claimed, v.corrupt = req.SHA256, corrupt
		if d := s.opts.ChecksumPassDelay; d > 0 {
			time.AfterFunc(d, func() { s.RunChecksumPass() })
		}
	}
	delete(s.uploads, u.id)
	s.completed[u.id] = n.id
	s.counts["upload.complete"]++
	writeJSON(w, http.StatusOK, s.toAPILocked(n))
}

func (s *Server) uploadAbort(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uploads[r.PathValue("id")] == nil {
		fail(w, http.StatusNotFound, driveapi.CodeNotFound, "no such upload", nil)
		return
	}
	delete(s.uploads, r.PathValue("id"))
	if s.opts.DataDir != "" {
		_ = os.RemoveAll(filepath.Join(s.opts.DataDir, "parts", r.PathValue("id")))
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) statusLocked(u *upload) driveapi.UploadStatus {
	up := u.uploadedLocked()
	var bytes int64
	for _, p := range up {
		bytes += p.Size
	}
	return driveapi.UploadStatus{UploadID: u.id, NodeID: u.nodeID, ParentID: u.parentID, Name: u.name, Size: u.size,
		Mime: u.mime, Method: u.method, PartSize: u.partSize, PartCount: u.partCount, Uploaded: up,
		UploadedBytes: bytes, Fingerprint: u.fingerprint, CreatedAt: u.created, ExpiresAt: u.expires}
}

func (s *Server) uploadGet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.uploads[r.PathValue("id")]
	if u == nil {
		fail(w, http.StatusNotFound, driveapi.CodeNotFound, "no such upload", nil)
		return
	}
	writeJSON(w, http.StatusOK, s.statusLocked(u))
}

func (s *Server) uploadList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := []driveapi.UploadStatus{}
	for _, u := range s.uploads {
		items = append(items, s.statusLocked(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) uploadCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Files     []driveapi.CheckFile `json:"files"`
		MtimeHint bool                 `json:"mtime_hint"`
	}
	if !decode(w, r, &req) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(req.Files) > driveapi.CheckMax {
		fail(w, http.StatusUnprocessableEntity, driveapi.CodeInvalid, "too many", nil)
		return
	}
	items := make([]driveapi.CheckResult, 0, len(req.Files))
	for _, f := range req.Files {
		p := s.nodes[f.ParentID]
		if p == nil || !s.liveLocked(p) {
			fail(w, http.StatusNotFound, driveapi.CodeNotFound, "no such folder", nil)
			return
		}
		if s.refuseLocked(w, p.id) {
			return
		}
		n := s.childByNameLocked(f.ParentID, f.Name)
		switch {
		case n == nil:
			items = append(items, driveapi.CheckResult{State: driveapi.CheckAbsent})
		case n.kind == driveapi.KindFile && n.size() == f.Size && n.current() != nil &&
			((f.Fingerprint != "" && n.current().fingerprint == f.Fingerprint) || (f.SHA256 != "" && n.current().sha256 == f.SHA256)):
			ev := "sha256"
			if f.Fingerprint != "" && n.current().fingerprint == f.Fingerprint {
				ev = "fingerprint"
			}
			items = append(items, driveapi.CheckResult{State: driveapi.CheckPresent, ID: n.id, Evidence: ev})
		default:
			res := driveapi.CheckResult{State: driveapi.CheckDifferent, ID: n.id, SameSize: n.size() == f.Size}
			if req.MtimeHint && res.SameSize && f.ModifiedAt != nil && n.modifiedAt != nil && n.modifiedAt.Equal(f.ModifiedAt.Time) {
				res.Probable = true
			}
			items = append(items, res)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
