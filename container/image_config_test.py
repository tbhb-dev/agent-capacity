# SPDX-License-Identifier: Apache-2.0
# Copyright Tony Burns

"""Check the runtime defaults in the built OCI image layout."""

import json
from pathlib import Path
import sys
import unittest


layout = Path(sys.argv[1])


def read_blob(digest):
    """Read a JSON blob through its OCI digest."""
    algorithm, value = digest.split(":", 1)
    return json.loads((layout / "blobs" / algorithm / value).read_text())


index = json.loads((layout / "index.json").read_text())
manifest = read_blob(index["manifests"][0]["digest"])
config = read_blob(manifest["config"]["digest"])["config"]
check = unittest.TestCase()
check.assertEqual(config["Entrypoint"], ["/agent-capacity", "serve"])
check.assertEqual(config["User"], "65532:65532")
check.assertEqual(
    [value for value in config["Env"] if value.startswith("AGENT_CAPACITY_SPOOL_DIR=")],
    ["AGENT_CAPACITY_SPOOL_DIR=/spool"],
)
