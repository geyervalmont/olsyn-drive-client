package fakedrive_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/fakedrive"
)

// The fake drive enforces the production pre-check limits: 1,000 files and
// 200 folders a call (422), and a rate per window (429 with Retry-After).
func TestPreCheckLimits(t *testing.T) {
	fd := fakedrive.New(fakedrive.Options{CheckRate: 2, RateWindow: 3 * time.Second})
	t.Cleanup(fd.Close)
	var folders []string
	for i := range 201 {
		folders = append(folders, fd.AddFolder(fd.RootID(), fmt.Sprint("F", i)))
	}
	post := func(files []driveapi.CheckFile) *http.Response {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"files": files})
		req, _ := http.NewRequest(http.MethodPost, fd.URL()+"/api/v1/uploads/check", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+fd.Token())
		req.Header.Set("Content-Type", "application/json")
		resp, err := fd.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	var wide, many []driveapi.CheckFile
	for _, f := range folders {
		wide = append(wide, driveapi.CheckFile{ParentID: f, Name: "a.txt", Size: 1})
	}
	for i := range 1001 {
		many = append(many, driveapi.CheckFile{ParentID: folders[0], Name: fmt.Sprint(i, ".txt"), Size: 1})
	}
	if r := post(wide); r.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("201 folders: %d", r.StatusCode)
	}
	if r := post(many); r.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("1,001 files: %d", r.StatusCode)
	}
	if r := post(wide[:200]); r.StatusCode != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" {
		t.Fatalf("third call in the window: %d, Retry-After %q", r.StatusCode, r.Header.Get("Retry-After"))
	}
	if fd.Count("check.422") != 2 || fd.Count("check.429") != 1 || fd.Count("check") != 0 {
		t.Fatalf("counts: 422 %d, 429 %d, answered %d", fd.Count("check.422"), fd.Count("check.429"), fd.Count("check"))
	}
	fd.SetRateLimits(-1, 0, 0)
	if r := post(wide[:200]); r.StatusCode != http.StatusOK {
		t.Fatalf("200 folders: %d", r.StatusCode)
	}
	if r := post(many[:1000]); r.StatusCode != http.StatusOK {
		t.Fatalf("1,000 files: %d", r.StatusCode)
	}
}
