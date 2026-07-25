// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package review

import (
	"fmt"
	"strings"

	"github.com/koryph/koryph/internal/phasecontrol"
	"github.com/koryph/koryph/internal/plan"
)

// AcceptanceCriterion is the planner's canonical criterion model. Keeping an
// alias here prevents reviewer prompts and evidence matrices from inventing a
// second ID grammar.
type AcceptanceCriterion = plan.Criterion

// ParseAcceptanceCriteria delegates to the planning grammar. U3 must use this
// for both prompt rendering and evidence enforcement.
func ParseAcceptanceCriteria(raw string) ([]AcceptanceCriterion, error) {
	return plan.ParseStrictCriteria(raw)
}

// EnforceCriteria fails a verdict closed unless every expected stable ID is
// evaluated exactly once with a valid status and non-empty evidence.
func EnforceCriteria(v *Verdict, criteria []AcceptanceCriterion) {
	if len(criteria) == 0 {
		return
	}
	expected := make(map[string]bool, len(criteria))
	assessments := make(map[string]CriterionAssessment, len(v.Criteria))
	duplicates := map[string]bool{}
	for _, criterion := range criteria {
		expected[criterion.ID] = true
	}
	for _, assessment := range v.Criteria {
		id := strings.ToUpper(strings.TrimSpace(assessment.ID))
		if _, exists := assessments[id]; exists {
			duplicates[id] = true
		}
		assessments[id] = assessment
		if !expected[id] {
			addCriterionFinding(v, fmt.Sprintf("unexpected criterion assessment %s", printableCriterionID(id)), false)
		}
	}
	for id := range duplicates {
		addCriterionFinding(v, fmt.Sprintf("%s was evaluated more than once", printableCriterionID(id)), false)
	}
	for _, criterion := range criteria {
		assessment, ok := assessments[criterion.ID]
		status := strings.ToLower(strings.TrimSpace(assessment.Status))
		evidence := strings.TrimSpace(assessment.Evidence)
		if !ok || evidence == "" ||
			(status != "satisfied" && status != "unsatisfied" && status != "not-applicable") {
			addCriterionFinding(v, fmt.Sprintf("%s was not evaluated with a valid status and evidence", criterion.ID), false)
			continue
		}
		if status == "unsatisfied" {
			addCriterionFinding(v, fmt.Sprintf("%s is unsatisfied: %s", criterion.ID, evidence), true)
		}
	}
}

func addCriterionFinding(v *Verdict, summary string, trackHistory bool) {
	v.Blocking = true
	v.Findings = append(v.Findings, Finding{
		Severity: "blocking", Summary: summary, TrackHistory: trackHistory,
	})
}

func printableCriterionID(id string) string {
	if id == "" {
		return "<missing ID>"
	}
	return id
}

// ValidateCompletionEvidence independently audits the exact worker evidence
// matrix threaded from phasecontrol.ResultManifest. Candidate admission already
// validates the same structure; repeating the cheap structural audit here
// prevents an engine call-site omission or translation bug from silently
// stripping the evidence the general reviewer is supposed to examine.
func ValidateCompletionEvidence(criteria []AcceptanceCriterion, evidence phasecontrol.Evidence) error {
	if len(criteria) == 0 {
		return nil
	}
	tests := make(map[string]phasecontrol.FocusedTestEvidence, len(evidence.FocusedTests))
	for _, test := range evidence.FocusedTests {
		command := strings.TrimSpace(test.Command)
		if command == "" {
			return fmt.Errorf("completion evidence has a focused test without a command")
		}
		if _, exists := tests[command]; exists {
			return fmt.Errorf("completion evidence duplicates focused test %q", command)
		}
		if test.ExitStatus != 0 {
			return fmt.Errorf("completion evidence focused test %q failed with exit status %d", command, test.ExitStatus)
		}
		if strings.TrimSpace(test.LogPath) == "" || strings.TrimSpace(test.LogDigest) == "" {
			return fmt.Errorf("completion evidence focused test %q lacks an authenticated log", command)
		}
		tests[command] = test
	}

	expected := make(map[string]bool, len(criteria))
	for _, criterion := range criteria {
		expected[criterion.ID] = true
	}
	seen := make(map[string]bool, len(evidence.Acceptance))
	for _, item := range evidence.Acceptance {
		id := strings.ToUpper(strings.TrimSpace(item.CriterionID))
		if !expected[id] {
			return fmt.Errorf("completion evidence contains unknown criterion %s", printableCriterionID(id))
		}
		if seen[id] {
			return fmt.Errorf("completion evidence duplicates criterion %s", id)
		}
		seen[id] = true
		if len(item.References) == 0 {
			return fmt.Errorf("completion evidence criterion %s has no references", id)
		}
		for _, ref := range item.References {
			switch strings.ToLower(strings.TrimSpace(ref.Kind)) {
			case "file":
				if strings.TrimSpace(ref.Path) == "" || strings.TrimSpace(ref.Digest) == "" ||
					strings.TrimSpace(ref.Command) != "" {
					return fmt.Errorf("completion evidence criterion %s has an invalid file reference", id)
				}
			case "focused-test":
				command := strings.TrimSpace(ref.Command)
				if command == "" || strings.TrimSpace(ref.Path) != "" || strings.TrimSpace(ref.Digest) != "" {
					return fmt.Errorf("completion evidence criterion %s has an invalid focused-test reference", id)
				}
				if _, ok := tests[command]; !ok {
					return fmt.Errorf("completion evidence criterion %s references unknown focused test %q", id, command)
				}
			default:
				return fmt.Errorf("completion evidence criterion %s has unsupported reference kind %q", id, ref.Kind)
			}
		}
	}
	for _, criterion := range criteria {
		if !seen[criterion.ID] {
			return fmt.Errorf("completion evidence omits criterion %s", criterion.ID)
		}
	}
	return nil
}

func acceptanceEvidenceByID(evidence phasecontrol.Evidence) map[string]phasecontrol.AcceptanceEvidence {
	out := make(map[string]phasecontrol.AcceptanceEvidence, len(evidence.Acceptance))
	for _, item := range evidence.Acceptance {
		out[strings.ToUpper(strings.TrimSpace(item.CriterionID))] = item
	}
	return out
}

func focusedTestByCommand(evidence phasecontrol.Evidence, command string) phasecontrol.FocusedTestEvidence {
	command = strings.TrimSpace(command)
	for _, test := range evidence.FocusedTests {
		if strings.TrimSpace(test.Command) == command {
			return test
		}
	}
	return phasecontrol.FocusedTestEvidence{}
}
