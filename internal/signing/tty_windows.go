// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

//go:build windows

package signing

import (
	"fmt"
	"os"
	"strings"
)

// readLineNoEcho reads a line from tty. On Windows echo-suppression requires
// the Windows Console API which is not currently implemented; this fallback
// reads with echo visible (acceptable since Windows support is best-effort and
// the operator controls the terminal).
func readLineNoEcho(tty *os.File) (string, error) {
	var line string
	_, err := fmt.Fscanln(tty, &line)
	return strings.TrimRight(line, "\r\n"), err
}
