// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package phasecontrol

import (
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/koryph/koryph/internal/fsx"
)

// WriteCapabilityBlock preserves the current heartbeat while adding the
// runtime-neutral structured block fields consumed by candidate assessment.
func WriteCapabilityBlock(statusPath, capability, detail string) error {
	if statusPath == "" {
		return errors.New("status path is required")
	}
	if err := ValidateCapability(capability); err != nil {
		return err
	}
	status := map[string]any{}
	if data, err := os.ReadFile(statusPath); err == nil {
		if err := json.Unmarshal(data, &status); err != nil {
			return errors.New("existing status is malformed")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	status["state"] = "blocked"
	status["block_kind"] = "capability"
	status["capability"] = capability
	status["detail"] = SanitizeDetail(detail)
	status["updated_at"] = time.Now().UTC().Format(time.RFC3339)
	return fsx.WriteJSONAtomic(statusPath, status)
}
