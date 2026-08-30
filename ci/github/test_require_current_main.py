import subprocess
import unittest
from unittest import mock

import require_current_main


SHA = "1" * 40


class RequireCurrentMainTest(unittest.TestCase):
    def test_retries_transient_failures_and_accepts_the_expected_tip(self) -> None:
        responses: list[str | subprocess.CalledProcessError] = [
            subprocess.CalledProcessError(1, ["gh"]),
            "not-a-sha\n",
            SHA + "\n",
        ]

        def execute(_args: list[str]) -> str:
            response = responses.pop(0)
            if isinstance(response, subprocess.CalledProcessError):
                raise response
            return response

        sleep = mock.Mock()
        require_current_main.require_current_main(
            SHA,
            "libops/example",
            execute=execute,
            sleep=sleep,
        )
        self.assertEqual([call.args[0] for call in sleep.call_args_list], [1, 2])

    def test_rejects_a_moved_main_tip(self) -> None:
        with self.assertRaisesRegex(RuntimeError, "main advanced"):
            require_current_main.require_current_main(
                SHA,
                "libops/example",
                execute=lambda _args: "2" * 40,
            )

    def test_empty_expected_commit_disables_the_guard(self) -> None:
        execute = mock.Mock(side_effect=AssertionError("must not execute"))
        require_current_main.require_current_main("", "", execute=execute)
        execute.assert_not_called()


if __name__ == "__main__":
    unittest.main()
