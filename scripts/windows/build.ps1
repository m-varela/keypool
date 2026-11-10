$ErrorActionPreference = "Stop"
$Root = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
Push-Location $Root
try {
    gofmt -w .
    if ($LASTEXITCODE -ne 0) { throw "gofmt failed" }
    go vet ./...
    if ($LASTEXITCODE -ne 0) { throw "go vet failed" }
    go build ./...
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }
    New-Item -ItemType Directory -Force -Path "bin" | Out-Null
    go build -o bin/keypoold.exe ./cmd/keypoold
    if ($LASTEXITCODE -ne 0) { throw "keypoold build failed" }
    go build -o bin/keypoolctl.exe ./cmd/keypoolctl
    if ($LASTEXITCODE -ne 0) { throw "keypoolctl build failed" }
} finally {
    Pop-Location
}
