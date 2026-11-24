package driveapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const secretToken = "odt_test_secret_value"

func newTestClient(t *testing.T, h http.Handler) (*Client, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	var logs bytes.Buffer
	c, err := New(Options{
		BaseURL: srv.URL,
		Token:   func(context.Context) (string, error) { return secretToken, nil },
		Logger:  slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, &logs
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		status    int
		code      string
		retryable bool
		permanent bool
		conflict  bool
		authLost  bool
	}{
		{503, "access_unavailable", true, false, false, false},
		{503, "building", true, false, false, false},
		{500, "internal", true, false, false, false},
		{429, "rate_limited", true, false, false, false},
		{423, "writes_paused", true, false, false, false},
		{423, "managed", false, true, false, false},
		{403, "sealed", false, true, false, false},
		{403, "forbidden", false, true, false, false},
		{423, "legal_hold", false, true, false, false},
		{409, "retention_period", false, true, false, false},
		{413, "quota_exceeded", false, true, false, false},
		{422, "invalid_name", false, true, false, false},
		{409, "archived", false, true, false, false},
		{409, "version_conflict", false, false, true, false},
		{409, "name_conflict", false, false, true, false},
		{401, "unauthenticated", false, false, false, true},
		{404, "not_found", false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": tc.code, "message": "m", "managed_by": "Integration X"})
			}))
			c.rateRetries = 0 // the 429 itself, not the waits (TestRateLimited*)
			_, err := c.GetNode(context.Background(), "n1")
			if err == nil {
				t.Fatal("want error")
			}
			if got := IsRetryable(err); got != tc.retryable {
				t.Errorf("IsRetryable = %v", got)
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Errorf("IsPermanent = %v", got)
			}
			if got := IsConflict(err); got != tc.conflict {
				t.Errorf("IsConflict = %v", got)
			}
			if got := IsAuthLost(err); got != tc.authLost {
				t.Errorf("IsAuthLost = %v", got)
			}
			if tc.permanent && Reason(err) == "" {
				t.Error("permanent refusal without a reason")
			}
			if RetryAfter(err) != 2*time.Second {
				t.Errorf("RetryAfter = %v", RetryAfter(err))
			}
		})
	}
	if !strings.Contains(Reason(&Error{Status: 423, Code: "managed", Extra: map[string]json.RawMessage{"managed_by": json.RawMessage(`"Integration X"`)}}), "Integration X") {
		t.Error("managed reason should name the integration")
	}
	if IsRetryable(&Error{Err: context.Canceled}) {
		t.Error("cancelled is not retryable")
	}
	if !IsRetryable(&Error{Err: io.ErrUnexpectedEOF}) {
		t.Error("network error is retryable")
	}
}

func TestTokenSentButNeverLogged(t *testing.T) {
	var gotAuth string
	c, logs := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"user": map[string]any{"id": "u1"}, "root_id": "root"})
	}))
	me, err := c.Me(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if me.RootID != "root" || gotAuth != "Bearer "+secretToken {
		t.Fatalf("me=%+v auth=%q", me, gotAuth)
	}
	if strings.Contains(logs.String(), secretToken) {
		t.Fatal("token leaked into logs")
	}
}

func TestLinkNoAuthAndPending(t *testing.T) {
	polls := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("link routes must not carry a token")
		}
		switch r.URL.Path {
		case "/api/mount/v1/link":
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(LinkStart{DeviceCode: "odl_x", UserCode: "BCDF-GHJK", Interval: 0, ExpiresIn: 30})
		case "/api/mount/v1/link/token":
			polls++
			if polls < 2 {
				w.WriteHeader(428)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(LinkResult{Token: "odt_new", DeviceID: "d1"})
		}
	}))
	ls, err := c.StartLink(context.Background(), LinkRequest{Client: "olsyn-drive-test", Machine: "PC"})
	if err != nil {
		t.Fatal(err)
	}
	ls.Interval = 1
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := c.WaitLink(ctx, ls)
	if err != nil || res.Token != "odt_new" {
		t.Fatalf("res=%v err=%v", res, err)
	}
}

func TestDownloadRangeAndDrop(t *testing.T) {
	content := []byte("0123456789abcdefghij")
	drop := false
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Range") != `"v-v1"` {
			t.Errorf("If-Range = %q", r.Header.Get("If-Range"))
		}
		w.Header().Set("ETag", `"v-v1"`)
		w.Header().Set("Content-Range", "bytes 5-14/20")
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(206)
		if drop {
			_, _ = w.Write(content[5:8])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		_, _ = w.Write(content[5:15])
	}))
	d, err := c.Download(context.Background(), "n1", "v1", 5, 10)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(d.Body)
	d.Body.Close()
	if err != nil || string(b) != "56789abcde" || d.Total != 20 {
		t.Fatalf("got %q total %d err %v", b, d.Total, err)
	}
	drop = true
	d, err = c.Download(context.Background(), "n1", "v1", 5, 10)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(d.Body)
	d.Body.Close()
	if !IsRetryable(err) {
		t.Fatalf("dropped body should be retryable, got %v", err)
	}
}

func TestStreamParsesEvents(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "retry: 3000\n\n: ping\n\nevent: change\nid: 7\ndata: {\"type\":\"file.created\",\"node_id\":\"a\",\"seq\":7}\n\nevent: notification\ndata:\n\n")
	}))
	var got []StreamEvent
	opened := false
	err := c.Stream(context.Background(), func() { opened = true }, func(ev StreamEvent) error {
		got = append(got, ev)
		return nil
	})
	if err != nil || !opened {
		t.Fatalf("err=%v opened=%v", err, opened)
	}
	if len(got) != 2 || got[0].Event != "change" || got[0].ID != "7" || got[1].Event != "notification" {
		t.Fatalf("events: %+v", got)
	}
	var ch StreamChange
	if err := json.Unmarshal(got[0].Data, &ch); err != nil || ch.Seq != 7 || ch.NodeID != "a" {
		t.Fatalf("change %+v err %v", ch, err)
	}
}

func TestModifiedAtFormat(t *testing.T) {
	tm := time.Date(2026, 11, 24, 3, 15, 7, 123456789, time.FixedZone("AEDT", 11*3600))
	if got := FormatModifiedAt(tm); got != "2026-11-23T16:15:07.123Z" {
		t.Fatalf("got %s", got)
	}
	b, _ := json.Marshal(NewTime(tm))
	if string(b) != `"2026-11-23T16:15:07.123Z"` {
		t.Fatalf("json %s", b)
	}
	var back Time
	if err := json.Unmarshal(b, &back); err != nil || !back.Equal(tm.Truncate(time.Millisecond)) {
		t.Fatalf("round trip %v %v", back, err)
	}
}

func TestPutPartStorageErrors(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("storage PUT must not carry the drive token")
		}
		switch r.URL.Path {
		case "/expired":
			w.WriteHeader(403)
			_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code></Error>")
		case "/gone":
			w.WriteHeader(404)
			_, _ = io.WriteString(w, "<Error><Code>NoSuchUpload</Code></Error>")
		case "/exists":
			w.WriteHeader(412)
		default:
			w.Header().Set("ETag", `"abc"`)
		}
	}))
	ctx := context.Background()
	base := c.BaseURL()
	if etag, err := c.PutPart(ctx, base+"/ok", nil, strings.NewReader("x"), 1); err != nil || etag != "abc" {
		t.Fatalf("etag %q err %v", etag, err)
	}
	if _, err := c.PutPart(ctx, base+"/expired?X-Amz-Signature=s", nil, strings.NewReader("x"), 1); !IsStorageURLExpired(err) || strings.Contains(err.Error(), "Signature") {
		t.Fatalf("expired: %v", err)
	}
	if _, err := c.PutPart(ctx, base+"/gone", nil, strings.NewReader("x"), 1); !IsUploadGone(err) {
		t.Fatalf("gone: %v", err)
	}
	if _, err := c.PutPart(ctx, base+"/exists", nil, strings.NewReader("x"), 1); !IsAlreadyStored(err) {
		t.Fatalf("exists: %v", err)
	}
	var e *Error
	_, err := c.PutPart(ctx, "http://127.0.0.1:1/x?sig=secret", nil, strings.NewReader("x"), 1)
	if !errors.As(err, &e) || !IsRetryable(err) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("network: %v", err)
	}
}
