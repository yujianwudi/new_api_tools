#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}/backend"

# This is intentionally a real modernc SQLite/Go drill. The test constructs a
# checksumless published-v9 database, takes an Online Backup, runs the actual
# v9-to-current migrations, injects a post-migration candidate failure/write,
# restores through SQLite's restore API, and reads the result as v9 again.
go test ./internal/toolstore \
  -run '^TestToolStoreV9CandidateFailureRollbackDrill$' \
  -count=1 -v
