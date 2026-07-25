// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package plan_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/koryph/koryph/internal/plan"
)

func TestParseCriteriaExplicitStableIDs(t *testing.T) {
	got, err := plan.ParseCriteria("AC1: observable outcome\nAC2: failure behavior\nAC3: exact validation evidence")
	if err != nil {
		t.Fatal(err)
	}
	want := []plan.Criterion{
		{ID: "AC1", Text: "observable outcome"},
		{ID: "AC2", Text: "failure behavior"},
		{ID: "AC3", Text: "exact validation evidence"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("criteria = %#v, want %#v", got, want)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `[{"id":"AC1","text":"observable outcome"},{"id":"AC2","text":"failure behavior"},{"id":"AC3","text":"exact validation evidence"}]` {
		t.Fatalf("snapshot JSON = %s", data)
	}
	if formatted := plan.FormatCriteria(got); formatted !=
		"AC1: observable outcome\nAC2: failure behavior\nAC3: exact validation evidence" {
		t.Fatalf("canonical filing = %q", formatted)
	}
}

func TestParseCriteriaLegacyLinesRemainCompatible(t *testing.T) {
	got, err := plan.ParseCriteria("- Acceptance remains compatible\n- Regression test passes")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].ID != "AC1" || got[1].ID != "AC2" {
		t.Fatalf("legacy IDs = %#v", got)
	}
}

func TestParseStrictCriteriaRejectsWhollyUnnumberedField(t *testing.T) {
	_, err := plan.ParseStrictCriteria("- Observable outcome\n- Regression passes")
	var criteriaErr *plan.CriteriaError
	if !errors.As(err, &criteriaErr) || criteriaErr.Code != plan.CriteriaIDMissing {
		t.Fatalf("error = %v, want CriteriaError code %q", err, plan.CriteriaIDMissing)
	}
}

func TestParseStrictCriteriaAcceptsCanonicalExplicitField(t *testing.T) {
	got, err := plan.ParseStrictCriteria("AC1: Observable outcome\nAC2: Regression passes")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].ID != "AC1" || got[1].ID != "AC2" {
		t.Fatalf("strict criteria = %#v", got)
	}
}

func TestParseCriteriaRejectsInvalidAtomicContracts(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		code string
	}{
		{"missing", " \n ", plan.CriteriaMissing},
		{"missing ID in explicit field", "AC1: first\nsecond", plan.CriteriaIDMissing},
		{"duplicate ID", "AC1: first\nAC1: second", plan.CriteriaIDDuplicate},
		{"sequence gap", "AC1: first\nAC3: third", plan.CriteriaIDSequence},
		{"empty outcome", "AC1:", plan.CriteriaTextMissing},
		{"semicolon packed", "AC1: build feature; add its tests", plan.CriteriaCompound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := plan.ParseCriteria(tc.raw)
			var criteriaErr *plan.CriteriaError
			if !errors.As(err, &criteriaErr) || criteriaErr.Code != tc.code {
				t.Fatalf("error = %v, want CriteriaError code %q", err, tc.code)
			}
		})
	}
}
