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
}

var childEnvPrefixes = []string{"LC_", "KORYPH_", "XDG_"}

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
