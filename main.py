#!/usr/bin/env python3
"""Thin launcher for the vendored git-forge binary.

The trial runs `python3 /bot/main.py` (Python 3.13, stdlib only). All of the
forge's behaviour lives in the statically linked Go binary `forge` next to
this file; this launcher only execs it so the entrypoint stays tiny and the
binary can be replaced without touching the lifecycle.
"""
import os
import sys


def main() -> int:
    here = os.path.dirname(os.path.abspath(__file__))
    binary = os.path.join(here, "forge")
    if not os.path.isfile(binary):
        sys.stderr.write("[entry] forge binary not found next to main.py\n")
        return 1
    if not os.access(binary, os.X_OK):
        # The trial copies the repo read-only; make sure the vendored binary
        # is executable before handing control over to it.
        try:
            os.chmod(binary, 0o755)
        except OSError as exc:
            sys.stderr.write(f"[entry] forge binary not executable: {exc}\n")
            return 1
    # Replace this process with the forge; env and argv pass through untouched.
    os.execv(binary, [binary, *sys.argv[1:]])
    return 1  # unreachable


if __name__ == "__main__":
    sys.exit(main())
