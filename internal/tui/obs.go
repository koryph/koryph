// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/koryph/koryph/internal/obs"
	"github.com/koryph/koryph/internal/registry"
)

// log is the package-level logger for the tui component. Safe to use at
// package-init time because obs.For performs lazy bootstrap.
var log = obs.For("tui")

// tuiActor identifies the operator-facing process in an audit Event.Actor,
// mirroring cmd/koryph's cliActor (koryph@<host>:<pid>) so `koryph adopt`'s
// audit trail reads consistently whether the action came from the CLI or the
// TUI cockpit.
func tuiActor() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("tui@%s:%d", host, os.Getpid())
}

// auditOperatorAction records a durable trail for an operator action taken
// from the TUI cockpit (koryph-5a1 #57): before this, `n` nudge and `D` drain
// left no trace once the in-memory action result banner scrolled away —
// unlike their `koryph nudge`/`koryph drain` CLI counterparts, which already
// append to the central registry's audit.jsonl (internal/cockpit/events.go's
// auditToEvent already renders "nudge"/"drain" audit kinds into the Events
// tab feed; nothing on the TUI side ever wrote one). Best-effort: a failed
// audit write must never block the operator action it is merely recording.
func auditOperatorAction(kind, projectID string, detail map[string]any) {
	ev := registry.Event{Kind: kind, ProjectID: projectID, Actor: tuiActor(), Detail: detail}
	if err := registry.NewStore().Audit(ev); err != nil {
		log.Warn("tui.operator_action.audit_failed",
			slog.String("kind", kind),
			slog.String(obs.KeyProject, projectID),
			obs.Err(err),
		)
		return
	}
	log.Info("tui.operator_action",
		slog.String("kind", kind),
		slog.String(obs.KeyProject, projectID),
	)
}
