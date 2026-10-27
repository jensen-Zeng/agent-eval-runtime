$ErrorActionPreference = 'Stop'
& (Join-Path $PSScriptRoot 'build.ps1')
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
$repo = Split-Path -Parent $PSScriptRoot
$exe = Join-Path $repo 'bin\runtime.exe'
$config = Join-Path $repo 'configs\local.json'
$task = Join-Path $repo 'tasks\fixtures\go-parser-001\task.json'
$artifacts = Join-Path $repo 'artifacts'
& $exe prepare --project $repo --config $config
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
& $exe doctor --config $config
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
& $exe run --task $task --project $repo --agent scripted --script success --artifacts $artifacts --config $config
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }