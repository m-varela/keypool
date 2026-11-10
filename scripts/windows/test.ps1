param(
    [switch]$Race,
    [switch]$Integration
)

$ErrorActionPreference = "Stop"
$Root = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
Push-Location $Root
try {
    go test ./...
    if ($LASTEXITCODE -ne 0) { throw "go test failed" }
    if ($Race) {
        go test -race ./internal/sched/... ./internal/router/... ./internal/gateway/... ./internal/store/... ./internal/usage/...
        if ($LASTEXITCODE -ne 0) { throw "go test -race failed" }
    }
    if ($Integration) {
        if ([string]::IsNullOrWhiteSpace($env:KEYPOOL_TEST_DATABASE_URL)) {
            throw "Set KEYPOOL_TEST_DATABASE_URL to a disposable PostgreSQL database before -Integration."
        }
        go test -tags integration ./internal/store/pgstore
        if ($LASTEXITCODE -ne 0) { throw "PostgreSQL integration tests failed" }
    }
} finally {
    Pop-Location
}
