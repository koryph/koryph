// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package anthro

import (
	"context"
	"go/parser"
	"go/token"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/shared"
)

// --- fakes -----------------------------------------------------------------

type fakeMessages struct {
	calls      int
	lastParams anthropic.MessageNewParams
	resp       *anthropic.Message
	err        error
}

func (f *fakeMessages) create(_ context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	f.calls++
	f.lastParams = params
	return f.resp, f.err
}

type fakeBatches struct {
	submitCalls  int
	submitParams anthropic.MessageBatchNewParams
	submitID     string
	statuses     []string
	statusIdx    int
	resultsOut   []anthropic.MessageBatchIndividualResponse
}

func (f *fakeBatches) submit(_ context.Context, params anthropic.MessageBatchNewParams) (string, error) {
	f.submitCalls++
	f.submitParams = params
	return f.submitID, nil
}

func (f *fakeBatches) status(_ context.Context, _ string) (string, error) {
	s := f.statuses[f.statusIdx]
	if f.statusIdx < len(f.statuses)-1 {
		f.statusIdx++
	}
	return s, nil
}

func (f *fakeBatches) results(_ context.Context, _ string) ([]anthropic.MessageBatchIndividualResponse, error) {
	return f.resultsOut, nil
}

func approx(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// --- construction guardrails -------------------------------------------------

func TestNewClientGuards(t *testing.T) {
	if _, err := NewClient(""); err == nil {
		t.Error("NewClient(\"\") succeeded; explicit key env var is required")
	}

	if _, err := NewClient("ANTHROPIC_API_KEY"); err == nil {
		t.Error("NewClient(ANTHROPIC_API_KEY) succeeded; ambient key must be refused")
	}
	// Even when the ambient var IS set, it must be refused by name.
	t.Setenv("ANTHROPIC_API_KEY", "sk-ambient")
	if _, err := NewClient("ANTHROPIC_API_KEY"); err == nil {
		t.Error("NewClient(ANTHROPIC_API_KEY) succeeded with value set; must still refuse")
	}

	t.Setenv("KORYPH_BATCH_API_KEY", "")
	if _, err := NewClient("KORYPH_BATCH_API_KEY"); err == nil {
		t.Error("NewClient with empty-valued var succeeded; must fail")
	} else if !strings.Contains(err.Error(), "KORYPH_BATCH_API_KEY") {
		t.Errorf("error %v does not name the env var", err)
	}

	t.Setenv("KORYPH_BATCH_API_KEY", "sk-test-batch")
	c, err := NewClient("KORYPH_BATCH_API_KEY")
	if err != nil || c == nil {
		t.Fatalf("NewClient with named, set var: %v", err)
	}
}

// --- Message (no network: fake backend) --------------------------------------

func TestMessageUnknownTier(t *testing.T) {
	fake := &fakeMessages{}
	c := &Client{messages: fake}
	_, _, err := c.Message(context.Background(), MsgReq{ID: "r1", Model: "gpt-4", User: "hi"})
	if err == nil || !strings.Contains(err.Error(), "unknown model tier") {
		t.Fatalf("err = %v, want unknown-tier error", err)
	}
	if fake.calls != 0 {
		t.Error("backend called despite tier mapping failure")
	}
}

func TestMessageBuildsParamsAndMapsUsage(t *testing.T) {
	fake := &fakeMessages{
		resp: &anthropic.Message{
			Content: []anthropic.ContentBlockUnion{
				{Type: "text", Text: "hello "},
				{Type: "thinking", Thinking: "..."},
				{Type: "text", Text: "world"},
			},
			Usage: anthropic.Usage{
				InputTokens:              100_000,
				OutputTokens:             10_000,
				CacheReadInputTokens:     50_000,
				CacheCreationInputTokens: 20_000,
			},
		},
	}
	c := &Client{messages: fake}
	text, usage, err := c.Message(context.Background(), MsgReq{
		ID:          "r1",
		Model:       "sonnet",
		System:      "stable prefix",
		User:        "volatile question",
		CachePrefix: true,
	})
	if err != nil {
		t.Fatalf("Message: %v", err)
	}
	if text != "hello world" {
		t.Errorf("text = %q", text)
	}

	p := fake.lastParams
	if got := string(p.Model); got != "claude-sonnet-5" {
		t.Errorf("Model = %q", got)
	}
	if p.MaxTokens != 4096 {
		t.Errorf("MaxTokens = %d, want default 4096", p.MaxTokens)
	}
	if len(p.System) != 1 || p.System[0].Text != "stable prefix" {
		t.Fatalf("System = %+v", p.System)
	}
	if p.System[0].CacheControl.TTL != anthropic.CacheControlEphemeralTTLTTL1h {
		t.Errorf("System cache_control TTL = %q, want 1h ephemeral", p.System[0].CacheControl.TTL)
	}
	if len(p.Messages) != 1 {
		t.Fatalf("Messages = %+v", p.Messages)
	}

	if usage.InputTokens != 100_000 || usage.OutputTokens != 10_000 || usage.CacheRead != 50_000 || usage.CacheWrite != 20_000 {
		t.Errorf("usage = %+v", usage)
	}
	// sonnet: in 0.3 + out 0.15 + read 0.015 + write(2x) 0.12 = 0.585
	approx(t, usage.EstimateUSD, 0.585, "usage.EstimateUSD")
}

func TestMessageNoCachePrefixNoSystem(t *testing.T) {
	fake := &fakeMessages{resp: &anthropic.Message{}}
	c := &Client{messages: fake}

	if _, _, err := c.Message(context.Background(), MsgReq{ID: "r", Model: "haiku", User: "q", MaxTokens: 128}); err != nil {
		t.Fatal(err)
	}
	if len(fake.lastParams.System) != 0 {
		t.Errorf("empty System produced a system block: %+v", fake.lastParams.System)
	}
	if fake.lastParams.MaxTokens != 128 {
		t.Errorf("MaxTokens = %d", fake.lastParams.MaxTokens)
	}

	if _, _, err := c.Message(context.Background(), MsgReq{ID: "r", Model: "haiku", System: "sys", User: "q"}); err != nil {
		t.Fatal(err)
	}
	if ttl := fake.lastParams.System[0].CacheControl.TTL; ttl != "" {
		t.Errorf("CachePrefix=false set cache_control TTL %q", ttl)
	}
}

// --- EstimateUSD --------------------------------------------------------------

func TestEstimateUSD(t *testing.T) {
	// Canonical list prices (internal/pricing, koryph-fiv finding #5): opus
	// $15/$75, haiku $0.8/$4 per MTok. Before consolidation anthro used a
	// drifted local copy ($5/$25 opus, $1/$5 haiku); these expectations track
	// the corrected canonical rates.

	// opus: (2000+2000)/4 = 1000 input tokens @ $15/M + 2000/2 = 1000 output @ $75/M
	req := MsgReq{Model: "opus", System: strings.Repeat("s", 2000), User: strings.Repeat("u", 2000), MaxTokens: 2000}
	approx(t, EstimateUSD([]MsgReq{req}), 0.015+0.075, "EstimateUSD(opus)")

	// haiku with default MaxTokens: (200+200)/4=100 in @ $0.8/M + 4096/2=2048 out @ $4/M
	req2 := MsgReq{Model: "haiku", System: strings.Repeat("s", 200), User: strings.Repeat("u", 200)}
	approx(t, EstimateUSD([]MsgReq{req2}), 100.0*0.8/1e6+2048*4/1e6, "EstimateUSD(haiku default max)")

	// Unknown tier contributes zero; list sums.
	unknown := MsgReq{Model: "gpt-4", User: "x", MaxTokens: 100}
	approx(t, EstimateUSD([]MsgReq{req, req2, unknown}), 0.09+100.0*0.8/1e6+2048*4/1e6, "EstimateUSD(sum)")

	approx(t, EstimateUSD(nil), 0, "EstimateUSD(nil)")
}

// --- Batches -------------------------------------------------------------------

func TestBatchSubmitUnconfirmed(t *testing.T) {
	fake := &fakeBatches{submitID: "batch_x"}
	c := &Client{batches: fake}
	_, err := c.BatchSubmit(context.Background(), []MsgReq{{ID: "a", Model: "haiku", User: "q"}}, Confirm{})
	if err == nil || !strings.Contains(err.Error(), "batch spend not confirmed") {
		t.Fatalf("err = %v, want spend-not-confirmed refusal", err)
	}
	if fake.submitCalls != 0 {
		t.Error("submit reached the backend despite missing confirmation")
	}
}

func TestBatchSubmitConfirmed(t *testing.T) {
	fake := &fakeBatches{submitID: "batch_123"}
	c := &Client{batches: fake}
	reqs := []MsgReq{
		{ID: "bead-1", Model: "haiku", System: "sys", User: "q1", CachePrefix: true},
		{ID: "bead-2", Model: "opus", User: "q2", MaxTokens: 64},
	}
	id, err := c.BatchSubmit(context.Background(), reqs, Confirm{Confirmed: true, EstimateUSD: 0.5, Reason: "user passed --yes"})
	if err != nil {
		t.Fatalf("BatchSubmit: %v", err)
	}
	if id != "batch_123" {
		t.Errorf("batch id = %q", id)
	}
	got := fake.submitParams.Requests
	if len(got) != 2 {
		t.Fatalf("requests = %+v", got)
	}
	if got[0].CustomID != "bead-1" || got[1].CustomID != "bead-2" {
		t.Errorf("custom ids = %q, %q", got[0].CustomID, got[1].CustomID)
	}
	if string(got[0].Params.Model) != "claude-haiku-4-5-20251001" {
		t.Errorf("req0 model = %q", got[0].Params.Model)
	}
	if got[0].Params.System[0].CacheControl.TTL != anthropic.CacheControlEphemeralTTLTTL1h {
		t.Error("req0 lost its cache_control breakpoint")
	}
	if got[1].Params.MaxTokens != 64 {
		t.Errorf("req1 MaxTokens = %d", got[1].Params.MaxTokens)
	}
}

func TestBatchSubmitUnknownTier(t *testing.T) {
	c := &Client{batches: &fakeBatches{}}
	_, err := c.BatchSubmit(context.Background(), []MsgReq{{ID: "a", Model: "nope", User: "q"}}, Confirm{Confirmed: true})
	if err == nil || !strings.Contains(err.Error(), "unknown model tier") {
		t.Fatalf("err = %v, want unknown-tier error", err)
	}
}

func TestBatchWait(t *testing.T) {
	fake := &fakeBatches{
		statuses: []string{"in_progress", "in_progress", "ended"},
		resultsOut: []anthropic.MessageBatchIndividualResponse{
			{
				CustomID: "bead-1",
				Result: anthropic.MessageBatchResultUnion{
					Type: "succeeded",
					Message: anthropic.Message{
						Model: anthropic.Model("claude-haiku-4-5-20251001"),
						Content: []anthropic.ContentBlockUnion{
							{Type: "text", Text: "answer one"},
						},
						Usage: anthropic.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
					},
				},
			},
			{
				CustomID: "bead-2",
				Result: anthropic.MessageBatchResultUnion{
					Type:  "errored",
					Error: shared.ErrorResponse{Error: shared.ErrorObjectUnion{Type: "invalid_request_error", Message: "boom"}},
				},
			},
			{
				CustomID: "bead-3",
				Result:   anthropic.MessageBatchResultUnion{Type: "canceled"},
			},
		},
	}
	c := &Client{batches: fake}
	results, err := c.BatchWait(context.Background(), "batch_123", time.Millisecond)
	if err != nil {
		t.Fatalf("BatchWait: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %+v", results)
	}

	if results[0].ID != "bead-1" || results[0].Text != "answer one" || results[0].Err != "" {
		t.Errorf("succeeded result = %+v", results[0])
	}
	// haiku canonical rates (internal/pricing): 1M in @ $0.8 + 1M out @ $4 = $4.80
	approx(t, results[0].Usage.EstimateUSD, 4.8, "succeeded usage estimate")

	if results[1].ID != "bead-2" || !strings.Contains(results[1].Err, "boom") {
		t.Errorf("errored result = %+v", results[1])
	}
	if results[2].ID != "bead-3" || results[2].Err != "canceled" {
		t.Errorf("canceled result = %+v", results[2])
	}
}

func TestBatchWaitContextCancel(t *testing.T) {
	fake := &fakeBatches{statuses: []string{"in_progress"}}
	c := &Client{batches: fake}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.BatchWait(ctx, "batch_123", time.Hour); err == nil {
		t.Fatal("BatchWait ignored context cancellation")
	}
}

// --- guardrail: internal/engine must never import internal/anthro --------------

func TestEngineNeverImportsAnthro(t *testing.T) {
	const forbidden = "github.com/koryph/koryph/internal/anthro"
	engineDir := filepath.Join("..", "engine")

	info, err := os.Stat(engineDir)
	if os.IsNotExist(err) {
		t.Log("internal/engine does not exist yet — guardrail passes and will guard the future engine")
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("%s exists but is not a directory", engineDir)
	}

	err = filepath.WalkDir(engineDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			t.Errorf("parsing %s: %v", path, perr)
			return nil
		}
		for _, imp := range file.Imports {
			val, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				continue
			}
			if val == forbidden {
				t.Errorf("%s imports %s — the engine loop must never spend per-token implicitly", path, forbidden)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
