// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package stage

import "github.com/koryph/koryph/internal/obs"

// log is the package-level logger for the stage component. Safe to use at
// package-init time because obs.For performs a lazy bootstrap.
var log = obs.For("stage")
