package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/geyervalmont/olsyn-drive-client/internal/agent"
)

// events replays a fixed list of events, then blocks until ctx ends, the way
// the agent's stream stays open.
func events(evs ...agent.StreamEvent) func(context.Context, string, func(agent.StreamEvent) error) error {
	return func(ctx context.Context, _ string, fn func(agent.StreamEvent) error) error {
		for _, ev := range evs {
			if err := fn(ev); err != nil {
				return err
			}
		}
		<-ctx.Done()
		return ctx.Err()
	}
}

func ev(t *testing.T, typ string, v any) agent.StreamEvent {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return agent.StreamEvent{Type: typ, Data: data}
}

func TestWaitSignIn(t *testing.T) {
	ctx := context.Background()
	soon := time.Now().Add(time.Minute)

	// The token could not be stored: the agent ends the link as failed.
	failed := agent.SignIn{State: "failed", Error: &agent.Problem{Code: "credential_store", Message: "The sign-in could not be saved."}}
	for _, stream := range [][]agent.StreamEvent{
		{ev(t, "signin", failed)},
		{ev(t, "state", agent.State{Connection: agent.ConnSignedOut, SignIn: &failed})},
	} {
		outcome, reason, err := waitSignIn(ctx, soon, events(stream...))
		if err != nil || outcome != "failed" || reason != "The sign-in could not be saved." {
			t.Fatalf("failed link: %q %q %v", outcome, reason, err)
		}
	}

	outcome, _, err := waitSignIn(ctx, soon, events(ev(t, "state", agent.State{Connection: agent.ConnStarting, Account: &agent.Account{Name: "Test"}})))
	if err != nil || outcome != "approved" {
		t.Fatalf("approved: %q %v", outcome, err)
	}

	// Nothing ever arrives: login gives up once the code has expired.
	start := time.Now()
	outcome, _, err = waitSignIn(ctx, time.Now().Add(-signInGrace+200*time.Millisecond), events())
	if err != nil || outcome != "timeout" {
		t.Fatalf("silent agent: %q %v", outcome, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("waited %s", time.Since(start))
	}
}
