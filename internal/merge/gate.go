// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package merge

import (
	"context"
	"strings"

	"github.com/koryph/koryph/internal/execx"
)

// RunGate runs cmds sequentially in dir via `sh -c` (wrapped with
// `direnv exec <dir>` when direnv is on PATH and the dir's .envrc is
// allowed), accumulating combined output. It stops at the first non-zero
// exit; ok is true only when every command exits 0.
func RunGate(ctx context.Context, dir string, cmds []string) (ok bool, output string) {
	var b strings.Builder
	for _, c := range cmds {
		name, args := shellCmd(dir, c)
		b.WriteString("$ " + c + "\n")
		res, err := execx.Run(ctx, execx.Cmd{Dir: dir, Name: name, Args: args})
		if err == nil && res.ExitCode != 0 && name == "direnv" && strings.Contains(res.Stderr, "is blocked") {
			// Fresh agent worktrees carry a never-approved .envrc; direnv
			// refuses to exec there. Fall back to a plain shell — the gate
			// must not fail on environment ceremony.
			b.WriteString("(direnv blocked; running without direnv)\n")
			res, err = execx.Run(ctx, execx.Cmd{Dir: dir, Name: "sh", Args: []string{"-c", c}})
		}
		b.WriteString(res.Stdout)
		b.WriteString(res.Stderr)
		if err != nil {
			b.WriteString("\nerror: " + err.Error() + "\n")
			return false, b.String()
		}
		if res.ExitCode != 0 {
			return false, b.String()
		}
	}
	return true, b.String()
}

// shellCmd builds a `sh -c` invocation, wrapped with `direnv exec <dir>` when
// direnv is available so project env is loaded before the gate command runs.
func shellCmd(dir, command string) (string, []string) {
	if execx.LookPath("direnv") {
		return "direnv", []string{"exec", dir, "sh", "-c", command}
	}
	return "sh", []string{"-c", command}
}
