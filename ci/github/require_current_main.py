#!/usr/bin/env python3
"""Fail closed unless the caller's expected commit is still the main tip."""

from __future__ import annotations

import os
import re
import subprocess
import sys
import time
from typing import Callable, Mapping, Sequence


ATTEMPTS = 4
SHA_PATTERN = re.compile(r"[0-9a-fA-F]{40}")


def command(args: Sequence[str]) -> str:
    result = subprocess.run(
        args,
        check=True,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
    )
    return result.stdout


def require_current_main(
    expected: str,
    repository: str,
    *,
    execute: Callable[[Sequence[str]], str] = command,
    sleep: Callable[[float], None] = time.sleep,
) -> None:
    if not expected:
        return
    if not SHA_PATTERN.fullmatch(expected):
        raise ValueError("EXPECTED_MAIN_SHA must be an exact 40-character commit")
    if not repository:
        raise ValueError("GITHUB_REPOSITORY is required")

    current = ""
    for attempt in range(ATTEMPTS):
        try:
            current = execute(
                [
                    "gh",
                    "api",
                    "--method",
                    "GET",
                    "--header",
                    "Accept: application/vnd.github+json",
                    "--header",
                    "X-GitHub-Api-Version: 2022-11-28",
                    f"repos/{repository}/git/ref/heads/main",
                    "--jq",
                    ".object.sha",
                ]
            ).strip()
        except subprocess.CalledProcessError:
            current = ""
        if SHA_PATTERN.fullmatch(current):
            break
        if attempt == ATTEMPTS - 1:
            raise RuntimeError(
                f"unable to verify main after {ATTEMPTS} GitHub API attempts; "
                f"refusing publication for {expected}"
            )
        print(
            f"GitHub main lookup attempt {attempt + 1}/{ATTEMPTS} failed; retrying",
            file=sys.stderr,
        )
        sleep(2**attempt)

    if current.lower() != expected.lower():
        raise RuntimeError(f"main advanced to {current}; refusing publication for {expected}")


def main(environment: Mapping[str, str] = os.environ) -> int:
    try:
        require_current_main(
            environment.get("EXPECTED_MAIN_SHA", ""),
            environment.get("GITHUB_REPOSITORY", ""),
        )
    except (RuntimeError, ValueError) as error:
        print(error, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
