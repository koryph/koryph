#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (c) 2026 The Koryph Developers

# Deterministic release acceptance for Koryph's production execution kernel.
# Every scenario names its exact test functions. The list probe fails closed
# when a test is renamed or removed, avoiding Go's successful "[no tests to
# run]" behavior. GOPROXY=off and GOTOOLCHAIN=local keep this boundary
# independent of network availability.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
log_dir="${1:-${repo_root}/.git/koryph-production-acceptance}"
mkdir -p "${log_dir}"

export GOPROXY=off
export GOTOOLCHAIN=local

fail_case() {
  local name="$1"
  local log="$2"
  printf '==> %s: FAIL\n' "${name}"
  tail -40 "${log}" || true
  printf 'full output: %s\n' "${log}"
  exit 1
}

run_case() {
  local name="$1"
  shift
  local log="${log_dir}/${name}.log"
  : >"${log}"

  local spec package test_name listed
  for spec in "$@"; do
    package="${spec%%:*}"
    test_name="${spec#*:}"
    printf '$ go test %s -run ^%s$ -count=1\n' "${package}" "${test_name}" >>"${log}"
    if ! listed="$(cd "${repo_root}" && go test "${package}" -list "^${test_name}$" 2>>"${log}")"; then
      fail_case "${name}" "${log}"
    fi
    if ! grep -qx "${test_name}" <<<"${listed}"; then
      printf 'required test %s was not discovered in %s\n' "${test_name}" "${package}" >>"${log}"
      fail_case "${name}" "${log}"
    fi
    if ! (cd "${repo_root}" && go test "${package}" -run "^${test_name}$" -count=1) >>"${log}" 2>&1; then
      fail_case "${name}" "${log}"
    fi
  done
  printf '==> %s: PASS\n' "${name}"
}

run_case scheduling \
  "./internal/beads:TestReady" \
  "./internal/sched:TestBuildWaveComprehensive" \
  "./internal/engine:TestMalformedAcceptanceParksBeforeModelDispatch"

run_case isolation \
  "./internal/engine:TestRollingInFlightFootprintHeldAcrossIterations" \
  "./internal/sched:TestBuildWaveResourceCapacityDefersSecondHolder"

run_case dispatch \
  "./internal/dispatch:TestDispatchInjectedRuntimeUsedForCommand" \
  "./internal/dispatch:TestDispatchLaunchesDetachedAgent" \
  "./internal/dispatch:TestControlPlaneBoundarySuppressesBeadsHooksAndCommands"

run_case completion-identity \
  "./internal/engine:TestCandidateEligibleAcceptsOnlyMatchingTerminalResult" \
  "./internal/engine:TestCandidateRejectsResultAfterCandidateSHAChanges"

run_case gate-before-review \
  "./internal/engine:TestProductionCandidateEntersGateBeforeReviewAndLanding"

run_case evidence-retry \
  "./internal/engine:TestGateFailureConfirmsThenDispatchesScopedRepairWithExactEvidence" \
  "./internal/engine:TestRepeatedUnrelatedGateFailureParksWithoutModelOrScopeExpansion" \
  "./internal/engine:TestDecideRetryTypedTransitionTable"

run_case slot-local-failure \
  "./internal/engine:TestCanonicalCanaryMissingTerminalExhaustionParksOnlyItsSlot" \
  "./internal/engine:TestDuplicateBroadCommandAlwaysBlocksCandidateAndOnlyExplicitlyTripsCircuit"

run_case safety-containment \
  "./internal/engine:TestNativeCanaryContainmentReapsTerminalizesAndReleases"

run_case merge-close \
  "./internal/engine:TestRunOnceMergesAndDrains"

run_case recovery-idle \
  "./internal/engine:TestResumeReplaysDurableNativeCanaryContainment" \
  "./internal/loop:TestIdleCreatesNoRunDirectoryAndNeverCallsEngine"

printf 'production acceptance: PASS (10/10 scenarios)\n'
printf 'full output: %s\n' "${log_dir}"
