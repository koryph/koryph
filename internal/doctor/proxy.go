// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/netx"
	"github.com/koryph/koryph/internal/registry"
)

// defaultProxyStatsPath is the request-counter endpoint tried when a
// project's agent_proxy.stats is unset (koryph-3l1.5): most proxies
// (including headroom-ai) expose a GET /stats endpoint with request counts
// by convention, so positive routing verification is attempted even when
// the operator never configures a stats path explicitly.
const defaultProxyStatsPath = "/stats"

// counterFieldNames is the accepted naming convention for a proxy's
// forwarded-request counter (koryph-3l1.5). headroom-ai's own /stats shape
// is not hardcoded as the only shape a proxy may report: any JSON number
// field whose key normalizes (lowercased, underscores stripped) to one of
// these names is understood, whether at the top level of the response body
// or nested one level deep (e.g. {"stats": {"total_requests": 4}}). A proxy
// that reports no such field degrades the check to a WARN naming the
// limitation rather than a silent pass — see checkOneProxyRouting.
var counterFieldNames = map[string]bool{
	"requests":             true,
	"requestcount":         true,
	"requeststotal":        true,
	"totalrequests":        true,
	"forwardedrequests":    true,
	"requestsforwarded":    true,
	"upstreamrequests":     true,
	"upstreamseen":         true,
	"upstreamrequestsseen": true,
}

// proxyHealthBody is the subset of a proxy health-endpoint JSON response that
// the doctor cares about (koryph-3l1.2). The proxy may return additional
// fields; they are ignored.
type proxyHealthBody struct {
	// Pin is the proxy's self-reported identity/version pin (e.g.
	// "headroom-ai==0.30.0"). Compared against AgentProxy.Pin when set.
	Pin string `json:"pin,omitempty"`
}

// checkProxy iterates every registered project that has an agent_proxy block
// and emits one or more findings per project (koryph-3l1.2):
//
//   - base_url loopback: always OK (registry already machine-checks this at
//     load; the finding confirms the invariant held at doctor time too).
//   - health endpoint reachable: GET <base_url><health>; 2xx → ok, else error.
//   - pin match: when AgentProxy.Pin is set, the health response body must
//     contain a "pin" JSON field equal to the configured pin; mismatch → error
//     with refuse-to-route guidance (a wrong pin means a different proxy
//     version is running — calibration populations are segmented by proxy ID
//     so routing through a mis-pinned proxy contaminates the wrong bucket).
//   - positive routing verification (koryph-3l1.5): compares the proxy's
//     self-reported forwarded-request counter against koryph's own ledger
//     count of dispatches routed to this proxy's arm — see
//     checkOneProxyRouting's doc for why health+pin alone don't catch a
//     silent bypass.
//
// No findings are emitted when no registry records exist or none have a proxy.
// Registry load failures are reported as a single WARN finding so a corrupt
// registry.d entry doesn't silence the check entirely.
func checkProxy(opts Options) []Finding {
	recs, err := opts.registryList()
	if err != nil {
		return []Finding{{
			Check:   checkNameProxy,
			Level:   LevelWarn,
			Message: "cannot list registry records for proxy check: " + err.Error(),
		}}
	}

	var findings []Finding
	anyProxy := false
	for _, rec := range recs {
		if rec.AgentProxy == nil {
			continue
		}
		anyProxy = true
		findings = append(findings, checkOneProxy(opts, rec.ProjectID, rec.AgentProxy.BaseURL, rec.AgentProxy.Health, rec.AgentProxy.Pin)...)
		findings = append(findings, checkOneProxyRouting(opts, rec)...)
	}
	if !anyProxy {
		// No projects have a proxy configured — emit nothing (the check is
		// opt-in; silence is correct when the feature isn't in use).
		return nil
	}
	return findings
}

// checkOneProxy emits findings for one project's agent_proxy configuration.
func checkOneProxy(opts Options, projectID, baseURL, health, pin string) []Finding {
	prefix := "project " + projectID + " agent_proxy"

	// 1. Loopback invariant — already enforced by the registry at load, but
	//    we confirm it held at doctor time (a hand-edited record that bypassed
	//    the registry's load path would show up here as a WARN).
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "http" || !netx.IsLoopbackHost(u.Hostname()) {
		return []Finding{{
			Check:   checkNameProxy,
			Level:   LevelError,
			Message: fmt.Sprintf("%s: base_url %q is not an http loopback URL — routing through a non-loopback proxy is refused; fix the registry record", prefix, baseURL),
		}}
	}

	var findings []Finding
	findings = append(findings, Finding{
		Check:   checkNameProxy,
		Level:   LevelOK,
		Message: fmt.Sprintf("%s: base_url %s is loopback (ok)", prefix, baseURL),
	})

	// 2. Health endpoint reachability + pin verification.
	if health == "" {
		findings = append(findings, Finding{
			Check:   checkNameProxy,
			Level:   LevelWarn,
			Message: fmt.Sprintf("%s: health endpoint not configured; cannot verify proxy is reachable (add agent_proxy.health)", prefix),
		})
		return findings
	}

	healthURL := strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(health, "/")
	code, body, err := opts.proxyHTTPGet(healthURL)
	if err != nil {
		return append(findings, Finding{
			Check:   checkNameProxy,
			Level:   LevelError,
			Message: fmt.Sprintf("%s: health endpoint %s unreachable: %v — proxy must be running before dispatch", prefix, healthURL, err),
		})
	}
	if code < 200 || code >= 300 {
		return append(findings, Finding{
			Check:   checkNameProxy,
			Level:   LevelError,
			Message: fmt.Sprintf("%s: health endpoint %s returned HTTP %d (want 2xx) — proxy may be misconfigured", prefix, healthURL, code),
		})
	}

	// Health is reachable.
	if pin == "" {
		findings = append(findings, Finding{
			Check:   checkNameProxy,
			Level:   LevelOK,
			Message: fmt.Sprintf("%s: health %s ok (no pin configured)", prefix, healthURL),
		})
		return findings
	}

	// 3. Pin match — parse the health response body for a "pin" field.
	var hb proxyHealthBody
	if jerr := json.Unmarshal(body, &hb); jerr != nil || hb.Pin == "" {
		// Proxy didn't return a "pin" field — we cannot verify.
		findings = append(findings, Finding{
			Check:   checkNameProxy,
			Level:   LevelWarn,
			Message: fmt.Sprintf("%s: health %s ok; pin %q configured but proxy did not report a \"pin\" field in the health response — cannot verify version match", prefix, healthURL, pin),
		})
		return findings
	}

	if hb.Pin != pin {
		return append(findings, Finding{
			Check: checkNameProxy,
			Level: LevelError,
			Message: fmt.Sprintf("%s: pin mismatch — configured %q, proxy reports %q; "+
				"refuse-to-route: update the registry pin to match the running proxy, "+
				"or install the pinned version (%s) before dispatching", prefix, pin, hb.Pin, pin),
		})
	}

	findings = append(findings, Finding{
		Check:   checkNameProxy,
		Level:   LevelOK,
		Message: fmt.Sprintf("%s: health %s ok; pin %q matched", prefix, healthURL, pin),
	})
	return findings
}

// checkOneProxyRouting emits the positive-routing-verification finding for
// one project's agent_proxy (koryph-3l1.5, design
// docs/designs/2026-07-token-economy.md §3 L5's fourth doctor check, §2 I1's
// "fail-open means bypass"): checkOneProxy's health+pin checks only prove the
// proxy process is up and correctly versioned — neither proves koryph's
// dispatched traffic is actually flowing THROUGH it. A proxy that is
// healthy, correctly pinned, and silently bypassed (e.g. ChildEnvSpec wiring
// regressed, or something downstream overrode ANTHROPIC_BASE_URL) would pass
// checkOneProxy clean while every dispatch goes direct — exactly I1's
// failure mode, uncaught. This check compares koryph's own ledger count of
// dispatches it believes it routed to this proxy's arm (ProxyConfigured &&
// ProxyID == AgentProxy.ID(), fp:read:ledger — read-only, mirrors the
// ledger.Store access pattern checkStalledRuns/checkOrphanWorktrees already
// use in project.go) against the proxy's self-reported forwarded-request
// counter (agent_proxy.stats, default "/stats" — see defaultProxyStatsPath).
//
// Outcomes, never a silent pass:
//   - no proxied-arm dispatches recorded yet → OK (nothing to verify).
//   - stats endpoint unreachable / non-2xx / unrecognized JSON schema → WARN
//     naming the limitation (counterFieldNames documents the accepted
//     schema convention; an unknown shape is not assumed to mean anything).
//   - dispatches recorded but the proxy reports zero upstream-seen requests
//     → ERROR with refuse-to-route guidance (configured-but-bypassed).
//   - dispatches recorded and the proxy reports a nonzero count → OK.
func checkOneProxyRouting(opts Options, rec *registry.Record) []Finding {
	prefix := "project " + rec.ProjectID + " agent_proxy"
	proxyID := rec.AgentProxy.ID()

	dispatched, derr := countProxiedDispatches(rec.Root, proxyID)
	if derr != nil {
		return []Finding{{
			Check:   checkNameProxy,
			Level:   LevelWarn,
			Message: fmt.Sprintf("%s: routing verification limited — cannot read ledger to count proxied-arm dispatches: %v", prefix, derr),
		}}
	}
	if dispatched == 0 {
		return []Finding{{
			Check:   checkNameProxy,
			Level:   LevelOK,
			Message: fmt.Sprintf("%s: routing verification skipped — no proxied-arm dispatches recorded in the ledger yet", prefix),
		}}
	}

	statsPath := rec.AgentProxy.Stats
	if statsPath == "" {
		statsPath = defaultProxyStatsPath
	}
	statsURL := strings.TrimRight(rec.AgentProxy.BaseURL, "/") + "/" + strings.TrimLeft(statsPath, "/")

	code, body, err := opts.proxyHTTPGet(statsURL)
	if err != nil {
		return []Finding{{
			Check: checkNameProxy,
			Level: LevelWarn,
			Message: fmt.Sprintf("%s: routing verification limited — stats endpoint %s unreachable: %v; koryph has routed %d proxied-arm dispatch(es) per the ledger but cannot confirm the proxy actually saw them",
				prefix, statsURL, err, dispatched),
		}}
	}
	if code < 200 || code >= 300 {
		return []Finding{{
			Check: checkNameProxy,
			Level: LevelWarn,
			Message: fmt.Sprintf("%s: routing verification limited — stats endpoint %s returned HTTP %d; koryph has routed %d proxied-arm dispatch(es) per the ledger but cannot confirm the proxy actually saw them",
				prefix, statsURL, code, dispatched),
		}}
	}

	seen, ok := extractRequestCounter(body)
	if !ok {
		return []Finding{{
			Check: checkNameProxy,
			Level: LevelWarn,
			Message: fmt.Sprintf("%s: routing verification limited — stats endpoint %s response did not contain a recognized request-counter field (looked for requests/request_count/total_requests-style names, top-level or nested one level); koryph has routed %d proxied-arm dispatch(es) per the ledger but cannot confirm the proxy actually saw them",
				prefix, statsURL, dispatched),
		}}
	}

	if seen == 0 {
		return []Finding{{
			Check: checkNameProxy,
			Level: LevelError,
			Message: fmt.Sprintf("%s: configured but not in path — koryph has routed %d proxied-arm dispatch(es) per the ledger, but %s reports 0 upstream-seen requests; refuse-to-route: dispatches are silently bypassing the proxy — verify ChildEnvSpec/ANTHROPIC_BASE_URL wiring at every spawn site (main dispatch, review, stage, epicreview) before dispatching further",
				prefix, dispatched, statsURL),
		}}
	}

	return []Finding{{
		Check: checkNameProxy,
		Level: LevelOK,
		Message: fmt.Sprintf("%s: routing verification ok — %s reports %.0f upstream-seen request(s) against %d proxied-arm dispatch(es) in the ledger",
			prefix, statsURL, seen, dispatched),
	}}
}

// countProxiedDispatches returns the number of ledger slots across every run
// recorded for repoRoot where ProxyConfigured is true and ProxyID equals
// proxyID — koryph's own count of dispatches it believes it routed to this
// proxy's arm (koryph-3l1.5). Read-only (fp:read:ledger): uses ledger.Store,
// the same access pattern checkStalledRuns/checkOrphanWorktrees already use
// in project.go. An empty/unreadable ledger tree is "0 dispatches, no
// error" (ledger.Store.ListRuns already treats a missing directory as no
// runs); a genuine read error on an existing run is skipped rather than
// aborting the whole count, matching those functions' fail-soft precedent.
func countProxiedDispatches(repoRoot, proxyID string) (int, error) {
	store := ledger.NewStore(repoRoot)
	runIDs, err := store.ListRuns()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, runID := range runIDs {
		run, rerr := store.LoadRun(runID)
		if rerr != nil {
			continue
		}
		for _, slot := range run.Slots {
			if slot == nil || !slot.ProxyConfigured {
				continue
			}
			if slot.ProxyID == proxyID {
				count++
			}
		}
	}
	return count, nil
}

// extractRequestCounter searches a stats-endpoint JSON response body for a
// plausible forwarded-request counter per counterFieldNames, at the top
// level and one level of nesting (e.g. {"stats": {"total_requests": 4}}).
// Returns ok=false when no recognized field is found — checkOneProxyRouting
// treats that as an unknown schema and WARNs rather than assuming a verdict.
func extractRequestCounter(body []byte) (float64, bool) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return 0, false
	}
	if f, ok := findCounterField(raw); ok {
		return f, true
	}
	for _, v := range raw {
		nested, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		if f, ok := findCounterField(nested); ok {
			return f, true
		}
	}
	return 0, false
}

// findCounterField returns the first field of m whose key normalizes to a
// name in counterFieldNames and whose value is a JSON number.
func findCounterField(m map[string]interface{}) (float64, bool) {
	for k, v := range m {
		if !counterFieldNames[normalizeCounterKey(k)] {
			continue
		}
		if f, ok := v.(float64); ok {
			return f, true
		}
	}
	return 0, false
}

// normalizeCounterKey lowercases and strips underscores so "request_count",
// "RequestCount", and "requestcount" are all recognized as the same field.
func normalizeCounterKey(k string) string {
	return strings.ToLower(strings.ReplaceAll(k, "_", ""))
}
