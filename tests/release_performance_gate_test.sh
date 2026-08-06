#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
build="${REPO_ROOT}/.github/workflows/build.yml"
recovery="${REPO_ROOT}/.github/workflows/release-recovery.yml"
performance="${REPO_ROOT}/.github/workflows/performance.yml"

for workflow in "$build" "$recovery"; do
  grep -Fq "TestAffiliateStatsPerformanceAcceptance" "$workflow"
  grep -Fq "TestModelStatusPerformanceSLO" "$workflow"
  grep -Fq "TestUserManagementPerformanceSLO" "$workflow"
  grep -Fq "AFFILIATE_PERF: '1'" "$workflow"
done

for workflow in "$build" "$recovery" "$performance"; do
  grep -Fq -- '-list "^${test_name}$"' "$workflow"
  grep -Fq -- 'grep -Fxq -- "$test_name"' "$workflow"
done

for test_name in \
  TestAffiliateStatsPerformanceAcceptance \
  TestModelStatusPerformanceSLO \
  TestUserManagementPerformanceSLO; do
  grep -Fq "$test_name" "$performance"
done

grep -Fq "if: github.ref_type == 'tag'" "$build"
grep -Fq "needs: quality" "$build"
grep -Fq "needs: [validate, quality]" "$recovery"

printf 'PASS: release and PR performance gates require exact SLO test selection\n'
