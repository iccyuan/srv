package main

import (
	"testing"
	"time"
)

func TestNextBackoff(t *testing.T) {
	cases := []struct {
		in, want time.Duration
	}{
		{time.Second, 2 * time.Second},
		{2 * time.Second, 4 * time.Second},
		{8 * time.Second, 16 * time.Second},
		{16 * time.Second, 30 * time.Second}, // hits cap
		{30 * time.Second, 30 * time.Second}, // stays at cap
		{45 * time.Second, 30 * time.Second}, // never above cap
	}
	for _, c := range cases {
		if got := nextBackoff(c.in, 30*time.Second); got != c.want {
			t.Errorf("nextBackoff(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestWaitOrStop_TimerFires(t *testing.T) {
	stop := make(chan struct{})
	got := waitOrStop(20*time.Millisecond, stop)
	if !got {
		t.Error("expected true (timer fired), got false")
	}
}

func TestWaitOrStop_StopFires(t *testing.T) {
	stop := make(chan struct{})
	close(stop)
	got := waitOrStop(time.Second, stop)
	if got {
		t.Error("expected false (stopped), got true")
	}
}
