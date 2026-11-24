package core_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/agent"
)

func (h *harness) cacheUsage() agent.CacheUsage {
	h.t.Helper()
	var u agent.CacheUsage
	if err := h.client.Do(context.Background(), http.MethodGet, "/v1/cache", nil, &u); err != nil {
		h.t.Fatal(err)
	}
	return u
}

func (h *harness) waitDone(n int) {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var page agent.TransferPage
		if err := h.client.Do(context.Background(), http.MethodGet, "/v1/transfers?state=done", nil, &page); err != nil {
			h.t.Fatal(err)
		}
		if len(page.Items) >= n && h.core.VFS().Pending() == 0 {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("%d of %d transfers done", len(page.Items), n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// GET /v1/cache and the state split the cache into unsent, just uploaded
// and opened; POST /v1/cache/free releases what the drive has; a shorter
// release time applies at once.
func TestCacheUsageFreeAndReleaseSetting(t *testing.T) {
	h := start(t)
	h.signIn()
	a := bytes.Repeat([]byte("a"), 300_000)
	b := bytes.Repeat([]byte("b"), 200_000)
	h.save("/a.bin", a)
	h.waitDone(1)
	u := h.cacheUsage()
	if u.UploadedBytes != int64(len(a)) || u.UnsentBytes != 0 || u.OpenedBytes != 0 || u.CapBytes != 10<<30 {
		t.Fatalf("after one upload %+v", u)
	}
	st := h.waitState("cache in the state", func(s agent.State) bool { return s.Cache.UploadedBytes == int64(len(a)) })
	if st.Cache.CapBytes != 10<<30 {
		t.Fatalf("state cache %+v", st.Cache)
	}

	var freed agent.CacheFreed
	if err := h.client.Do(context.Background(), http.MethodPost, "/v1/cache/free", nil, &freed); err != nil {
		t.Fatal(err)
	}
	if freed.Files != 1 || freed.Bytes < int64(len(a)) || freed.Cache.UsedBytes != 0 {
		t.Fatalf("freed %+v", freed)
	}

	// release at once after upload
	var sr agent.SettingsResponse
	if err := h.client.Do(context.Background(), http.MethodPatch, "/v1/settings",
		map[string]any{"cache_release": map[string]int{"after_upload_seconds": 0}}, &sr); err != nil {
		t.Fatal(err)
	}
	if sr.Settings.CacheRelease.AfterUploadSeconds != 0 || sr.Settings.CacheRelease.KeepOpenedSeconds != 1800 {
		t.Fatalf("settings %+v", sr.Settings.CacheRelease)
	}
	h.save("/b.bin", b)
	h.waitDone(2)
	deadline := time.Now().Add(5 * time.Second)
	for h.cacheUsage().UsedBytes != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("not released: %+v", h.cacheUsage())
		}
		time.Sleep(50 * time.Millisecond)
	}
}
