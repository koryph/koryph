// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package cockpit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/paths"
)

const (
	// eventsRingMax is the maximum number of events retained in the ring buffer.
	eventsRingMax = 500
	// eventsAuditMax is the maximum number of bytes to tail from the audit log
	// on each refresh. Bounded to avoid reading arbitrarily large files.
	eventsAuditMax = 256 * 1024 // 256 KB
)

// eventCollector synthesises TUIEvents from two sources:
//  1. Ledger slot state transitions (dispatch / merge / requeue).
//  2. The machine-wide audit log (~/.koryph/audit.jsonl): drain, resize, nudge.
//
// It maintains a bounded ring buffer of up to eventsRingMax events, newest last.
// All methods are single-threaded; callers must not use it concurrently.
type eventCollector struct {
	// ring is the bounded event buffer, oldest first.
	ring []TUIEvent

	// prevSlotStatus tracks the last known status for each phaseID so we can
	// detect transitions.
	prevSlotStatus map[string]string

	// auditOffset is the byte position in the audit log up to which we have
	// already read. Monotonically increasing; reset to zero only if the file
	// shrinks (log rotation, which is not expected).
	auditOffset int64

	// lastPatrolAt is the newest patrol event already pushed to the ring, so
	// each patrol run's warn findings are emitted exactly once.
	lastPatrolAt time.Time
}

// newEventCollector returns an empty collector ready for use.
func newEventCollector() *eventCollector {
	return &eventCollector{
		prevSlotStatus: make(map[string]string),
	}
}

// Snapshot returns a copy of the current ring buffer as an EventsSnapshot.
func (c *eventCollector) Snapshot() EventsSnapshot {
	out := make([]TUIEvent, len(c.ring))
	copy(out, c.ring)
	return EventsSnapshot{Events: out}
}

// Collect ingests a fresh Snapshot to derive ledger events, then tails the
// audit log for operator-level events. All new events are appended to the ring.
func (c *eventCollector) Collect(snap Snapshot) {
	c.collectSlotEvents(snap)
	c.collectPatrolEvents(snap)
	c.collectAuditEvents(snap.ProjectID)
}

// collectPatrolEvents pushes warn-level health-patrol findings (stuck claims,
// stale worktrees, …) into the feed — the engine's own "something needs a
// look" channel, previously written to the ledger but never surfaced.
func (c *eventCollector) collectPatrolEvents(snap Snapshot) {
	for _, pe := range snap.Patrol {
		if !pe.At.After(c.lastPatrolAt) {
			continue
		}
		for _, f := range pe.Findings {
			if f.Level != "warn" {
				continue
			}
			msg := fmt.Sprintf("patrol    %s: %s", f.Check, f.Message)
			if f.Fixed {
				msg += " (auto-fixed)"
			}
			level := "warn"
			if f.Fixed {
				level = "info"
			}
			c.push(TUIEvent{Time: pe.At, Kind: "patrol", Level: level, Message: msg})
		}
	}
	if n := len(snap.Patrol); n > 0 && snap.Patrol[n-1].At.After(c.lastPatrolAt) {
		c.lastPatrolAt = snap.Patrol[n-1].At
	}
}

// push appends one event to the ring, dropping the oldest if the ring is full.
func (c *eventCollector) push(ev TUIEvent) {
	if len(c.ring) >= eventsRingMax {
		// Shift: discard oldest.
		copy(c.ring, c.ring[1:])
		c.ring = c.ring[:eventsRingMax-1]
	}
	c.ring = append(c.ring, ev)
}

// collectSlotEvents compares the snapshot's slot list against the previous
// state and emits one TUIEvent per detected transition.
func (c *eventCollector) collectSlotEvents(snap Snapshot) {
	now := time.Now()
	seen := make(map[string]struct{}, len(snap.Slots))

	for _, sl := range snap.Slots {
		seen[sl.PhaseID] = struct{}{}
		prev, hadPrev := c.prevSlotStatus[sl.PhaseID]

		switch {
		case !hadPrev && sl.Stage == "running":
			// Newly dispatched.
			model := sl.Model
			if len(model) > 20 {
				model = model[:20]
			}
			msg := fmt.Sprintf("dispatch  %s  model %s", slotLabel(sl), model)
			if sl.Attempt > 1 {
				msg += fmt.Sprintf("  attempt %d", sl.Attempt)
			}
			c.push(TUIEvent{Time: now, Kind: "dispatch", Level: "info", BeadID: sl.PhaseID, Message: msg})

		case hadPrev && prev == "running" && sl.Stage == "merged":
			cost := ""
			if sl.CostUSD > 0 {
				cost = fmt.Sprintf("  $%.3f", sl.CostUSD)
			}
			c.push(TUIEvent{
				Time:    now,
				Kind:    "merge",
				Level:   "info",
				BeadID:  sl.PhaseID,
				Message: fmt.Sprintf("merged    %s%s", slotLabel(sl), cost),
			})

		case hadPrev && prev != "running" && sl.Stage == "running" && sl.Attempt > 1:
			// Requeue (came back to running after a non-running state).
			c.push(TUIEvent{
				Time:    now,
				Kind:    "requeue",
				Level:   "warn",
				BeadID:  sl.PhaseID,
				Message: fmt.Sprintf("requeued  %s  attempt %d", slotLabel(sl), sl.Attempt),
			})

		case hadPrev && prev == "running" && (sl.Stage == "failed" || sl.Stage == "conflict" || sl.Stage == "blocked"):
			// A running slot dying is a FAILURE, not a "requeue" — the old
			// kind/level buried exactly the events an escalation watcher (or a
			// higher-tier model deciding whether to take over) needs. The
			// engine's death classification and any block note travel with it.
			msg := fmt.Sprintf("failed    %s  → %s", slotLabel(sl), sl.Stage)
			if sl.DeathReason != "" {
				msg += "  (" + sl.DeathReason + ")"
			}
			if sl.Note != "" {
				msg += "  " + truncateStr(sl.Note, 60)
			}
			c.push(TUIEvent{
				Time:    now,
				Kind:    "fail",
				Level:   "error",
				BeadID:  sl.PhaseID,
				Message: msg,
			})
		}

		c.prevSlotStatus[sl.PhaseID] = sl.Stage
	}

	// Clean up entries for slots that have disappeared.
	for id := range c.prevSlotStatus {
		if _, ok := seen[id]; !ok {
			delete(c.prevSlotStatus, id)
		}
	}
}

// slotLabel is the display label for a slot in event messages: the bead's
// short title when known (ids say nothing at a glance), with the id appended
// for cross-referencing; bare id otherwise.
func slotLabel(sl SlotSnapshot) string {
	id := sl.BeadID
	if id == "" {
		id = sl.PhaseID
	}
	if sl.Title != "" && sl.Title != id {
		return truncateStr(sl.Title, 44) + " [" + id + "]"
	}
	return id
}

// truncateStr limits s to maxLen runes, appending "…" when truncated.
func truncateStr(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen-1]) + "…"
}

// auditRecord is the minimal shape of a registry audit.jsonl record.
type auditRecord struct {
	At        string `json:"at"`
	Kind      string `json:"kind"`
	ProjectID string `json:"project_id"`
	Actor     string `json:"actor"`
}

// collectAuditEvents tails the machine-wide audit log from the current offset
// and emits TUIEvents for entries matching the active project.
func (c *eventCollector) collectAuditEvents(projectID string) {
	path := paths.AuditLog()
	f, err := os.Open(path)
	if err != nil {
		return // audit log absent is fine — no events
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return
	}
	size := fi.Size()

	// Detect log rotation / truncation.
	if size < c.auditOffset {
		c.auditOffset = 0
	}
	if size == c.auditOffset {
		return // nothing new
	}

	// Limit how far back we go on first read.
	start := c.auditOffset
	if start == 0 && size > eventsAuditMax {
		start = size - eventsAuditMax
	}

	if _, err := f.Seek(start, 0); err != nil {
		return
	}

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Bytes()

		var rec auditRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		// Only surface events for the active project (or project-less events).
		if rec.ProjectID != "" && rec.ProjectID != projectID {
			continue
		}
		ev := c.auditToEvent(rec)
		if ev.Kind != "" {
			c.push(ev)
		}
	}
	// Advance the offset to the end of what was readable when we started.
	// Computing per-line byte counts is fragile (the final line may lack a
	// trailing newline, causing a one-byte overshoot). Setting auditOffset
	// to `size` is safe: any bytes appended during the scan are at offsets
	// >= size and will be picked up on the next Collect call.
	// Only advance when the scanner terminated cleanly; on error we leave the
	// offset unchanged so we retry the same bytes next tick.
	if sc.Err() == nil {
		c.auditOffset = size
	}
}

// auditToEvent converts one audit record to a TUIEvent.
// Returns an empty TUIEvent (Kind=="") for unknown/irrelevant kinds.
func (c *eventCollector) auditToEvent(rec auditRecord) TUIEvent {
	t := time.Now()
	if rec.At != "" {
		if parsed, err := time.Parse(time.RFC3339, rec.At); err == nil {
			t = parsed
		}
	}

	actor := rec.Actor
	if idx := strings.LastIndex(actor, ":"); idx >= 0 {
		actor = actor[idx+1:]
	}

	switch rec.Kind {
	case "drain":
		return TUIEvent{
			Time:    t,
			Kind:    "drain",
			Level:   "warn",
			Message: fmt.Sprintf("drain     project %s  by %s", rec.ProjectID, actor),
		}
	case "resize":
		return TUIEvent{
			Time:    t,
			Kind:    "cap-change",
			Level:   "info",
			Message: fmt.Sprintf("resize    project %s  by %s", rec.ProjectID, actor),
		}
	case "nudge":
		return TUIEvent{
			Time:    t,
			Kind:    "nudge",
			Level:   "info",
			Message: fmt.Sprintf("nudge     by %s", actor),
		}
	default:
		return TUIEvent{} // unknown / not displayed
	}
}
