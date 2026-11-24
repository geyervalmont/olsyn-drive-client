package vfs

import (
	"context"
	"testing"
	"time"
)

func TestFeedPacerBudget(t *testing.T) {
	p := newFeedPacer(time.Second, 5*time.Second, 5)
	ctx := context.Background()
	start := time.Now()
	for range 5 {
		if err := p.take(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("5 calls within a budget of 5 waited %v", d)
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := p.take(short); err == nil {
		t.Fatal("a 6th call in the minute went")
	}
	if err := newFeedPacer(time.Second, time.Second, -1).take(short); err != nil {
		t.Fatal("no budget still waited")
	}
}

func TestFeedPacerInterval(t *testing.T) {
	p := newFeedPacer(2*time.Second, 10*time.Second, 40)
	t0 := time.Now()
	if d := p.wait(t0); d != 0 {
		t.Fatalf("first wake-up waits %v", d)
	}
	p.done(t0, 0)
	if d := p.wait(t0.Add(500 * time.Millisecond)); d != 1500*time.Millisecond {
		t.Fatalf("a wake-up 0.5 s after a read waits %v, want 1.5 s", d)
	}
	// quiet here: the least interval
	p.done(t0.Add(3*time.Second), 0)
	if p.interval != 2*time.Second {
		t.Fatalf("quiet: %v", p.interval)
	}
	// 2 own changes a second: half way
	p.done(t0.Add(5*time.Second), 4)
	if p.interval != 6*time.Second {
		t.Fatalf("2 a second: %v", p.interval)
	}
	// a bulk copy: the longest
	p.done(t0.Add(7*time.Second), 104)
	if p.interval != 10*time.Second {
		t.Fatalf("50 a second: %v", p.interval)
	}
	if d := p.wait(t0.Add(30 * time.Second)); d != 0 {
		t.Fatalf("a wake-up long after waits %v", d)
	}
}
