// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package loop

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCanaryGenerationDigestBindsBinaryAndStablePolicy(t *testing.T) {
	spec := CanarySpec{
		Cohort: []string{"b2", "b1"}, TargetWidth: 2,
		InactivityLimit:        DefaultCanaryInactivityLimit,
		HardStops:              CanonicalCanaryHardStops(nil),
		InstalledCommit:        strings.Repeat("a", 40),
		BinaryVersion:          "0.10.0",
		BuildIdentity:          "build-one",
		ContractDigest:         "sha256:" + strings.Repeat("b", 64),
		RegistryIdentityDigest: "sha256:" + strings.Repeat("c", 64),
		AutonomyPolicyDigest:   "sha256:" + strings.Repeat("d", 64),
		ExecutionPolicyDigest:  "sha256:" + strings.Repeat("e", 64),
		ReportPath: filepath.Join(
			t.TempDir(), ".plan-logs", "koryph", "canary", "report.json",
		),
	}
	base, err := CanaryGenerationDigest("demo", spec)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*CanarySpec)
	}{
		{"commit", func(s *CanarySpec) { s.InstalledCommit = strings.Repeat("f", 40) }},
		{"build", func(s *CanarySpec) { s.BuildIdentity = "build-two" }},
		{"contract", func(s *CanarySpec) { s.ContractDigest = "sha256:" + strings.Repeat("f", 64) }},
		{"registry", func(s *CanarySpec) { s.RegistryIdentityDigest = "sha256:" + strings.Repeat("f", 64) }},
		{"cohort", func(s *CanarySpec) { s.Cohort = []string{"b1", "b3"} }},
		{"execution policy", func(s *CanarySpec) { s.ExecutionPolicyDigest = "sha256:" + strings.Repeat("f", 64) }},
		{"report path", func(s *CanarySpec) { s.ReportPath += ".other" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed := spec
			changed.Cohort = append([]string(nil), spec.Cohort...)
			tc.mutate(&changed)
			digest, err := CanaryGenerationDigest("demo", changed)
			if err != nil {
				t.Fatal(err)
			}
			if digest == base {
				t.Fatalf("%s did not change generation", tc.name)
			}
		})
	}
}
