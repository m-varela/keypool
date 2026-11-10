param(
    [switch]$Race
)

$ErrorActionPreference = "Stop"
$Root = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
Push-Location $Root
try {
    go test ./...
    if ($LASTEXITCODE -ne 0) { throw "go test failed" }
    if ($Race) {
        go test -race ./internal/sched/... ./internal/router/... ./internal/gateway/... ./internal/store/... ./cmd/...
        if ($LASTEXITCODE -ne 0) { throw "go test -race failed" }
    }
} finally {
    Pop-Location
}
