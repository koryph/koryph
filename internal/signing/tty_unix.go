// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

//go:build !windows

package signing

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

// readLineNoEcho reads a line from tty with terminal echo disabled.
// Returns the line without trailing newline or carriage return.
func readLineNoEcho(tty *os.File) (string, error) {
	buf, err := term.ReadPassword(int(tty.Fd()))
	if err != nil {
		// Fallback: read without disabling echo (e.g. dumb terminal in tests).
		var line string
		_, err2 := fmt.Fscan(tty, &line)
		return line, err2
	}
	return string(buf), nil
}
