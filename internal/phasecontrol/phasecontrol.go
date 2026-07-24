// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package phasecontrol defines the narrow, file-based control protocol between
// a sandboxed worker phase and the orchestrator that owns privileged actions.
package phasecontrol

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/fsx"
)

const (
	SchemaVersion = 1

	OperationLabelAdd      = "label-add"
	OperationRuntimeCanary = "runtime-canary"

	ResponseApplied  = "applied"
	ResponseRejected = "rejected"
	ResponseFailed   = "failed"

	controlDirName  = "phase-control"
	requestDirName  = "requests"
	responseDirName = "responses"
	maxRecordBytes  = 64 << 10
)

var (
	idRE         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	capabilityRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	labelRE      = regexp.MustCompile(`^(area|fp|res):[a-z0-9][a-z0-9._:/-]{0,126}$`)
)

// Request is an untrusted worker-authored request. Operation-specific
// arguments are explicit fields so the protocol cannot become an arbitrary
// command or payload proxy.
type Request struct {
	SchemaVersion int    `json:"schema_version"`
	ID            string `json:"id"`
	PhaseID       string `json:"phase_id"`
	Operation     string `json:"operation"`
	Label         string `json:"label,omitempty"`
	Runtime       string `json:"runtime,omitempty"`
	CreatedAt     string `json:"created_at"`
}

// Response is the orchestrator-authored, sanitized result copied back to the
// worker's phase directory.
type Response struct {
	SchemaVersion int    `json:"schema_version"`
	ID            string `json:"id"`
	RequestDigest string `json:"request_digest"`
	State         string `json:"state"`
	Detail        string `json:"detail,omitempty"`
	CompletedAt   string `json:"completed_at"`
}

// Envelope retains a malformed record's filename identity so the engine can
// audit it without trusting its JSON body.
type Envelope struct {
	ID      string
	Request Request
	Err     error
}

// NewRequest constructs a request with a cryptographically random replay key.
func NewRequest(phaseID, operation string) (Request, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Request{}, fmt.Errorf("phase control: request id: %w", err)
	}
	return Request{
		SchemaVersion: SchemaVersion,
		ID:            hex.EncodeToString(raw[:]),
		PhaseID:       strings.TrimSpace(phaseID),
		Operation:     strings.TrimSpace(operation),
		CreatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	}, nil
}

// ValidateRequest validates the common envelope. The engine separately
// validates the operation arguments and owning slot.
func ValidateRequest(req Request) error {
	if req.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema version %d", req.SchemaVersion)
	}
	if !idRE.MatchString(req.ID) {
		return errors.New("invalid request id")
	}
	if strings.TrimSpace(req.PhaseID) == "" || strings.ContainsAny(req.PhaseID, `/\`) {
		return errors.New("invalid phase id")
	}
	switch req.Operation {
	case OperationLabelAdd, OperationRuntimeCanary:
	default:
		return fmt.Errorf("unsupported operation %q", req.Operation)
	}
	if _, err := time.Parse(time.RFC3339Nano, req.CreatedAt); err != nil {
		return errors.New("invalid creation time")
	}
	return nil
}

// ValidateSchedulingLabel accepts only declarative scheduling metadata. It
// intentionally excludes routing/control labels such as model:* and runtime:*.
func ValidateSchedulingLabel(label string) error {
	if len(label) > 130 || !labelRE.MatchString(label) {
		return errors.New("label must be a canonical area:*, fp:*, or res:* label")
	}
	return nil
}

// ValidateCapability accepts the stable token written to a structured block.
func ValidateCapability(capability string) error {
	if !capabilityRE.MatchString(capability) {
		return errors.New("capability must be a lowercase hyphenated token")
	}
	return nil
}

// Digest returns the canonical digest binding a response to one exact request.
func Digest(req Request) (string, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Submit atomically publishes req. Reusing an ID with identical content is
// idempotent; reusing it with different content is rejected.
func Submit(phaseDir string, req Request) error {
	if err := ValidateRequest(req); err != nil {
		return err
	}
	dir, err := safeProtocolDir(phaseDir, requestDirName, true)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, req.ID+".json")
	var existing Request
	if err := readRegularJSON(path, &existing); err == nil {
		oldDigest, oldErr := Digest(existing)
		newDigest, newErr := Digest(req)
		if oldErr == nil && newErr == nil && oldDigest == newDigest {
			return nil
		}
		return errors.New("request id already exists with different content")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fsx.WriteJSONAtomic(path, req)
}

// ListRequests returns request records in stable filename order.
func ListRequests(phaseDir string) ([]Envelope, error) {
	dir, err := safeProtocolDir(phaseDir, requestDirName, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	out := make([]Envelope, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		env := Envelope{ID: id}
		if !idRE.MatchString(id) {
			env.Err = errors.New("invalid request filename")
			out = append(out, env)
			continue
		}
		if err := readRegularJSON(filepath.Join(dir, entry.Name()), &env.Request); err != nil {
			env.Err = err
		} else if env.Request.ID != id {
			env.Err = errors.New("request id does not match filename")
		} else {
			env.Err = ValidateRequest(env.Request)
		}
		out = append(out, env)
	}
	return out, nil
}

// PublishResponse writes a sanitized response into the worker-visible phase
// directory. The authoritative engine journal uses WriteResponseDir directly.
func PublishResponse(phaseDir string, resp Response) error {
	dir, err := safeProtocolDir(phaseDir, responseDirName, true)
	if err != nil {
		return err
	}
	return WriteResponseDir(dir, resp, 0o644)
}

// WriteResponseDir writes resp to an already-selected response/journal
// directory. The caller controls the file mode because engine journals are
// private while worker-visible responses are not.
func WriteResponseDir(dir string, resp Response, perm os.FileMode) error {
	if !idRE.MatchString(resp.ID) {
		return errors.New("invalid response id")
	}
	if resp.SchemaVersion != SchemaVersion {
		return errors.New("invalid response schema")
	}
	if resp.State != ResponseApplied && resp.State != ResponseRejected && resp.State != ResponseFailed {
		return errors.New("invalid response state")
	}
	if err := ensurePlainDir(dir); err != nil {
		return err
	}
	return fsx.WriteJSONAtomicPerm(filepath.Join(dir, resp.ID+".json"), resp, perm)
}

// ReadResponseDir reads one response from dir.
func ReadResponseDir(dir, id string) (Response, error) {
	if !idRE.MatchString(id) {
		return Response{}, errors.New("invalid response id")
	}
	var resp Response
	err := readRegularJSON(filepath.Join(dir, id+".json"), &resp)
	return resp, err
}

// WaitResponse waits for the worker-visible response matching req and rejects
// stale or forged responses whose digest does not bind to the submitted body.
func WaitResponse(ctx context.Context, phaseDir string, req Request) (Response, error) {
	digest, err := Digest(req)
	if err != nil {
		return Response{}, err
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		dir, dirErr := safeProtocolDir(phaseDir, responseDirName, false)
		if dirErr == nil {
			resp, readErr := ReadResponseDir(dir, req.ID)
			if readErr == nil {
				if resp.RequestDigest != digest {
					return Response{}, errors.New("response digest does not match request")
				}
				return resp, nil
			}
			if !errors.Is(readErr, os.ErrNotExist) {
				return Response{}, readErr
			}
		} else if !errors.Is(dirErr, os.ErrNotExist) {
			return Response{}, dirErr
		}
		select {
		case <-ctx.Done():
			return Response{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// SanitizeDetail normalizes worker-visible and telemetry detail. It is not a
// credential detector; callers must still avoid passing provider output.
func SanitizeDetail(detail string) string {
	detail = strings.Join(strings.Fields(detail), " ")
	if len(detail) > 512 {
		detail = detail[:512]
	}
	return detail
}

func safeProtocolDir(phaseDir, leaf string, create bool) (string, error) {
	if strings.TrimSpace(phaseDir) == "" {
		return "", errors.New("phase directory is required")
	}
	control := filepath.Join(phaseDir, controlDirName)
	dir := filepath.Join(control, leaf)
	if create {
		if err := ensurePlainDir(control); err != nil {
			return "", err
		}
		if err := ensurePlainDir(dir); err != nil {
			return "", err
		}
		return dir, nil
	}
	for _, path := range []string{control, dir} {
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("phase control path is not a plain directory")
		}
	}
	return dir, nil
}

func ensurePlainDir(dir string) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		info, err = os.Lstat(dir)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("phase control path is not a plain directory")
	}
	return nil
}

func readRegularJSON(path string, dst any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxRecordBytes {
		return errors.New("phase control record is not a bounded regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("invalid phase control JSON: %w", err)
	}
	return nil
}
