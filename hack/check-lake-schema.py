#!/usr/bin/env python3
# SPDX-License-Identifier: MIT

"""Verify the vendored Lake schema against its pinned upstream source or a local copy."""

import hashlib
import json
from pathlib import Path
import subprocess
import sys


def main():
    root = Path(__file__).resolve().parent.parent
    assets = root / "internal/controller/assets"
    source = json.loads((assets / "lake-schema.source.json").read_text())
    vendored = (assets / "lake-schema.sql").read_bytes()
    if hashlib.sha256(vendored).hexdigest() != source["sha256"]:
        raise SystemExit("vendored schema differs from the recorded upstream SHA-256")
    if len(sys.argv) > 2:
        raise SystemExit("usage: hack/check-lake-schema.py [path/to/lake/db/schema.sql]")
    if len(sys.argv) == 2:
        upstream = Path(sys.argv[1]).read_bytes()
    else:
        endpoint = (
            f'repos/{source["repository"]}/contents/{source["path"]}'
            f'?ref={source["commit"]}'
        )
        upstream = subprocess.check_output(
            ["gh", "api", "-H", "Accept: application/vnd.github.raw+json", endpoint]
        )
    if vendored != upstream:
        raise SystemExit("vendored schema is not byte-identical to the source")
    print(f'byte-identical: {source["repository"]}@{source["commit"]} {source["path"]}')
    print(f'SHA-256: {source["sha256"]}')


if __name__ == "__main__":
    main()
