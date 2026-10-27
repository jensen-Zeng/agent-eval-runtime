param(
    [string]$BaseUrl = 'http://127.0.0.1:8787',
    [string]$JobFile = 'examples/v0.2-batch.json',
    [string]$Output = 'artifacts/batches/v02-demo.json',
    [string]$Timeout = '10m'
)

$ErrorActionPreference = 'Stop'
$projectRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
Set-Location -LiteralPath $projectRoot
$runtime = Join-Path $projectRoot 'bin\runtime.exe'
if (-not (Test-Path -LiteralPath $runtime -PathType Leaf)) {
    throw 'runtime.exe is missing; build it before running this demo.'
}
$jobPath = [System.IO.Path]::GetFullPath((Join-Path $projectRoot $JobFile))
if (-not (Test-Path -LiteralPath $jobPath -PathType Leaf)) {
    throw "Batch input file not found: $jobPath"
}
$outputPath = [System.IO.Path]::GetFullPath((Join-Path $projectRoot $Output))
$outputDirectory = Split-Path -Parent $outputPath
New-Item -ItemType Directory -Path $outputDirectory -Force | Out-Null

Write-Host "Submitting batch to $BaseUrl"
Write-Host "Input:  $jobPath"
Write-Host "Output: $outputPath"
Write-Host 'This script does not start PostgreSQL, Docker, or the runtime service.'
& $runtime batch --url $BaseUrl --file $jobPath --output $outputPath --wait --timeout $Timeout
exit $LASTEXITCODE