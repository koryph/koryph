// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package posture_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/posture"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// describeSource builds a LocalSource with both a repo-settings.json and a
// rulesets directory populated from the provided maps.
func describeSource(t *testing.T, settings map[string]interface{}, rulesets map[string]interface{}) posture.LocalSource {
	t.Helper()
	root := t.TempDir()

	// Write repo-settings.json.
	if settings != nil {
		if err := os.MkdirAll(filepath.Join(root, ".github"), 0o755); err != nil {
			t.Fatal(err)
		}
		data, err := json.MarshalIndent(settings, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".github", "repo-settings.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Write ruleset files.
	if rulesets != nil {
		dir := filepath.Join(root, ".github", "rulesets")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, content := range rulesets {
			data, err := json.MarshalIndent(content, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name+".json"), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	return posture.LocalSource{Root: root}
}

// ─── DescribeSource (no live) ─────────────────────────────────────────────────

func TestDescribeSource_SettingsOnly_NoLive(t *testing.T) {
	src := describeSource(t, map[string]interface{}{
		"repo": map[string]interface{}{
			"allow_merge_commit":     false,
			"delete_branch_on_merge": true,
		},
		"vulnerability_alerts": true,
	}, nil)

	desc, err := posture.DescribeSource(context.Background(), "gh", src, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(desc.Settings) == 0 {
		t.Fatal("expected at least one setting entry")
	}

	// Every entry should have an empty LiveValue (no live comparison requested).
	for _, s := range desc.Settings {
		if s.LiveValue != "" {
			t.Errorf("expected empty LiveValue without --repo; got %q for key %s", s.LiveValue, s.Key)
		}
	}
}

func TestDescribeSource_RepoFlagsHaveRationale(t *testing.T) {
	src := describeSource(t, map[string]interface{}{
		"repo": map[string]interface{}{
			"allow_merge_commit":          false,
			"delete_branch_on_merge":      true,
			"web_commit_signoff_required": true,
		},
	}, nil)

	desc, err := posture.DescribeSource(context.Background(), "gh", src, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, s := range desc.Settings {
		if s.Rationale == "" {
			t.Errorf("expected rationale for key %q; got empty string", s.Key)
		}
	}
}

func TestDescribeSource_SecurityAndAnalysisHaveRationale(t *testing.T) {
	src := describeSource(t, map[string]interface{}{
		"security_and_analysis": map[string]interface{}{
			"secret_scanning":                 "enabled",
			"secret_scanning_push_protection": "enabled",
			"dependabot_security_updates":     "enabled",
		},
	}, nil)

	desc, err := posture.DescribeSource(context.Background(), "gh", src, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(desc.Settings) == 0 {
		t.Fatal("expected settings entries")
	}
	for _, s := range desc.Settings {
		if s.Rationale == "" {
			t.Errorf("expected rationale for security key %q; got empty string", s.Key)
		}
		if s.Section != "security & analysis" {
			t.Errorf("expected section 'security & analysis'; got %q for key %s", s.Section, s.Key)
		}
	}
}

func TestDescribeSource_VulnerabilityAlerts(t *testing.T) {
	enabled := true
	src := describeSource(t, map[string]interface{}{
		"vulnerability_alerts": enabled,
	}, nil)

	desc, err := posture.DescribeSource(context.Background(), "gh", src, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(desc.Settings) != 1 {
		t.Fatalf("expected 1 setting; got %d", len(desc.Settings))
	}
	s := desc.Settings[0]
	if s.Key != "enabled" {
		t.Errorf("expected key 'enabled'; got %q", s.Key)
	}
	if s.WantValue != "true" {
		t.Errorf("expected WantValue 'true'; got %q", s.WantValue)
	}
	if s.Section != "vulnerability alerts" {
		t.Errorf("expected section 'vulnerability alerts'; got %q", s.Section)
	}
	if s.Rationale == "" {
		t.Error("expected rationale for vulnerability_alerts")
	}
}

func TestDescribeSource_ProfileDescriptionsOverrideBuiltin(t *testing.T) {
	customRationale := "Custom rationale for allow_merge_commit."
	src := describeSource(t, map[string]interface{}{
		"repo": map[string]interface{}{
			"allow_merge_commit": false,
		},
		"descriptions": map[string]string{
			"allow_merge_commit": customRationale,
		},
	}, nil)

	desc, err := posture.DescribeSource(context.Background(), "gh", src, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(desc.Settings) == 0 {
		t.Fatal("expected at least one setting")
	}
	if desc.Settings[0].Rationale != customRationale {
		t.Errorf("expected custom rationale %q; got %q", customRationale, desc.Settings[0].Rationale)
	}
}

func TestDescribeSource_ExtraDescsOverrideAll(t *testing.T) {
	extra := map[string]string{
		"allow_merge_commit": "Top-level override rationale.",
	}
	src := describeSource(t, map[string]interface{}{
		"repo": map[string]interface{}{
			"allow_merge_commit": false,
		},
		"descriptions": map[string]string{
			"allow_merge_commit": "Profile-file rationale.",
		},
	}, nil)

	desc, err := posture.DescribeSource(context.Background(), "gh", src, "", extra)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(desc.Settings) == 0 {
		t.Fatal("expected at least one setting")
	}
	if desc.Settings[0].Rationale != "Top-level override rationale." {
		t.Errorf("expected extra-desc override; got %q", desc.Settings[0].Rationale)
	}
}

// ─── RulesetEntry parsing ─────────────────────────────────────────────────────

func TestDescribeSource_RulesetEntry_BasicFields(t *testing.T) {
	src := describeSource(t, nil, map[string]interface{}{
		"signed-commits": map[string]interface{}{
			"name":   "signed-commits",
			"target": "branch",
			"conditions": map[string]interface{}{
				"ref_name": map[string]interface{}{
					"include": []string{"~DEFAULT_BRANCH"},
					"exclude": []string{},
				},
			},
			"rules": []interface{}{
				map[string]interface{}{"type": "required_signatures"},
				map[string]interface{}{"type": "non_fast_forward"},
				map[string]interface{}{"type": "deletion"},
			},
			"enforcement": "active",
		},
	})

	desc, err := posture.DescribeSource(context.Background(), "gh", src, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(desc.Rulesets) != 1 {
		t.Fatalf("expected 1 ruleset; got %d", len(desc.Rulesets))
	}
	rs := desc.Rulesets[0]
	if rs.Name != "signed-commits" {
		t.Errorf("expected name 'signed-commits'; got %q", rs.Name)
	}
	if rs.Target != "branch" {
		t.Errorf("expected target 'branch'; got %q", rs.Target)
	}
	if len(rs.Conditions) == 0 || rs.Conditions[0] != "~DEFAULT_BRANCH" {
		t.Errorf("expected conditions [~DEFAULT_BRANCH]; got %v", rs.Conditions)
	}
	if len(rs.Rules) != 3 {
		t.Fatalf("expected 3 rules; got %d", len(rs.Rules))
	}
}

func TestDescribeSource_RulesetEntry_BuiltinRuleRationale(t *testing.T) {
	src := describeSource(t, nil, map[string]interface{}{
		"signed-commits": map[string]interface{}{
			"name":        "signed-commits",
			"target":      "branch",
			"enforcement": "active",
			"rules": []interface{}{
				map[string]interface{}{"type": "required_signatures"},
			},
		},
	})

	desc, err := posture.DescribeSource(context.Background(), "gh", src, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(desc.Rulesets) == 0 || len(desc.Rulesets[0].Rules) == 0 {
		t.Fatal("expected ruleset with rules")
	}
	if desc.Rulesets[0].Rules[0].Rationale == "" {
		t.Error("expected built-in rationale for required_signatures")
	}
}

func TestDescribeSource_RulesetEntry_RationaleFromFile(t *testing.T) {
	customRationale := "Our ruleset protects the main branch."
	src := describeSource(t, nil, map[string]interface{}{
		"myruleset": map[string]interface{}{
			"name":        "myruleset",
			"target":      "branch",
			"enforcement": "active",
			"_rationale":  customRationale,
			"rules": []interface{}{
				map[string]interface{}{"type": "deletion"},
			},
		},
	})

	desc, err := posture.DescribeSource(context.Background(), "gh", src, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(desc.Rulesets) == 0 {
		t.Fatal("expected 1 ruleset")
	}
	if desc.Rulesets[0].Rationale != customRationale {
		t.Errorf("expected _rationale from file; got %q", desc.Rulesets[0].Rationale)
	}
}

func TestDescribeSource_RulesetEntry_RuleDescriptionsFromFile(t *testing.T) {
	customRuleRationale := "Custom deletion rationale."
	src := describeSource(t, nil, map[string]interface{}{
		"myruleset": map[string]interface{}{
			"name":        "myruleset",
			"target":      "branch",
			"enforcement": "active",
			"_rule_descriptions": map[string]string{
				"deletion": customRuleRationale,
			},
			"rules": []interface{}{
				map[string]interface{}{"type": "deletion"},
			},
		},
	})

	desc, err := posture.DescribeSource(context.Background(), "gh", src, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(desc.Rulesets) == 0 || len(desc.Rulesets[0].Rules) == 0 {
		t.Fatal("expected ruleset with rules")
	}
	if desc.Rulesets[0].Rules[0].Rationale != customRuleRationale {
		t.Errorf("expected _rule_descriptions override; got %q", desc.Rulesets[0].Rules[0].Rationale)
	}
}

func TestDescribeSource_RulesetEntry_PullRequestParamsSummary(t *testing.T) {
	src := describeSource(t, nil, map[string]interface{}{
		"pr-checks": map[string]interface{}{
			"name":        "pr-checks",
			"target":      "branch",
			"enforcement": "active",
			"rules": []interface{}{
				map[string]interface{}{
					"type": "pull_request",
					"parameters": map[string]interface{}{
						"required_approving_review_count": 2,
					},
				},
			},
		},
	})

	desc, err := posture.DescribeSource(context.Background(), "gh", src, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(desc.Rulesets) == 0 || len(desc.Rulesets[0].Rules) == 0 {
		t.Fatal("expected ruleset with rules")
	}
	r := desc.Rulesets[0].Rules[0]
	if !strings.Contains(r.ParamsSummary, "2") {
		t.Errorf("expected ParamsSummary to mention count 2; got %q", r.ParamsSummary)
	}
}

// ─── _rationale/_rule_descriptions stripped from normalizeRuleset ─────────────

func TestDescribeMetadata_StrippedFromNormalization(t *testing.T) {
	// A ruleset with _rationale and _rule_descriptions should compare equal to
	// the same ruleset without those fields after normalisation — they are
	// describe-only metadata and must be invisible to check/apply.
	withMeta := map[string]interface{}{
		"name":        "protect-main",
		"enforcement": "active",
		"target":      "branch",
		"_rationale":  "This is metadata.",
		"_rule_descriptions": map[string]string{
			"deletion": "Protects the branch.",
		},
		"rules": []interface{}{
			map[string]interface{}{"type": "deletion"},
		},
	}
	withoutMeta := map[string]interface{}{
		"name":        "protect-main",
		"enforcement": "active",
		"target":      "branch",
		"rules": []interface{}{
			map[string]interface{}{"type": "deletion"},
		},
	}

	srcWith := rulesetSource(t, map[string]interface{}{"protect-main": withMeta})
	srcWithout := rulesetSource(t, map[string]interface{}{"protect-main": withoutMeta})

	// The live ruleset is the "without" version — check should see no drift.
	live := map[string]interface{}{
		"id":          1,
		"name":        "protect-main",
		"enforcement": "active",
		"target":      "branch",
		"rules": []interface{}{
			map[string]interface{}{"type": "deletion"},
		},
	}
	listResp, _ := json.Marshal([]map[string]interface{}{{"id": 1, "name": "protect-main"}})
	liveResp, _ := json.Marshal(live)

	script := `args="$*"
case "$args" in
  "api repos/acme/r/rulesets") echo '` + string(listResp) + `' ;;
  "api repos/acme/r/rulesets/1") echo '` + string(liveResp) + `' ;;
  *) echo "unhandled: $args" >&2; exit 1 ;;
esac`
	ghBin := fakeGH(t, script)

	var out bytes.Buffer
	// Source WITH metadata should produce no drift against live WITHOUT metadata.
	drift, err := posture.CheckRulesets(context.Background(), ghBin, "acme/r", srcWith, &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if drift {
		t.Errorf("expected no drift when _rationale/_rule_descriptions are stripped; output:\n%s", out.String())
	}

	out.Reset()
	// Source WITHOUT metadata — baseline check still passes.
	drift, err = posture.CheckRulesets(context.Background(), ghBin, "acme/r", srcWithout, &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if drift {
		t.Errorf("expected no drift in baseline check; output:\n%s", out.String())
	}
}

// ─── PrintDescription ────────────────────────────────────────────────────────

func TestPrintDescription_ProfileHeader(t *testing.T) {
	desc := &posture.Description{
		Settings: []posture.SettingEntry{
			{
				Section:   "repo flags",
				Key:       "allow_merge_commit",
				WantValue: "false",
				Rationale: "Prevents merge commits.",
			},
		},
	}
	var buf bytes.Buffer
	posture.PrintDescription(&buf, desc, "my-profile", "A test profile.")
	out := buf.String()
	if !strings.Contains(out, "Profile: my-profile") {
		t.Errorf("expected profile header; got:\n%s", out)
	}
	if !strings.Contains(out, "A test profile.") {
		t.Errorf("expected profile description; got:\n%s", out)
	}
}

func TestPrintDescription_NoHeader_WhenNoProfileName(t *testing.T) {
	desc := &posture.Description{
		Settings: []posture.SettingEntry{
			{Section: "repo flags", Key: "allow_merge_commit", WantValue: "false"},
		},
	}
	var buf bytes.Buffer
	posture.PrintDescription(&buf, desc, "", "")
	out := buf.String()
	if strings.Contains(out, "Profile:") {
		t.Errorf("expected no profile header when name is empty; got:\n%s", out)
	}
}

func TestPrintDescription_SettingsAndRulesets(t *testing.T) {
	desc := &posture.Description{
		Settings: []posture.SettingEntry{
			{
				Section:   "repo flags",
				Key:       "allow_merge_commit",
				WantValue: "false",
				Rationale: "Prevents merge commits.",
			},
		},
		Rulesets: []posture.RulesetEntry{
			{
				Name:       "signed-commits",
				Target:     "branch",
				Conditions: []string{"~DEFAULT_BRANCH"},
				Rationale:  "Enforces signing.",
				Rules: []posture.RuleEntry{
					{
						Type:      "required_signatures",
						Rationale: "All commits must be signed.",
					},
				},
			},
		},
	}
	var buf bytes.Buffer
	posture.PrintDescription(&buf, desc, "", "")
	out := buf.String()

	if !strings.Contains(out, "allow_merge_commit") {
		t.Errorf("expected allow_merge_commit in output; got:\n%s", out)
	}
	if !strings.Contains(out, "signed-commits") {
		t.Errorf("expected signed-commits in output; got:\n%s", out)
	}
	if !strings.Contains(out, "required_signatures") {
		t.Errorf("expected required_signatures in output; got:\n%s", out)
	}
	if !strings.Contains(out, "Prevents merge commits.") {
		t.Errorf("expected rationale text; got:\n%s", out)
	}
}

func TestPrintDescription_LiveValue_NoChange(t *testing.T) {
	desc := &posture.Description{
		Settings: []posture.SettingEntry{
			{
				Section:     "repo flags",
				Key:         "allow_merge_commit",
				WantValue:   "false",
				LiveValue:   "false",
				WouldChange: false,
				Rationale:   "test",
			},
		},
	}
	var buf bytes.Buffer
	posture.PrintDescription(&buf, desc, "", "")
	out := buf.String()
	if !strings.Contains(out, "no change") {
		t.Errorf("expected 'no change' marker for matching live value; got:\n%s", out)
	}
}

func TestPrintDescription_LiveValue_WouldChange(t *testing.T) {
	desc := &posture.Description{
		Settings: []posture.SettingEntry{
			{
				Section:     "security & analysis",
				Key:         "secret_scanning",
				WantValue:   `"enabled"`,
				LiveValue:   `"disabled"`,
				WouldChange: true,
				Rationale:   "test",
			},
		},
	}
	var buf bytes.Buffer
	posture.PrintDescription(&buf, desc, "", "")
	out := buf.String()
	if !strings.Contains(out, "WOULD CHANGE") {
		t.Errorf("expected 'WOULD CHANGE' marker for drifting live value; got:\n%s", out)
	}
}

func TestPrintDescription_RulesetLiveState(t *testing.T) {
	desc := &posture.Description{
		Rulesets: []posture.RulesetEntry{
			{Name: "signed-commits", Target: "branch", LiveState: "missing"},
			{Name: "pr-checks", Target: "branch", LiveState: "ok"},
			{Name: "extra-rule", Target: "branch", LiveState: "drift"},
		},
	}
	var buf bytes.Buffer
	posture.PrintDescription(&buf, desc, "", "")
	out := buf.String()
	if !strings.Contains(out, "[live: MISSING]") {
		t.Errorf("expected [live: MISSING]; got:\n%s", out)
	}
	if !strings.Contains(out, "[live: OK]") {
		t.Errorf("expected [live: OK]; got:\n%s", out)
	}
	if !strings.Contains(out, "[live: DRIFT]") {
		t.Errorf("expected [live: DRIFT]; got:\n%s", out)
	}
}

// ─── Builtin profile describe ─────────────────────────────────────────────────

func TestDescribeSource_BuiltinProfile_HasAllSettings(t *testing.T) {
	home := t.TempDir()
	src, cleanup, err := posture.RenderProfile("oss-solo-maintainer", nil, home)
	if err != nil {
		t.Fatalf("RenderProfile: %v", err)
	}
	defer cleanup()

	desc, err := posture.DescribeSource(context.Background(), "gh", src, "", nil)
	if err != nil {
		t.Fatalf("DescribeSource: %v", err)
	}

	if len(desc.Settings) == 0 {
		t.Fatal("expected settings from oss-solo-maintainer profile")
	}
	if len(desc.Rulesets) == 0 {
		t.Fatal("expected rulesets from oss-solo-maintainer profile")
	}

	// Every setting should have a rationale.
	for _, s := range desc.Settings {
		if s.Rationale == "" {
			t.Errorf("missing rationale for setting %q in section %q", s.Key, s.Section)
		}
	}

	// Both rulesets should be present.
	rulesetNames := make(map[string]bool)
	for _, rs := range desc.Rulesets {
		rulesetNames[rs.Name] = true
	}
	for _, want := range []string{"signed-commits", "pr-checks"} {
		if !rulesetNames[want] {
			t.Errorf("expected ruleset %q; found: %v", want, rulesetNames)
		}
	}
}

func TestDescribeSource_BuiltinProfile_AllRulesHaveRationale(t *testing.T) {
	home := t.TempDir()
	src, cleanup, err := posture.RenderProfile("oss-solo-maintainer", nil, home)
	if err != nil {
		t.Fatalf("RenderProfile: %v", err)
	}
	defer cleanup()

	desc, err := posture.DescribeSource(context.Background(), "gh", src, "", nil)
	if err != nil {
		t.Fatalf("DescribeSource: %v", err)
	}

	for _, rs := range desc.Rulesets {
		for _, r := range rs.Rules {
			if r.Rationale == "" {
				t.Errorf("missing rationale for rule type %q in ruleset %q", r.Type, rs.Name)
			}
		}
	}
}

func TestDescribeSource_BuiltinProfile_PrintRoundTrip(t *testing.T) {
	// Smoke-test: rendering the full describe output doesn't panic and produces
	// non-empty output containing key landmarks.
	home := t.TempDir()
	src, cleanup, err := posture.RenderProfile("oss-solo-maintainer", nil, home)
	if err != nil {
		t.Fatalf("RenderProfile: %v", err)
	}
	defer cleanup()

	desc, err := posture.DescribeSource(context.Background(), "gh", src, "", nil)
	if err != nil {
		t.Fatalf("DescribeSource: %v", err)
	}

	var buf bytes.Buffer
	posture.PrintDescription(&buf, desc, "oss-solo-maintainer",
		"Baseline posture for an OSS project with a solo maintainer.")
	out := buf.String()

	for _, landmark := range []string{
		"Profile: oss-solo-maintainer",
		"allow_merge_commit",
		"secret_scanning",
		"signed-commits",
		"pr-checks",
	} {
		if !strings.Contains(out, landmark) {
			t.Errorf("expected %q in describe output; got:\n%s", landmark, out)
		}
	}
}
