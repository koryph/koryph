// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/execx"
)

// CosignKeyEnv is the environment variable that carries the signing key to
// cosign (`--key env://KORYPH_COSIGN_KEY`). The key exists only in the
// child process env — never on disk, never in logs.
const CosignKeyEnv = "KORYPH_COSIGN_KEY"

// envCosignBin overrides the cosign binary (tests use a fake).
const envCosignBin = "KORYPH_COSIGN_BIN"

// cosignTimeout bounds one sign-blob invocation.
const cosignTimeout = 120 * time.Second

// SignBlob signs the file at path with cosign v3
// (`cosign sign-blob --yes --key env://KORYPH_COSIGN_KEY`), fetching the
// private key from the configured vault provider and passing it via the
// child environment only. The signature lands at <path>.sig.
//
// Encrypted cosign keys read their passphrase from COSIGN_PASSWORD, which is
// inherited from the operator's environment when set.
func SignBlob(ctx context.Context, v *VaultConfig, cfg *Config, path string) (sigPath string, err error) {
	if cfg == nil || !cfg.Artifacts {
		return "", fmt.Errorf("signing: artifact signing is not enabled for this project (signing.artifacts)")
	}
	if !fileExists(path) {
		return "", fmt.Errorf("signing: blob %s does not exist", path)
	}
	key, err := v.Fetch(ctx, cfg.Provider, cfg.KeyRef)
	if err != nil {
		return "", err
	}

	bin := os.Getenv(envCosignBin)
	if bin == "" {
		bin = "cosign"
	}
	sigPath = path + ".sig"
	env := append(execx.BaseEnv(CosignKeyEnv), CosignKeyEnv+"="+strings.TrimRight(string(key), "\n"))
	res, err := execx.Run(ctx, execx.Cmd{
		Name: bin,
		Args: []string{"sign-blob", "--yes", "--key", "env://" + CosignKeyEnv, "--output-signature", sigPath, path},
		Env:  env, Timeout: cosignTimeout,
	})
	if err != nil {
		return "", fmt.Errorf("signing: cosign sign-blob: %w", err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("signing: cosign sign-blob exited %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return sigPath, nil
}

// fileExists reports whether path exists (local helper; avoids an fsx import
// for one probe).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
