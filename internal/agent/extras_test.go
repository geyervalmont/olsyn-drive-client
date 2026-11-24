package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFirstRunUntilSignedInOnce(t *testing.T) {
	h := newHarness(t, nil)
	st := h.waitState("ready", func(s State) bool { return s.Ready })
	if !st.FirstRun {
		t.Fatal("a fresh PC is not first_run")
	}
	h2 := newHarness(t, seedAccount)
	if st := h2.waitState("ready", func(s State) bool { return s.Ready }); st.FirstRun {
		t.Fatal("a signed-in PC is first_run")
	}
}

func TestOpenUI(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.post("/v1/open", OpenAction{Target: "ui", Path: "transfers/held"}, nil); err != nil {
		t.Fatal(err)
	}
	var ae *APIError
	if err := h.post("/v1/open", OpenAction{Target: "ui", Path: "../../etc"}, nil); !errors.As(err, &ae) || ae.Status != 422 {
		t.Fatalf("bad route: %v", err)
	}
	h.system.mu.Lock()
	defer h.system.mu.Unlock()
	if len(h.system.routes) != 1 || h.system.routes[0] != "transfers/held" {
		t.Fatalf("routes %v", h.system.routes)
	}
}

func TestCacheFoldersAndSend(t *testing.T) {
	h := newHarness(t, seedAccount)
	h.waitState("connected", func(s State) bool { return s.Connection == ConnConnected })
	ctx := context.Background()
	var cu CacheUsage
	if err := h.client.Do(ctx, http.MethodGet, "/v1/cache", nil, &cu); err != nil {
		t.Fatal(err)
	}
	if cu.UsedBytes != 5<<20 || cu.CapBytes == 0 || cu.DiskFreeBytes != 100<<30 || cu.Dir == "" {
		t.Fatalf("cache %+v", cu)
	}
	var df DriveFolders
	if err := h.client.Do(ctx, http.MethodGet, "/v1/drive/folders?parent=/", nil, &df); err != nil {
		t.Fatal(err)
	}
	if len(df.Folders) != 1 || df.Folders[0].Path != "/Projects" || !df.Folders[0].CanWrite {
		t.Fatalf("folders %+v", df)
	}
	var ae *APIError
	if err := h.client.Do(ctx, http.MethodGet, "/v1/drive/folders?parent=/missing", nil, nil); !errors.As(err, &ae) || ae.Code != "drive_folder_not_found" {
		t.Fatalf("missing folder: %v", err)
	}

	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b.txt")
	os.Mkdir(a, 0o755)
	os.WriteFile(b, []byte("x"), 0o644)
	var res SendResult
	body := map[string]any{"local_paths": []string{a, b, a}, "drive_folder": "/Projects"}
	if err := h.post("/v1/send", body, &res); err != nil {
		t.Fatal(err)
	}
	if res.Files != 2 {
		t.Fatalf("duplicates not dropped: %+v", res)
	}
	body["local_paths"] = []string{a, filepath.Join(dir, "gone")}
	if err := h.post("/v1/send", body, nil); !errors.As(err, &ae) || ae.Code != "local_not_found" {
		t.Fatalf("missing local path: %v", err)
	}
}

func TestActivityIsPagedAndKept(t *testing.T) {
	h := newHarness(t, nil)
	h.waitState("ready", func(s State) bool { return s.Ready })
	for i := range 60 {
		h.fs.events <- FSEvent{Notification: &Notification{Kind: "finished", Title: fmt.Sprintf("Saved %d", i)}}
	}
	ctx := context.Background()
	var p ActivityPage
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := h.client.Do(ctx, http.MethodGet, "/v1/activity?limit=50", nil, &p); err != nil {
			t.Fatal(err)
		}
		if len(p.Items) == 50 && p.Items[0].Title == "Saved 59" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("activity %d items", len(p.Items))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if p.NextCursor == nil {
		t.Fatal("no next cursor")
	}
	var p2 ActivityPage
	if err := h.client.Do(ctx, http.MethodGet, "/v1/activity?cursor="+*p.NextCursor, nil, &p2); err != nil {
		t.Fatal(err)
	}
	if len(p2.Items) != 10 || p2.Items[9].Title != "Saved 0" || p2.NextCursor != nil {
		t.Fatalf("second page %d %+v", len(p2.Items), p2.NextCursor)
	}
	// kept on disk, with ids that keep counting up after a restart
	log := openActivity(filepath.Join(h.paths.State, "activity.json"))
	if pg, _ := log.page("", 1); len(pg.Items) != 1 || log.lastID() != 60 {
		t.Fatalf("not persisted: %d, last id %d", len(pg.Items), log.lastID())
	}
}

func TestLicencesEndpoint(t *testing.T) {
	h := newHarness(t, nil)
	var ae *APIError
	if err := h.client.Do(context.Background(), http.MethodGet, "/v1/licences", nil, nil); !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("no notices: %v", err)
	}
}
