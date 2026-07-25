// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package loop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadStateRejectsSymlinkDuplicateAndUnknownIdentity(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		store := NewStore(root)
		if err := store.SaveState(State{ProjectID: "demo"}); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(root, "outside.json")
		raw, err := os.ReadFile(store.StatePath())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(outside, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(store.StatePath()); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, store.StatePath()); err != nil {
			t.Fatal(err)
		}
		if _, err := store.LoadState(); err == nil ||
			!strings.Contains(err.Error(), "symlink") {
			t.Fatalf("LoadState symlink error = %v", err)
		}
	})

	for _, test := range []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "duplicate",
			raw:  `{"schema_version":2,"project_id":"demo","project_id":"foreign","mode":"stopped","updated_at":"2026-07-25T12:00:00Z"}`,
			want: "duplicate JSON object key",
		},
		{
			name: "unknown",
			raw:  `{"schema_version":2,"project_id":"demo","mode":"stopped","updated_at":"2026-07-25T12:00:00Z","foreign_identity":"accepted"}`,
			want: "unknown field",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := NewStore(t.TempDir())
			if err := os.MkdirAll(store.Root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(store.StatePath(), []byte(test.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.LoadState(); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadState error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadControlRejectsSymlinkAndUnknownFields(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		store := NewStore(root)
		if err := os.MkdirAll(store.Root, 0o700); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(root, "control.json")
		if err := os.WriteFile(outside, []byte(`{"schema_version":2,"inject":["b1"]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, store.ControlPath()); err != nil {
			t.Fatal(err)
		}
		if _, err := store.LoadControl(); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("LoadControl symlink error = %v", err)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		store := NewStore(t.TempDir())
		if err := os.MkdirAll(store.Root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			store.ControlPath(),
			[]byte(`{"schema_version":2,"operator_bypass":true}`),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := store.LoadControl(); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("LoadControl unknown-field error = %v", err)
		}
	})
}
