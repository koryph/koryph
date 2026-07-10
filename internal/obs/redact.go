// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import (
	"log/slog"
	"regexp"
	"strings"
)

// Redacted is the replacement value placed wherever a secret is detected.
const Redacted = "[REDACTED]"

// secretKeyPatterns matches slog attribute key names that are likely to carry
// secret values. Keys are matched case-insensitively (the pattern already
// covers both cases via alternation).
var secretKeyPattern = regexp.MustCompile(
	`(?i)(token|secret|password|passwd|api[_-]?key|auth|bearer|credential|private[_-]?key|vault|passphrase)`,
)

// secretValuePatterns match common secret-shaped strings in arbitrary values.
var secretValuePatterns = []*regexp.Regexp{
	// PEM blocks (any label)
	regexp.MustCompile(`-----BEGIN [A-Z ]+-----[\s\S]+?-----END [A-Z ]+-----`),
	// Authorization header values: "Bearer ..." / "Basic ..." / "Token ..."
	regexp.MustCompile(`(?i)(bearer|basic|token)\s+[A-Za-z0-9+/=._\-]{8,}`),
	// Generic high-entropy base64-ish tokens ≥ 32 chars (heuristic)
	regexp.MustCompile(`[A-Za-z0-9+/=]{32,}`),
	// sk-... / ghp_... / glpat-... style API keys
	regexp.MustCompile(`(?:sk-|ghp_|glpat-|xoxb-|xoxp-|anthropic-)[A-Za-z0-9_\-]{16,}`),
}

// RedactValue scrubs a string value that may contain secrets.
// It applies value-level patterns (PEM, bearer, long tokens) and returns the
// sanitised string.
func RedactValue(v string) string {
	for _, re := range secretValuePatterns {
		v = re.ReplaceAllString(v, Redacted)
	}
	return v
}

// RedactAttr inspects a single slog.Attr and returns a safe copy.
// - If the key matches secretKeyPattern the value is replaced with Redacted.
// - Otherwise the string value is scanned for secret-shaped content.
// Group attrs are recursively redacted.
func RedactAttr(a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindGroup:
		attrs := a.Value.Group()
		cleaned := make([]slog.Attr, len(attrs))
		for i, sub := range attrs {
			cleaned[i] = RedactAttr(sub)
		}
		return slog.Group(a.Key, attrsToAny(cleaned)...)

	case slog.KindString:
		if secretKeyPattern.MatchString(a.Key) {
			return slog.String(a.Key, Redacted)
		}
		clean := RedactValue(a.Value.String())
		if clean != a.Value.String() {
			return slog.String(a.Key, clean)
		}

	case slog.KindAny:
		if secretKeyPattern.MatchString(a.Key) {
			return slog.Any(a.Key, Redacted)
		}
		// If the any value is a string-er, scrub it.
		if s, ok := a.Value.Any().(string); ok {
			clean := RedactValue(s)
			if clean != s {
				return slog.String(a.Key, clean)
			}
		}
	}
	return a
}

// RedactRecord returns a shallow copy of r with the message AND all attributes
// redacted. The message is scanned too (not just attrs): the "never put secrets
// in messages" convention was unenforced, and the engine's dominant
// progress-logging idiom formats raw Go errors with %v straight into the
// message — errors that routinely wrap subprocess (git/gh/gate) stdout+stderr,
// exactly the token/PEM-shaped content the redaction layer exists to catch.
// RedactValue is a no-op on clean strings, so the common case is unaffected.
func RedactRecord(r slog.Record) slog.Record {
	out := slog.NewRecord(r.Time, r.Level, RedactValue(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(RedactAttr(a))
		return true
	})
	return out
}

// IsSecretKey returns true if the attribute key name looks like it may carry a
// secret. Call this in tests to assert new fields are either safe or explicitly
// redacted before reaching a handler.
func IsSecretKey(key string) bool {
	return secretKeyPattern.MatchString(key)
}

// IsSecretValue returns true if v contains a recognised secret shape.
func IsSecretValue(v string) bool {
	if strings.Contains(v, "-----BEGIN ") {
		return true
	}
	for _, re := range secretValuePatterns {
		if re.MatchString(v) {
			return true
		}
	}
	return false
}

// attrsToAny converts []slog.Attr to []any for slog.Group.
func attrsToAny(attrs []slog.Attr) []any {
	out := make([]any, len(attrs))
	for i, a := range attrs {
		out[i] = a
	}
	return out
}
