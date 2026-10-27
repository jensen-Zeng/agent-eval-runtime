$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$env:GOCACHE = Join-Path $env:TEMP 'agenteval-go-build-cache'
$go = Get-Command go -ErrorAction SilentlyContinue
if ($go) { $goExe = $go.Source } else {
  $candidate = Join-Path $env:LOCALAPPDATA 'Programs\Go\bin\go.exe'
  if (-not (Test-Path $candidate)) { throw 'Go not found. Install Go 1.27.1 or open a terminal with Go on PATH.' }
  $goExe = $candidate
}
Push-Location $repo
try {
  & $goExe test ./... -count=1 -timeout 10m
  if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
  & $goExe vet ./...
  if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
} finally { Pop-Location }