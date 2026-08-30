import json
import re
import subprocess
import tempfile
import unittest
from pathlib import Path


REPOSITORY_ROOT = Path(__file__).resolve().parents[2]
PINNED_LIBOPS_USE = re.compile(
    r"uses:\s+libops/[^@\s]+@(?:[0-9a-fA-F]{40}|FULL_40_CHARACTER_COMMIT_SHA)"
)


class ManagedRefTest(unittest.TestCase):
    def test_libops_actions_and_workflows_follow_managed_channels(self) -> None:
        paths = [REPOSITORY_ROOT / "README.md"]
        paths.extend((REPOSITORY_ROOT / ".github").rglob("*.md"))
        paths.extend((REPOSITORY_ROOT / ".github").rglob("*.yml"))
        paths.extend((REPOSITORY_ROOT / ".github").rglob("*.yaml"))

        violations = []
        for path in paths:
            for line_number, line in enumerate(path.read_text().splitlines(), start=1):
                if PINNED_LIBOPS_USE.search(line):
                    violations.append(f"{path.relative_to(REPOSITORY_ROOT)}:{line_number}")

        self.assertEqual(
            violations,
            [],
            "LibOps-owned actions and workflows must follow a managed branch or release channel; "
            "record the resolved commit as generated evidence instead",
        )

    def test_release_manifest_separates_managed_identity_from_exact_builder(self) -> None:
        fixture = REPOSITORY_ROOT / "ci/github/testdata/platform-release.valid.json"
        manifest = json.loads(fixture.read_text())
        attestations = manifest["platformImages"][0]["image"]["attestations"]
        self.assertEqual(
            attestations["certificateIdentity"],
            (
                "https://github.com/libops/.github/.github/workflows/"
                "build-push.yaml@refs/heads/main"
            ),
        )
        self.assertRegex(attestations["builderCommit"], r"^[0-9a-f]{40}$")
        self.assert_validator_accepts(manifest)

        attestations["builderCommit"] = "refs/heads/main"
        result = self.run_validator(manifest)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("builderCommit must bind the exact resolved", result.stderr)

    def test_release_manifest_rejects_sha_pinned_managed_certificate_identity(self) -> None:
        fixture = REPOSITORY_ROOT / "ci/github/testdata/platform-release.valid.json"
        manifest = json.loads(fixture.read_text())
        manifest["platformImages"][0]["image"]["attestations"]["certificateIdentity"] = (
            "https://github.com/libops/.github/.github/workflows/build-push.yaml@" + "1" * 40
        )
        result = self.run_validator(manifest)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("must bind a managed shared-publisher ref", result.stderr)

    def assert_validator_accepts(self, manifest: dict[str, object]) -> None:
        result = self.run_validator(manifest)
        self.assertEqual(result.returncode, 0, result.stderr)

    def run_validator(self, manifest: dict[str, object]) -> subprocess.CompletedProcess[str]:
        with tempfile.NamedTemporaryFile(mode="w", suffix=".json") as candidate:
            json.dump(manifest, candidate)
            candidate.flush()
            return subprocess.run(
                [
                    "node",
                    str(
                        REPOSITORY_ROOT
                        / ".github/actions/validate-platform-compatibility/validate.mjs"
                    ),
                    "--schema",
                    str(REPOSITORY_ROOT / ".github/compatibility/platform-release.schema.json"),
                    "--owners",
                    str(REPOSITORY_ROOT / ".github/compatibility/platform-release.owners.json"),
                    candidate.name,
                ],
                check=False,
                capture_output=True,
                text=True,
            )


if __name__ == "__main__":
    unittest.main()
