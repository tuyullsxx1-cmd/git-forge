#!/usr/bin/env python3
"""Thin launcher for the vendored git-forge binary.

The trial runs `python3 /bot/main.py` (Python 3.13, stdlib only) with /bot
mounted read-only. All of the forge's behaviour lives in the statically
linked Go binary `forge` next to this file; this launcher only execs it so
the entrypoint stays tiny and the binary can be replaced without touching
the lifecycle.

The binary as committed is mode 100644 (the web upload strips the exec
bit), so it cannot be executed in place and /bot cannot be chmodded.
When the vendored copy is not executable we therefore copy it to a fresh
writable directory under /tmp, chmod the copy, and exec from there.
"""
import os
import shutil
import sys
import tempfile


def _exec(binary: str) -> None:
    # Replace this process with the forge; env and argv pass through
    # untouched.
    os.execv(binary, [binary, *sys.argv[1:]])


def main() -> int:
    here = os.path.dirname(os.path.abspath(__file__))
    binary = os.path.join(here, "forge")
    if not os.path.isfile(binary):
        sys.stderr.write("[entry] forge binary not found next to main.py\n")
        return 1
    if os.access(binary, os.X_OK):
        _exec(binary)
    # Not executable: copy to a writable location, make the copy
    # executable, and run that. /bot is read-only in the trial pod, so
    # chmodding in place cannot work there.
    try:
        tmpdir = tempfile.mkdtemp(prefix="git-forge-")
        staged = os.path.join(tmpdir, "forge")
        shutil.copyfile(binary, staged)
        os.chmod(staged, 0o755)
    except OSError as exc:
        sys.stderr.write(f"[entry] could not stage forge binary: {exc}\n")
        # Last resort: try the old in-place chmod (works when /bot is
        # writable, e.g. local grading).
        try:
            os.chmod(binary, 0o755)
        except OSError as exc2:
            sys.stderr.write(f"[entry] forge binary not executable: {exc2}\n")
            return 1
        _exec(binary)
    _exec(staged)
    return 1  # unreachable


if __name__ == "__main__":
    sys.exit(main())
