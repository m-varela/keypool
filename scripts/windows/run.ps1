param(
    [string]$Config = "configs/examples/keypool.json"
)

$ErrorActionPreference = "Stop"
$Root = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
Push-Location $Root
try {
    go run ./cmd/keypoold serve --config $Config
    if ($LASTEXITCODE -ne 0) {
        throw "keypoold serve failed with exit code $LASTEXITCODE"
    }
} finally {
    Pop-Location
}
