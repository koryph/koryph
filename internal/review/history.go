// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package review

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const ReviewArtifactSchema = "koryph.review-artifact/v1"

const (
	ReviewKindGeneral  = "general"
	ReviewKindSecurity = "security"
)

// HistoricalFinding is one immutable blocker identity accumulated across
// review passes. ID is a compact spelling of Digest; Digest authenticates the
// normalized file/line/summary tuple. Severity is deliberately outside the
// identity so changing a finding from major to blocking does not make the same
// unresolved defect look new.
type HistoricalFinding struct {
	ID                    string `json:"id"`
	Digest                string `json:"digest"`
	Severity              string `json:"severity"`
	File                  string `json:"file,omitempty"`
	Line                  int    `json:"line,omitempty"`
	Summary               string `json:"summary"`
	FirstSeenCandidateSHA string `json:"first_seen_candidate_sha,omitempty"`
}

// Artifact is the durable result of one review. History is cumulative: every
// blocking or major finding ever observed in this lane remains present even
// after a later pass resolves it. PriorArtifactDigests makes the exact input
// chain auditable without making path spellings part of the content identity.
type Artifact struct {
	Schema                string              `json:"schema"`
	Kind                  string              `json:"kind"`
	CandidateSHA          string              `json:"candidate_sha,omitempty"`
	BaseSHA               string              `json:"base_sha,omitempty"`
	PriorArtifactDigests  []string            `json:"prior_artifact_digests,omitempty"`
	HistoryDigest         string              `json:"history_digest"`
	History               []HistoricalFinding `json:"history,omitempty"`
	CurrentBlockerDigests []string            `json:"current_blocker_digests,omitempty"`
	Verdict               Verdict             `json:"verdict"`
}

func reviewKind(security bool) string {
	if security {
		return ReviewKindSecurity
	}
	return ReviewKindGeneral
}

func findingIdentity(f Finding) (id, digest string) {
	canonical := struct {
		File    string `json:"file"`
		Line    int    `json:"line"`
		Summary string `json:"summary"`
	}{
		File:    filepath.ToSlash(strings.TrimSpace(f.File)),
		Line:    f.Line,
		Summary: strings.Join(strings.Fields(f.Summary), " "),
	}
	raw, _ := json.Marshal(canonical)
	sum := sha256.Sum256(raw)
	hexDigest := hex.EncodeToString(sum[:])
	return "PF-" + hexDigest[:16], "sha256:" + hexDigest
}

func historicalFinding(f Finding, candidateSHA string) HistoricalFinding {
	id, digest := findingIdentity(f)
	return HistoricalFinding{
		ID: id, Digest: digest,
		Severity:              strings.ToLower(strings.TrimSpace(f.Severity)),
		File:                  filepath.ToSlash(strings.TrimSpace(f.File)),
		Line:                  f.Line,
		Summary:               strings.TrimSpace(f.Summary),
		FirstSeenCandidateSHA: strings.TrimSpace(candidateSHA),
	}
}

func historicalAsFinding(f HistoricalFinding) Finding {
	return Finding{Severity: f.Severity, File: f.File, Line: f.Line, Summary: f.Summary}
}

func blockingSeverity(severity string) bool {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "blocking", "major":
		return true
	default:
		return false
	}
}

func mergeHistory(base []HistoricalFinding, additions []HistoricalFinding) ([]HistoricalFinding, error) {
	byID := make(map[string]HistoricalFinding, len(base)+len(additions))
	for _, finding := range append(append([]HistoricalFinding(nil), base...), additions...) {
		if err := validateHistoricalFinding(finding); err != nil {
			return nil, err
		}
		if previous, ok := byID[finding.ID]; ok {
			if previous.Digest != finding.Digest {
				return nil, fmt.Errorf("prior finding ID %s has conflicting digests", finding.ID)
			}
			if previous.FirstSeenCandidateSHA != "" {
				finding.FirstSeenCandidateSHA = previous.FirstSeenCandidateSHA
			}
		}
		byID[finding.ID] = finding
	}
	out := make([]HistoricalFinding, 0, len(byID))
	for _, finding := range byID {
		out = append(out, finding)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func validateHistoricalFinding(f HistoricalFinding) error {
	if !blockingSeverity(f.Severity) {
		return fmt.Errorf("prior finding %q has non-blocking severity %q", f.ID, f.Severity)
	}
	if strings.TrimSpace(f.Summary) == "" {
		return fmt.Errorf("prior finding %q has no summary", f.ID)
	}
	wantID, wantDigest := findingIdentity(historicalAsFinding(f))
	if f.ID != wantID || f.Digest != wantDigest {
		return fmt.Errorf("prior finding %q identity or digest does not match its content", f.ID)
	}
	return nil
}

func historyDigest(history []HistoricalFinding) (string, error) {
	raw, err := json.Marshal(history)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func contentDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func priorPaths(o Opts) ([]string, error) {
	paths := append([]string(nil), o.PriorVerdictPaths...)
	if strings.TrimSpace(o.PriorVerdictPath) != "" {
		paths = append(paths, o.PriorVerdictPath)
	}
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, raw := range paths {
		path := filepath.Clean(strings.TrimSpace(raw))
		if path == "." || path == "" {
			return nil, errors.New("prior review artifact path is empty")
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, path)
	}
	return out, nil
}

func loadPriorBlockingFindings(o Opts) ([]priorBlockingFinding, []string, error) {
	paths, err := priorPaths(o)
	if err != nil {
		return nil, nil, err
	}
	var history []HistoricalFinding
	var artifactDigests []string
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", path, err)
		}
		artifactDigests = append(artifactDigests, contentDigest(raw))
		loaded, err := decodePriorArtifact(raw, reviewKind(o.Security))
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", path, err)
		}
		history, err = mergeHistory(history, loaded)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	sort.Strings(artifactDigests)
	artifactDigests = compactStrings(artifactDigests)
	out := make([]priorBlockingFinding, 0, len(history))
	for _, finding := range history {
		out = append(out, priorBlockingFinding{
			ID: finding.ID, Finding: historicalAsFinding(finding), History: finding,
		})
	}
	return out, artifactDigests, nil
}

func compactStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, value := range in[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func decodePriorArtifact(raw []byte, wantKind string) ([]HistoricalFinding, error) {
	var header struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return nil, err
	}
	if header.Schema == "" {
		// Controlled migration for pre-v1 verdicts. Once read, their blockers
		// receive stable digest IDs and are carried in every new v1 artifact.
		// A legacy verdict always has an explicit top-level blocking field.
		// Requiring it prevents deleting an artifact's schema from silently
		// turning its nested verdict into an empty legacy verdict and thereby
		// erasing cumulative history.
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
		if _, ok := fields["blocking"]; !ok {
			return nil, errors.New("review input is neither a versioned artifact nor a legacy verdict")
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		var verdict Verdict
		if err := dec.Decode(&verdict); err != nil {
			return nil, err
		}
		var trailing any
		if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
			if err == nil {
				return nil, errors.New("legacy review verdict contains trailing JSON")
			}
			return nil, err
		}
		var history []HistoricalFinding
		for _, finding := range verdict.Findings {
			if blockingSeverity(finding.Severity) {
				history = append(history, historicalFinding(finding, ""))
			}
		}
		return mergeHistory(nil, history)
	}
	if header.Schema != ReviewArtifactSchema {
		return nil, fmt.Errorf("unsupported review artifact schema %q", header.Schema)
	}
	artifact, err := ParseArtifact(raw, wantKind)
	if err != nil {
		return nil, err
	}
	return artifact.History, nil
}

// ParseArtifact strictly decodes and validates one versioned review artifact.
// Engine callers still authenticate the containing bytes against the trusted
// ledger digest and bind CandidateSHA/BaseSHA to their expected generation.
func ParseArtifact(raw []byte, wantKind string) (Artifact, error) {
	if wantKind != ReviewKindGeneral && wantKind != ReviewKindSecurity {
		return Artifact{}, fmt.Errorf("unsupported review kind %q", wantKind)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var artifact Artifact
	if err := dec.Decode(&artifact); err != nil {
		return Artifact{}, err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Artifact{}, errors.New("review artifact contains trailing JSON")
		}
		return Artifact{}, err
	}
	if artifact.Schema != ReviewArtifactSchema {
		return Artifact{}, fmt.Errorf("unsupported review artifact schema %q", artifact.Schema)
	}
	if artifact.Kind != wantKind {
		return Artifact{}, fmt.Errorf("review artifact kind %q does not match %q lane", artifact.Kind, wantKind)
	}
	if err := validateVerdictValue(artifact.Verdict); err != nil {
		return Artifact{}, fmt.Errorf("review artifact verdict: %w", err)
	}
	history, err := mergeHistory(nil, artifact.History)
	if err != nil {
		return Artifact{}, err
	}
	digest, err := historyDigest(history)
	if err != nil {
		return Artifact{}, err
	}
	if artifact.HistoryDigest != digest {
		return Artifact{}, errors.New("review artifact history digest does not match its cumulative findings")
	}
	current := make(map[string]bool, len(artifact.CurrentBlockerDigests))
	for _, digest := range artifact.CurrentBlockerDigests {
		if current[digest] {
			return Artifact{}, fmt.Errorf("review artifact duplicates current blocker digest %s", digest)
		}
		current[digest] = true
		found := false
		for _, item := range history {
			if item.Digest == digest {
				found = true
				break
			}
		}
		if !found {
			return Artifact{}, fmt.Errorf("review artifact current blocker %s is absent from cumulative history", digest)
		}
	}
	artifact.History = history
	return artifact, nil
}

func buildArtifact(
	o Opts,
	v Verdict,
	prior []priorBlockingFinding,
	priorDigests []string,
) (Artifact, []byte, string, error) {
	history := make([]HistoricalFinding, 0, len(prior)+len(v.Findings))
	var currentDigests []string
	for _, finding := range prior {
		history = append(history, finding.History)
	}
	for _, finding := range v.Findings {
		if blockingSeverity(finding.Severity) && finding.TrackHistory {
			item := historicalFinding(finding, o.CandidateSHA)
			history = append(history, item)
			currentDigests = append(currentDigests, item.Digest)
		}
	}
	sort.Strings(currentDigests)
	history, err := mergeHistory(nil, history)
	if err != nil {
		return Artifact{}, nil, "", err
	}
	historyKey, err := historyDigest(history)
	if err != nil {
		return Artifact{}, nil, "", err
	}
	artifact := Artifact{
		Schema: ReviewArtifactSchema, Kind: reviewKind(o.Security),
		CandidateSHA:          strings.TrimSpace(o.CandidateSHA),
		BaseSHA:               strings.TrimSpace(o.BaseSHA),
		PriorArtifactDigests:  append([]string(nil), priorDigests...),
		HistoryDigest:         historyKey,
		History:               history,
		CurrentBlockerDigests: currentDigests,
		Verdict:               v,
	}
	raw, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return Artifact{}, nil, "", err
	}
	raw = append(raw, '\n')
	return artifact, raw, contentDigest(raw), nil
}

func artifactPath(o Opts, digest string) (string, error) {
	if o.ArtifactDir != "" && o.OutPath != "" {
		return "", errors.New("review artifact requires exactly one of ArtifactDir or OutPath")
	}
	if o.OutPath != "" {
		return filepath.Clean(o.OutPath), nil
	}
	if o.ArtifactDir == "" {
		return "", nil
	}
	candidate := strings.TrimSpace(o.CandidateSHA)
	if len(candidate) > 12 {
		candidate = candidate[:12]
	}
	if candidate == "" {
		candidate = "unpinned"
	}
	digestToken := strings.TrimPrefix(digest, "sha256:")
	if len(digestToken) > 16 {
		digestToken = digestToken[:16]
	}
	return filepath.Join(o.ArtifactDir,
		fmt.Sprintf("%s-review-%s-%s.json", reviewKind(o.Security), candidate, digestToken)), nil
}

func persistArtifact(o Opts, v *Verdict, prior []priorBlockingFinding, priorDigests []string) error {
	artifact, raw, digest, err := buildArtifact(o, *v, prior, priorDigests)
	if err != nil {
		return err
	}
	path, err := artifactPath(o, digest)
	if err != nil {
		return err
	}
	v.HistoryDigest = artifact.HistoryDigest
	v.BlockingHistory = append([]HistoricalFinding(nil), artifact.History...)
	v.ArtifactDigest = digest
	v.ArtifactPath = path
	if path == "" {
		return nil
	}
	return writeImmutable(path, raw, 0o644)
}

// writeImmutable atomically creates path without ever replacing existing
// content. Linking a fully synced sibling temp file gives create-if-absent
// semantics; a byte-identical replay is idempotent.
func writeImmutable(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".review-artifact-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmpName, path); err != nil {
		if !os.IsExist(err) {
			return err
		}
		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(existing, data) {
			return fmt.Errorf("immutable review artifact %s already exists with different content", path)
		}
		return nil
	}
	if directory, err := os.Open(dir); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func relatedArtifactPath(verdictPath, suffix string) string {
	ext := filepath.Ext(verdictPath)
	base := strings.TrimSuffix(verdictPath, ext)
	if ext == "" {
		ext = ".json"
	}
	return base + "-" + suffix + ext
}
