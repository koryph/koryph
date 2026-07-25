// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package review

import (
	"reflect"
	"testing"

	"github.com/koryph/koryph/internal/plan"
)

func TestReviewerUsesPlannerCriterionModel(t *testing.T) {
	raw := "AC1: observable outcome\nAC2: exact validation evidence"
	want, err := plan.ParseCriteria(raw)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseAcceptanceCriteria(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("review criteria = %#v, planner criteria = %#v", got, want)
	}
}

func TestReviewerRejectsLegacyUnnumberedCriteria(t *testing.T) {
	if _, err := ParseAcceptanceCriteria("- feature exists\n- regression passes"); err == nil {
		t.Fatal("live reviewer accepted migration-only unnumbered criteria")
	}
}

func TestEnforceCriteriaFailsClosed(t *testing.T) {
	criteria := []AcceptanceCriterion{
		{ID: "AC1", Text: "feature exists"},
		{ID: "AC2", Text: "regression passes"},
	}
	tests := []struct {
		name        string
		assessments []CriterionAssessment
	}{
		{"missing", []CriterionAssessment{{ID: "AC1", Status: "satisfied", Evidence: "feature.go"}}},
		{"duplicate", []CriterionAssessment{
			{ID: "AC1", Status: "satisfied", Evidence: "feature.go"},
			{ID: "AC1", Status: "satisfied", Evidence: "feature_test.go"},
			{ID: "AC2", Status: "satisfied", Evidence: "go test ./..."},
		}},
		{"unevaluated", []CriterionAssessment{
			{ID: "AC1", Status: "satisfied", Evidence: "feature.go"},
			{ID: "AC2", Status: "", Evidence: ""},
		}},
		{"unknown", []CriterionAssessment{
			{ID: "AC1", Status: "satisfied", Evidence: "feature.go"},
			{ID: "AC2", Status: "satisfied", Evidence: "go test ./..."},
			{ID: "AC3", Status: "satisfied", Evidence: "unexpected"},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := Verdict{Criteria: tc.assessments}
			EnforceCriteria(&v, criteria)
			if !v.Blocking || len(v.Findings) == 0 {
				t.Fatalf("verdict did not fail closed: %+v", v)
			}
		})
	}
}

func TestEnforceCriteriaAcceptsCompleteUniqueEvidence(t *testing.T) {
	v := Verdict{Criteria: []CriterionAssessment{
		{ID: "AC1", Status: "satisfied", Evidence: "feature.go"},
		{ID: "AC2", Status: "not-applicable", Evidence: "no migration in diff"},
	}}
	EnforceCriteria(&v, []AcceptanceCriterion{
		{ID: "AC1", Text: "feature exists"},
		{ID: "AC2", Text: "migration safety"},
	})
	if v.Blocking || len(v.Findings) != 0 {
		t.Fatalf("complete evidence rejected: %+v", v)
	}
}
