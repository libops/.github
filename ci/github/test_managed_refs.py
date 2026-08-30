import re
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


if __name__ == "__main__":
    unittest.main()
