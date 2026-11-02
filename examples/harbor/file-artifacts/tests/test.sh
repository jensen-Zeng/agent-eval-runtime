#!/bin/sh
set -u
mkdir -p /logs/verifier
printf '{"reward":0.0,"valid":0.0}\n' > /logs/verifier/reward.json
python3 /tests/grade.py
exit 0
