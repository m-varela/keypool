param(
    [string]$Path = (Join-Path $env:LOCALAPPDATA "keypool")
)

$ErrorActionPreference = "Stop"
$FullPath = (Resolve-Path -LiteralPath $Path -ErrorAction Stop).Path
$Identity = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
$Icacls = (Get-Command icacls.exe -ErrorAction Stop).Source
& $Icacls $FullPath /reset
if ($LASTEXITCODE -ne 0) { throw "Could not reset ACL entries" }
& $Icacls $FullPath /inheritance:r
if ($LASTEXITCODE -ne 0) { throw "Could not disable inherited ACL entries" }
& $Icacls $FullPath /grant:r "${Identity}:(OI)(CI)(F)"
if ($LASTEXITCODE -ne 0) { throw "Could not grant the current user access to the data directory" }
Write-Host "Data directory and child files are restricted to $Identity"
