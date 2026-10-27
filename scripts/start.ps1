param(
  [ValidateRange(1,8)][int]$Workers = 2,
  [ValidateRange(1,1000)][int]$QueueCapacity = 32,
  [string]$ConfigFile = 'configs/local.json',
  [switch]$SkipPrepare
)
$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$runtime = Join-Path $repo 'bin/runtime.exe'
Push-Location $repo
try {
  if (-not (Get-Command docker -ErrorAction SilentlyContinue)) { throw 'Docker CLI not found' }
  docker info --format '{{.OSType}}' | Out-Null
  if ($LASTEXITCODE -ne 0) { throw 'Start Docker Desktop with a Linux engine first' }
  if (-not $env:POSTGRES_PASSWORD) {
    $securePassword = Read-Host 'PostgreSQL password (reuse the existing database password)' -AsSecureString
    $env:POSTGRES_PASSWORD = [System.Net.NetworkCredential]::new('', $securePassword).Password
  }
  if (-not $env:POSTGRES_PASSWORD) { throw 'PostgreSQL password cannot be empty' }
  & (Join-Path $PSScriptRoot 'build.ps1')
  if ($LASTEXITCODE -ne 0) { throw 'Build failed' }
  docker compose -p agenteval-runtime -f deployments/compose.postgres.yml up -d --wait --wait-timeout 90
  if ($LASTEXITCODE -ne 0) { throw 'PostgreSQL startup failed; existing volume password must match' }
  $encodedPassword = [uri]::EscapeDataString($env:POSTGRES_PASSWORD)
  $env:AGENTEVAL_DATABASE_URL = "postgres://agenteval:$encodedPassword@127.0.0.1:5432/agenteval?sslmode=disable"
  if ($SkipPrepare) {
    & $runtime doctor --config $ConfigFile
    if ($LASTEXITCODE -ne 0) { throw 'The prebuilt dependency image is not ready. Build it before starting the service.' }
  } else {
    & $runtime prepare --project . --config $ConfigFile
    if ($LASTEXITCODE -ne 0) { throw 'Sandbox image preparation failed' }
  }
  & $runtime serve --project . --config $ConfigFile --workers $Workers --queue-capacity $QueueCapacity
  $serveExit = $LASTEXITCODE
} finally { Pop-Location }
exit $serveExit
