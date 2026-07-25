// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package plan

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/koryph/koryph/internal/beads"
)

const UnitContractVersion = "koryph.unit/v1"

// UnitContract is the deterministic dispatch-unit contract stored in a bead's
// dedicated Design field. Provides remains a slice so the audit can report a
// useful "multiple outcomes" finding instead of failing to parse the field.
type UnitContract struct {
	Kind           string
	Provides       []string
	Owns           []string
	Consumes       []string
	CohesionReason string
	RoutingReason  string
}

var (
	unitKeyRE  = regexp.MustCompile(`(?:^|\s)(kind|provides|owns|consumes|cohesion_reason|routing_reason)=`)
	unitSlugRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

// ParseUnitContract parses the first koryph.unit/v1 contract in design.
// Values extend until the next known key, which lets rationale fields contain
// ordinary spaces without requiring a shell/YAML quoting convention.
func ParseUnitContract(design string) (UnitContract, bool, error) {
	start := strings.Index(design, UnitContractVersion)
	if start < 0 {
		return UnitContract{}, false, nil
	}
	body := strings.TrimSpace(design[start+len(UnitContractVersion):])
	matches := unitKeyRE.FindAllStringSubmatchIndex(body, -1)
	if len(matches) == 0 {
		return UnitContract{}, true, fmt.Errorf("%s has no fields", UnitContractVersion)
	}

	values := map[string]string{}
	for i, match := range matches {
		key := body[match[2]:match[3]]
		if _, duplicate := values[key]; duplicate {
			return UnitContract{}, true, fmt.Errorf("%s field %q is duplicated", UnitContractVersion, key)
		}
		valueStart := match[1]
		valueEnd := len(body)
		if i+1 < len(matches) {
			valueEnd = matches[i+1][0]
		}
		values[key] = strings.TrimSpace(body[valueStart:valueEnd])
	}

	return UnitContract{
		Kind:           values["kind"],
		Provides:       splitContractList(values["provides"]),
		Owns:           splitContractList(values["owns"]),
		Consumes:       splitContractList(values["consumes"]),
		CohesionReason: values["cohesion_reason"],
		RoutingReason:  values["routing_reason"],
	}, true, nil
}

func splitContractList(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" || value == "-" || value == "none" {
		return nil
	}
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func contractListDuplicates(values []string) []string {
	seen := map[string]bool{}
	dupes := map[string]bool{}
	for _, value := range values {
		if seen[value] {
			dupes[value] = true
		}
		seen[value] = true
	}
	out := make([]string, 0, len(dupes))
	for value := range dupes {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func validCapabilitySlug(value string) bool {
	return unitSlugRE.MatchString(value)
}

var broadOwnedRoots = map[string]bool{
	".": true, "/": true,
	"internal": true, "cmd": true, "docs": true, "agents": true,
	"commands": true, ".claude": true, ".agents": true, ".github": true,
}

func validOwnedPath(value string) bool {
	if value == "" || strings.ContainsAny(value, "*?[") || path.IsAbs(value) {
		return false
	}
	clean := path.Clean(value)
	if clean != value || clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}
	return !broadOwnedRoots[clean]
}

func productionOwnershipGroup(value string) string {
	parts := strings.Split(value, "/")
	switch parts[0] {
	case "internal":
		if len(parts) >= 2 {
			return strings.Join(parts[:2], "/")
		}
	case "cmd":
		if len(parts) >= 3 {
			return strings.Join(parts[:3], "/")
		}
		if len(parts) >= 2 {
			return strings.Join(parts[:2], "/")
		}
	case "commands", "agents":
		return parts[0]
	case ".claude":
		if len(parts) >= 2 {
			return strings.Join(parts[:2], "/")
		}
	default:
		return parts[0]
	}
	return value
}

func isDocumentationOwnership(value string) bool {
	return strings.HasPrefix(value, "docs/") ||
		value == "README.md" || strings.HasSuffix(value, "/README.md")
}

func auditUnitContracts(active, allChildren []beads.Issue, deps map[string][]string) []QualityFinding {
	var findings []QualityFinding
	add := func(severity, code, issueID, message, remediation string) {
		findings = append(findings, QualityFinding{
			Severity: severity, Code: code, IssueID: issueID,
			Message: message, Remediation: remediation,
		})
	}

	contracts := map[string]UnitContract{}
	providers := map[string][]string{}
	var ids []string
	for _, child := range allChildren {
		ids = append(ids, child.ID)
		if !dispatchType(child.IssueType) {
			continue
		}
		contract, present, err := ParseUnitContract(child.Design)
		if present && err == nil {
			for _, capability := range contract.Provides {
				providers[capability] = append(providers[capability], child.ID)
			}
		}
	}
	activeIDs := map[string]bool{}
	for _, child := range active {
		activeIDs[child.ID] = true
		if !dispatchType(child.IssueType) {
			continue
		}
		contract, present, err := ParseUnitContract(child.Design)
		switch {
		case !present:
			add("error", "unit-contract-missing", child.ID,
				"dispatchable child has no koryph.unit/v1 contract in its design field",
				"set --design with kind, one provides slug, exact owns paths, and consumes slugs")
			continue
		case err != nil:
			add("error", "unit-contract-invalid", child.ID, err.Error(),
				"rewrite the design field as one valid koryph.unit/v1 contract")
			continue
		}
		contracts[child.ID] = contract
		auditOneUnitContract(child, contract, add)
	}

	for capability, ownerIDs := range providers {
		if len(ownerIDs) <= 1 {
			continue
		}
		sort.Strings(ownerIDs)
		for _, ownerID := range ownerIDs {
			if !activeIDs[ownerID] {
				continue
			}
			add("error", "unit-provider-duplicate", ownerID,
				fmt.Sprintf("capability %q has multiple providers: %s", capability, strings.Join(ownerIDs, ", ")),
				"assign one observable outcome to exactly one provider bead")
		}
	}

	reach := transitiveClosure(ids, deps)
	for childID, contract := range contracts {
		for _, capability := range contract.Consumes {
			ownerIDs := providers[capability]
			switch len(ownerIDs) {
			case 0:
				add("error", "unit-consumer-provider-missing", childID,
					fmt.Sprintf("consumed capability %q has no provider in this epic", capability),
					"add the provider unit or remove the stale consumes entry")
			case 1:
				providerID := ownerIDs[0]
				if providerID == childID || !reach[childID][providerID] {
					add("error", "unit-consumer-edge-missing", childID,
						fmt.Sprintf("consumer of %q does not depend on provider %s", capability, providerID),
						fmt.Sprintf("add dependency: bd dep add %s --blocked-by %s", childID, providerID))
				}
			}
		}
	}
	return findings
}

func auditOneUnitContract(child beads.Issue, contract UnitContract, add func(string, string, string, string, string)) {
	if !validUnitKind(contract.Kind) {
		add("error", "unit-kind-invalid", child.ID,
			fmt.Sprintf("unit kind %q is not a koryph.unit/v1 kind", contract.Kind),
			"use foundation, implementation, integration, docs, test, or operator")
	}
	if len(contract.Provides) != 1 {
		add("error", "unit-outcome-count", child.ID,
			fmt.Sprintf("unit declares %d provided outcomes; exactly one is required", len(contract.Provides)),
			"split independent outcomes into separate provider beads")
	}
	for _, capability := range append(append([]string{}, contract.Provides...), contract.Consumes...) {
		if !validCapabilitySlug(capability) {
			add("error", "unit-capability-invalid", child.ID,
				fmt.Sprintf("capability %q is not a lowercase hyphenated slug", capability),
				"use lowercase letters, digits, and single hyphens")
		}
	}
	for _, duplicate := range append(contractListDuplicates(contract.Provides), contractListDuplicates(contract.Consumes)...) {
		add("error", "unit-capability-duplicate", child.ID,
			fmt.Sprintf("capability %q is declared more than once", duplicate),
			"remove the duplicate capability")
	}
	if len(contract.Owns) == 0 {
		add("error", "unit-ownership-missing", child.ID,
			"unit declares no owned paths", "list exact repository path prefixes in owns=")
	}
	for _, owned := range contract.Owns {
		if !validOwnedPath(owned) {
			add("error", "unit-ownership-broad", child.ID,
				fmt.Sprintf("owned path %q is broad, absolute, non-canonical, or contains a glob", owned),
				"use exact file or package prefixes; never a repository-wide root or wildcard")
		}
	}
	for _, duplicate := range contractListDuplicates(contract.Owns) {
		add("error", "unit-ownership-duplicate", child.ID,
			fmt.Sprintf("owned path %q is declared more than once", duplicate),
			"remove the duplicate owned path")
	}

	breadthRationale := (contract.Kind == "integration" || contract.Kind == "operator") &&
		strings.TrimSpace(contract.CohesionReason) != ""
	areaSet := map[string]bool{}
	for _, label := range child.Labels {
		if area, ok := strings.CutPrefix(label, "area:"); ok && area != "" {
			areaSet[area] = true
		}
	}
	groupSet := map[string]bool{}
	hasDocs, hasProduction := false, false
	for _, owned := range contract.Owns {
		if isDocumentationOwnership(owned) {
			hasDocs = true
			continue
		}
		hasProduction = true
		groupSet[productionOwnershipGroup(owned)] = true
	}
	if !breadthRationale && len(areaSet) > 2 {
		add("error", "unit-area-breadth", child.ID,
			fmt.Sprintf("ordinary unit spans %d write areas", len(areaSet)),
			"split by outcome/area; only true integration/operator units may use a concrete cohesion_reason exception")
	}
	if !breadthRationale && (len(groupSet) > 4 || len(contract.Owns) > 8) {
		add("error", "unit-package-breadth", child.ID,
			fmt.Sprintf("ordinary unit owns %d subsystem groups across %d path prefixes", len(groupSet), len(contract.Owns)),
			"split along observable provider seams or justify a true integration unit")
	}
	if hasDocs && hasProduction && !breadthRationale {
		add("error", "unit-docs-production-mixed", child.ID,
			"unit mixes documentation and production ownership without an integration rationale",
			"split docs from production; only true integration/operator units may use a concrete cohesion_reason exception")
	}

	equivLabels, modelLabels, effortLabels := []string{}, []string{}, []string{}
	for _, label := range child.Labels {
		switch {
		case strings.HasPrefix(label, "equiv:"):
			equivLabels = append(equivLabels, label)
			parts := strings.Split(label, ":")
			if len(parts) != 3 || !portableTier(parts[1]) || !portableEffort(parts[2]) {
				add("error", "routing-equivalent-invalid", child.ID,
					fmt.Sprintf("portable route %q must be equiv:<frontier|standard|light>:<effort>", label),
					"use effort low, medium, high, xhigh, max, or ultra")
			}
		case strings.HasPrefix(label, "model:"):
			modelLabels = append(modelLabels, label)
			parts := strings.Split(label, ":")
			if len(parts) != 2 || parts[1] == "" {
				add("error", "routing-model-invalid", child.ID,
					fmt.Sprintf("exact route %q must be model:<runtime-native-id>", label),
					"remove stage-scoped/portable model spellings; use equiv:<tier>:<effort> for portability")
			}
		case strings.HasPrefix(label, "effort:"):
			effortLabels = append(effortLabels, label)
		}
	}
	if len(equivLabels) > 0 && len(modelLabels) > 0 {
		add("error", "routing-override-conflict", child.ID,
			"unit combines portable equivalency and exact-model overrides",
			"choose either equiv:<tier>:<effort> or model:<id>")
	}
	if len(equivLabels) > 1 || len(modelLabels) > 1 || len(effortLabels) > 1 {
		add("error", "routing-override-duplicate", child.ID,
			"unit declares more than one override in the same routing family",
			"keep exactly one portable equivalency or exact model override")
	}
	if len(equivLabels)+len(modelLabels)+len(effortLabels) > 0 && strings.TrimSpace(contract.RoutingReason) == "" {
		add("error", "routing-rationale-missing", child.ID,
			"explicit model or effort override has no unit-contract routing_reason",
			"remove the routine override or add routing_reason=<why the default is insufficient>")
	}
}

func portableTier(value string) bool {
	return value == "frontier" || value == "standard" || value == "light"
}

func validUnitKind(value string) bool {
	switch value {
	case "foundation", "implementation", "integration", "docs", "test", "operator":
		return true
	default:
		return false
	}
}

func portableEffort(value string) bool {
	switch value {
	case "low", "medium", "high", "xhigh", "max", "ultra":
		return true
	default:
		return false
	}
}
