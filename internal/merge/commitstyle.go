// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package merge

import (
	"context"
	"regexp"
	"strings"

	"github.com/koryph/koryph/internal/execx"
)

// conventionalTypes are the commit types promptc promises to agents
// (internal/promptc/compile.go): type(scope): subject.
var conventionalTypes = map[string]bool{
	"feat": true, "fix": true, "docs": true, "chore": true, "refactor": true,
	"test": true, "ci": true, "build": true, "perf": true, "style": true,
}

// conventionalSubject matches the STRUCTURAL Conventional Commits grammar:
// a lowercase type, an optional (scope), an optional ! breaking-change marker,
// then ": " and a non-empty subject. The type word is validated separately
// against conventionalTypes. Stylistic rules the prompt also asks for
// (imperative mood, <=72 chars) are advisory and not enforced here — they
// would produce false rejections of legitimate subjects.
var conventionalSubject = regexp.MustCompile(`^[a-z]+(\([^)]+\))?!?: .+`)

// nonConventionalSubjects returns the subjects that fail the Conventional
// Commits grammar. An empty result means every subject conforms. Blank lines
// are ignored.
func nonConventionalSubjects(subjects []string) []string {
	var bad []string
	for _, s := range subjects {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !conventionalSubject.MatchString(s) || !conventionalTypes[commitType(s)] {
			bad = append(bad, s)
		}
	}
	return bad
}

// commitType returns the leading type word of a subject (the run of lowercase
// letters before any scope, !, or colon), or "" if the subject does not start
// with one.
func commitType(subject string) string {
	i := 0
	for i < len(subject) && subject[i] >= 'a' && subject[i] <= 'z' {
		i++
	}
	return subject[:i]
}

// logSubjects returns the commit subjects (first lines) unique to branch
// relative to base — i.e. every candidate commit that a merge or PR would
// carry, not just HEAD.
func logSubjects(ctx context.Context, dir, base, branch string) ([]string, error) {
	res, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: dir, Name: "git", Args: []string{"log", "--format=%s", base + ".." + branch},
	})
	if err != nil {
		return nil, err
	}
	var subs []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			subs = append(subs, line)
		}
	}
	return subs, nil
}
