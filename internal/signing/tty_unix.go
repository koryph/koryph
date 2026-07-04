// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

//go:build !windows

package signing

import (
	"bufio"
	"os"
	"strings"

	"golang.org/x/term"
)

// readLineNoEcho reads a line from tty with terminal echo disabled.
// Returns the line without trailing newline or carriage return.
func readLineNoEcho(tty *os.File) (string, error) {
	buf, err := term.ReadPassword(int(tty.Fd()))
	if err != nil {
		// Fallback: read with echo visible (e.g. dumb terminal in tests).
		// Use bufio.ReadString so that passphrases containing spaces are
		// preserved in full — fmt.Fscan stops at the first whitespace and
		// silently truncates the passphrase.
		reader := bufio.NewReader(tty)
		line, err2 := reader.ReadString('\n')
		return strings.TrimRight(line, "\r\n"), err2
	}
	return string(buf), nil
}
