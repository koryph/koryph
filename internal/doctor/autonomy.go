// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/fsx"
	loopsupervisor "github.com/koryph/koryph/internal/loop"
	"github.com/koryph/koryph/internal/metrics"
	"github.com/koryph/koryph/internal/strictjson"
)

const checkNameAutonomyCanary = "autonomy-canary"

// AutonomyReportPath is the fixed release-canary report location beneath a
// project root.
func AutonomyReportPath(repoRoot string) string {
	return filepath.Join(repoRoot, filepath.FromSlash(metrics.DefaultAutonomyReportRelativePath))
}

// CheckCanaryGenerationArchives authenticates create-once rollover manifests
// and reports the current versus retained generation count. The bool is false
// when the project has no archived generations.
func CheckCanaryGenerationArchives(repoRoot, currentGeneration string) (Finding, bool) {
	loopRoot := loopsupervisor.NewStore(repoRoot).Root
	files, err := filepath.Glob(filepath.Join(loopRoot, "canary-history", "*.json"))
	if err != nil || len(files) == 0 {
		return Finding{}, false
	}
	for _, path := range files {
		read, readErr := fsx.ReadRegularConfined(path, 16<<20, loopRoot)
		if readErr != nil {
			return Finding{
				Check: checkNameAutonomyCanary, Level: LevelError,
				Message: "autonomy canary generation archive is unreadable: " + readErr.Error(),
			}, true
		}
		var archive loopsupervisor.CanaryGenerationArchive
		if decodeErr := strictjson.Decode(read.Data, &archive); decodeErr != nil ||
			archive.SchemaVersion < 2 ||
			archive.SchemaVersion > loopsupervisor.CanaryGenerationArchiveVersion ||
			archive.ProjectID == "" ||
			archive.PreviousGenerationDigest == "" ||
			archive.CurrentGenerationDigest == "" ||
			!validCanaryPolicySupersession(archive.PolicySupersession) {
			return Finding{
				Check: checkNameAutonomyCanary, Level: LevelError,
				Message: "autonomy canary generation archive manifest is invalid: " + path,
			}, true
		}
		for _, artifact := range []struct {
			path, digest string
		}{
			{archive.ArchivedStatePath, archive.ArchivedStateDigest},
			{archive.ArchivedReportPath, archive.ArchivedReportDigest},
		} {
			artifactRead, artifactErr := fsx.ReadRegularConfined(
				artifact.path, 64<<20, repoRoot,
			)
			if artifactErr != nil || "sha256:"+artifactRead.Digest != artifact.digest {
				return Finding{
					Check: checkNameAutonomyCanary, Level: LevelError,
					Message: "autonomy canary generation archive artifact is invalid: " + artifact.path,
				}, true
			}
		}
	}
	current := strings.TrimPrefix(currentGeneration, "sha256:")
	if len(current) > 12 {
		current = current[:12]
	}
	if current == "" {
		current = "none"
	}
	return Finding{
		Check: checkNameAutonomyCanary, Level: LevelOK,
		Message: fmt.Sprintf(
			"autonomy canary generations: current %s, archived %d",
			current, len(files),
		),
	}, true
}

func validCanaryPolicySupersession(
	supersession *loopsupervisor.CanaryPolicySupersession,
) bool {
	if supersession == nil {
		return true
	}
	return supersession.OperatorRequested &&
		supersession.PreviousContractDigest != "" &&
		supersession.CurrentContractDigest != "" &&
		supersession.PreviousAutonomyPolicyDigest != "" &&
		supersession.CurrentAutonomyPolicyDigest != "" &&
		supersession.PreviousExecutionPolicyDigest != "" &&
		supersession.CurrentExecutionPolicyDigest != ""
}

// CheckAutonomyReport fails closed when the caller omits the one complete live
// release expectation, or when report evidence is absent, foreign, stale,
// invalid, unsafe, or below threshold. The variadic form preserves source
// compatibility while ensuring a generic self-consistent report cannot pass a
// release check.
func CheckAutonomyReport(path string, expected ...metrics.AutonomyReportExpectation) Finding {
	if len(expected) != 1 {
		return Finding{
			Check: checkNameAutonomyCanary, Level: LevelError,
			Message: "autonomy canary live release expectation is missing",
		}
	}
	report, err := metrics.LoadAutonomyReportExpected(path, expected[0])
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			return Finding{
				Check: checkNameAutonomyCanary, Level: LevelError,
				Message: "autonomy canary report is missing: " + path,
			}
		default:
			return Finding{
				Check: checkNameAutonomyCanary, Level: LevelError,
				Message: "autonomy canary report is invalid: " + err.Error(),
			}
		}
	}
	if !report.Decision.Passed {
		var failed []string
		for _, check := range report.Decision.Checks {
			if !check.Passed {
				failed = append(failed, check.Name)
			}
		}
		detail := append([]string(nil), report.Decision.SafetyViolations...)
		for _, name := range failed {
			detail = append(detail, "slo:"+name)
		}
		return Finding{
			Check: checkNameAutonomyCanary, Level: LevelError,
			Message: fmt.Sprintf(
				"autonomy canary failed for %s (%d eligible/%d cohort): %s",
				report.ProjectID, report.Metrics.EligibleBeads, report.Metrics.CohortBeads,
				strings.Join(detail, ", "),
			),
		}
	}
	return Finding{
		Check: checkNameAutonomyCanary, Level: LevelOK,
		Message: fmt.Sprintf(
			"autonomy canary passed for %s: completion %.1f%%, first review %.1f%%, retries %.1f%%, median %dms, p95 %dms",
			report.ProjectID,
			report.Metrics.AutonomousCompletion.Rate*100,
			report.Metrics.FirstReviewPass.Rate*100,
			report.Metrics.RetryDispatches.Rate*100,
			report.Metrics.DispatchToTerminal.MedianMS,
			report.Metrics.DispatchToTerminal.P95MS,
		),
	}
}
