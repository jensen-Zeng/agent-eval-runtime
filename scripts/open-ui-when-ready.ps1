param(
  [Parameter(Mandatory=$true)][string]$ConfigFile,
  [ValidateRange(1,600)][int]$TimeoutSeconds = 300
)
$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
Push-Location $repo
try {
  $config = Get-Content -LiteralPath $ConfigFile -Raw -Encoding utf8 | ConvertFrom-Json
  $listen = [string]$config.server.listen
  if (-not $listen) { throw 'server.listen is missing' }
  if ($listen -match '^(127\.0\.0\.1|localhost):([0-9]+)$') { $hostName = $Matches[1]; $port = $Matches[2] }
  elseif ($listen -match '^\[::1\]:([0-9]+)$') { $hostName = '[::1]'; $port = $Matches[1] }
  else { throw 'The console launcher only opens an explicitly configured loopback Runtime listener.' }
  $baseUrl = "http://${hostName}:${port}"
  $readyUrl = "$baseUrl/readyz"
  $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
  while ([DateTime]::UtcNow -lt $deadline) {
    try {
      $response = Invoke-WebRequest -Uri $readyUrl -Method Get -TimeoutSec 2 -UseBasicParsing
      if ($response.StatusCode -ge 200 -and $response.StatusCode -lt 300) {
        Start-Process $baseUrl | Out-Null
        exit 0
      }
    } catch { }
    Start-Sleep -Milliseconds 900
  }
} catch { }
finally { Pop-Location }
exit 0
