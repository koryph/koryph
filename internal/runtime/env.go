// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package runtime

import "github.com/koryph/koryph/internal/execx"

// EnvSpec describes the common safe environment envelope used by every
// runtime adapter.  The adapter supplies its account selector and credential
// names; this package owns the allowlist discipline so a new runtime cannot
// accidentally inherit the operator's ambient credentials.
type EnvSpec struct {
	AccountEnv       []string
	APIKeyEnvVar     string
	APIKey           string
	CredentialEnvVar string
	Credential       string
	SSHAuthSock      string
	Passthrough      []string
	Extra            []string
}

var childEnvAllow = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "COLORTERM",
	"TMPDIR", "TZ", "LANG", "GOPATH", "GOCACHE", "GOMODCACHE", "GOFLAGS",
	"GOTOOLCHAIN", "GOPROXY", "HOMEBREW_PREFIX", "HOMEBREW_CELLAR",
	"HOMEBREW_REPOSITORY",
	// Trust-store locations are non-secret build/runtime inputs. Nix shells in
	// particular require NIX_SSL_CERT_FILE; dropping it makes package/bootstrap
	// tools fail TLS verification inside an otherwise network-enabled phase.
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NIX_SSL_CERT_FILE",
	"REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
}

var childEnvPrefixes = []string{"LC_", "KORYPH_", "XDG_"}

// CertificateEnvNames returns the standard non-secret trust-store variables
// ChildEnv preserves. Runtime sandboxes use the same list to grant exact read
// access to existing absolute bundle paths.
func CertificateEnvNames() []string {
	return []string{
		"SSL_CERT_FILE", "SSL_CERT_DIR", "NIX_SSL_CERT_FILE",
		"REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
	}
}

// ChildEnv builds a complete, credential-minimal environment for an
// autonomous agent. CredentialEnvVar is authoritative over APIKeyEnvVar so a
// launch never receives two competing credentials.
func ChildEnv(spec EnvSpec) []string {
	allow := append([]string{}, childEnvAllow...)
	allow = append(allow, spec.Passthrough...)
	env := execx.AllowEnv(allow, childEnvPrefixes)
	env = append(env, spec.AccountEnv...)
	switch {
	case spec.CredentialEnvVar != "":
		env = append(env, spec.CredentialEnvVar+"="+spec.Credential)
	case spec.APIKeyEnvVar != "" && spec.APIKey != "":
		env = append(env, spec.APIKeyEnvVar+"="+spec.APIKey)
	}
	if spec.SSHAuthSock != "" {
		env = append(env, "SSH_AUTH_SOCK="+spec.SSHAuthSock)
	}
	return append(env, spec.Extra...)
}
