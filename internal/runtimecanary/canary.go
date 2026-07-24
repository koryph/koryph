// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package runtimecanary executes the fixed authenticated/headless capability
// probe used by phase control. It accepts no requester-authored prompt,
// command, environment, or expected output.
package runtimecanary

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/agentjson"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/runtime"
)

const (
	Token          = "koryph-runtime-canary-v1"
	defaultTimeout = 2 * time.Minute
)

const promptTemplate = `You are the Koryph authenticated runtime capability canary.
Do not inspect or modify the repository. Use your shell/Bash tool exactly once
to run this harmless local command:

%s

Only after the tool succeeds, return exactly this JSON object and no prose:
{"ok":true,"token":"koryph-runtime-canary-v1"}`

var proofNonceRE = regexp.MustCompile(`^[a-f0-9]{32}$`)

// VerifyFunc performs the target account's host-side identity check. It may
// implement native-profile identity or credential fingerprint verification.
type VerifyFunc func(context.Context) (string, error)

// SpawnFunc is the runtime spawn seam used by tests.
type SpawnFunc func(context.Context, runtime.Runtime, runtime.JSONSpec, runtime.JSONExec) (execx.Result, error)

// Options contains only orchestrator-resolved values.
type Options struct {
	Runtime runtime.Runtime

	RepoRoot   string
	Worktree   string
	ScratchDir string
	Model      string
	Effort     string
	Timeout    time.Duration

	Profile          runtime.Profile
	Billing          runtime.BillingMode
	APIKey           string
	SSHAuthSock      string
	ProxyBaseURL     string
	EnvPassthrough   []string
	Credential       string
	CredentialEnvVar string

	// ProofNonce is an orchestrator-only deterministic test seam. Production
	// leaves it empty and Run generates a fresh unpredictable nonce.
	ProofNonce string

	Verify VerifyFunc
	Spawn  SpawnFunc
}

// Result is deliberately credential-free and safe to copy into phase output.
type Result struct {
	OK       bool
	Runtime  string
	Identity string
	Kind     string
	Detail   string
}

type verdict struct {
	OK    bool   `json:"ok"`
	Token string `json:"token"`
}

// Run verifies the target identity and then executes the fixed headless tool
// contract through the target runtime adapter.
func Run(ctx context.Context, o Options) Result {
	out := Result{Kind: "configuration"}
	if o.Runtime == nil {
		out.Detail = "target runtime is not registered"
		return out
	}
	out.Runtime = o.Runtime.Name()
	if o.Verify == nil {
		out.Detail = "target runtime has no identity verifier"
		return out
	}
	identity, err := o.Verify(ctx)
	if err != nil {
		out.Kind = "identity"
		out.Detail = "target runtime identity verification failed"
		return out
	}
	out.Identity = identity
	if strings.TrimSpace(o.Model) == "" {
		out.Detail = "target runtime has no standard-tier model mapping"
		return out
	}
	if strings.TrimSpace(o.ScratchDir) == "" {
		out.Detail = "target runtime has no canary scratch directory"
		return out
	}
	nonce := o.ProofNonce
	if nonce == "" {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			out.Detail = "could not create canary proof nonce"
			return out
		}
		nonce = fmt.Sprintf("%x", raw[:])
	}
	if !proofNonceRE.MatchString(nonce) {
		out.Detail = "invalid canary proof nonce"
		return out
	}
	proofPath := filepath.Join(o.ScratchDir, "runtime-canary-proof-"+nonce)
	command := "printf '%s\\n' " + shellQuote(nonce) + " > " + shellQuote(proofPath)
	canaryPrompt := fmt.Sprintf(promptTemplate, command)
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	spawn := o.Spawn
	if spawn == nil {
		spawn = runtime.SpawnJSON
	}
	res, err := spawn(ctx, o.Runtime, runtime.JSONSpec{
		RepoRoot:         o.RepoRoot,
		ScratchDir:       o.ScratchDir,
		Persona:          "koryph-implementer",
		Model:            o.Model,
		Effort:           o.Effort,
		PermissionMode:   "dontAsk",
		SpawnKind:        "runtime-canary",
		Profile:          o.Profile,
		Billing:          o.Billing,
		APIKey:           o.APIKey,
		SSHAuthSock:      o.SSHAuthSock,
		ProxyBaseURL:     o.ProxyBaseURL,
		EnvPassthrough:   append([]string(nil), o.EnvPassthrough...),
		Credential:       o.Credential,
		CredentialEnvVar: o.CredentialEnvVar,
	}, runtime.JSONExec{
		Dir:     o.Worktree,
		Stdin:   canaryPrompt,
		Timeout: timeout,
	})
	if err != nil {
		out.Kind = "spawn"
		if errors.Is(err, execx.ErrTimeout) {
			out.Kind = "transient"
			out.Detail = "target runtime canary timed out"
		} else {
			out.Detail = "target runtime canary could not start"
		}
		return out
	}
	if res.ExitCode != 0 {
		if res.TimedOut {
			out.Kind = "transient"
			out.Detail = "target runtime canary timed out"
		} else {
			out.Kind = "execution"
			out.Detail = fmt.Sprintf("target runtime canary exited %d", res.ExitCode)
		}
		return out
	}
	proofInfo, err := os.Lstat(proofPath)
	if err != nil || !proofInfo.Mode().IsRegular() || proofInfo.Size() > 128 {
		out.Kind = "tool"
		out.Detail = "target runtime did not create the fixed headless tool proof"
		return out
	}
	proof, err := os.ReadFile(proofPath)
	if err != nil || strings.TrimSpace(string(proof)) != nonce {
		out.Kind = "tool"
		out.Detail = "target runtime produced an invalid headless tool proof"
		return out
	}
	raw, err := agentjson.ParseEnvelopeVerdict(strings.TrimSpace(res.Stdout), "ok", "token")
	if err != nil {
		out.Kind = "protocol"
		out.Detail = "target runtime canary returned an invalid result"
		return out
	}
	var got verdict
	if err := json.Unmarshal([]byte(raw), &got); err != nil || !got.OK || got.Token != Token {
		out.Kind = "tool"
		out.Detail = "target runtime did not prove the fixed headless tool action"
		return out
	}
	out.OK = true
	out.Kind = "ok"
	out.Detail = "authenticated headless canary passed"
	return out
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
