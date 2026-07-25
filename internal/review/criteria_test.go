// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package review

import (
	"reflect"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/phasecontrol"
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

func TestValidateCompletionEvidenceCanonicalMatrix(t *testing.T) {
	criteria := []AcceptanceCriterion{
		{ID: "AC1", Text: "feature exists"},
		{ID: "AC2", Text: "regression passes"},
	}
	valid := phasecontrol.Evidence{
		FocusedTests: []phasecontrol.FocusedTestEvidence{{
			Command: "go test ./internal/review -run TestFeature", ExitStatus: 0,
			LogPath: "focused.log", LogDigest: "sha256:focused",
		}},
		Acceptance: []phasecontrol.AcceptanceEvidence{
			{CriterionID: "AC1", References: []phasecontrol.EvidenceReference{{
				Kind: "file", Path: "internal/review/review.go", Digest: "sha256:file",
			}}},
			{CriterionID: "AC2", References: []phasecontrol.EvidenceReference{{
				Kind: "focused-test", Command: "go test ./internal/review -run TestFeature",
			}}},
		},
	}
	if err := ValidateCompletionEvidence(criteria, valid); err != nil {
		t.Fatalf("valid matrix rejected: %v", err)
	}

	tests := []struct {
		name string
		edit func(*phasecontrol.Evidence)
		want string
	}{
		{"missing criterion", func(e *phasecontrol.Evidence) {
			e.Acceptance = e.Acceptance[:1]
		}, "omits criterion AC2"},
		{"duplicate criterion", func(e *phasecontrol.Evidence) {
			e.Acceptance = append(e.Acceptance, e.Acceptance[0])
		}, "duplicates criterion AC1"},
		{"unknown criterion", func(e *phasecontrol.Evidence) {
			e.Acceptance[0].CriterionID = "AC9"
		}, "unknown criterion AC9"},
		{"unknown focused test", func(e *phasecontrol.Evidence) {
			e.Acceptance[1].References[0].Command = "go test ./unknown"
		}, "references unknown focused test"},
		{"failed focused test", func(e *phasecontrol.Evidence) {
			e.FocusedTests[0].ExitStatus = 1
		}, "failed with exit status 1"},
		{"unauthenticated file", func(e *phasecontrol.Evidence) {
			e.Acceptance[0].References[0].Digest = ""
		}, "invalid file reference"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			evidence := valid
			evidence.FocusedTests = append([]phasecontrol.FocusedTestEvidence(nil), valid.FocusedTests...)
			evidence.Acceptance = append([]phasecontrol.AcceptanceEvidence(nil), valid.Acceptance...)
			for i := range evidence.Acceptance {
				evidence.Acceptance[i].References = append(
					[]phasecontrol.EvidenceReference(nil), valid.Acceptance[i].References...,
				)
			}
			tc.edit(&evidence)
			err := ValidateCompletionEvidence(criteria, evidence)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}
