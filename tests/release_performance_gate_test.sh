#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
build="${REPO_ROOT}/.github/workflows/build.yml"
recovery="${REPO_ROOT}/.github/workflows/release-recovery.yml"

for workflow in "$build" "$recovery"; do
  grep -Fq "TestAffiliateStatsPerformanceAcceptance" "$workflow"
  grep -Fq "TestModelStatusPerformanceSLO" "$workflow"
  grep -Fq "TestUserManagementPerformanceSLO" "$workflow"
  grep -Fq "AFFILIATE_PERF: '1'" "$workflow"
done

grep -Fq "if: github.ref_type == 'tag'" "$build"
grep -Fq "needs: quality" "$build"
grep -Fq "needs: [validate, quality]" "$recovery"

printf 'PASS: tag and recovery image builds depend on all three performance gates\n'
