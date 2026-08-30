#!/usr/bin/env python3
"""Sign published image indexes and attest their exact native manifests."""

from __future__ import annotations

import base64
import hashlib
import json
import os
import re
import subprocess
import sys
import time
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Callable, Mapping, Sequence


DIGEST_PATTERN = re.compile(r"sha256:[0-9a-f]{64}")
COMMIT_PATTERN = re.compile(r"(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})")
OIDC_ISSUER = "https://token.actions.githubusercontent.com"
TRUSTED_WORKFLOW_PREFIX = "libops/.github/.github/workflows/build-push.yaml@"
COSIGN_ATTEMPTS = 8
COSIGN_MAX_RETRY_DELAY_SECONDS = 30


def command(args: Sequence[str], *, capture: bool = False) -> str:
    suppress_success_output = (
        len(args) > 1
        and args[0] == "cosign"
        and args[1] in {"verify", "verify-attestation"}
    )
    result = subprocess.run(
        args,
        check=True,
        text=True,
        stdout=(
            subprocess.PIPE
            if capture
            else subprocess.DEVNULL
            if suppress_success_output
            else None
        ),
    )
    return result.stdout if capture else ""


def retry_cosign(
    args: Sequence[str],
    *,
    execute: Callable[..., str] = command,
    before_attempt: Callable[[], None] | None = None,
) -> None:
    for attempt in range(COSIGN_ATTEMPTS):
        if before_attempt is not None:
            before_attempt()
        try:
            execute(args)
            return
        except subprocess.CalledProcessError:
            if attempt == COSIGN_ATTEMPTS - 1:
                raise
            print(
                f"Cosign {args[1]} attempt {attempt + 1}/{COSIGN_ATTEMPTS} failed; retrying",
                file=sys.stderr,
            )
            time.sleep(min(5 * (2**attempt), COSIGN_MAX_RETRY_DELAY_SECONDS))
    raise AssertionError("unreachable")


def required(environment: Mapping[str, str], name: str) -> str:
    value = environment.get(name, "")
    if not value:
        raise ValueError(f"{name} is required")
    return value


@dataclass(frozen=True)
class Configuration:
    additional_gar_registry: str
    additional_image: str
    additional_image_names: tuple[str, ...]
    build_args: str
    build_context: str
    caller_ref: str
    caller_workflow_ref: str
    digest_dir: Path
    dockerfile: str
    expected_main_sha: str
    github_ref: str
    github_repository: str
    github_run_attempt: str
    github_run_id: str
    github_sha: str
    job_workflow_ref: str
    job_workflow_sha: str
    oidc_request_token: str
    oidc_request_url: str
    primary_image: str
    primary_registry: str
    publication_tag: str
    sbom_paths: Mapping[str, Path]
    provenance_path: Path

    @classmethod
    def from_environment(cls, environment: Mapping[str, str]) -> "Configuration":
        additional_image_names = json.loads(environment.get("ADDITIONAL_IMAGE_NAMES", "[]"))
        if not isinstance(additional_image_names, list) or not all(
            isinstance(name, str) and name for name in additional_image_names
        ):
            raise ValueError("ADDITIONAL_IMAGE_NAMES must be a JSON list of nonempty strings")
        return cls(
            additional_gar_registry=environment.get("ADDITIONAL_GAR_REGISTRY", ""),
            additional_image=environment.get("ADDITIONAL_IMAGE", ""),
            additional_image_names=tuple(additional_image_names),
            build_args=environment.get("BUILD_ARGS", ""),
            build_context=required(environment, "BUILD_CONTEXT"),
            caller_ref=environment.get("CALLER_REF", ""),
            caller_workflow_ref=required(environment, "CALLER_WORKFLOW_REF"),
            digest_dir=Path(required(environment, "DIGEST_DIR")),
            dockerfile=required(environment, "DOCKER_FILE"),
            expected_main_sha=environment.get("EXPECTED_MAIN_SHA", ""),
            github_ref=required(environment, "GITHUB_REF"),
            github_repository=required(environment, "GITHUB_REPOSITORY"),
            github_run_attempt=required(environment, "GITHUB_RUN_ATTEMPT"),
            github_run_id=required(environment, "GITHUB_RUN_ID"),
            github_sha=required(environment, "GITHUB_SHA"),
            job_workflow_ref=required(environment, "JOB_WORKFLOW_REF"),
            job_workflow_sha=required(environment, "JOB_WORKFLOW_SHA"),
            oidc_request_token=required(environment, "ACTIONS_ID_TOKEN_REQUEST_TOKEN"),
            oidc_request_url=required(environment, "ACTIONS_ID_TOKEN_REQUEST_URL"),
            primary_image=required(environment, "PRIMARY_IMAGE"),
            primary_registry=required(environment, "PRIMARY_REGISTRY"),
            publication_tag=required(environment, "PUBLICATION_TAG"),
            sbom_paths={
                "amd64": Path(required(environment, "SBOM_AMD64_PATH")),
                "arm64": Path(required(environment, "SBOM_ARM64_PATH")),
            },
            provenance_path=Path(required(environment, "SLSA_PROVENANCE_PATH")),
        )

    @property
    def certificate_identity(self) -> str:
        return trusted_workflow_identity(self.job_workflow_ref)

    @property
    def resolved_builder_identity(self) -> str:
        if not COMMIT_PATTERN.fullmatch(self.job_workflow_sha):
            raise ValueError("the managed build-push workflow must resolve to an exact commit")
        return (
            "https://github.com/"
            + TRUSTED_WORKFLOW_PREFIX
            + self.job_workflow_sha.lower()
        )


def trusted_workflow_identity(job_workflow_ref: str) -> str:
    if not job_workflow_ref.startswith(TRUSTED_WORKFLOW_PREFIX):
        raise ValueError("signing must run from the managed LibOps build-push workflow")
    ref = job_workflow_ref.removeprefix(TRUSTED_WORKFLOW_PREFIX)
    if not (ref.startswith("refs/heads/") or ref.startswith("refs/tags/")):
        raise ValueError("the managed build-push workflow must be called by branch or tag")
    return "https://github.com/" + job_workflow_ref


def decode_jwt_claims(token: str) -> Mapping[str, object]:
    parts = token.split(".")
    if len(parts) != 3:
        raise ValueError("GitHub returned an invalid OIDC token")
    payload = parts[1] + "=" * (-len(parts[1]) % 4)
    try:
        claims = json.loads(base64.urlsafe_b64decode(payload).decode())
    except (ValueError, UnicodeDecodeError) as error:
        raise ValueError("GitHub returned an invalid OIDC token") from error
    if not isinstance(claims, dict):
        raise ValueError("GitHub returned invalid OIDC claims")
    return claims


def validate_oidc_claims(config: Configuration, claims: Mapping[str, object]) -> None:
    trusted_workflow_identity(config.job_workflow_ref)
    config.resolved_builder_identity
    expected = {
        "aud": "sigstore",
        "iss": OIDC_ISSUER,
        "job_workflow_ref": config.job_workflow_ref,
        "job_workflow_sha": config.job_workflow_sha,
        "workflow_ref": config.caller_workflow_ref,
        "repository": config.github_repository,
        "ref": config.github_ref,
        "sha": config.github_sha,
    }
    mismatches = [name for name, value in expected.items() if claims.get(name) != value]
    if mismatches:
        raise ValueError(
            "GitHub OIDC identity does not match the managed builder and exact caller: "
            + ", ".join(mismatches)
        )


def request_oidc_claims(config: Configuration) -> Mapping[str, object]:
    separator = "&" if "?" in config.oidc_request_url else "?"
    request = urllib.request.Request(
        config.oidc_request_url + separator + "audience=sigstore",
        headers={"Authorization": f"bearer {config.oidc_request_token}"},
    )
    for attempt in range(3):
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                result = json.load(response)
            if not isinstance(result, dict) or not isinstance(result.get("value"), str):
                raise ValueError("GitHub returned an invalid OIDC response")
            return decode_jwt_claims(result["value"])
        except OSError:
            if attempt == 2:
                raise
            time.sleep(2**attempt)
    raise AssertionError("unreachable")


def require_current_main(config: Configuration, execute: Callable[..., str] = command) -> None:
    if not config.expected_main_sha:
        return
    current_main = ""
    for attempt in range(4):
        try:
            current_main = execute(
                [
                    "gh",
                    "api",
                    "--method",
                    "GET",
                    "--header",
                    "Accept: application/vnd.github+json",
                    "--header",
                    "X-GitHub-Api-Version: 2022-11-28",
                    f"repos/{config.github_repository}/git/ref/heads/main",
                    "--jq",
                    ".object.sha",
                ],
                capture=True,
            ).strip()
        except subprocess.CalledProcessError:
            current_main = ""
        if re.fullmatch(r"[0-9a-fA-F]{40}", current_main):
            break
        if attempt == 3:
            raise RuntimeError(
                "unable to verify main after 4 GitHub API attempts; "
                f"refusing publication for {config.expected_main_sha}"
            )
        print(f"GitHub main lookup attempt {attempt + 1}/4 failed; retrying", file=sys.stderr)
        time.sleep(2**attempt)
    if current_main.lower() != config.expected_main_sha.lower():
        raise RuntimeError(
            f"main advanced to {current_main}; refusing publication for {config.expected_main_sha}"
        )


def validate_digest(value: str) -> str:
    if not DIGEST_PATTERN.fullmatch(value):
        raise ValueError(f"invalid image digest: {value}")
    return value


def manifest_digest(
    config: Configuration,
    image: str,
    execute: Callable[..., str] = command,
) -> str:
    output = execute(
        [
            "docker",
            "buildx",
            "imagetools",
            "inspect",
            f"{image}:{config.publication_tag}",
            "--format",
            "{{json .Manifest}}",
        ],
        capture=True,
    )
    manifest = json.loads(output)
    if not isinstance(manifest, dict):
        raise ValueError("registry returned an invalid image manifest")
    return validate_digest(str(manifest.get("digest", "")))


def load_native_digests(config: Configuration) -> Mapping[str, str]:
    return {
        architecture: validate_digest(
            (config.digest_dir / f"{architecture}.primary").read_text().strip()
        )
        for architecture in ("amd64", "arm64")
    }


def build_provenance(
    config: Configuration,
    native_digests: Mapping[str, str],
    final_digest: str,
) -> Mapping[str, object]:
    source_commit = config.caller_ref or config.github_sha
    if not COMMIT_PATTERN.fullmatch(source_commit):
        raise ValueError("cannot create provenance from a non-immutable source ref")
    return {
        "buildDefinition": {
            "buildType": config.resolved_builder_identity,
            "externalParameters": {
                "repository": config.github_repository,
                "sourceCommit": source_commit.lower(),
                "buildArgsSha256": hashlib.sha256(config.build_args.encode()).hexdigest(),
                "context": config.build_context,
                "dockerfile": config.dockerfile,
                "publicationTag": config.publication_tag,
            },
            "internalParameters": {
                "callerWorkflowRef": config.caller_workflow_ref,
                "jobWorkflowRef": config.job_workflow_ref,
            },
            "resolvedDependencies": [
                {
                    "uri": f"git+https://github.com/{config.github_repository}.git",
                    "digest": {"gitCommit": source_commit.lower()},
                },
                {
                    "uri": (
                        "git+https://github.com/libops/.github.git"
                        "#path=.github/workflows/build-push.yaml"
                    ),
                    "digest": {"gitCommit": config.job_workflow_sha.lower()},
                },
                {
                    "uri": "oci:libops-native?platform=linux/amd64",
                    "digest": {"sha256": native_digests["amd64"].removeprefix("sha256:")},
                },
                {
                    "uri": "oci:libops-native?platform=linux/arm64",
                    "digest": {"sha256": native_digests["arm64"].removeprefix("sha256:")},
                },
                {
                    "uri": "oci:libops-final-manifest",
                    "digest": {"sha256": final_digest.removeprefix("sha256:")},
                },
            ],
        },
        "runDetails": {
            "builder": {"id": config.resolved_builder_identity},
            "metadata": {
                "invocationId": (
                    f"https://github.com/{config.github_repository}/actions/runs/"
                    f"{config.github_run_id}/attempts/{config.github_run_attempt}"
                )
            },
        },
    }


def write_attestation_predicates(
    config: Configuration,
    native_digests: Mapping[str, str],
    final_digest: str,
    execute: Callable[..., str] = command,
) -> None:
    for architecture in ("amd64", "arm64"):
        execute(
            [
                "syft",
                f"{config.primary_image}@{native_digests[architecture]}",
                "--platform",
                f"linux/{architecture}",
                "--output",
                f"spdx-json={config.sbom_paths[architecture]}",
            ]
        )
        sbom = json.loads(config.sbom_paths[architecture].read_text())
        if not isinstance(sbom, dict) or not str(sbom.get("spdxVersion", "")).startswith("SPDX-"):
            raise ValueError(f"Syft returned an invalid linux/{architecture} SPDX document")
    config.provenance_path.write_text(
        json.dumps(build_provenance(config, native_digests, final_digest), separators=(",", ":"))
        + "\n"
    )


def certificate_verification_args(config: Configuration) -> list[str]:
    return [
        "--certificate-identity",
        config.certificate_identity,
        "--certificate-oidc-issuer",
        OIDC_ISSUER,
        "--certificate-github-workflow-repository",
        config.github_repository,
        "--certificate-github-workflow-ref",
        config.github_ref,
        "--certificate-github-workflow-sha",
        config.github_sha,
    ]


def sign_attest_and_verify(
    config: Configuration,
    native_digests: Mapping[str, str],
    image: str,
    final_digest: str,
    *,
    execute: Callable[..., str] = command,
    guard: Callable[[], None] | None = None,
) -> None:
    guard = guard or (lambda: require_current_main(config, execute))
    final_ref = f"{image}@{final_digest}"
    caller_annotation = f"caller-workflow-ref={config.caller_workflow_ref}"
    verification = certificate_verification_args(config)

    retry_cosign(
        ["cosign", "sign", "--yes", "-a", caller_annotation, final_ref],
        execute=execute,
        before_attempt=guard,
    )
    retry_cosign(
        ["cosign", "verify", *verification, "-a", caller_annotation, final_ref],
        execute=execute,
    )

    for architecture in ("amd64", "arm64"):
        native_ref = f"{image}@{native_digests[architecture]}"
        retry_cosign(
            [
                "cosign",
                "attest",
                "--yes",
                "--type",
                "https://spdx.dev/Document",
                "--predicate",
                str(config.sbom_paths[architecture]),
                native_ref,
            ],
            execute=execute,
            before_attempt=guard,
        )
        retry_cosign(
            [
                "cosign",
                "verify-attestation",
                "--type",
                "https://spdx.dev/Document",
                *verification,
                native_ref,
            ],
            execute=execute,
        )

    retry_cosign(
        [
            "cosign",
            "attest",
            "--yes",
            "--type",
            "https://slsa.dev/provenance/v1",
            "--predicate",
            str(config.provenance_path),
            final_ref,
        ],
        execute=execute,
        before_attempt=guard,
    )
    retry_cosign(
        [
            "cosign",
            "verify-attestation",
            "--type",
            "https://slsa.dev/provenance/v1",
            *verification,
            final_ref,
        ],
        execute=execute,
    )


def publication_images(config: Configuration) -> tuple[str, ...]:
    images = [config.primary_image]
    if config.additional_image:
        images.append(config.additional_image)
    for name in config.additional_image_names:
        images.append(f"{config.primary_registry}/{name}")
        if config.additional_gar_registry:
            images.append(f"{config.additional_gar_registry}/{name}")
    return tuple(images)


def build_publication_record(
    config: Configuration,
    native_digests: Mapping[str, str],
    final_digest: str,
) -> Mapping[str, object]:
    source_commit = config.caller_ref or config.github_sha
    if not COMMIT_PATTERN.fullmatch(source_commit):
        raise ValueError("cannot record publication from a non-immutable source ref")
    validated_native_digests = {
        f"linux/{architecture}": validate_digest(native_digests[architecture])
        for architecture in ("amd64", "arm64")
    }
    validated_final_digest = validate_digest(final_digest)
    builder_commit = config.resolved_builder_identity.rsplit("@", 1)[1]
    source = {
        "repository": f"https://github.com/{config.github_repository}",
        "commit": source_commit.lower(),
    }
    publisher = {
        "certificateIdentity": config.certificate_identity,
        "builderCommit": builder_commit,
        "callerWorkflowRef": config.caller_workflow_ref,
    }
    publication_run = (
        f"https://github.com/{config.github_repository}/actions/runs/"
        f"{config.github_run_id}"
    )
    attestations = {
        **publisher,
        "sbom": {
            "predicateType": "https://spdx.dev/Document",
            "platforms": ["linux/amd64", "linux/arm64"],
            "verificationRun": publication_run,
        },
        "provenance": {
            "predicateType": "https://slsa.dev/provenance/v1",
            "verificationRun": publication_run,
        },
    }
    return {
        "schemaVersion": 1,
        "source": source,
        "publisher": publisher,
        "publicationRun": publication_run,
        "nativeDigests": validated_native_digests,
        "images": [
            {
                "reference": (
                    f"{image}:{config.publication_tag}@{validated_final_digest}"
                ),
                "source": source,
                "attestations": attestations,
            }
            for image in publication_images(config)
        ],
    }


def write_publication_record(
    path: Path,
    record: Mapping[str, object],
) -> None:
    path.write_text(json.dumps(record, sort_keys=True, separators=(",", ":")) + "\n")


def main() -> int:
    config = Configuration.from_environment(os.environ)
    validate_oidc_claims(config, request_oidc_claims(config))
    native_digests = load_native_digests(config)
    final_digest = manifest_digest(config, config.primary_image)
    write_attestation_predicates(config, native_digests, final_digest)
    for image in publication_images(config):
        image_digest = manifest_digest(config, image)
        if image_digest != final_digest:
            raise ValueError(f"{image} does not match the verified primary manifest")
        sign_attest_and_verify(config, native_digests, image, image_digest)
    write_publication_record(
        Path(required(os.environ, "PUBLICATION_RECORD_PATH")),
        build_publication_record(config, native_digests, final_digest),
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (
        json.JSONDecodeError,
        OSError,
        RuntimeError,
        subprocess.CalledProcessError,
        ValueError,
    ) as error:
        print(f"image signing failed: {error}", file=sys.stderr)
        raise SystemExit(1) from error
