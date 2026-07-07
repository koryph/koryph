// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

//go:build !linux && !darwin

package sysmem

// available reports no memory signal on platforms without a probe. Callers fail
// open on ErrUnsupported (the memory admission gate is disabled there).
func available() (Stat, error) { return Stat{}, ErrUnsupported }
