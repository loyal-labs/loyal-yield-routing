#!/usr/bin/env python3

"""Make a restored Cargo target directory safe and useful across CI checkouts."""

from __future__ import annotations

import argparse
import os
from pathlib import Path
import subprocess
import sys
import time


OLD_MTIME = 315532800  # 1980-01-01 UTC, older than every repository commit.
MARKER_NAME = ".ci-source-revision"


def git_command(*args: str) -> list[str]:
    # GitHub job containers run as root while actions/checkout leaves the
    # mounted workspace owned by the host runner. Scope the ownership exception
    # to this invocation instead of changing global Git configuration.
    return ["git", "-c", f"safe.directory={Path.cwd().resolve()}", *args]


def git(*args: str, check: bool = True) -> bytes:
    return subprocess.run(
        git_command(*args),
        check=check,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    ).stdout


def nul_paths(output: bytes) -> set[Path]:
    return {Path(item.decode("utf-8", "surrogateescape")) for item in output.split(b"\0") if item}


def touch(path: Path, mtime: float) -> None:
    try:
        os.utime(path, (mtime, mtime), follow_symlinks=False)
    except FileNotFoundError:
        pass


def restore(target_dir: Path) -> int:
    marker = target_dir / MARKER_NAME
    if not marker.is_file() or not (target_dir / "release").is_dir():
        print("Cargo target cache has no trusted source revision; using normal freshness checks")
        return 0

    base_revision = marker.read_text(encoding="ascii").strip()
    if len(base_revision) != 40 or any(char not in "0123456789abcdef" for char in base_revision):
        print("Cargo target cache revision marker is invalid; using normal freshness checks")
        return 0

    if subprocess.run(
        git_command("merge-base", "--is-ancestor", base_revision, "HEAD"),
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    ).returncode != 0:
        print("Cargo target cache revision is not an ancestor of HEAD; using normal freshness checks")
        return 0

    tracked = nul_paths(git("ls-files", "-z"))
    changed = nul_paths(git("diff", "--name-only", "-z", base_revision, "HEAD", "--"))
    changed.update(nul_paths(git("diff", "--name-only", "-z", "HEAD", "--")))
    changed.update(nul_paths(git("diff", "--cached", "--name-only", "-z", "HEAD", "--")))
    changed.update(nul_paths(git("ls-files", "--others", "--exclude-standard", "-z")))

    for path in tracked:
        touch(path, OLD_MTIME)
    changed_mtime = time.time()
    for path in changed:
        touch(path, changed_mtime)

    print(
        f"Prepared Cargo target cache from {base_revision[:12]}: "
        f"{len(changed)} changed paths, {len(tracked) - len(changed & tracked)} unchanged tracked paths"
    )
    return 0


def record(target_dir: Path) -> int:
    target_dir.mkdir(parents=True, exist_ok=True)
    revision = git("rev-parse", "HEAD").decode("ascii").strip()
    marker = target_dir / MARKER_NAME
    temporary = target_dir / f"{MARKER_NAME}.tmp"
    temporary.write_text(f"{revision}\n", encoding="ascii")
    temporary.replace(marker)
    print(f"Recorded Cargo target cache source revision {revision[:12]}")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=("restore", "record"))
    parser.add_argument("--target-dir", type=Path, default=Path("target"))
    args = parser.parse_args()
    return restore(args.target_dir) if args.mode == "restore" else record(args.target_dir)


if __name__ == "__main__":
    sys.exit(main())
