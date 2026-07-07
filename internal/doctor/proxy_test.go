// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/registry"
)

// fakeGet builds an injectable ProxyHTTPGet that returns the given code and body.
func fakeGet(code int, body string) func(string) (int, []byte, error) {
	return func(_ string) (int, []byte, error) {
		return code, []byte(body), nil
	}
}

// fakeGetErr builds an injectable ProxyHTTPGet that returns an error.
func fakeGetErr(msg string) func(string) (int, []byte, error) {
	return func(_ string) (int, []byte, error) {
		return 0, nil, fmt.Errorf("%s", msg)
	}
}

// proxyRecord builds a minimal registry.Record with an agent_proxy block.
func proxyRecord(baseURL, health, pin string) *registry.Record {
	return &registry.Record{
		ProjectID: "test-proj",
		AgentProxy: &registry.AgentProxy{
			BaseURL: baseURL,
			Health:  health,
			Pin:     pin,
		},
	}
}

// pinBody encodes a health-response body carrying the given pin.
func pinBody(pin string) string {
	b, _ := json.Marshal(proxyHealthBody{Pin: pin})
	return string(b)
}

// --- checkProxy -----------------------------------------------------------------

func TestCheckProxyNoRecords(t *testing.T) {
	o := Options{
		RegistryList: func() ([]*registry.Record, error) { return nil, nil },
	}
	fs := checkProxy(o)
	if len(fs) != 0 {
		t.Errorf("no-record case: want 0 findings, got %d: %v", len(fs), fs)
	}
}

func TestCheckProxyNoProxyConfigured(t *testing.T) {
	o := Options{
		RegistryList: func() ([]*registry.Record, error) {
			return []*registry.Record{{ProjectID: "foo", AgentProxy: nil}}, nil
		},
	}
	fs := checkProxy(o)
	if len(fs) != 0 {
		t.Errorf("no-proxy case: want 0 findings, got %d: %v", len(fs), fs)
	}
}

func TestCheckProxyRegistryError(t *testing.T) {
	o := Options{
		RegistryList: func() ([]*registry.Record, error) {
			return nil, fmt.Errorf("registry exploded")
		},
	}
	fs := checkProxy(o)
	if len(fs) != 1 || fs[0].Level != LevelWarn {
		t.Errorf("registry error: want 1 WARN, got %v", fs)
	}
}

// --- checkOneProxy ---------------------------------------------------------------

func TestCheckProxyHealthNoEndpoint(t *testing.T) {
	rec := proxyRecord("http://127.0.0.1:8787", "", "")
	fs := checkOneProxy(Options{}, rec.ProjectID, rec.AgentProxy.BaseURL, rec.AgentProxy.Health, rec.AgentProxy.Pin)
	// expect: loopback OK + health-missing WARN
	if got := levelCount(fs, LevelWarn); got != 1 {
		t.Errorf("no-health: want 1 WARN, got %d: %v", got, fs)
	}
	if got := levelCount(fs, LevelError); got != 0 {
		t.Errorf("no-health: want 0 ERROR, got %d: %v", got, fs)
	}
}

func TestCheckProxyHealthUnreachable(t *testing.T) {
	o := Options{ProxyHTTPGet: fakeGetErr("connection refused")}
	rec := proxyRecord("http://127.0.0.1:8787", "/health", "")
	fs := checkOneProxy(o, rec.ProjectID, rec.AgentProxy.BaseURL, rec.AgentProxy.Health, rec.AgentProxy.Pin)
	if got := levelCount(fs, LevelError); got != 1 {
		t.Errorf("unreachable: want 1 ERROR, got %d: %v", got, fs)
	}
	assertContains(t, fs, LevelError, "unreachable")
}

func TestCheckProxyHealthNon2xx(t *testing.T) {
	o := Options{ProxyHTTPGet: fakeGet(503, `{"status":"overloaded"}`)}
	rec := proxyRecord("http://127.0.0.1:8787", "/health", "")
	fs := checkOneProxy(o, rec.ProjectID, rec.AgentProxy.BaseURL, rec.AgentProxy.Health, rec.AgentProxy.Pin)
	if got := levelCount(fs, LevelError); got != 1 {
		t.Errorf("non-2xx: want 1 ERROR, got %d: %v", got, fs)
	}
	assertContains(t, fs, LevelError, "HTTP 503")
}

func TestCheckProxyHealthOKNoPinConfigured(t *testing.T) {
	o := Options{ProxyHTTPGet: fakeGet(200, `{"status":"ok"}`)}
	rec := proxyRecord("http://127.0.0.1:8787", "/health", "")
	fs := checkOneProxy(o, rec.ProjectID, rec.AgentProxy.BaseURL, rec.AgentProxy.Health, rec.AgentProxy.Pin)
	if got := levelCount(fs, LevelOK); got < 1 {
		t.Errorf("health ok no pin: want ≥1 OK, got %d: %v", got, fs)
	}
	if got := levelCount(fs, LevelError); got != 0 {
		t.Errorf("health ok no pin: want 0 ERROR, got %d: %v", got, fs)
	}
}

func TestCheckProxyPinMatch(t *testing.T) {
	const configuredPin = "headroom-ai==0.30.0"
	o := Options{ProxyHTTPGet: fakeGet(200, pinBody(configuredPin))}
	rec := proxyRecord("http://127.0.0.1:8787", "/health", configuredPin)
	fs := checkOneProxy(o, rec.ProjectID, rec.AgentProxy.BaseURL, rec.AgentProxy.Health, rec.AgentProxy.Pin)
	if got := levelCount(fs, LevelError); got != 0 {
		t.Errorf("pin match: want 0 ERROR, got %d: %v", got, fs)
	}
	if got := levelCount(fs, LevelWarn); got != 0 {
		t.Errorf("pin match: want 0 WARN, got %d: %v", got, fs)
	}
	assertContains(t, fs, LevelOK, "pin")
	assertContains(t, fs, LevelOK, "matched")
}

func TestCheckProxyPinMismatch(t *testing.T) {
	const configuredPin = "headroom-ai==0.30.0"
	const runningPin = "headroom-ai==0.29.0"
	o := Options{ProxyHTTPGet: fakeGet(200, pinBody(runningPin))}
	rec := proxyRecord("http://127.0.0.1:8787", "/health", configuredPin)
	fs := checkOneProxy(o, rec.ProjectID, rec.AgentProxy.BaseURL, rec.AgentProxy.Health, rec.AgentProxy.Pin)
	if got := levelCount(fs, LevelError); got != 1 {
		t.Errorf("pin mismatch: want 1 ERROR, got %d: %v", got, fs)
	}
	assertContains(t, fs, LevelError, "pin mismatch")
	assertContains(t, fs, LevelError, "refuse-to-route")
}

func TestCheckProxyPinConfiguredButProxyDoesNotReport(t *testing.T) {
	const configuredPin = "headroom-ai==0.30.0"
	// Proxy returns 200 but no "pin" field.
	o := Options{ProxyHTTPGet: fakeGet(200, `{"status":"ok"}`)}
	rec := proxyRecord("http://127.0.0.1:8787", "/health", configuredPin)
	fs := checkOneProxy(o, rec.ProjectID, rec.AgentProxy.BaseURL, rec.AgentProxy.Health, rec.AgentProxy.Pin)
	if got := levelCount(fs, LevelWarn); got != 1 {
		t.Errorf("pin not reported: want 1 WARN, got %d: %v", got, fs)
	}
	if got := levelCount(fs, LevelError); got != 0 {
		t.Errorf("pin not reported: want 0 ERROR, got %d: %v", got, fs)
	}
	assertContains(t, fs, LevelWarn, "cannot verify")
}

// --- positive routing verification (koryph-3l1.5) --------------------------------

// routingRecord builds a registry.Record with an agent_proxy block rooted at
// a fresh temp dir (for ledger reads) — the fixture every routing-check test
// starts from.
func routingRecord(t *testing.T, baseURL, pin string) *registry.Record {
	t.Helper()
	return &registry.Record{
		ProjectID: "test-proj",
		Root:      t.TempDir(),
		AgentProxy: &registry.AgentProxy{
			BaseURL: baseURL,
			Health:  "/health",
			Pin:     pin,
		},
	}
}

// writeProxiedSlot records one terminal ledger slot dispatched through
// proxyID (ProxyConfigured=true) — the "koryph believes it routed this
// dispatch through the proxy" fixture that countProxiedDispatches reads.
func writeProxiedSlot(t *testing.T, repoRoot, runID, phaseID, proxyID string) {
	t.Helper()
	writeLedgerRun(t, repoRoot, &ledger.Run{
		RunID:     runID,
		ProjectID: "test-proj",
		Status:    ledger.RunDone,
		Slots: map[string]*ledger.Slot{
			phaseID: {
				PhaseID:         phaseID,
				Status:          ledger.SlotDone,
				ProxyConfigured: true,
				ProxyID:         proxyID,
			},
		},
	})
}

func TestCheckOneProxyRoutingNoDispatchesSkipsOK(t *testing.T) {
	rec := routingRecord(t, "http://127.0.0.1:8787", "")
	fs := checkOneProxyRouting(Options{}, rec)
	if len(fs) != 1 || fs[0].Level != LevelOK {
		t.Fatalf("no dispatches: want 1 OK, got %v", fs)
	}
	if !strings.Contains(fs[0].Message, "skipped") {
		t.Errorf("no dispatches: want message to note the check was skipped, got %q", fs[0].Message)
	}
}

func TestCheckOneProxyRoutingInPathOK(t *testing.T) {
	rec := routingRecord(t, "http://127.0.0.1:8787", "")
	proxyID := rec.AgentProxy.ID()
	writeProxiedSlot(t, rec.Root, "20260707-000000", "koryph-1", proxyID)

	o := Options{ProxyHTTPGet: fakeGet(200, `{"requests": 42}`)}
	fs := checkOneProxyRouting(o, rec)
	if len(fs) != 1 || fs[0].Level != LevelOK {
		t.Fatalf("in-path: want 1 OK, got %v", fs)
	}
	if !strings.Contains(fs[0].Message, "42") {
		t.Errorf("in-path: want message to cite the observed count, got %q", fs[0].Message)
	}
}

func TestCheckOneProxyRoutingConfiguredButBypassedError(t *testing.T) {
	rec := routingRecord(t, "http://127.0.0.1:8787", "")
	proxyID := rec.AgentProxy.ID()
	writeProxiedSlot(t, rec.Root, "20260707-000000", "koryph-1", proxyID)
	writeProxiedSlot(t, rec.Root, "20260707-000100", "koryph-2", proxyID)

	o := Options{ProxyHTTPGet: fakeGet(200, `{"requests": 0}`)}
	fs := checkOneProxyRouting(o, rec)
	if len(fs) != 1 || fs[0].Level != LevelError {
		t.Fatalf("configured-but-bypassed: want 1 ERROR, got %v", fs)
	}
	if !strings.Contains(fs[0].Message, "refuse-to-route") {
		t.Errorf("configured-but-bypassed: want refuse-to-route guidance, got %q", fs[0].Message)
	}
	if !strings.Contains(fs[0].Message, "2 proxied-arm dispatch") {
		t.Errorf("configured-but-bypassed: want the ledger-observed dispatch count cited, got %q", fs[0].Message)
	}
}

func TestCheckOneProxyRoutingNoCounterEndpointWarnsOnUnreachable(t *testing.T) {
	rec := routingRecord(t, "http://127.0.0.1:8787", "")
	proxyID := rec.AgentProxy.ID()
	writeProxiedSlot(t, rec.Root, "20260707-000000", "koryph-1", proxyID)

	o := Options{ProxyHTTPGet: fakeGetErr("connection refused")}
	fs := checkOneProxyRouting(o, rec)
	if len(fs) != 1 || fs[0].Level != LevelWarn {
		t.Fatalf("no-counter (unreachable): want 1 WARN, got %v", fs)
	}
	if !strings.Contains(fs[0].Message, "unreachable") {
		t.Errorf("no-counter (unreachable): want message to name the limitation, got %q", fs[0].Message)
	}
}

func TestCheckOneProxyRoutingNoCounterEndpointWarnsOnUnknownSchema(t *testing.T) {
	rec := routingRecord(t, "http://127.0.0.1:8787", "")
	proxyID := rec.AgentProxy.ID()
	writeProxiedSlot(t, rec.Root, "20260707-000000", "koryph-1", proxyID)

	// 200 OK, but no field this doctor recognizes as a request counter.
	o := Options{ProxyHTTPGet: fakeGet(200, `{"status":"ok","uptime_seconds":120}`)}
	fs := checkOneProxyRouting(o, rec)
	if len(fs) != 1 || fs[0].Level != LevelWarn {
		t.Fatalf("no-counter (unknown schema): want 1 WARN, got %v", fs)
	}
	if !strings.Contains(fs[0].Message, "did not contain a recognized request-counter field") {
		t.Errorf("no-counter (unknown schema): want message to name the limitation, got %q", fs[0].Message)
	}
}

func TestCheckOneProxyRoutingUsesDefaultStatsPath(t *testing.T) {
	rec := routingRecord(t, "http://127.0.0.1:8787", "")
	proxyID := rec.AgentProxy.ID()
	writeProxiedSlot(t, rec.Root, "20260707-000000", "koryph-1", proxyID)

	var gotURL string
	o := Options{ProxyHTTPGet: func(url string) (int, []byte, error) {
		gotURL = url
		return 200, []byte(`{"requests": 1}`), nil
	}}
	checkOneProxyRouting(o, rec)
	if want := "http://127.0.0.1:8787/stats"; gotURL != want {
		t.Errorf("default stats path: want GET %s, got %s", want, gotURL)
	}
}

func TestCheckOneProxyRoutingHonorsConfiguredStatsPath(t *testing.T) {
	rec := routingRecord(t, "http://127.0.0.1:8787", "")
	rec.AgentProxy.Stats = "/v1/counters"
	proxyID := rec.AgentProxy.ID()
	writeProxiedSlot(t, rec.Root, "20260707-000000", "koryph-1", proxyID)

	var gotURL string
	o := Options{ProxyHTTPGet: func(url string) (int, []byte, error) {
		gotURL = url
		return 200, []byte(`{"requests": 1}`), nil
	}}
	checkOneProxyRouting(o, rec)
	if want := "http://127.0.0.1:8787/v1/counters"; gotURL != want {
		t.Errorf("configured stats path: want GET %s, got %s", want, gotURL)
	}
}

func TestExtractRequestCounterNestedField(t *testing.T) {
	f, ok := extractRequestCounter([]byte(`{"stats": {"total_requests": 7}}`))
	if !ok || f != 7 {
		t.Errorf("nested field: want (7, true), got (%v, %v)", f, ok)
	}
}

func TestExtractRequestCounterUnknownSchema(t *testing.T) {
	if _, ok := extractRequestCounter([]byte(`{"status":"ok"}`)); ok {
		t.Error("unknown schema: want ok=false")
	}
}

// --- calibration stale check ---------------------------------------------------

func TestCheckQuotaCalibrationStaleFlag(t *testing.T) {
	home := fabricate(t)
	// Write a calibrated quota config with CalibrationStale=true.
	writeQuotaJSON(t, home, "acct", map[string]interface{}{
		"account":                  "acct",
		"window_ceiling_usd":       100.0,
		"weekly_ceiling_usd":       1000.0,
		"calibration_stale":        true,
		"calibration_stale_reason": "agent_proxy changed for project foo",
	})
	o := opts(home)
	r, _ := Run(o)
	var staleFindings []Finding
	for _, f := range r.Findings {
		if f.Check == checkNameQuota && f.Level == LevelWarn && strings.Contains(f.Message, "calibration stale") {
			staleFindings = append(staleFindings, f)
		}
	}
	if len(staleFindings) != 1 {
		t.Errorf("calibration stale: want 1 stale finding, got %d (quota findings: %v)", len(staleFindings), quotaFindings(r))
	} else if !strings.Contains(staleFindings[0].Message, "koryph quota calibrate") {
		t.Errorf("calibration stale: message should mention koryph quota calibrate, got %q", staleFindings[0].Message)
	}
}

func TestCheckQuotaCalibrationNotStaleWhenFlagAbsent(t *testing.T) {
	home := fabricate(t)
	writeQuotaJSON(t, home, "acct", map[string]interface{}{
		"account":            "acct",
		"window_ceiling_usd": 100.0,
		"weekly_ceiling_usd": 1000.0,
	})
	o := opts(home)
	r, _ := Run(o)
	for _, f := range r.Findings {
		if f.Check == checkNameQuota && f.Level == LevelWarn && strings.Contains(f.Message, "calibration stale") {
			t.Errorf("no stale flag: unexpected stale finding: %v", f)
		}
	}
}

// --- helpers -------------------------------------------------------------------

func levelCount(fs []Finding, level Level) int {
	n := 0
	for _, f := range fs {
		if f.Level == level {
			n++
		}
	}
	return n
}

func assertContains(t *testing.T, fs []Finding, level Level, substr string) {
	t.Helper()
	for _, f := range fs {
		if f.Level == level && strings.Contains(f.Message, substr) {
			return
		}
	}
	t.Errorf("want finding level=%s containing %q in %v", level, substr, fs)
}

func quotaFindings(r *Report) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Check == checkNameQuota {
			out = append(out, f)
		}
	}
	return out
}

func writeQuotaJSON(t *testing.T, home, account string, v interface{}) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "quota", account+".json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
