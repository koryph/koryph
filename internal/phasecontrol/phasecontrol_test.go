// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package phasecontrol

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testRequest(t *testing.T) Request {
	t.Helper()
	req, err := NewRequest("bead-1", OperationLabelAdd)
	if err != nil {
		t.Fatal(err)
	}
	req.Label = "area:docs"
	return req
}

func TestSubmitListAndReplay(t *testing.T) {
	dir := t.TempDir()
	req := testRequest(t)
	if err := Submit(dir, req); err != nil {
		t.Fatal(err)
	}
	if err := Submit(dir, req); err != nil {
		t.Fatalf("identical replay: %v", err)
	}
	got, err := ListRequests(dir)
	if err != nil || len(got) != 1 || got[0].Err != nil || got[0].Request.Label != req.Label {
		t.Fatalf("requests = %+v, err=%v", got, err)
	}
	changed := req
	changed.Label = "area:engine"
	if err := Submit(dir, changed); err == nil {
		t.Fatal("changed replay unexpectedly accepted")
	}
}

func TestSchedulingLabelAllowlist(t *testing.T) {
	for _, label := range []string{"area:docs", "fp:go:engine", "fp:read:docs-nav", "res:kind-cluster"} {
		if err := ValidateSchedulingLabel(label); err != nil {
			t.Errorf("%s: %v", label, err)
		}
	}
	for _, label := range []string{"model:frontier", "runtime:claude", "no-dispatch", "refactor-core", "area:", "area:Docs"} {
		if err := ValidateSchedulingLabel(label); err == nil {
			t.Errorf("%s unexpectedly accepted", label)
		}
	}
}

func TestWaitResponseChecksDigest(t *testing.T) {
	dir := t.TempDir()
	req := testRequest(t)
	if err := Submit(dir, req); err != nil {
		t.Fatal(err)
	}
	digest, _ := Digest(req)
	resp := Response{
		SchemaVersion: SchemaVersion,
		ID:            req.ID,
		RequestDigest: digest,
		State:         ResponseApplied,
		CompletedAt:   time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := PublishResponse(dir, resp); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := WaitResponse(ctx, dir, req)
	if err != nil || got.State != ResponseApplied {
		t.Fatalf("response = %+v, err=%v", got, err)
	}
}

func TestRejectsSymlinkedControlDirectory(t *testing.T) {
	dir := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(dir, controlDirName)); err != nil {
		t.Fatal(err)
	}
	if err := Submit(dir, testRequest(t)); err == nil {
		t.Fatal("symlinked control directory unexpectedly accepted")
	}
}

func TestWriteCapabilityBlockPreservesHeartbeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(path, []byte(`{"state":"running","step":"test","pct":50}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteCapabilityBlock(path, "beads-metadata", " needs operator \n help "); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := readRegularJSON(path, &got); err != nil {
		t.Fatal(err)
	}
	if got["state"] != "blocked" || got["block_kind"] != "capability" ||
		got["capability"] != "beads-metadata" || got["step"] != "test" {
		t.Fatalf("status = %#v", got)
	}
}
