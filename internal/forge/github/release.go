// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package github

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// githubReleaseSvc implements [forge.ReleaseService] for GitHub using the
// gh CLI. GitHub supports draft releases, so the normal flow is:
//
//  1. CreateDraft — gh release create --draft
//  2. UploadAsset — gh release upload (repeat per artifact)
//  3. Publish     — gh release edit --draft=false
//
// This preserves the draft-until-complete invariant: the release is not
// publicly visible until all assets are attached and Publish is called.
//
// The gh binary path can be overridden via the KORYPH_GH_BIN environment
// variable (same convention as the rest of the koryph codebase).
type githubReleaseSvc struct{}

func (s *githubReleaseSvc) ghBin() string {
	if v := os.Getenv("KORYPH_GH_BIN"); v != "" {
		return v
	}
	return "gh"
}

// Create publishes a release for the given tag immediately (not a draft).
// Prefer CreateDraft + UploadAsset(s) + Publish for artifact-bearing releases
// to preserve the draft-until-complete invariant.
func (s *githubReleaseSvc) Create(_ context.Context, owner, repo, tag, body string) (string, error) {
	ownerRepo := owner + "/" + repo
	args := []string{
		"release", "create", tag,
		"--repo", ownerRepo,
		"--notes", body,
	}
	out, err := exec.Command(s.ghBin(), args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("gh release create %s %s: %w\n%s",
			ownerRepo, tag, err, strings.TrimSpace(string(out)))
	}
	// gh release create prints the release URL on stdout; use tag as the stable ID.
	return tag, nil
}

// CreateDraft creates a draft (not yet publicly visible) release.
// The returned releaseID is the git tag string, which gh release subcommands
// accept as a stable identifier across create/upload/edit.
func (s *githubReleaseSvc) CreateDraft(_ context.Context, owner, repo, tag, body string) (string, error) {
	ownerRepo := owner + "/" + repo
	args := []string{
		"release", "create", tag,
		"--repo", ownerRepo,
		"--draft",
		"--notes", body,
	}
	out, err := exec.Command(s.ghBin(), args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("gh release create --draft %s %s: %w\n%s",
			ownerRepo, tag, err, strings.TrimSpace(string(out)))
	}
	return tag, nil
}

// UploadAsset attaches a named asset to an existing release. The releaseID is
// the tag returned by [CreateDraft] or [Create].
func (s *githubReleaseSvc) UploadAsset(_ context.Context, owner, repo, releaseID, filename string, r io.Reader) error {
	ownerRepo := owner + "/" + repo

	// gh release upload reads from a file path, not from an io.Reader.
	// Write the content to a temporary file then upload it.
	tmp, err := os.CreateTemp("", "koryph-release-asset-*-"+filename)
	if err != nil {
		return fmt.Errorf("gh release upload: create temp file for %s: %w", filename, err)
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	if _, err := io.Copy(tmp, r); err != nil {
		return fmt.Errorf("gh release upload: write temp file for %s: %w", filename, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("gh release upload: close temp file for %s: %w", filename, err)
	}

	args := []string{
		"release", "upload", releaseID,
		tmp.Name() + "#" + filename, // gh uses PATH#LABEL to set display name
		"--repo", ownerRepo,
		"--clobber", // allow re-upload if asset already exists
	}
	out, err := exec.Command(s.ghBin(), args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return fmt.Errorf("gh release upload %s %s: %w\n%s",
			ownerRepo, filename, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Publish makes a draft release publicly visible by removing the draft flag.
// The releaseID is the tag string returned by [CreateDraft].
func (s *githubReleaseSvc) Publish(_ context.Context, owner, repo, releaseID string) error {
	ownerRepo := owner + "/" + repo
	args := []string{
		"release", "edit", releaseID,
		"--repo", ownerRepo,
		"--draft=false",
	}
	out, err := exec.Command(s.ghBin(), args...).CombinedOutput() //nolint:gosec
	if err != nil {
		return fmt.Errorf("gh release edit --draft=false %s %s: %w\n%s",
			ownerRepo, releaseID, err, strings.TrimSpace(string(out)))
	}
	return nil
}
