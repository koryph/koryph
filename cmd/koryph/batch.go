// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/anthro"
	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/registry"
)

func init() {
	registerCmd(command{
		name:    "batch",
		summary: "submit a Message Batch (explicit per-token spend)",
		run:     cmdBatch,
		DocLinks: []string{
			"user-guide/billing-and-quota.md",
		},
		subs: []command{
			{
				name:     "run",
				summary:  "submit a batch from a JSONL file",
				run:      cmdBatchRun,
				DocLinks: []string{"user-guide/billing-and-quota.md"},
			},
		},
	})
}

// batchLine is one input record from the JSONL file.
type batchLine struct {
	ID     string `json:"id"`
	System string `json:"system"`
	User   string `json:"user"`
}

// cmdBatch dispatches the batch verbs (currently only `run`).
func cmdBatch(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "batch", "submit Anthropic Message Batches (explicit per-token spend)", []subVerb{
			{"run --key-env VAR --model TIER --input FILE.jsonl [flags]", "submit a Message Batch and collect results"},
		})
		return 0
	}
	if args[0] != "run" {
		return usageErr(stderr, fmt.Sprintf("unknown batch subcommand %q (want run)", args[0]))
	}
	return cmdBatchRun(args[1:], stdout, stderr)
}

func cmdBatchRun(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("batch run", stderr)
	keyEnv := fs.String("key-env", "", "env var NAME holding the API key (required; never ANTHROPIC_API_KEY)")
	model := fs.String("model", "", "model tier: haiku|sonnet|opus|fable (required)")
	input := fs.String("input", "", "JSONL input file with {id,system,user} lines (required)")
	maxTokens := fs.Int("max-tokens", 0, "max output tokens per request (default 4096)")
	cachePrefix := fs.Bool("cache-prefix", false, "apply a 1h cache breakpoint to the shared system prefix (default from --project's prompt_cache_policy)")
	project := fs.String("project", "", "registered project ID whose prompt_cache_policy defaults --cache-prefix")
	out := fs.String("out", "", "results JSONL destination (default stdout)")
	yes := fs.Bool("yes", false, "confirm the estimated spend and submit")
	setUsage(fs, stdout, "submit a Message Batch (explicit per-token spend)",
		"--key-env VAR --model TIER --input FILE.jsonl [flags]")
	if _, err := parseFlags(fs, args); err != nil {
		return flagExit(err)
	}
	if *keyEnv == "" || *model == "" || *input == "" {
		return usageErr(stderr, "batch run: --key-env, --model and --input are required")
	}

	cache, err := resolveBatchCachePrefix(*cachePrefix, flagPassed(fs, "cache-prefix"), *project)
	if err != nil {
		return fail(stderr, err)
	}

	reqs, err := readBatchInput(*input, *model, *maxTokens, cache)
	if err != nil {
		return fail(stderr, err)
	}
	if len(reqs) == 0 {
		return fail(stderr, fmt.Errorf("batch run: %s has no request lines", *input))
	}

	est := anthro.EstimateUSD(reqs)
	fmt.Fprintf(stdout, "batch: %d request(s), estimated spend $%.2f (model %s)\n", len(reqs), est, *model)
	if !*yes {
		fmt.Fprintln(stderr, "koryph: refusing to spend — re-run with --yes to confirm spend")
		return engine.ExitFatal
	}

	ctx := context.Background()
	client, err := anthro.NewClient(*keyEnv)
	if err != nil {
		return fail(stderr, err)
	}
	batchID, err := client.BatchSubmit(ctx, reqs, anthro.Confirm{
		Confirmed:   true,
		EstimateUSD: est,
		Reason:      "operator passed --yes after estimate",
	})
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "submitted batch %s; waiting for results...\n", batchID)

	results, err := client.BatchWait(ctx, batchID, 30*time.Second)
	if err != nil {
		return fail(stderr, err)
	}

	sink := stdout
	if *out != "" {
		f, ferr := os.Create(*out)
		if ferr != nil {
			return fail(stderr, ferr)
		}
		defer f.Close()
		sink = f
	}
	for _, r := range results {
		data, merr := json.Marshal(r)
		if merr != nil {
			return fail(stderr, merr)
		}
		fmt.Fprintln(sink, string(data))
	}
	fmt.Fprintf(stdout, "wrote %d result(s)\n", len(results))
	return 0
}

// resolveBatchCachePrefix decides the shared-prefix cache breakpoint for a
// batch run (koryph-6au). An explicit --cache-prefix (explicitPassed) always
// wins so an operator can force the breakpoint on or off per batch. Otherwise,
// when --project names a registered project, that project's
// prompt_cache_policy decides via PromptCacheEnabled (default on) — the live
// consumer that makes the re-introduced registry field earn its keep. With no
// project and no explicit flag, the plain flag default (off) stands.
func resolveBatchCachePrefix(explicitCache, explicitPassed bool, project string) (bool, error) {
	if project == "" || explicitPassed {
		return explicitCache, nil
	}
	rec, err := registry.NewStore().Get(project)
	if err != nil {
		return false, err
	}
	return rec.PromptCacheEnabled(), nil
}

// readBatchInput parses the JSONL input into anthro requests.
func readBatchInput(path, model string, maxTokens int, cachePrefix bool) ([]anthro.MsgReq, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var reqs []anthro.MsgReq
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var bl batchLine
		if err := json.Unmarshal([]byte(raw), &bl); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, lineNo, err)
		}
		if bl.ID == "" {
			return nil, fmt.Errorf("%s:%d: missing \"id\"", path, lineNo)
		}
		reqs = append(reqs, anthro.MsgReq{
			ID:          bl.ID,
			Model:       model,
			System:      bl.System,
			User:        bl.User,
			MaxTokens:   maxTokens,
			CachePrefix: cachePrefix,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return reqs, nil
}
