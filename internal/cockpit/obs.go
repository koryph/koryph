// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package cockpit

import (
	"fmt"
	"log/slog"

	"github.com/koryph/koryph/internal/obs"
)

// log is the package-level logger for the cockpit component. Safe to use at
// package-init time because obs.For performs lazy bootstrap.
var log = obs.For("cockpit")

// logDerivedPanic records a recovered panic from a background refreshDerived
// pass (koryph-b01). Recovering — rather than letting the goroutine crash the
// whole TUI — keeps the cockpit alive on a data-assembly bug; the WARN makes
// the failure diagnosable instead of silent.
func logDerivedPanic(projectID string, r any) {
	log.Warn("cockpit.derived.panic",
		slog.String(obs.KeyProject, projectID),
		slog.String(obs.KeyError, fmt.Sprint(r)),
	)
}
