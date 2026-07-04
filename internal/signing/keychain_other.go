// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

//go:build !darwin

package signing

import "fmt"

// FetchKeychain is a non-darwin stub that always returns an error. The
// keychain provider is only valid on darwin; Config.Validate() rejects it on
// other platforms at setup time.
func FetchKeychain(_ string) ([]byte, error) {
	return nil, fmt.Errorf("signing: provider keychain is only supported on macOS (darwin)")
}

// StoreKeychain is a non-darwin stub.
func StoreKeychain(_ string, _ []byte) error {
	return fmt.Errorf("signing: provider keychain is only supported on macOS (darwin)")
}
