import base64
import dataclasses
import json
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import sign_and_attest


SHA = "1" * 40
AMD64_DIGEST = "sha256:" + "a" * 64
ARM64_DIGEST = "sha256:" + "b" * 64
FINAL_DIGEST = "sha256:" + "c" * 64


def configuration(directory: Path) -> sign_and_attest.Configuration:
    return sign_and_attest.Configuration(
        additional_gar_registry="us-docker.pkg.dev/libops",
        additional_image="us-docker.pkg.dev/libops/primary",
        additional_image_names=("alias",),
        build_args="A=1",
        build_context=".",
        caller_ref=SHA,
        caller_workflow_ref="libops/example/.github/workflows/push.yaml@refs/heads/main",
        digest_dir=directory,
        dockerfile="Dockerfile",
        expected_main_sha=SHA,
        github_ref="refs/heads/main",
        github_repository="libops/example",
        github_run_attempt="1",
        github_run_id="42",
        github_sha=SHA,
        job_workflow_ref=(
            "libops/.github/.github/workflows/build-push.yaml@refs/heads/main"
        ),
        oidc_request_token="token",
        oidc_request_url="https://example.invalid/oidc?x=1",
        primary_image="ghcr.io/libops/example",
        primary_registry="ghcr.io/libops",
        publication_tag="main",
        sbom_paths={
            "amd64": directory / "amd64.spdx.json",
            "arm64": directory / "arm64.spdx.json",
        },
        provenance_path=directory / "provenance.json",
    )


class SignAndAttestTest(unittest.TestCase):
    def test_oidc_claims_bind_managed_builder_and_exact_caller(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            config = configuration(Path(directory))
            claims = {
                "aud": "sigstore",
                "iss": sign_and_attest.OIDC_ISSUER,
                "job_workflow_ref": config.job_workflow_ref,
                "workflow_ref": config.caller_workflow_ref,
                "repository": config.github_repository,
                "ref": config.github_ref,
                "sha": config.github_sha,
            }
            sign_and_attest.validate_oidc_claims(config, claims)
            claims["sha"] = "2" * 40
            with self.assertRaisesRegex(ValueError, "sha"):
                sign_and_attest.validate_oidc_claims(config, claims)

    def test_managed_builder_rejects_sha_pins_and_foreign_workflows(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            config = configuration(Path(directory))
            for workflow_ref in (
                "libops/.github/.github/workflows/build-push.yaml@" + SHA,
                "someone-else/.github/.github/workflows/build-push.yaml@refs/heads/main",
            ):
                with self.subTest(workflow_ref=workflow_ref):
                    with self.assertRaises(ValueError):
                        sign_and_attest.trusted_workflow_identity(workflow_ref)
                    invalid_config = dataclasses.replace(config, job_workflow_ref=workflow_ref)
                    with self.assertRaises(ValueError):
                        sign_and_attest.validate_oidc_claims(invalid_config, {})

    def test_jwt_decoder_accepts_base64url_without_padding(self) -> None:
        payload = base64.urlsafe_b64encode(json.dumps({"aud": "sigstore"}).encode()).decode()
        token = "header." + payload.rstrip("=") + ".signature"
        self.assertEqual(sign_and_attest.decode_jwt_claims(token)["aud"], "sigstore")

    def test_provenance_names_every_exact_input_digest(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            config = configuration(Path(directory))
            provenance = sign_and_attest.build_provenance(
                config,
                {"amd64": AMD64_DIGEST, "arm64": ARM64_DIGEST},
                FINAL_DIGEST,
            )
            dependencies = provenance["buildDefinition"]["resolvedDependencies"]
            self.assertEqual(
                provenance["buildDefinition"]["buildType"], config.certificate_identity
            )
            self.assertEqual(
                [dependency["digest"] for dependency in dependencies],
                [
                    {"gitCommit": SHA},
                    {"sha256": "a" * 64},
                    {"sha256": "b" * 64},
                    {"sha256": "c" * 64},
                ],
            )

    def test_main_guard_retries_transient_responses_and_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            config = configuration(Path(directory))
            responses = iter(("not-a-sha\n", "still-not-a-sha\n", SHA + "\n"))
            attempts = 0

            def execute(_args: list[str], *, capture: bool = False) -> str:
                nonlocal attempts
                self.assertTrue(capture)
                attempts += 1
                return next(responses)

            with mock.patch.object(sign_and_attest.time, "sleep") as sleep:
                sign_and_attest.require_current_main(config, execute)
            self.assertEqual(attempts, 3)
            self.assertEqual([call.args[0] for call in sleep.call_args_list], [1, 2])

            def mismatch(_args: list[str], *, capture: bool = False) -> str:
                self.assertTrue(capture)
                return "2" * 40 + "\n"

            with self.assertRaisesRegex(RuntimeError, "main advanced"):
                sign_and_attest.require_current_main(config, mismatch)

    def test_sboms_attest_native_manifests_without_unsupported_annotations(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            config = configuration(Path(directory))
            calls: list[list[str]] = []
            guards = 0

            def execute(args: list[str], *, capture: bool = False) -> str:
                self.assertFalse(capture)
                calls.append(args)
                return ""

            def guard() -> None:
                nonlocal guards
                guards += 1

            sign_and_attest.sign_attest_and_verify(
                config,
                {"amd64": AMD64_DIGEST, "arm64": ARM64_DIGEST},
                config.primary_image,
                FINAL_DIGEST,
                execute=execute,
                guard=guard,
            )

            attestations = [call for call in calls if call[:2] == ["cosign", "attest"]]
            self.assertEqual(guards, 4)
            self.assertEqual(
                [call[-1] for call in attestations],
                [
                    f"{config.primary_image}@{AMD64_DIGEST}",
                    f"{config.primary_image}@{ARM64_DIGEST}",
                    f"{config.primary_image}@{FINAL_DIGEST}",
                ],
            )
            self.assertTrue(all("-a" not in call for call in attestations))
            verified = [
                call for call in calls if call[:2] == ["cosign", "verify-attestation"]
            ]
            self.assertEqual([call[-1] for call in verified], [call[-1] for call in attestations])
            self.assertTrue(all("-a" not in call for call in verified))
            signature = next(call for call in calls if call[:2] == ["cosign", "sign"])
            self.assertEqual(signature[-1], f"{config.primary_image}@{FINAL_DIGEST}")
            self.assertIn("-a", signature)

    def test_cosign_retries_transient_failures_and_rechecks_main(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            config = configuration(Path(directory))
            sign_attempts = 0
            guards = 0

            def execute(args: list[str], *, capture: bool = False) -> str:
                nonlocal sign_attempts
                self.assertFalse(capture)
                if args[:2] == ["cosign", "sign"]:
                    sign_attempts += 1
                    if sign_attempts < 3:
                        raise subprocess.CalledProcessError(1, args)
                return ""

            def guard() -> None:
                nonlocal guards
                guards += 1

            with mock.patch.object(sign_and_attest.time, "sleep") as sleep:
                sign_and_attest.sign_attest_and_verify(
                    config,
                    {"amd64": AMD64_DIGEST, "arm64": ARM64_DIGEST},
                    config.primary_image,
                    FINAL_DIGEST,
                    execute=execute,
                    guard=guard,
                )

            self.assertEqual(sign_attempts, 3)
            self.assertEqual(guards, 6)
            self.assertEqual([call.args[0] for call in sleep.call_args_list], [1, 2])

    def test_publication_images_include_primary_mirror_and_aliases(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            config = configuration(Path(directory))
            self.assertEqual(
                sign_and_attest.publication_images(config),
                (
                    "ghcr.io/libops/example",
                    "us-docker.pkg.dev/libops/primary",
                    "ghcr.io/libops/alias",
                    "us-docker.pkg.dev/libops/alias",
                ),
            )


if __name__ == "__main__":
    unittest.main()
