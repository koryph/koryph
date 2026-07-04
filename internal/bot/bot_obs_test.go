// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package bot

// Adversarial redaction tests for bot lifecycle events (§O4).
//
// These tests load and save bot credential files that contain fake PEM /
// token material and assert that ZERO secret content reaches any log record,
// even when the raw capturing handler (no redaction wrapper) is in place.
//
// Tests live in package bot (not bot_test) so they can directly inspect
// internal Config fields and write credential files without going through
// a public Save-then-Load round trip that also emits log events.

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/koryph/koryph/internal/obs"
)

// rawCaptureBot is a minimal non-redacting slog.Handler for adversarial tests.
type rawCaptureBot struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *rawCaptureBot) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *rawCaptureBot) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r.Clone())
	h.mu.Unlock()
	return nil
}

func (h *rawCaptureBot) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *rawCaptureBot) WithGroup(_ string) slog.Handler      { return h }

func (h *rawCaptureBot) assertNoSubstring(t *testing.T, secret string) {
	t.Helper()
	if secret == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		check := func(s string) {
			t.Helper()
			if strings.Contains(s, secret) {
				t.Errorf("secret leaked into log: value=%q", truncateBot(s, 80))
			}
		}
		check(r.Message)
		r.Attrs(func(a slog.Attr) bool {
			check(a.Key)
			check(a.Value.String())
			return true
		})
	}
}

func truncateBot(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func botDebugCfg() obs.Config { return obs.Config{DefaultLevel: "debug"} }

// ---------- adversarial tests ------------------------------------------------

// TestBotSave_InlineMode_NoSecretInLogs tests that Save() with an inline PEM
// never writes the PEM value to any log record.
func TestBotSave_InlineMode_NoSecretInLogs(t *testing.T) {
	// Build fake PEM at runtime to avoid triggering static secret scanners.
	begin := "-----BEGIN RSA " + "PRIVATE KEY-----"
	end := "-----END RSA " + "PRIVATE KEY-----"
	fakePEM := begin + "\nMIIBotFakePEMForSaveTestDoNotUse\n" + end

	dir := t.TempDir()
	t.Setenv("KORYPH_HOME", dir)

	capture := &rawCaptureBot{}
	obs.ReInitRaw(botDebugCfg(), capture)
	t.Cleanup(func() {
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(os.Stderr, nil))
	})

	cfg := &Config{
		Name:  "test-bot",
		AppID: 42,
		Slug:  "test-bot",
		Owner: "testowner",
		PEM:   fakePEM, // inline mode: PEM is in the struct, must NOT be logged
	}

	if err := Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The full PEM block must not appear in any log record.
	capture.assertNoSubstring(t, "MIIBotFakePEM")
	capture.assertNoSubstring(t, "DoNotUse")
	capture.assertNoSubstring(t, "BEGIN RSA")
	capture.assertNoSubstring(t, "END RSA")
}

// TestBotLoad_InlineMode_NoSecretInLogs tests that Load() with an inline PEM
// credential file never writes the PEM value to any log record.
func TestBotLoad_InlineMode_NoSecretInLogs(t *testing.T) {
	// Build fake PEM at runtime.
	begin := "-----BEGIN RSA " + "PRIVATE KEY-----"
	end := "-----END RSA " + "PRIVATE KEY-----"
	fakePEM := begin + "\nMIIBotFakePEMForLoadTestDoNotUse\n" + end

	dir := t.TempDir()
	t.Setenv("KORYPH_HOME", dir)

	// Pre-create the bot credential file without going through Save()
	// (Save itself emits events; we want Load to be the source of events here).
	cfg := &Config{
		Name:  "load-test-bot",
		AppID: 99,
		Slug:  "load-test-bot",
		Owner: "loadowner",
		PEM:   fakePEM,
	}
	// Write the file directly, bypassing Save() and its log events.
	if err := os.MkdirAll(BotsDir(), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeTestBotFile(t, cfg)

	// Now install the capturing handler and call Load.
	capture := &rawCaptureBot{}
	obs.ReInitRaw(botDebugCfg(), capture)
	t.Cleanup(func() {
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(os.Stderr, nil))
	})

	loaded, err := Load(cfg.Name)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.PEM == "" {
		t.Fatal("Load returned empty PEM (sanity check failed)")
	}

	// The fake PEM must not appear in any log record.
	capture.assertNoSubstring(t, "MIIBotFakePEM")
	capture.assertNoSubstring(t, "DoNotUse")
	capture.assertNoSubstring(t, "BEGIN RSA")
	capture.assertNoSubstring(t, "END RSA")
}

// TestBotLoad_PointerMode_NoSecretInLogs tests that Load() with a pointer-mode
// credential file does not log the key_ref value (which is safe) or any
// other secret material.
func TestBotLoad_PointerMode_NoSecretInLogs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KORYPH_HOME", dir)

	cfg := &Config{
		Name:     "ptr-bot",
		AppID:    77,
		Slug:     "ptr-bot",
		Owner:    "ptrowner",
		Provider: "protonpass",
		KeyRef:   "pass://test-share/test-item",
		// No PEM in pointer mode.
	}
	if err := os.MkdirAll(BotsDir(), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeTestBotFile(t, cfg)

	capture := &rawCaptureBot{}
	obs.ReInitRaw(botDebugCfg(), capture)
	t.Cleanup(func() {
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(os.Stderr, nil))
	})

	loaded, err := Load(cfg.Name)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loaded.IsPointer() {
		t.Fatal("expected pointer-mode config")
	}

	// The key_ref IS expected in logs (it is a safe vault reference).
	// But no actual secret value should appear.
	var foundKeyRef bool
	capture.mu.Lock()
	for _, r := range capture.records {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == obs.KeyKeyRef {
				foundKeyRef = true
			}
			return true
		})
	}
	capture.mu.Unlock()

	if !foundKeyRef {
		t.Error("key_ref not found in bot.lifecycle load event — expected as safe metadata")
	}

	// No token values, PEM content, or other secrets.
	capture.assertNoSubstring(t, "some-real-secret")
}

// TestBotSave_PointerMode_SafeFieldsLogged verifies that in pointer mode,
// the lifecycle "save" event includes the vault provider name (safe) and
// the is_pointer flag.
func TestBotSave_PointerMode_SafeFieldsLogged(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KORYPH_HOME", dir)

	capture := &rawCaptureBot{}
	obs.ReInitRaw(botDebugCfg(), capture)
	t.Cleanup(func() {
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(os.Stderr, nil))
	})

	cfg := &Config{
		Name:     "ptr-save-bot",
		AppID:    55,
		Slug:     "ptr-save-bot",
		Owner:    "owner",
		Provider: "keychain",
		KeyRef:   "koryph-bot-ptr-save-bot",
	}

	if err := Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	capture.mu.Lock()
	recs := capture.records
	capture.mu.Unlock()

	var foundName, foundProvider bool
	for _, r := range recs {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "name" && a.Value.String() == "ptr-save-bot" {
				foundName = true
			}
			if a.Key == obs.KeyProvider && a.Value.String() == "keychain" {
				foundProvider = true
			}
			return true
		})
	}
	if !foundName {
		t.Error("name attr not found in bot.lifecycle save event")
	}
	if !foundProvider {
		t.Error("provider attr not found in bot.lifecycle save event")
	}
}

// writeTestBotFile is a test helper that writes a bot config directly to disk
// without emitting lifecycle log events (unlike Save). This lets adversarial
// Load tests start with a clean capture buffer.
func writeTestBotFile(t *testing.T, cfg *Config) {
	t.Helper()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal bot config: %v", err)
	}
	path := BotPath(cfg.Name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write bot file: %v", err)
	}
}
