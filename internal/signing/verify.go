// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing

import (
	"context"
	"fmt"
	"strings"

	"github.com/koryph/koryph/internal/execx"
)

// Verify checks that every commit on base..branch carries a GOOD signature
// (`git log --format='%H %G?'`, run in the worktree; every commit must be
// 'G'). It returns the offending commits as "sha (reason)" strings.
//
// For SSH signatures %G? only reports 'G' when gpg.ssh.allowedSignersFile is
// configured (ConfigureRepo sets it; worktrees share the main repo's
// .git/config). An empty range verifies trivially.
func Verify(ctx context.Context, worktree, base, branch string) (bad []string, err error) {
	res, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: worktree, Name: "git",
		Args: []string{"log", "--format=%H %G?", base + ".." + branch},
	})
	if err != nil {
		return nil, fmt.Errorf("signing: verify %s..%s: %w", base, branch, err)
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		sha := f[0]
		status := ""
		if len(f) > 1 {
			status = f[1]
		}
		if status != "G" {
			bad = append(bad, fmt.Sprintf("%s (%s)", sha, sigStatusWord(status)))
		}
	}
	return bad, nil
}

// sigStatusWord translates a %G? code into a human-readable reason.
func sigStatusWord(code string) string {
	switch code {
	case "N", "":
		return "no signature"
	case "B":
		return "BAD signature"
	case "U":
		return "good signature, unknown validity"
	case "X":
		return "good signature, expired"
	case "Y":
		return "good signature, expired key"
	case "R":
		return "good signature, revoked key"
	case "E":
		return "signature cannot be checked (missing key / no allowed_signers?)"
	default:
		return "unverified (%G?=" + code + ")"
	}
}
