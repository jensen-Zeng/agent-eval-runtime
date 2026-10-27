#!/usr/bin/env bash
set -euo pipefail
export GOTOOLCHAIN=local
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mkdir -p "$repo/bin"
cd "$repo"
go build -trimpath -o bin/runtime ./cmd/runtime