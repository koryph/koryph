// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/phasecontrol"
)

func init() {
	registerCmd(command{
		name:    "phase",
		summary: "request orchestrator-owned actions from the current worker phase",
		run:     cmdPhase,
		subs: []command{
			{
				name:    "request",
				summary: "submit a typed phase request",
				subs: []command{
					{name: "label-add", summary: "add an allowlisted scheduling label"},
					{name: "runtime-canary", summary: "run a fixed authenticated runtime canary"},
				},
			},
			{name: "block", summary: "report a structured capability block"},
		},
		DocLinks: []string{"user-guide/running-waves.md"},
	})
}

func cmdPhase(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		fmt.Fprintln(stdout, "koryph phase — request orchestrator-owned actions from the current worker phase")
		fmt.Fprintln(stdout, "\nUSAGE")
		fmt.Fprintln(stdout, "  koryph phase request label-add --label LABEL")
		fmt.Fprintln(stdout, "  koryph phase request runtime-canary --runtime NAME")
		fmt.Fprintln(stdout, "  koryph phase block --capability NAME --detail TEXT")
		return 0
	}
	switch args[0] {
	case "request":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "koryph phase request: an operation is required")
			return engine.ExitUsage
		}
		switch args[1] {
		case "label-add":
			return cmdPhaseLabelAdd(args[2:], stdout, stderr)
		case "runtime-canary":
			return cmdPhaseRuntimeCanary(args[2:], stdout, stderr)
		default:
			fmt.Fprintf(stderr, "koryph phase request: unknown operation %q\n", args[1])
			return engine.ExitUsage
		}
	case "block":
		return cmdPhaseBlock(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "koryph phase: unknown subcommand %q\n", args[0])
		return engine.ExitUsage
	}
}

func cmdPhaseLabelAdd(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("phase request label-add", stderr)
	var label, requestID string
	var timeout time.Duration
	fs.StringVar(&label, "label", "", "allowlisted area:*, fp:*, or res:* label")
	fs.StringVar(&requestID, "request-id", "", "stable idempotency key (normally generated)")
	fs.DurationVar(&timeout, "timeout", 2*time.Minute, "maximum wait for the orchestrator response")
	setUsage(fs, stdout, "add an allowlisted scheduling label to the current bead", "--label LABEL [flags]")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) != 0 || label == "" || timeout <= 0 {
		fmt.Fprintln(stderr, "koryph phase request label-add: --label and a positive --timeout are required")
		return engine.ExitUsage
	}
	if err := phasecontrol.ValidateSchedulingLabel(label); err != nil {
		fmt.Fprintf(stderr, "koryph phase request label-add: %v\n", err)
		return engine.ExitUsage
	}
	req, phaseDir, err := phaseRequest(phasecontrol.OperationLabelAdd, requestID)
	if err != nil {
		fmt.Fprintf(stderr, "koryph phase request label-add: %v\n", err)
		return engine.ExitFatal
	}
	req.Label = label
	return submitPhaseRequest(req, phaseDir, timeout, stdout, stderr)
}

// cmdPhaseRuntimeCanary is completed by the runtime-canary implementation
// unit. Keeping the stable command route in the protocol foundation makes old
// binaries fail explicitly rather than silently accepting an unknown request.
func cmdPhaseRuntimeCanary(_ []string, _ io.Writer, stderr io.Writer) int {
	fmt.Fprintln(stderr, "koryph phase request runtime-canary: this binary does not provide the runtime-canary handler")
	return engine.ExitFatal
}

func cmdPhaseBlock(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("phase block", stderr)
	var capability, detail string
	fs.StringVar(&capability, "capability", "", "lowercase capability token")
	fs.StringVar(&detail, "detail", "", "sanitized explanation for the orchestrator")
	setUsage(fs, stdout, "report a structured capability block", "--capability NAME --detail TEXT")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	if len(pos) != 0 || capability == "" || strings.TrimSpace(detail) == "" {
		fmt.Fprintln(stderr, "koryph phase block: --capability and --detail are required")
		return engine.ExitUsage
	}
	if err := phasecontrol.ValidateCapability(capability); err != nil {
		fmt.Fprintf(stderr, "koryph phase block: %v\n", err)
		return engine.ExitUsage
	}
	phaseDir, _, err := currentPhase()
	if err != nil {
		fmt.Fprintf(stderr, "koryph phase block: %v\n", err)
		return engine.ExitFatal
	}
	statusPath := strings.TrimSpace(os.Getenv("KORYPH_STATUS_PATH"))
	if statusPath == "" {
		statusPath = filepath.Join(phaseDir, "status.json")
	}
	if err := phasecontrol.WriteCapabilityBlock(statusPath, capability, detail); err != nil {
		fmt.Fprintf(stderr, "koryph phase block: %v\n", err)
		return engine.ExitFatal
	}
	fmt.Fprintf(stdout, "reported capability block %s\n", capability)
	return 0
}

func phaseRequest(operation, requestID string) (phasecontrol.Request, string, error) {
	phaseDir, phaseID, err := currentPhase()
	if err != nil {
		return phasecontrol.Request{}, "", err
	}
	req, err := phasecontrol.NewRequest(phaseID, operation)
	if err != nil {
		return phasecontrol.Request{}, "", err
	}
	if requestID != "" {
		req.ID = requestID
	}
	return req, phaseDir, nil
}

func currentPhase() (string, string, error) {
	phaseDir := strings.TrimSpace(os.Getenv("KORYPH_PHASE_DIR"))
	if phaseDir == "" {
		phaseDir = strings.TrimSpace(os.Getenv("KORYPH_DIR"))
	}
	phaseID := strings.TrimSpace(os.Getenv("KORYPH_PHASE_ID"))
	if phaseDir == "" || phaseID == "" {
		return "", "", errors.New("not running inside a koryph worker phase")
	}
	return phaseDir, phaseID, nil
}

func submitPhaseRequest(req phasecontrol.Request, phaseDir string, timeout time.Duration, stdout, stderr io.Writer) int {
	if err := phasecontrol.Submit(phaseDir, req); err != nil {
		fmt.Fprintf(stderr, "koryph phase request: %v\n", err)
		return engine.ExitFatal
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	resp, err := phasecontrol.WaitResponse(ctx, phaseDir, req)
	if err != nil {
		fmt.Fprintf(stderr, "koryph phase request: waiting for orchestrator: %v\n", err)
		return engine.ExitFatal
	}
	if resp.State != phasecontrol.ResponseApplied {
		fmt.Fprintf(stderr, "koryph phase request: %s: %s\n", resp.State, resp.Detail)
		return engine.ExitFatal
	}
	fmt.Fprintln(stdout, resp.Detail)
	return 0
}
