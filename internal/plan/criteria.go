// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package plan

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Criterion is one atomic, evidence-addressable acceptance outcome. The
// ordered slice position and explicit ID are both durable snapshot data.
type Criterion struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// CriteriaError is a stable machine-readable acceptance grammar failure.
type CriteriaError struct {
	Code   string
	Line   int
	Detail string
}

func (e *CriteriaError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("acceptance line %d: %s", e.Line, e.Detail)
	}
	return "acceptance: " + e.Detail
}

const (
	CriteriaMissing     = "missing"
	CriteriaIDMissing   = "id-missing"
	CriteriaIDDuplicate = "id-duplicate"
	CriteriaIDSequence  = "id-sequence"
	CriteriaTextMissing = "text-missing"
	CriteriaCompound    = "compound"
)

var (
	criterionIDRE          = regexp.MustCompile(`(?i)^AC([1-9][0-9]*):[ \t]*(.*)$`)
	criterionMalformedIDRE = regexp.MustCompile(`(?i)^AC(?:[0-9]+)?(?:[ \t]|:)`)
)

type parsedCriterionLine struct {
	line     int
	text     string
	id       string
	explicit bool
}

// ParseCriteria parses the Beads acceptance field into an ordered array.
//
// New plans use explicit canonical IDs (AC1:, AC2:, ...). For compatibility
// with already-filed valid epics, a field with no explicit IDs receives
// deterministic positional IDs. Mixing the two forms, skipping/duplicating
// IDs, empty outcomes, and semicolon-packed clauses fail closed.
func ParseCriteria(raw string) ([]Criterion, error) {
	return parseCriteria(raw, false)
}

// ParseStrictCriteria parses criteria at the post-filing quality gate.
// Unlike ParseCriteria's controlled legacy-adoption mode, every non-empty
// line must carry its explicit canonical sequential AC<n>: ID.
func ParseStrictCriteria(raw string) ([]Criterion, error) {
	return parseCriteria(raw, true)
}

func parseCriteria(raw string, requireExplicit bool) ([]Criterion, error) {
	raw = strings.ReplaceAll(raw, `\n`, "\n")
	var lines []parsedCriterionLine
	anyExplicit := false
	for i, original := range strings.Split(raw, "\n") {
		line := stripCriterionBullet(original)
		if line == "" {
			continue
		}
		parsed := parsedCriterionLine{line: i + 1, text: line}
		if match := criterionIDRE.FindStringSubmatch(line); match != nil {
			n, _ := strconv.Atoi(match[1])
			parsed.id = fmt.Sprintf("AC%d", n)
			parsed.text = strings.TrimSpace(match[2])
			parsed.explicit = true
			anyExplicit = true
		} else if criterionMalformedIDRE.MatchString(line) {
			return nil, &CriteriaError{
				Code: CriteriaIDMissing, Line: i + 1,
				Detail: "criterion has a malformed or missing canonical ID (want AC<n>: outcome)",
			}
		}
		if parsed.text == "" {
			return nil, &CriteriaError{Code: CriteriaTextMissing, Line: i + 1, Detail: "criterion has no observable outcome"}
		}
		if compoundCriterion(parsed.text) {
			return nil, &CriteriaError{
				Code: CriteriaCompound, Line: i + 1,
				Detail: "criterion packs multiple clauses with a semicolon; write one criterion per line",
			}
		}
		lines = append(lines, parsed)
	}
	if len(lines) == 0 {
		return nil, &CriteriaError{Code: CriteriaMissing, Detail: "no acceptance criteria"}
	}

	criteria := make([]Criterion, 0, len(lines))
	seen := map[string]bool{}
	for i, line := range lines {
		wantID := fmt.Sprintf("AC%d", i+1)
		if requireExplicit && !line.explicit {
			return nil, &CriteriaError{
				Code: CriteriaIDMissing, Line: line.line,
				Detail: fmt.Sprintf("criterion is missing canonical ID %s", wantID),
			}
		}
		if anyExplicit {
			if !line.explicit {
				return nil, &CriteriaError{
					Code: CriteriaIDMissing, Line: line.line,
					Detail: "criterion is missing an ID while neighboring criteria use explicit IDs",
				}
			}
			if seen[line.id] {
				return nil, &CriteriaError{
					Code: CriteriaIDDuplicate, Line: line.line,
					Detail: fmt.Sprintf("criterion ID %s is duplicated", line.id),
				}
			}
			if line.id != wantID {
				return nil, &CriteriaError{
					Code: CriteriaIDSequence, Line: line.line,
					Detail: fmt.Sprintf("criterion ID %s is out of sequence; want %s", line.id, wantID),
				}
			}
		} else {
			line.id = wantID
		}
		seen[line.id] = true
		criteria = append(criteria, Criterion{ID: line.id, Text: line.text})
	}
	return criteria, nil
}

// FormatCriteria renders the canonical filing form: one stable ID and one
// atomic criterion per line.
func FormatCriteria(criteria []Criterion) string {
	lines := make([]string, 0, len(criteria))
	for _, criterion := range criteria {
		lines = append(lines, criterion.ID+": "+criterion.Text)
	}
	return strings.Join(lines, "\n")
}

func stripCriterionBullet(line string) string {
	line = strings.TrimSpace(line)
	for _, prefix := range []string{"- ", "* ", "• "} {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return line
}

func compoundCriterion(text string) bool {
	parts := strings.Split(text, ";")
	if len(parts) < 2 {
		return false
	}
	nonempty := 0
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			nonempty++
		}
	}
	return nonempty > 1
}
