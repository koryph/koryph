// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/koryph/koryph/internal/execx"
)

// Verify checks that every commit on base..branch carries a GOOD signature
// (`git log --format='%H %G?'`, run in the worktree; every commit must be
// 'G'). It returns the offending commits as "sha (reason)" strings.
//
// Trust anchor. Verification is pinned to the allowed-signers set committed on
// the DEFAULT branch (`git show <base>:.allowed_signers`), passed explicitly
// with `git -c gpg.ssh.allowedSignersFile=<trusted-copy>`. A dispatched agent
// controls its own branch and worktree, so honoring the in-worktree
// .allowed_signers (or an agent-set gpg.ssh.allowedSignersFile) would let it
// append a self-generated key and sign past the merge gate. Reading the file
// from committed history on base — which the agent cannot rewrite, and which is
// a protected path it cannot merge changes into — makes the trust set
// unforgeable. When base carries no committed .allowed_signers (signing not yet
// committed), it falls back to the repo-configured file; the protected-path and
// git-config-vector guards remain the backstop in that window.
//
// For SSH signatures %G? only reports 'G' when gpg.ssh.allowedSignersFile is
// configured (ConfigureRepo sets it; worktrees share the main repo's
// .git/config). An empty range verifies trivially.
func Verify(ctx context.Context, worktree, base, branch string) (bad []string, err error) {
	args := []string{"log", "--format=%H %G?", base + ".." + branch}
	trusted, ok, terr := trustedSigners(ctx, worktree, base)
	if terr != nil {
		return nil, terr
	}
	if ok {
		defer os.Remove(trusted)
		args = append([]string{"-c", "gpg.ssh.allowedSignersFile=" + trusted}, args...)
	}

	res, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: worktree, Name: "git", Args: args,
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

// trustedSigners extracts <base>:.allowed_signers (the allowed-signers set
// committed on the default branch) into a temp file and returns its path. ok is
// false with no error when the default branch carries no committed
// allowed-signers file — callers then fall back to the repo-configured file.
// The caller is responsible for removing the returned temp file.
func trustedSigners(ctx context.Context, worktree, base string) (path string, ok bool, err error) {
	res, rerr := execx.Run(ctx, execx.Cmd{
		Dir: worktree, Name: "git",
		Args: []string{"show", base + ":" + AllowedSignersFileName},
	})
	if rerr != nil {
		return "", false, fmt.Errorf("signing: read %s:%s: %w", base, AllowedSignersFileName, rerr)
	}
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) == "" {
		return "", false, nil // no committed trust anchor on base; fall back
	}
	f, ferr := os.CreateTemp("", "koryph-allowed-signers-*")
	if ferr != nil {
		return "", false, fmt.Errorf("signing: temp signers file: %w", ferr)
	}
	if _, werr := f.WriteString(res.Stdout); werr != nil {
		f.Close()
		os.Remove(f.Name())
		return "", false, fmt.Errorf("signing: write temp signers file: %w", werr)
	}
	if cerr := f.Close(); cerr != nil {
		os.Remove(f.Name())
		return "", false, fmt.Errorf("signing: close temp signers file: %w", cerr)
	}
	return f.Name(), true, nil
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
