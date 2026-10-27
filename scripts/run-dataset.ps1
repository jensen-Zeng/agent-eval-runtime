param(
  [Parameter(Mandatory=$true)][string]$ImportReport,
  [string[]]$Models = @(),
  [ValidateRange(1,100)][int]$Repeats = 2,
  [switch]$IncludeBaseline,
  [string]$Name = 'dataset-repeat',
  [string]$ApiUrl = 'http://127.0.0.1:8787',
  [string]$OutputDirectory = '',
  [string]$ArtifactRoot = 'artifacts',
  [string]$Timeout = '30m'
)
$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
Push-Location $repo
try {
  $runtime = Join-Path $repo 'bin/runtime.exe'
  if (-not (Test-Path -LiteralPath $runtime)) { throw 'Build the CLI first with scripts/build.ps1.' }
  if (-not $OutputDirectory) {
    $suffix = (Get-Date -Format 'yyyyMMdd-HHmmss') + '-' + [guid]::NewGuid().ToString('N').Substring(0,8)
    $OutputDirectory = Join-Path $repo ('artifacts/dataset-runs/' + $suffix)
  }
  if (Test-Path -LiteralPath $OutputDirectory) { throw 'Output directory exists; use a new directory or runtime experiment --resume for the original experiment.' }
  New-Item -ItemType Directory -Path $OutputDirectory | Out-Null
  $matrix = Join-Path $OutputDirectory 'matrix.json'
  $experiment = Join-Path $OutputDirectory 'experiment.json'
  $csv = Join-Path $OutputDirectory 'summary.csv'
  $bundles = Join-Path $OutputDirectory 'bundles'
  $matrixArgs = @('dataset','matrix','--report',$ImportReport,'--output',$matrix,'--name',$Name,'--repeats',"$Repeats")
  if ($Models.Count -gt 0) { $matrixArgs += @('--models',($Models -join ',')) }
  if ($IncludeBaseline) { $matrixArgs += '--baseline' }
  & $runtime @matrixArgs
  if ($LASTEXITCODE -ne 0) { throw 'Matrix generation failed; inspect the import report.' }
  & $runtime experiment --url $ApiUrl --file $matrix --output $experiment --csv $csv --wait --timeout $Timeout
  $experimentCode = $LASTEXITCODE
  if ($experimentCode -notin @(0,2)) { throw "Experiment interrupted/failed. Resume with runtime experiment --url `"$ApiUrl`" --file `"$matrix`" --output `"$experiment`" --csv `"$csv`" --wait --resume." }
  & $runtime dataset export --experiment $experiment --artifacts $ArtifactRoot --output $bundles
  $exportCode = $LASTEXITCODE
  if ($exportCode -notin @(0,2)) { throw 'Bundle export failed; the experiment manifest and original run artifacts are preserved.' }
  Write-Host "Experiment: $experiment"
  Write-Host "Grouped CSV: $csv"
  Write-Host "Evidence bundles: $bundles"
  if ($experimentCode -eq 2 -or $exportCode -eq 2) { exit 2 }
} finally { Pop-Location }
