// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package posture

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// repoSettingsFile is the structure of .github/repo-settings.json.
// The four managed sections mirror ensure-repo-settings.sh exactly.
type repoSettingsFile struct {
	Repo                json.RawMessage `json:"repo"`
	SecurityAndAnalysis json.RawMessage `json:"security_and_analysis"`
	VulnerabilityAlerts *bool           `json:"vulnerability_alerts"`
	ActionsWorkflow     json.RawMessage `json:"actions_workflow"`
	Unmanaged           []string        `json:"unmanaged"`
}

// CheckSettings compares each managed section of the desired-state file
// against the live GitHub repository settings for repo.  Informational
// "unmanaged" items are printed as INFO lines and never cause drift.
//
// Returns (true, nil) on drift, (false, nil) when everything matches.
func CheckSettings(ctx context.Context, ghBin, repo string, src Source, w io.Writer) (bool, error) {
	return applySettings(ctx, ghBin, repo, src, w, false)
}

// ApplySettings patches each managed section of the desired-state file to
// match the live GitHub repository settings for repo.
func ApplySettings(ctx context.Context, ghBin, repo string, src Source, w io.Writer) error {
	_, err := applySettings(ctx, ghBin, repo, src, w, true)
	return err
}

// applySettings is the shared implementation for CheckSettings / ApplySettings.
func applySettings(ctx context.Context, ghBin, repo string, src Source, w io.Writer, apply bool) (bool, error) {
	settingsPath, err := src.RepoSettingsFile()
	if err != nil {
		return false, err
	}

	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		return false, fmt.Errorf("posture: read settings file: %w", err)
	}
	var desired repoSettingsFile
	if err := json.Unmarshal(raw, &desired); err != nil {
		return false, fmt.Errorf("posture: parse settings file: %w", err)
	}

	// Fetch the full live repo record once.
	repoEndpoint := fmt.Sprintf("repos/%s", repo)
	liveRepRaw, code, err := ghRun(ctx, ghBin, []string{"api", repoEndpoint}, nil)
	if err != nil {
		return false, fmt.Errorf("posture: fetch repo: %w", err)
	}
	if code != 0 {
		return false, fmt.Errorf("posture: fetch repo: gh exited %d", code)
	}
	var liveRepoFull map[string]json.RawMessage
	if err := json.Unmarshal(liveRepRaw, &liveRepoFull); err != nil {
		return false, fmt.Errorf("posture: parse live repo: %w", err)
	}

	drift := false

	// --- section 1: repo flags -------------------------------------------
	if desired.Repo != nil {
		liveRepo, err := extractRepoFlags(liveRepoFull)
		if err != nil {
			return false, err
		}
		d, err := sectionCheck(w, "repo flags", liveRepo, desired.Repo)
		if err != nil {
			return false, err
		}
		if d {
			drift = true
			if apply {
				payload, err := wrapField("", desired.Repo)
				if err != nil {
					return false, err
				}
				// The PATCH body is the repo flags object directly (no wrapper key).
				_, code, err := ghRun(ctx, ghBin, []string{"api", "-X", "PATCH", repoEndpoint}, payload)
				if err != nil {
					return false, fmt.Errorf("posture: patch repo flags: %w", err)
				}
				if code != 0 {
					return false, fmt.Errorf("posture: patch repo flags: gh exited %d", code)
				}
				fmt.Fprintln(w, "UPDATED  repo flags")
			}
		}
	}

	// --- section 2: security & analysis -----------------------------------
	if desired.SecurityAndAnalysis != nil {
		liveSec, err := extractSecurityAndAnalysis(liveRepoFull)
		if err != nil {
			return false, err
		}
		d, err := sectionCheck(w, "security & analysis", liveSec, desired.SecurityAndAnalysis)
		if err != nil {
			return false, err
		}
		if d {
			drift = true
			if apply {
				// Wrap in the nested security_and_analysis structure GitHub expects.
				payload, err := buildSecurityPayload(desired.SecurityAndAnalysis)
				if err != nil {
					return false, err
				}
				_, code, err := ghRun(ctx, ghBin, []string{"api", "-X", "PATCH", repoEndpoint}, payload)
				if err != nil {
					return false, fmt.Errorf("posture: patch security & analysis: %w", err)
				}
				if code != 0 {
					return false, fmt.Errorf("posture: patch security & analysis: gh exited %d", code)
				}
				fmt.Fprintln(w, "UPDATED  security & analysis")
			}
		}
	}

	// --- section 3: vulnerability alerts ----------------------------------
	if desired.VulnerabilityAlerts != nil {
		wantVuln := *desired.VulnerabilityAlerts
		vulnEndpoint := fmt.Sprintf("repos/%s/vulnerability-alerts", repo)
		_, vulnCode, err := ghRun(ctx, ghBin, []string{"api", vulnEndpoint}, nil)
		if err != nil {
			return false, fmt.Errorf("posture: check vuln alerts: %w", err)
		}
		liveVuln := (vulnCode == 0) // 204 = enabled; 404 = disabled

		liveVulnJSON, _ := json.Marshal(map[string]bool{"enabled": liveVuln})
		wantVulnJSON, _ := json.Marshal(map[string]bool{"enabled": wantVuln})

		d, err := sectionCheck(w, "vulnerability alerts", liveVulnJSON, wantVulnJSON)
		if err != nil {
			return false, err
		}
		if d {
			drift = true
			if apply {
				var method string
				if wantVuln {
					method = "PUT"
				} else {
					method = "DELETE"
				}
				_, code, err := ghRun(ctx, ghBin, []string{"api", "-X", method, vulnEndpoint}, nil)
				if err != nil {
					return false, fmt.Errorf("posture: set vuln alerts: %w", err)
				}
				if code != 0 {
					return false, fmt.Errorf("posture: set vuln alerts: gh exited %d", code)
				}
				fmt.Fprintln(w, "UPDATED  vulnerability alerts")
			}
		}
	}

	// --- section 4: actions workflow permissions --------------------------
	if desired.ActionsWorkflow != nil {
		actionsEndpoint := fmt.Sprintf("repos/%s/actions/permissions/workflow", repo)
		liveActRaw, code, err := ghRun(ctx, ghBin, []string{"api", actionsEndpoint}, nil)
		if err != nil {
			return false, fmt.Errorf("posture: fetch actions perms: %w", err)
		}
		if code != 0 {
			return false, fmt.Errorf("posture: fetch actions perms: gh exited %d", code)
		}

		liveActNorm, err := jsonSortKeys(liveActRaw)
		if err != nil {
			return false, fmt.Errorf("posture: parse live actions perms: %w", err)
		}
		wantActNorm, err := jsonSortKeys(desired.ActionsWorkflow)
		if err != nil {
			return false, fmt.Errorf("posture: parse want actions perms: %w", err)
		}

		d, err := sectionCheck(w, "actions workflow permissions", liveActNorm, wantActNorm)
		if err != nil {
			return false, err
		}
		if d {
			drift = true
			if apply {
				_, code, err := ghRun(ctx, ghBin, []string{"api", "-X", "PUT", actionsEndpoint}, desired.ActionsWorkflow)
				if err != nil {
					return false, fmt.Errorf("posture: set actions perms: %w", err)
				}
				if code != 0 {
					return false, fmt.Errorf("posture: set actions perms: gh exited %d", code)
				}
				fmt.Fprintln(w, "UPDATED  actions workflow permissions")
			}
		}
	}

	// --- unmanaged (informational, never drift) ---------------------------
	for _, item := range desired.Unmanaged {
		fmt.Fprintf(w, "INFO     unmanaged: %s\n", item)
	}

	return drift, nil
}

// sectionCheck compares live and want JSON byte slices (both already
// canonicalized) and writes OK or DRIFT output to w.
// It returns (true, nil) on drift, (false, nil) when equal.
func sectionCheck(w io.Writer, label string, live, want []byte) (bool, error) {
	liveNorm, err := jsonSortKeys(live)
	if err != nil {
		return false, fmt.Errorf("posture: normalize live %s: %w", label, err)
	}
	wantNorm, err := jsonSortKeys(want)
	if err != nil {
		return false, fmt.Errorf("posture: normalize want %s: %w", label, err)
	}
	if string(liveNorm) == string(wantNorm) {
		fmt.Fprintf(w, "OK       %s\n", label)
		return false, nil
	}
	fmt.Fprintf(w, "DRIFT    %s:\n", label)
	printDiff(w, liveNorm, wantNorm)
	return true, nil
}

// extractRepoFlags picks the repo-flags fields the desired-state file manages
// from the full live repo object.  The extracted set must exactly mirror what
// .repo in repo-settings.json covers so comparisons are apples-to-apples.
func extractRepoFlags(full map[string]json.RawMessage) ([]byte, error) {
	keys := []string{
		"allow_merge_commit", "allow_squash_merge", "allow_rebase_merge",
		"allow_auto_merge", "delete_branch_on_merge", "allow_update_branch",
		"web_commit_signoff_required", "description", "homepage",
	}
	out := make(map[string]json.RawMessage, len(keys))
	for _, k := range keys {
		if v, ok := full[k]; ok {
			out[k] = v
		}
	}
	return json.Marshal(out)
}

// extractSecurityAndAnalysis builds the flat status map the desired-state file
// uses from the nested security_and_analysis block in the live repo object.
func extractSecurityAndAnalysis(full map[string]json.RawMessage) ([]byte, error) {
	secRaw, ok := full["security_and_analysis"]
	if !ok {
		// GitHub might not return this field on private repos with certain plans.
		return json.Marshal(map[string]string{})
	}
	var sec struct {
		SecretScanning struct {
			Status string `json:"status"`
		} `json:"secret_scanning"`
		SecretScanningPushProtection struct {
			Status string `json:"status"`
		} `json:"secret_scanning_push_protection"`
		DependabotSecurityUpdates struct {
			Status string `json:"status"`
		} `json:"dependabot_security_updates"`
	}
	if err := json.Unmarshal(secRaw, &sec); err != nil {
		return nil, fmt.Errorf("posture: parse security_and_analysis: %w", err)
	}
	flat := map[string]string{
		"secret_scanning":                 sec.SecretScanning.Status,
		"secret_scanning_push_protection": sec.SecretScanningPushProtection.Status,
		"dependabot_security_updates":     sec.DependabotSecurityUpdates.Status,
	}
	return json.Marshal(flat)
}

// buildSecurityPayload wraps the flat desired security_and_analysis map into
// the nested structure GitHub's PATCH endpoint expects.
func buildSecurityPayload(desired json.RawMessage) ([]byte, error) {
	var flat map[string]string
	if err := json.Unmarshal(desired, &flat); err != nil {
		return nil, fmt.Errorf("posture: parse desired security: %w", err)
	}
	nested := map[string]interface{}{
		"security_and_analysis": map[string]interface{}{
			"secret_scanning":                 map[string]string{"status": flat["secret_scanning"]},
			"secret_scanning_push_protection": map[string]string{"status": flat["secret_scanning_push_protection"]},
			"dependabot_security_updates":     map[string]string{"status": flat["dependabot_security_updates"]},
		},
	}
	return json.Marshal(nested)
}

// wrapField marshals v as the top-level patch payload (v is already a
// json.RawMessage and is returned directly — no wrapper key is needed for
// the repo flags PATCH).
func wrapField(_ string, v json.RawMessage) ([]byte, error) {
	return v, nil
}
