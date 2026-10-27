#!/usr/bin/env bash
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bash "$repo/scripts/build.sh"
config="$repo/configs/local.json"
task="$repo/tasks/fixtures/go-parser-001/task.json"
artifacts="$repo/artifacts"
"$repo/bin/runtime" prepare --project "$repo" --config "$config"
"$repo/bin/runtime" doctor --config "$config"
"$repo/bin/runtime" run --task "$task" --project "$repo" --agent scripted --script success --artifacts "$artifacts" --config "$config"
