package main

import (
	"testing"
	"time"
)

func TestPassTrackerStuck(t *testing.T) {
	var p passTracker
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	if err := p.stuck(t0, time.Hour); err != nil {
		t.Errorf("idle loop stuck = %v, want nil", err)
	}
	p.start(t0)
	if err := p.stuck(t0.Add(59*time.Minute), time.Hour); err != nil {
		t.Errorf("pass inside the limit stuck = %v, want nil", err)
	}
	if err := p.stuck(t0.Add(61*time.Minute), time.Hour); err == nil {
		t.Error("pass beyond the limit stuck = nil, want an error")
	}
	p.finish()
	if err := p.stuck(t0.Add(5*time.Hour), time.Hour); err != nil {
		t.Errorf("finished pass stuck = %v, want nil (waiting for the ticker is healthy)", err)
	}
}

func TestPassLimit(t *testing.T) {
	if got := passLimit(5 * time.Minute); got != time.Hour {
		t.Errorf("passLimit(5m) = %s, want the 1h floor", got)
	}
	if got := passLimit(30 * time.Minute); got != 90*time.Minute {
		t.Errorf("passLimit(30m) = %s, want 3 intervals", got)
	}
}
