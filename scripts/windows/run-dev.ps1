param(
    [string]$Config = "configs/examples/keypool.dev.yaml"
)

$ErrorActionPreference = "Stop"
$Root = (Resolve-Path (Join-Path $PSScriptRoot "../..")).Path
if (-not [System.IO.Path]::IsPathRooted($Config)) {
    $Config = Join-Path $Root $Config
}
if (-not (Test-Path -LiteralPath $Config -PathType Leaf)) {
    throw "Development config not found: $Config"
}
Push-Location $Root
try {
    go run ./cmd/keypoold serve --dev $Config
    if ($LASTEXITCODE -ne 0) {
        throw "keypoold serve --dev failed with exit code $LASTEXITCODE"
    }
} finally {
    Pop-Location
}
