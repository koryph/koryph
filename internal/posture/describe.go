// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package posture

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SettingEntry describes one managed setting key inside a profile or IaC source.
type SettingEntry struct {
	// Section is the human-readable section name: "repo flags",
	// "security & analysis", "vulnerability alerts", or
	// "actions workflow permissions".
	Section string
	// Key is the JSON field name, e.g. "allow_merge_commit".
	Key string
	// WantValue is the JSON-encoded desired value, e.g. "false" or "\"enabled\"".
	WantValue string
	// LiveValue is the JSON-encoded live value (empty string when no live
	// comparison was requested via --repo).
	LiveValue string
	// WouldChange is true when LiveValue is set and differs semantically from
	// WantValue (after JSON normalisation).
	WouldChange bool
	// Rationale is the human-readable security explanation for this setting.
	Rationale string
}

// RuleEntry describes one rule within a ruleset.
type RuleEntry struct {
	// Type is the GitHub rule type, e.g. "required_signatures".
	Type string
	// ParamsSummary is a short parenthetical of notable parameters,
	// e.g. "required_approving_review_count: 1".  May be empty.
	ParamsSummary string
	// Rationale is the human-readable explanation of what this rule prevents.
	Rationale string
}

// RulesetEntry describes one managed ruleset shipped by a profile or IaC source.
type RulesetEntry struct {
	// Name is the ruleset name.
	Name string
	// Target is the ruleset target, e.g. "branch".
	Target string
	// Conditions are the branch/tag patterns the ruleset applies to,
	// e.g. ["~DEFAULT_BRANCH"].
	Conditions []string
	// Rules are the individual rules enforced by the ruleset.
	Rules []RuleEntry
	// Rationale is the per-ruleset human-readable description (from the
	// _rationale field in the ruleset JSON file).
	Rationale string
	// LiveState is one of "", "ok", "missing", or "drift".  It is populated
	// when live-repo comparison is requested via DescribeSource.
	LiveState string
}

// Description is the complete describe output for a posture source.
type Description struct {
	Settings []SettingEntry
	Rulesets []RulesetEntry
}

// builtinSettingRationale holds fallback rationale for well-known repo-setting
// JSON field names.  Profile files may override these via their descriptions map.
var builtinSettingRationale = map[string]string{
	"allow_merge_commit": "Prevents merge commits on the default branch, " +
		"enforcing a clean, bisectable history where every change is either " +
		"squash-merged or rebase-merged.",
	"allow_squash_merge": "Allows squash-merging pull requests into a single " +
		"commit, keeping the default branch history linear and easy to read.",
	"allow_rebase_merge": "Allows rebase-merging pull requests for a linear " +
		"history without an extra merge commit.",
	"allow_auto_merge": "When disabled, prevents pull requests from merging " +
		"automatically without an explicit human action even when all checks pass.",
	"delete_branch_on_merge": "Automatically deletes merged branches to " +
		"prevent stale branch accumulation and keep the repository tidy.",
	"allow_update_branch": "Allows contributors to update a pull-request branch " +
		"with the latest base-branch changes before merging, reducing " +
		"stale-base merge conflicts.",
	"web_commit_signoff_required": "Requires a DCO sign-off on all commits " +
		"made through the GitHub web UI, matching the CLI requirement and " +
		"ensuring contributor agreement for every change regardless of authoring tool.",
	// security_and_analysis keys
	"secret_scanning": "Scans commits for high-entropy strings and known " +
		"secret patterns (API keys, tokens, passwords); alerts the repository " +
		"owner on detection to reduce the exposure window of accidentally " +
		"committed credentials.",
	"secret_scanning_push_protection": "Blocks pushes that contain detected " +
		"secrets before they reach GitHub — prevents credential leaks at push " +
		"time rather than retroactively after a scan alert.",
	"dependabot_security_updates": "Automatically opens pull requests to " +
		"update dependencies with known CVEs, keeping the dependency graph " +
		"free of publicly-disclosed vulnerabilities with minimal manual effort.",
	// vulnerability_alerts (keyed by section key "enabled")
	"vulnerability_alerts": "Alerts the repository owner when a dependency " +
		"has a known security vulnerability (Dependabot alert), giving early " +
		"warning of exposure in the direct and transitive dependency graph.",
	// actions_workflow keys
	"default_workflow_permissions": "Limits GITHUB_TOKEN to read-only scope " +
		"by default, requiring workflows to explicitly request write access and " +
		"reducing the blast radius of a compromised or misconfigured workflow.",
	"can_approve_pull_request_reviews": "Allows workflow runs to approve pull " +
		"requests, which is required for automated merge bots and release " +
		"automation that must unblock their own PRs.",
}

// builtinRuleRationale holds fallback rationale for well-known ruleset rule types.
var builtinRuleRationale = map[string]string{
	"required_signatures": "All commits on the protected branch must be GPG " +
		"or SSH signed, proving commit provenance and making unauthorized " +
		"history modifications detectable.",
	"non_fast_forward": "Prevents force-pushes that rewrite history on the " +
		"protected branch, ensuring the commit graph is append-only and " +
		"existing signed commits cannot be silently replaced.",
	"deletion": "Prevents accidental or malicious deletion of the protected branch.",
	"pull_request": "Requires pull requests with at least the configured " +
		"number of approvals before a change can be merged, preventing " +
		"unreviewed code from landing on the default branch.",
	"required_status_checks": "Blocks merging a pull request until the " +
		"named CI checks all pass, preventing broken or untested code from " +
		"landing on the default branch.",
	"commit_author_email_pattern": "Enforces a specific email pattern on " +
		"commit authors, supporting identity verification and contribution " +
		"auditing policies.",
	"commit_message_pattern": "Enforces a commit message pattern (e.g. " +
		"Conventional Commits), keeping the history machine-readable and " +
		"enabling automated changelog generation.",
	"branch_name_pattern": "Enforces a branch naming convention for " +
		"consistency and automation compatibility.",
	"tag_name_pattern": "Enforces a tag naming convention, ensuring releases " +
		"follow a predictable versioning scheme.",
	"update": "Requires that pull-request branches be up-to-date with the " +
		"base before merging, preventing stale-base merge issues.",
	"creation": "Prevents new branches or tags from being created matching " +
		"the target pattern, guarding protected namespaces.",
}

// DescribeSource builds a Description for the given Source.
//
// When liveRepo is non-empty, it fetches the live settings from GitHub via
// ghBin to populate LiveValue/WouldChange on each SettingEntry and LiveState
// on each RulesetEntry.  Pass an empty string to produce a profile-only
// description without any live comparison.
//
// extraDescs may override the built-in rationale for any setting key; pass nil
// to use built-in rationale only.
func DescribeSource(ctx context.Context, ghBin string, src Source, liveRepo string, extraDescs map[string]string) (*Description, error) {
	desc := &Description{}

	// --- repo settings -------------------------------------------------------
	if settingsPath, err := src.RepoSettingsFile(); err == nil {
		raw, readErr := os.ReadFile(settingsPath)
		if readErr != nil {
			return nil, fmt.Errorf("posture: read settings file: %w", readErr)
		}
		var sf repoSettingsFile
		if err := json.Unmarshal(raw, &sf); err != nil {
			return nil, fmt.Errorf("posture: parse settings file: %w", err)
		}

		// Merge rationale: built-in < profile-file descriptions < extraDescs.
		rationale := mergedRationale(sf.Descriptions, extraDescs)

		// Optionally fetch live repo data once.
		var liveRepoMap map[string]json.RawMessage
		var liveActRaw []byte
		var liveVuln bool
		if liveRepo != "" {
			var ferr error
			liveRepoMap, liveActRaw, liveVuln, ferr = fetchLiveForDescribe(ctx, ghBin, liveRepo)
			if ferr != nil {
				return nil, ferr
			}
		}

		// Section 1: repo flags.
		if sf.Repo != nil {
			var desired map[string]json.RawMessage
			if err := json.Unmarshal(sf.Repo, &desired); err != nil {
				return nil, fmt.Errorf("posture: parse repo flags: %w", err)
			}
			for _, k := range sortedKeys(desired) {
				wantVal := string(desired[k])
				var liveVal string
				var wouldChange bool
				if liveRepoMap != nil {
					if lv, ok := liveRepoMap[k]; ok {
						liveVal, wouldChange = compareJSONValues(wantVal, string(lv))
					} else {
						liveVal = "(not present)"
						wouldChange = true
					}
				}
				desc.Settings = append(desc.Settings, SettingEntry{
					Section:     "repo flags",
					Key:         k,
					WantValue:   wantVal,
					LiveValue:   liveVal,
					WouldChange: wouldChange,
					Rationale:   lookupRationale(k, rationale),
				})
			}
		}

		// Section 2: security & analysis.
		if sf.SecurityAndAnalysis != nil {
			var desired map[string]json.RawMessage
			if err := json.Unmarshal(sf.SecurityAndAnalysis, &desired); err != nil {
				return nil, fmt.Errorf("posture: parse security_and_analysis: %w", err)
			}
			var liveSecMap map[string]json.RawMessage
			if liveRepoMap != nil {
				if liveSec, err := extractSecurityAndAnalysis(liveRepoMap); err == nil {
					// extractSecurityAndAnalysis returns map[string]string JSON;
					// unmarshal to map[string]json.RawMessage for uniform comparison.
					var flat map[string]interface{}
					if json.Unmarshal(liveSec, &flat) == nil {
						liveSecMap = make(map[string]json.RawMessage, len(flat))
						for fk, fv := range flat {
							if b, err := json.Marshal(fv); err == nil {
								liveSecMap[fk] = b
							}
						}
					}
				}
			}
			for _, k := range sortedKeys(desired) {
				wantVal := string(desired[k])
				var liveVal string
				var wouldChange bool
				if liveSecMap != nil {
					if lv, ok := liveSecMap[k]; ok {
						liveVal, wouldChange = compareJSONValues(wantVal, string(lv))
					} else {
						liveVal = "(not present)"
						wouldChange = true
					}
				}
				desc.Settings = append(desc.Settings, SettingEntry{
					Section:     "security & analysis",
					Key:         k,
					WantValue:   wantVal,
					LiveValue:   liveVal,
					WouldChange: wouldChange,
					Rationale:   lookupRationale(k, rationale),
				})
			}
		}

		// Section 3: vulnerability alerts.
		if sf.VulnerabilityAlerts != nil {
			wantVal := "false"
			if *sf.VulnerabilityAlerts {
				wantVal = "true"
			}
			var liveVal string
			var wouldChange bool
			if liveRepo != "" {
				if liveVuln {
					liveVal = "true"
				} else {
					liveVal = "false"
				}
				wouldChange = liveVal != wantVal
			}
			desc.Settings = append(desc.Settings, SettingEntry{
				Section:     "vulnerability alerts",
				Key:         "enabled",
				WantValue:   wantVal,
				LiveValue:   liveVal,
				WouldChange: wouldChange,
				Rationale:   lookupRationale("vulnerability_alerts", rationale),
			})
		}

		// Section 4: actions workflow permissions.
		if sf.ActionsWorkflow != nil {
			var desired map[string]json.RawMessage
			if err := json.Unmarshal(sf.ActionsWorkflow, &desired); err != nil {
				return nil, fmt.Errorf("posture: parse actions_workflow: %w", err)
			}
			var liveActMap map[string]json.RawMessage
			if liveActRaw != nil {
				if json.Unmarshal(liveActRaw, &liveActMap) != nil {
					liveActMap = nil
				}
			}
			for _, k := range sortedKeys(desired) {
				wantVal := string(desired[k])
				var liveVal string
				var wouldChange bool
				if liveActMap != nil {
					if lv, ok := liveActMap[k]; ok {
						liveVal, wouldChange = compareJSONValues(wantVal, string(lv))
					} else {
						liveVal = "(not present)"
						wouldChange = true
					}
				}
				desc.Settings = append(desc.Settings, SettingEntry{
					Section:     "actions workflow permissions",
					Key:         k,
					WantValue:   wantVal,
					LiveValue:   liveVal,
					WouldChange: wouldChange,
					Rationale:   lookupRationale(k, rationale),
				})
			}
		}
	}

	// --- rulesets ------------------------------------------------------------
	if dir, err := src.RulesetsDir(); err == nil {
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			return nil, fmt.Errorf("posture: read rulesets dir: %w", readErr)
		}

		// Pre-fetch live ruleset states when live comparison is requested.
		var liveStates map[string]string
		if liveRepo != "" {
			var ferr error
			liveStates, ferr = describeRulesetStates(ctx, ghBin, liveRepo, dir)
			if ferr != nil {
				return nil, ferr
			}
		}

		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
				continue
			}
			path := filepath.Join(dir, e.Name())
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil, fmt.Errorf("posture: read ruleset %s: %w", path, readErr)
			}
			re, parseErr := parseRulesetEntry(raw, extraDescs)
			if parseErr != nil {
				return nil, fmt.Errorf("posture: describe ruleset %s: %w", path, parseErr)
			}
			if liveStates != nil {
				if state, ok := liveStates[re.Name]; ok {
					re.LiveState = state
				} else {
					re.LiveState = "missing"
				}
			}
			desc.Rulesets = append(desc.Rulesets, re)
		}
	}

	return desc, nil
}

// PrintDescription writes the human-readable description to w.
// profileName and profileSummary are printed as a header when non-empty
// (pass empty strings for "repo describe" which has no named profile).
func PrintDescription(w io.Writer, d *Description, profileName, profileSummary string) {
	if profileName != "" {
		fmt.Fprintf(w, "Profile: %s\n", profileName)
		if profileSummary != "" {
			fmt.Fprintf(w, "%s\n", wrapText(profileSummary, 80, ""))
		}
		fmt.Fprintln(w)
	}

	if len(d.Settings) > 0 {
		fmt.Fprintln(w, "── Repo Settings ───────────────────────────────────────────────────────────")
		currentSection := ""
		for _, s := range d.Settings {
			if s.Section != currentSection {
				fmt.Fprintf(w, "\n  [%s]\n", s.Section)
				currentSection = s.Section
			}
			wantDisplay := jsonUnquote(s.WantValue)
			fmt.Fprintf(w, "  %-42s %s\n", s.Key, wantDisplay)
			if s.LiveValue != "" {
				liveDisplay := jsonUnquote(s.LiveValue)
				marker := "✓ no change"
				if s.WouldChange {
					marker = "→ WOULD CHANGE"
				}
				fmt.Fprintf(w, "    live: %-37s [%s]\n", liveDisplay, marker)
			}
			if s.Rationale != "" {
				fmt.Fprintf(w, "    %s\n", wrapText(s.Rationale, 76, "    "))
			}
			fmt.Fprintln(w)
		}
	}

	if len(d.Rulesets) > 0 {
		fmt.Fprintln(w, "── Rulesets ────────────────────────────────────────────────────────────────")
		for _, rs := range d.Rulesets {
			conditions := strings.Join(rs.Conditions, ", ")
			if conditions == "" {
				conditions = "(all branches)"
			}
			liveTag := ""
			if rs.LiveState != "" {
				switch rs.LiveState {
				case "ok":
					liveTag = "  [live: OK]"
				case "missing":
					liveTag = "  [live: MISSING]"
				case "drift":
					liveTag = "  [live: DRIFT]"
				}
			}
			fmt.Fprintf(w, "\n  [%s] target: %s  conditions: %s%s\n",
				rs.Name, rs.Target, conditions, liveTag)
			if rs.Rationale != "" {
				fmt.Fprintf(w, "  %s\n", wrapText(rs.Rationale, 76, "  "))
			}
			for _, r := range rs.Rules {
				if r.ParamsSummary != "" {
					fmt.Fprintf(w, "\n    %s  (%s)\n", r.Type, r.ParamsSummary)
				} else {
					fmt.Fprintf(w, "\n    %s\n", r.Type)
				}
				if r.Rationale != "" {
					fmt.Fprintf(w, "      %s\n", wrapText(r.Rationale, 74, "      "))
				}
			}
			fmt.Fprintln(w)
		}
	}
}

// ─── internal helpers ────────────────────────────────────────────────────────

// rulesetDescFile is the extended ruleset schema used for describe (includes
// optional metadata fields _rationale and _rule_descriptions that are stripped
// during normalisation and therefore invisible to check/apply).
type rulesetDescFile struct {
	Name             string            `json:"name"`
	Target           string            `json:"target"`
	Conditions       rulesetConditions `json:"conditions"`
	Rules            []rulesetRuleRaw  `json:"rules"`
	Rationale        string            `json:"_rationale"`
	RuleDescriptions map[string]string `json:"_rule_descriptions"`
}

type rulesetConditions struct {
	RefName struct {
		Include []string `json:"include"`
		Exclude []string `json:"exclude"`
	} `json:"ref_name"`
}

type rulesetRuleRaw struct {
	Type       string          `json:"type"`
	Parameters json.RawMessage `json:"parameters"`
}

// parseRulesetEntry builds a RulesetEntry from raw ruleset JSON.
// extraDescs may supply rationale for rule types keyed as "rule.<type>".
func parseRulesetEntry(raw []byte, extraDescs map[string]string) (RulesetEntry, error) {
	var rf rulesetDescFile
	if err := json.Unmarshal(raw, &rf); err != nil {
		return RulesetEntry{}, fmt.Errorf("parse ruleset: %w", err)
	}

	re := RulesetEntry{
		Name:       rf.Name,
		Target:     rf.Target,
		Conditions: rf.Conditions.RefName.Include,
		Rationale:  rf.Rationale,
	}

	for _, r := range rf.Rules {
		// Rationale lookup: _rule_descriptions in file > extraDescs > built-in.
		rat := ""
		if rf.RuleDescriptions != nil {
			rat = rf.RuleDescriptions[r.Type]
		}
		if rat == "" && extraDescs != nil {
			rat = extraDescs["rule."+r.Type]
		}
		if rat == "" {
			rat = builtinRuleRationale[r.Type]
		}

		re.Rules = append(re.Rules, RuleEntry{
			Type:          r.Type,
			ParamsSummary: ruleParamsSummary(r.Type, r.Parameters),
			Rationale:     rat,
		})
	}

	return re, nil
}

// ruleParamsSummary returns a short human-readable summary of notable rule
// parameters.  Returns empty string when nothing notable is present.
func ruleParamsSummary(ruleType string, params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	switch ruleType {
	case "pull_request":
		var p struct {
			RequiredApprovingReviewCount int `json:"required_approving_review_count"`
		}
		if json.Unmarshal(params, &p) == nil && p.RequiredApprovingReviewCount > 0 {
			return fmt.Sprintf("required_approving_review_count: %d", p.RequiredApprovingReviewCount)
		}
	case "required_status_checks":
		var p struct {
			RequiredStatusChecks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		}
		if json.Unmarshal(params, &p) == nil && len(p.RequiredStatusChecks) > 0 {
			names := make([]string, len(p.RequiredStatusChecks))
			for i, c := range p.RequiredStatusChecks {
				names[i] = c.Context
			}
			return strings.Join(names, ", ")
		}
	}
	return ""
}

// fetchLiveForDescribe fetches the live data required for describe's live comparison.
// Returns (repoMap, actionsRaw, vulnEnabled, error).
func fetchLiveForDescribe(ctx context.Context, ghBin, repo string) (map[string]json.RawMessage, []byte, bool, error) {
	repoEndpoint := fmt.Sprintf("repos/%s", repo)
	liveRepRaw, code, err := ghRun(ctx, ghBin, []string{"api", repoEndpoint}, nil)
	if err != nil {
		return nil, nil, false, fmt.Errorf("posture: fetch repo for describe: %w", err)
	}
	if code != 0 {
		return nil, nil, false, fmt.Errorf("posture: fetch repo for describe: gh exited %d", code)
	}
	var repoMap map[string]json.RawMessage
	if err := json.Unmarshal(liveRepRaw, &repoMap); err != nil {
		return nil, nil, false, fmt.Errorf("posture: parse live repo for describe: %w", err)
	}

	// Extract repo flags sub-map so callers can look up individual keys.
	repoFlagsRaw, err := extractRepoFlags(repoMap)
	if err != nil {
		return nil, nil, false, err
	}
	var repoFlagsMap map[string]json.RawMessage
	if err := json.Unmarshal(repoFlagsRaw, &repoFlagsMap); err != nil {
		return nil, nil, false, fmt.Errorf("posture: parse repo flags for describe: %w", err)
	}
	// Merge repo flags back into repoMap for uniform key lookup by callers.
	for k, v := range repoFlagsMap {
		repoMap[k] = v
	}

	// Actions workflow permissions.
	actEndpoint := fmt.Sprintf("repos/%s/actions/permissions/workflow", repo)
	actRaw, code, err := ghRun(ctx, ghBin, []string{"api", actEndpoint}, nil)
	if err != nil {
		return nil, nil, false, fmt.Errorf("posture: fetch actions perms for describe: %w", err)
	}
	if code != 0 {
		actRaw = nil // treat as unknown; won't block describe
	}

	// Vulnerability alerts.
	vulnEndpoint := fmt.Sprintf("repos/%s/vulnerability-alerts", repo)
	_, vulnCode, err := ghRun(ctx, ghBin, []string{"api", vulnEndpoint}, nil)
	if err != nil {
		return nil, nil, false, fmt.Errorf("posture: check vuln alerts for describe: %w", err)
	}
	vulnEnabled := (vulnCode == 0) // 204 = enabled; 404 = disabled

	return repoMap, actRaw, vulnEnabled, nil
}

// describeRulesetStates returns a map of ruleset-name → "ok"|"missing"|"drift"
// by comparing the desired rulesets in dir against the live GitHub state.
// It reuses normalizeRuleset for the comparison (same as check/apply).
func describeRulesetStates(ctx context.Context, ghBin, repo, dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("posture: read rulesets dir for describe: %w", err)
	}

	liveList, err := fetchRulesetList(ctx, ghBin, repo)
	if err != nil {
		return nil, err
	}
	nameToID := make(map[string]int64, len(liveList))
	for _, r := range liveList {
		nameToID[r.name] = r.id
	}

	states := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		wantRaw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var meta struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(wantRaw, &meta); err != nil {
			continue
		}
		name := meta.Name

		liveID, exists := nameToID[name]
		if !exists {
			states[name] = "missing"
			continue
		}

		endpoint := fmt.Sprintf("repos/%s/rulesets/%d", repo, liveID)
		liveRaw, code, err := ghRun(ctx, ghBin, []string{"api", endpoint}, nil)
		if err != nil || code != 0 {
			states[name] = "drift" // can't fetch → conservative
			continue
		}

		liveNorm, err := normalizeRuleset(liveRaw)
		if err != nil {
			states[name] = "drift"
			continue
		}
		wantNorm, err := normalizeRuleset(wantRaw)
		if err != nil {
			states[name] = "drift"
			continue
		}
		if string(liveNorm) == string(wantNorm) {
			states[name] = "ok"
		} else {
			states[name] = "drift"
		}
	}
	return states, nil
}

// ─── rationale helpers ────────────────────────────────────────────────────────

// mergedRationale builds the effective rationale map by layering:
// built-in fallbacks < profile-file descriptions < extra overrides (manifest).
func mergedRationale(profileFile, extra map[string]string) map[string]string {
	out := make(map[string]string, len(builtinSettingRationale))
	for k, v := range builtinSettingRationale {
		out[k] = v
	}
	for k, v := range profileFile {
		if v != "" {
			out[k] = v
		}
	}
	for k, v := range extra {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// lookupRationale returns the rationale for key from the merged map.
func lookupRationale(key string, rationale map[string]string) string {
	return rationale[key]
}

// ─── JSON helpers ─────────────────────────────────────────────────────────────

// compareJSONValues normalizes both JSON values and returns
// (normalizedLiveValue, wouldChange).
func compareJSONValues(want, live string) (string, bool) {
	wn, err1 := jsonSortKeys([]byte(want))
	ln, err2 := jsonSortKeys([]byte(live))
	if err1 != nil || err2 != nil {
		return live, want != live
	}
	return string(ln), string(wn) != string(ln)
}

// sortedKeys returns the keys of a map[string]json.RawMessage in alphabetical order.
func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// jsonUnquote strips JSON string quotes from a value like `"enabled"` → `enabled`.
// Non-string JSON values are returned as-is (e.g. false, 1).
func jsonUnquote(s string) string {
	var v string
	if json.Unmarshal([]byte(s), &v) == nil {
		return v
	}
	return s
}

// wrapText wraps text at maxWidth characters with indent on continuation lines.
// The first line is not indented (the caller provides the initial indent).
func wrapText(text string, maxWidth int, indent string) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}
	effectiveWidth := maxWidth - len(indent)
	if effectiveWidth < 20 {
		effectiveWidth = 20
	}
	var lines []string
	line := ""
	for _, w := range words {
		switch {
		case line == "":
			line = w
		case len(line)+1+len(w) <= effectiveWidth:
			line += " " + w
		default:
			lines = append(lines, line)
			line = w
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"+indent)
}
