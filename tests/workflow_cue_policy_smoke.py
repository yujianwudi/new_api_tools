#!/usr/bin/env python3
"""Render and mutation-test the CUE policies embedded in release workflows."""

from __future__ import annotations

import argparse
import copy
import json
import re
import subprocess
import textwrap
from pathlib import Path
from typing import Any


SHA_A = "a" * 64
SHA_B = "b" * 64
SHA_C = "c" * 64
COMMIT_D = "d" * 40
TAG_OID_E = "e" * 40
WORKFLOW_SHA_F = "f" * 40
IMAGE = "ghcr.io/yujianwudi/new_api_tools"
REPOSITORY = "yujianwudi/new_api_tools"
TAG = "v0.6.2"
TAG_REF = f"refs/tags/{TAG}"


def extract_policy(workflow: Path) -> str:
    lines = workflow.read_text(encoding="utf-8").splitlines()
    starts = [
        index
        for index, line in enumerate(lines)
        if 'cat > "$policy_file" <<EOF' in line
    ]
    if len(starts) != 1:
        raise RuntimeError(
            f"expected exactly one inline CUE policy in {workflow}, found {len(starts)}"
        )
    start = starts[0] + 1
    try:
        end = next(
            index
            for index in range(start, len(lines))
            if lines[index].strip() == "EOF"
        )
    except StopIteration as exc:
        raise RuntimeError(f"unterminated inline CUE policy in {workflow}") from exc
    return textwrap.dedent("\n".join(lines[start:end])) + "\n"


def render_policy(template: str, values: dict[str, str], workflow: Path) -> str:
    token = re.compile(r"\$\{([^}]+)\}")

    def replace(match: re.Match[str]) -> str:
        key = match.group(1)
        if key not in values:
            raise RuntimeError(f"unknown policy template token {key!r} in {workflow}")
        return values[key]

    rendered = token.sub(replace, template)
    if "${" in rendered:
        raise RuntimeError(f"unrendered policy token remains in {workflow}")
    return rendered


def statement(build_type: str, *, recovery: bool) -> dict[str, Any]:
    external_parameters: dict[str, Any] = {
        "repository": f"https://github.com/{REPOSITORY}",
        "ref": TAG_REF,
        "revision": COMMIT_D,
        "tag": TAG,
        "manifest_digest": f"sha256:{SHA_A}",
        "platform_digests": {
            "linux/amd64": f"sha256:{SHA_B}",
            "linux/arm64": f"sha256:{SHA_C}",
        },
    }
    internal_parameters: dict[str, Any] = {}
    if recovery:
        external_parameters["tag_oid"] = TAG_OID_E
        internal_parameters = {
            "workflow_ref": (
                f"{REPOSITORY}/.github/workflows/release-recovery.yml@refs/heads/main"
            ),
            "workflow_sha": WORKFLOW_SHA_F,
        }
    return {
        # Cosign v3.1.2 intentionally wraps SLSA provenance v1 predicates in
        # the legacy in-toto Statement v0.1 envelope.
        "_type": "https://in-toto.io/Statement/v0.1",
        "subject": [{"name": IMAGE, "digest": {"sha256": SHA_A}}],
        "predicateType": "https://slsa.dev/provenance/v1",
        "predicate": {
            "buildDefinition": {
                "buildType": build_type,
                "externalParameters": external_parameters,
                "internalParameters": internal_parameters,
                "resolvedDependencies": [
                    {
                        "uri": f"git+https://github.com/{REPOSITORY}@{TAG_REF}",
                        "digest": {"gitCommit": COMMIT_D},
                    }
                ],
            },
            "runDetails": {
                "builder": {"id": build_type},
                "metadata": {"invocationId": "https://github.com/example/actions/runs/1"},
            },
        },
    }


def set_path(document: dict[str, Any], path: tuple[Any, ...], value: Any) -> None:
    current: Any = document
    for component in path[:-1]:
        current = current[component]
    current[path[-1]] = value


def cue_vet(cue: Path, policy: Path, payload: Path) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [str(cue), "vet", "-c", str(policy), str(payload)],
        check=False,
        capture_output=True,
        text=True,
    )


def validate_workflow(
    repo_root: Path,
    cue: Path,
    work_dir: Path,
    workflow_name: str,
    *,
    recovery: bool,
) -> None:
    workflow = repo_root / ".github" / "workflows" / workflow_name
    if recovery:
        build_type = (
            f"https://github.com/{REPOSITORY}/.github/workflows/"
            "release-recovery.yml@refs/heads/main"
        )
        values = {
            "image": IMAGE,
            "version_digest#sha256:": SHA_A,
            "version_digest": f"sha256:{SHA_A}",
            "certificate_identity": build_type,
            "RELEASE_TAG": TAG,
            "EXPECTED_TAG_COMMIT": COMMIT_D,
            "EXPECTED_TAG_OID": TAG_OID_E,
            "amd64_digest": f"sha256:{SHA_B}",
            "arm64_digest": f"sha256:{SHA_C}",
            "RECOVERY_WORKFLOW_REF": (
                f"{REPOSITORY}/.github/workflows/release-recovery.yml@refs/heads/main"
            ),
            "RECOVERY_WORKFLOW_SHA": WORKFLOW_SHA_F,
        }
    else:
        build_type = (
            f"https://github.com/{REPOSITORY}/.github/workflows/build.yml@{TAG_REF}"
        )
        values = {
            "REGISTRY": "ghcr.io",
            "IMAGE_NAME": REPOSITORY,
            "manifest_digest#sha256:": SHA_A,
            "manifest_digest": f"sha256:{SHA_A}",
            "certificate_identity": build_type,
            "GITHUB_REPOSITORY": REPOSITORY,
            "GITHUB_REF": TAG_REF,
            "GITHUB_SHA": COMMIT_D,
            "GITHUB_REF_NAME": TAG,
            "amd64_digest": f"sha256:{SHA_B}",
            "arm64_digest": f"sha256:{SHA_C}",
        }

    case_dir = work_dir / workflow.stem
    case_dir.mkdir(parents=True, exist_ok=True)
    policy_path = case_dir / "rendered-policy.cue"
    valid_path = case_dir / "valid-statement.json"
    rendered = render_policy(extract_policy(workflow), values, workflow)
    policy_path.write_text(rendered, encoding="utf-8")
    valid_statement = statement(build_type, recovery=recovery)
    valid_path.write_text(json.dumps(valid_statement), encoding="utf-8")

    positive = cue_vet(cue, policy_path, valid_path)
    if positive.returncode != 0:
        raise RuntimeError(
            f"rendered policy from {workflow_name} rejected its valid statement:\n"
            f"{positive.stdout}{positive.stderr}"
        )

    mutations: dict[str, tuple[tuple[Any, ...], Any]] = {
        "statement_type": (("_type",), "https://in-toto.io/Statement/v1"),
        "predicate_type": (
            ("predicateType",),
            "https://example.com/forged-predicate/v1",
        ),
        "subject_name": (("subject", 0, "name"), "ghcr.io/forged/image"),
        "subject_digest": (("subject", 0, "digest", "sha256"), "9" * 64),
        "manifest_digest": (
            (
                "predicate",
                "buildDefinition",
                "externalParameters",
                "manifest_digest",
            ),
            f"sha256:{'9' * 64}",
        ),
        "revision": (
            ("predicate", "buildDefinition", "externalParameters", "revision"),
            "9" * 40,
        ),
        "repository": (
            ("predicate", "buildDefinition", "externalParameters", "repository"),
            "https://github.com/forged/repository",
        ),
        "ref": (
            ("predicate", "buildDefinition", "externalParameters", "ref"),
            "refs/tags/v9.9.9",
        ),
        "tag": (
            ("predicate", "buildDefinition", "externalParameters", "tag"),
            "v9.9.9",
        ),
        "build_type": (
            ("predicate", "buildDefinition", "buildType"),
            "https://github.com/forged/workflow.yml@refs/heads/main",
        ),
        "builder_identity": (
            ("predicate", "runDetails", "builder", "id"),
            "https://github.com/forged/workflow.yml@refs/heads/main",
        ),
        "dependency_uri": (
            (
                "predicate",
                "buildDefinition",
                "resolvedDependencies",
                0,
                "uri",
            ),
            "git+https://github.com/forged/repo@refs/tags/v9.9.9",
        ),
        "dependency_commit": (
            (
                "predicate",
                "buildDefinition",
                "resolvedDependencies",
                0,
                "digest",
                "gitCommit",
            ),
            "9" * 40,
        ),
        "platform_digest": (
            (
                "predicate",
                "buildDefinition",
                "externalParameters",
                "platform_digests",
                "linux/amd64",
            ),
            f"sha256:{'9' * 64}",
        ),
    }
    if recovery:
        mutations.update(
            {
                "tag_oid": (
                    (
                        "predicate",
                        "buildDefinition",
                        "externalParameters",
                        "tag_oid",
                    ),
                    "9" * 40,
                ),
                "workflow_ref": (
                    (
                        "predicate",
                        "buildDefinition",
                        "internalParameters",
                        "workflow_ref",
                    ),
                    f"{REPOSITORY}/.github/workflows/forged.yml@refs/heads/main",
                ),
                "workflow_sha": (
                    (
                        "predicate",
                        "buildDefinition",
                        "internalParameters",
                        "workflow_sha",
                    ),
                    "9" * 40,
                ),
            }
        )

    for mutation_name, (path, value) in mutations.items():
        mutated = copy.deepcopy(valid_statement)
        set_path(mutated, path, value)
        mutation_path = case_dir / f"forged-{mutation_name}.json"
        mutation_path.write_text(json.dumps(mutated), encoding="utf-8")
        result = cue_vet(cue, policy_path, mutation_path)
        if result.returncode == 0:
            raise RuntimeError(
                f"rendered policy from {workflow_name} accepted {mutation_name} mutation"
            )

    extra_platform = copy.deepcopy(valid_statement)
    extra_platform["predicate"]["buildDefinition"]["externalParameters"][
        "platform_digests"
    ]["linux/s390x"] = f"sha256:{'9' * 64}"
    extra_path = case_dir / "forged-extra-platform.json"
    extra_path.write_text(json.dumps(extra_platform), encoding="utf-8")
    result = cue_vet(cue, policy_path, extra_path)
    if result.returncode == 0:
        raise RuntimeError(
            f"rendered policy from {workflow_name} accepted an unexpected platform"
        )

    print(
        f"{workflow_name}: rendered inline policy passed positive and "
        f"{len(mutations) + 1} negative mutations"
    )


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--cue", required=True, type=Path)
    parser.add_argument("--work-dir", required=True, type=Path)
    args = parser.parse_args()

    repo_root = Path(__file__).resolve().parents[1]
    args.work_dir.mkdir(parents=True, exist_ok=True)
    validate_workflow(
        repo_root, args.cue, args.work_dir, "build.yml", recovery=False
    )
    validate_workflow(
        repo_root,
        args.cue,
        args.work_dir,
        "release-recovery.yml",
        recovery=True,
    )


if __name__ == "__main__":
    main()
