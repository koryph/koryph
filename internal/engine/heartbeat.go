// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/koryph/koryph/internal/obs"
)

// envHeartbeatSec overrides the liveness-heartbeat cadence (tests only;
// production leaves it unset). defaultHeartbeatSec matches the koryph-lwnq
// acceptance criterion: "an interval heartbeat line (default 60s)".
const (
	envHeartbeatSec     = "KORYPH_HEARTBEAT_SEC"
	defaultHeartbeatSec = 60
)

// heartbeatInterval resolves the liveness-heartbeat cadence: KORYPH_HEARTBEAT_SEC
// env override, else defaultHeartbeatSec. Mirrors pollInterval's env-override
// idiom (run.go).
func heartbeatInterval() time.Duration {
	if v, ok := envInt(envHeartbeatSec); ok && v > 0 {
		return time.Duration(v) * time.Second
	}
	return defaultHeartbeatSec * time.Second
}

// heartbeatState is the liveness snapshot the background heartbeat ticker
// reads (koryph-lwnq, E3: "a live engine ... produced ZERO log lines for 7+
// minutes ... undiagnosable from outside: 'genuinely hung' vs 'silently
// waiting' cannot be told apart").
//
// It is written ONLY from the loop goroutine (waveLoop/rollingLoop/
// pollUntilIdle), at safe checkpoints between blocking calls — never mid-call
// — and read ONLY under mu by the heartbeat goroutine. This split is
// deliberate: r.run and its Slots map are owned exclusively by the loop
// goroutine and are NOT safe for concurrent access, so the heartbeat ticker
// must never read them directly. Instead the loop goroutine copies out the
// handful of plain values the heartbeat reports, under mu, at each iteration
// boundary. The payoff: the ticker keeps firing on its own schedule even
// while the loop goroutine is blocked inside a subprocess call, a lock
// acquisition, or any other synchronous wait — a genuinely wedged loop then
// shows up as a heartbeat whose "last action" age grows without bound,
// rather than as silence indistinguishable from a healthy quiet period.
type heartbeatState struct {
	mu             sync.Mutex
	active         int
	ready          int
	wave           int
	lastActionAt   time.Time
	lastActionWhat string
}

// setCounts records the loop's current active/ready/wave counts. Called once
// per iteration of each of the three tick loops (waveLoop, rollingLoop,
// pollUntilIdle), from the loop goroutine only.
func (h *heartbeatState) setCounts(active, ready, wave int) {
	h.mu.Lock()
	h.active, h.ready, h.wave = active, ready, wave
	h.mu.Unlock()
}

// noteAction records the most recent human-readable progress line and when it
// was emitted, so the heartbeat can report "last action <what> <ago>".
func (h *heartbeatState) noteAction(what string, at time.Time) {
	h.mu.Lock()
	h.lastActionWhat, h.lastActionAt = what, at
	h.mu.Unlock()
}

// snapshot returns a consistent copy of the current state for logging.
func (h *heartbeatState) snapshot() (active, ready, wave int, lastActionWhat string, lastActionAt time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.active, h.ready, h.wave, h.lastActionWhat, h.lastActionAt
}

// startHeartbeat launches a background goroutine that emits one INFO liveness
// line every heartbeatInterval() until ctx is cancelled or the returned stop
// func is called (koryph-lwnq). Callers invoke this once at loop entry and
// defer the returned stop.
func (r *runner) startHeartbeat(ctx context.Context) (stop func()) {
	interval := heartbeatInterval()
	stopCh := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			case <-t.C:
				r.emitHeartbeat()
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(stopCh) }) }
}

// emitHeartbeat logs the current liveness snapshot at INFO: quiet-hours
// friendly (a single line per interval, unconditional on activity) per the
// koryph-lwnq acceptance criterion. Message text is deliberately human-
// scannable — "engine alive: N active, M ready, wave K, last action <what>
// <ago>" — with the same fields also attached as structured attrs for
// queryable telemetry.
func (r *runner) emitHeartbeat() {
	active, ready, wave, what, at := r.hb.snapshot()
	ago := "n/a"
	var agoSeconds float64
	if !at.IsZero() {
		d := time.Since(at)
		ago = d.Round(time.Second).String() + " ago"
		agoSeconds = d.Seconds()
	}
	if what == "" {
		what = "(none yet this run)"
		ago = ""
	}
	msg := fmt.Sprintf("engine alive: %d active, %d ready, wave %d, last action %s %s",
		active, ready, wave, what, ago)
	attrs := append(r.runLogAttrs(),
		slog.Int("active", active),
		slog.Int(obs.KeyWave, wave),
		slog.Int("ready", ready),
		slog.String("last_action", what),
		slog.Float64("last_action_ago_seconds", agoSeconds),
	)
	log.Info(obs.RedactValue(msg), attrs...)
}
