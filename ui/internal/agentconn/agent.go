package agentconn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/blocked"
)

// Launcher starts the agent without waiting for it (a *Starter in the app).
type Launcher interface {
	Start() error
}

// Agent is a small client for the calls the shell makes itself.
type Agent struct {
	HTTP      *http.Client
	UserAgent string
	Starter   Launcher
	Log       *slog.Logger
	// Blocks, when set, records each start and names security software
	// when the agent cannot be started or keeps stopping (*blocked.Error).
	Blocks *blocked.Record

	ensureMu sync.Mutex
}

// GetState reads GET /v1/state.
func (a *Agent) GetState(ctx context.Context) (State, error) {
	var s State
	err := a.call(ctx, http.MethodGet, "/v1/state", nil, &s)
	return s, err
}

// Open calls POST /v1/open.
func (a *Agent) Open(ctx context.Context, target OpenTarget) error {
	return a.call(ctx, http.MethodPost, "/v1/open", target, nil)
}

// Pause and Resume call POST /v1/pause and /v1/resume.
func (a *Agent) Pause(ctx context.Context) error {
	return a.call(ctx, http.MethodPost, "/v1/pause", struct{}{}, nil)
}
func (a *Agent) Resume(ctx context.Context) error {
	return a.call(ctx, http.MethodPost, "/v1/resume", struct{}{}, nil)
}

// APIError is an error response in the shape docs/ipc.md defines.
type APIError struct {
	Status  int
	Code    string `json:"error"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("agent answered %d %s: %s", e.Status, e.Code, e.Message)
}

func (a *Agent) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, BaseURL+path, body)
	if err != nil {
		return fmt.Errorf("build %s %s: %w", method, path, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", a.UserAgent)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 400 {
		apiErr := &APIError{Status: resp.StatusCode}
		_ = json.Unmarshal(data, apiErr)
		return apiErr
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	}
	return nil
}

func (a *Agent) info(msg string, args ...any) {
	if a.Log != nil {
		a.Log.Info(msg, args...)
	}
}

func (a *Agent) warn(msg string, args ...any) {
	if a.Log != nil {
		a.Log.Warn(msg, args...)
	}
}

// Answering reports whether the agent answers GET /v1/state within a second.
func (a *Agent) Answering(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, err := a.GetState(ctx)
	var apiErr *APIError
	return err == nil || errors.As(err, &apiErr)
}

// StartWait is how long the UI waits for a started agent (docs/ipc.md: 10 s).
const StartWait = 10 * time.Second

// EnsureRunning starts the agent when its endpoint does not answer and waits
// up to StartWait for it. Concurrent callers share one attempt.
func (a *Agent) EnsureRunning(ctx context.Context) error {
	if a.Answering(ctx) {
		return nil
	}
	a.ensureMu.Lock()
	defer a.ensureMu.Unlock()
	if a.Answering(ctx) {
		return nil
	}
	if a.Starter == nil {
		return errors.New("the agent is not running and no way to start it is configured")
	}
	if a.Blocks != nil {
		if err := a.Blocks.NoteStart(); err != nil {
			a.warn("could not record the agent start", "err", err)
		}
	}
	if err := a.Starter.Start(); err != nil {
		err = a.explain(err)
		a.warn("could not start the agent", "err", err)
		return err
	}
	started := time.Now()
	a.info("started the agent", "agent", fmt.Sprint(a.Starter))
	deadline := started.Add(StartWait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			a.warn("stopped waiting for the started agent", "err", ctx.Err())
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if a.Answering(ctx) {
			a.info("the started agent is answering", "after_ms", time.Since(started).Milliseconds())
			return nil
		}
	}
	err := a.explain(fmt.Errorf("the agent did not answer within %s of starting", StartWait))
	a.warn("the started agent is not answering", "err", err)
	return err
}

// explain names security software as the cause of err when the recorded
// facts point to it.
func (a *Agent) explain(err error) error {
	if a.Blocks == nil {
		return err
	}
	return a.Blocks.Explain(err)
}
