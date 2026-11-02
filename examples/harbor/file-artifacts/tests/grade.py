"""Owned demonstration grader. Missing/malformed submissions are valid zeros."""
import json
from pathlib import Path

try:
    answer = json.loads(Path("/app/answer.json").read_text(encoding="utf-8"))
    number_ok = type(answer.get("value")) in (int, float) and answer["value"] == 42
    binary_ok = Path("/app/answer.bin").read_bytes() == bytes([0, 255, 10, 128])
    reward = (int(number_ok) + int(binary_ok)) / 2
except (OSError, ValueError, TypeError, AttributeError):
    reward = 0.0
Path("/logs/verifier/reward.json").write_text(
    json.dumps({"reward": reward, "valid": 1.0}) + "\n", encoding="utf-8"
)
