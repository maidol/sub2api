"""Make refresh.py and the vendored exporter in third_party/ importable in tests."""

import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
for path in (HERE, HERE / "third_party"):
    if str(path) not in sys.path:
        sys.path.insert(0, str(path))
