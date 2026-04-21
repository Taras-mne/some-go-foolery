package main

import (
	"context"
	"testing"
	"time"
)

func TestSleepOrDone_Elapses(t *testing.T) {
	start := time.Now()
	done := sleepOrDone(context.Background(), 20*time.Millisecond)
	if done {
		t.Errorf("expected timer to elapse, got cancellation")
	}
	if time.Since(start) < 15*time.Millisecond {
		t.Errorf("returned too fast: %v", time.Since(start))
	}
}

func TestSleepOrDone_Cancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := sleepOrDone(ctx, time.Hour)
	if !done {
		t.Error("expected cancellation signal")
	}
}
