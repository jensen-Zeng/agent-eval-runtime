param(
  [string]$ConfigFile = ''
)
$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
Push-Location $repo
try {
  if (-not $ConfigFile) {
    if (Test-Path -LiteralPath (Join-Path $repo 'configs/e3-local.json')) { $ConfigFile = 'configs/e3-local.json' }
    else { $ConfigFile = 'configs/local.json' }
  }
  if (-not (Test-Path -LiteralPath $ConfigFile)) { throw "Runtime config was not found: $ConfigFile" }
  $helper = Join-Path $PSScriptRoot 'open-ui-when-ready.ps1'
  $powershell = (Get-Command powershell.exe -ErrorAction Stop).Source
  Start-Process -FilePath $powershell -WindowStyle Hidden -ArgumentList @('-NoProfile','-ExecutionPolicy','Bypass','-File',('"' + $helper + '"'),'-ConfigFile',('"' + $ConfigFile + '"')) | Out-Null
  & (Join-Path $PSScriptRoot 'start.ps1') -ConfigFile $ConfigFile -SkipPrepare
  if ($LASTEXITCODE -ne 0) { throw 'AgentEval Runtime stopped with an error. Check the message in this window.' }
} finally { Pop-Location }
