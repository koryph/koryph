// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

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
	if err != nil || u.Scheme != "http" || !isLoopbackAddr(u.Hostname()) {
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

// isLoopbackAddr reports whether host (already stripped of port/brackets by
// url.URL.Hostname()) is a loopback address. Mirrors registry.isLoopbackHost
// but lives here to avoid cross-package import just for one predicate.
func isLoopbackAddr(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	// 127.0.0.0/8
	if strings.HasPrefix(host, "127.") {
		return true
	}
	// ::1
	if host == "::1" || host == "[::1]" {
		return true
	}
	return false
}
