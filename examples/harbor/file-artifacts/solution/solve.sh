#!/bin/sh
set -eu
cd "${1:-/app}"
python3 - <<'PY'
import json
from pathlib import Path
value = int(Path("input.txt").read_text(encoding="utf-8").strip()) * 2
Path("answer.json").write_text(json.dumps({"value": value}), encoding="utf-8")
Path("answer.bin").write_bytes(bytes([0, 255, 10, 128]))
PY
