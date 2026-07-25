// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package runtime

import (
	"strings"
	"testing"
)

func TestChildEnvPreservesTrustStoreWithoutCredentials(t *testing.T) {
	t.Setenv("NIX_SSL_CERT_FILE", "/nix/store/cacert/etc/ssl/certs/ca-bundle.crt")
	t.Setenv("SSL_CERT_DIR", "/etc/ssl/certs")
	t.Setenv("GH_TOKEN", "must-not-leak")

	env := ChildEnv(EnvSpec{})
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, want := range []string{
		"\nNIX_SSL_CERT_FILE=/nix/store/cacert/etc/ssl/certs/ca-bundle.crt\n",
		"\nSSL_CERT_DIR=/etc/ssl/certs\n",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("child env missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "\nGH_TOKEN=") {
		t.Fatalf("credential leaked while projecting trust store:\n%s", joined)
	}
}
