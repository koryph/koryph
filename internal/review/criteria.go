// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package review

import (
	"fmt"
	"strings"

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
			addCriterionFinding(v, fmt.Sprintf("unexpected criterion assessment %s", printableCriterionID(id)))
		}
	}
	for id := range duplicates {
		addCriterionFinding(v, fmt.Sprintf("%s was evaluated more than once", printableCriterionID(id)))
	}
	for _, criterion := range criteria {
		assessment, ok := assessments[criterion.ID]
		status := strings.ToLower(strings.TrimSpace(assessment.Status))
		evidence := strings.TrimSpace(assessment.Evidence)
		if !ok || evidence == "" ||
			(status != "satisfied" && status != "unsatisfied" && status != "not-applicable") {
			addCriterionFinding(v, fmt.Sprintf("%s was not evaluated with a valid status and evidence", criterion.ID))
			continue
		}
		if status == "unsatisfied" {
			v.Blocking = true
		}
	}
}

func addCriterionFinding(v *Verdict, summary string) {
	v.Blocking = true
	v.Findings = append(v.Findings, Finding{Severity: "blocking", Summary: summary})
}

func printableCriterionID(id string) string {
	if id == "" {
		return "<missing ID>"
	}
	return id
}
