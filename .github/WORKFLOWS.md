# GitHub Actions layout

The default branch keeps four workflow entry points:

| File | Responsibility | Write access |
| --- | --- | --- |
| `build.yml` | Required tests, read-only PR image builds, multi-platform publication, signing, and provenance | Only `build-publish` and `merge` |
| `codeql.yml` | Go and JavaScript/TypeScript CodeQL analysis | Security events only |
| `performance.yml` | The three required performance acceptance checks | None |
| `release-recovery.yml` | Re-sign and attest an existing immutable release manifest | Only the final publish job |

`.github/actions/build-platform/action.yml` contains the shared platform-build
steps. The PR caller has only `contents: read`; the publishing caller separately
requests `packages: write`. Do not combine those callers into one privileged job.

## Required check names

The `main` ruleset depends on these job names. Change the ruleset in the same
maintenance window if any of them must be renamed:

- `Test and security gates`
- `build (linux/amd64, ubuntu-latest)`
- `build (linux/arm64, ubuntu-24.04-arm)`
- `CodeQL (go)`
- `CodeQL (javascript-typescript)`
- `100k-user SQLite p95 and query-plan gate`
- `Authenticated status and durable config p95`
- `100k users and 30-day billing-log SLO`

## Repository conventions

- Pin every remote Action to a full 40-character commit SHA.
- Start from an explicit top-level permission baseline and grant writes only to
  the job that needs them.
- Give every job a timeout and every workflow a concurrency policy.
- Use the shared strict Bash shell (`-euo pipefail`) for workflow commands.
- Keep `actions/checkout` credentials disabled unless a reviewed operation
  explicitly needs Git credentials.
- Do not add path filters to required checks; a skipped required workflow can
  leave pull requests permanently pending.
- Do not rename `build.yml` or `release-recovery.yml`. Their paths are part of
  the verified Sigstore certificate identity and provenance policy.
- Recovery must reuse the recorded exact manifest digest. It must not rebuild
  an image, overwrite an exact tag, or move the minor alias.

The long recovery verification block remains self-contained on purpose: the
workflow must be able to recover an already-created tag whose commit does not
contain newer helper scripts. Moving that block requires a separately audited
protected-workflow checkout and must not be treated as a formatting-only edit.

## Local validation

Run before opening a pull request:

```bash
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7
yamllint -s .github/workflows .github/actions .yamllint.yml
bash tests/supply_chain_pins_test.sh
bash tests/release_performance_gate_test.sh
bash tests/create_manifest_test.sh
bash tests/cosign_v3_compat_test.sh
```
