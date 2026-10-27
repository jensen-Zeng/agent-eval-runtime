$ErrorActionPreference = 'Stop'
$env:GOTOOLCHAIN = 'local'
$repo = Split-Path -Parent $PSScriptRoot
$env:GOCACHE = Join-Path $env:TEMP 'agenteval-go-build-cache'
$go = Get-Command go -ErrorAction SilentlyContinue
if ($go) { $goExe = $go.Source } else {
  $candidate = Join-Path $env:LOCALAPPDATA 'Programs\Go\bin\go.exe'
  if (-not (Test-Path $candidate)) { throw 'Go not found. Install Go 1.27.1 or open a terminal with Go on PATH.' }
  $goExe = $candidate
}
New-Item -ItemType Directory -Force (Join-Path $repo 'bin') | Out-Null
Push-Location $repo
try {
  & $goExe build -trimpath -o (Join-Path $repo 'bin\runtime.exe') ./cmd/runtime
  $buildExitCode = $LASTEXITCODE
} finally {
  Pop-Location
}
if ($buildExitCode -ne 0) { exit $buildExitCode }