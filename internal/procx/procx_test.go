// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package procx

import (
	"os"
	"testing"
	"time"
)

func TestAlive(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Error("Alive(self) = false, want true")
	}
	if Alive(0) || Alive(-1) {
		t.Error("Alive(non-positive) = true, want false")
	}
	// A PID far above any pid_max is a dead process (ESRCH).
	if dead := 2000000000; Alive(dead) {
		t.Skipf("chosen dead pid %d is unexpectedly alive; skipping", dead)
	}
}

func TestStartTimeSelf(t *testing.T) {
	started, ok := StartTime(os.Getpid())
	if !ok {
		t.Skip("ps -o etime= unavailable on this platform; skipping")
	}
	// The test binary itself cannot have started more than a few minutes ago,
	// and StartTime must never report the future.
	now := time.Now()
	if started.After(now.Add(time.Second)) {
		t.Errorf("StartTime(self) = %v, after now %v", started, now)
	}
	if now.Sub(started) > 10*time.Minute {
		t.Errorf("StartTime(self) = %v, more than 10m before now %v", started, now)
	}
}

func TestStartTimeNonPositivePID(t *testing.T) {
	if _, ok := StartTime(0); ok {
		t.Error("StartTime(0) ok = true, want false")
	}
	if _, ok := StartTime(-1); ok {
		t.Error("StartTime(-1) ok = true, want false")
	}
}

func TestStartTimeDeadPID(t *testing.T) {
	dead := 2000000000
	if Alive(dead) {
		t.Skipf("chosen dead pid %d is unexpectedly alive; skipping", dead)
	}
	if _, ok := StartTime(dead); ok {
		t.Errorf("StartTime(dead pid) ok = true, want false")
	}
}

func TestParseEtime(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"00:00", 0, true},
		{"01:02", time.Minute + 2*time.Second, true},
		{"1:02:03", time.Hour + 2*time.Minute + 3*time.Second, true},
		{"2-03:04:05", 2*24*time.Hour + 3*time.Hour + 4*time.Minute + 5*time.Second, true},
		{"", 0, false},
		{"garbage", 0, false},
		{"1:2:3:4", 0, false},
	}
	for _, c := range cases {
		got, ok := parseEtime(c.in)
		if ok != c.ok {
			t.Errorf("parseEtime(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("parseEtime(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
