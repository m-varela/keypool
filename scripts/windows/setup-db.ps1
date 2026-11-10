param(
    [string]$HostName = "127.0.0.1",
    [int]$Port = 5432,
    [string]$AdminUser = "postgres",
    [string]$Role = "keypool",
    [string]$Database = "keypool"
)

$ErrorActionPreference = "Stop"
foreach ($value in @($Role, $Database)) {
    if ($value -notmatch '^[a-z_][a-z0-9_]*$') { throw "Unsafe PostgreSQL identifier: $value" }
}
$Psql = (Get-Command psql -ErrorAction Stop).Source
$SecurePassword = Read-Host "PostgreSQL admin password (empty if not required)" -AsSecureString
$BSTR = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($SecurePassword)
try {
    $env:PGPASSWORD = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($BSTR)
    $RoleResult = & $Psql -h $HostName -p $Port -U $AdminUser -d postgres -Atqc "SELECT 1 FROM pg_roles WHERE rolname='$Role'"
    if ($LASTEXITCODE -ne 0) { throw "Could not query PostgreSQL roles" }
    $RoleExists = ([string]$RoleResult).Trim() -eq "1"
    if (-not $RoleExists) {
        & $Psql -h $HostName -p $Port -U $AdminUser -d postgres -v ON_ERROR_STOP=1 -c "CREATE ROLE $Role LOGIN"
        if ($LASTEXITCODE -ne 0) { throw "Could not create PostgreSQL role" }
    }
    $DatabaseResult = & $Psql -h $HostName -p $Port -U $AdminUser -d postgres -Atqc "SELECT 1 FROM pg_database WHERE datname='$Database'"
    if ($LASTEXITCODE -ne 0) { throw "Could not query PostgreSQL databases" }
    $DatabaseExists = ([string]$DatabaseResult).Trim() -eq "1"
    if (-not $DatabaseExists) {
        & $Psql -h $HostName -p $Port -U $AdminUser -d postgres -v ON_ERROR_STOP=1 -c "CREATE DATABASE $Database OWNER $Role"
        if ($LASTEXITCODE -ne 0) { throw "Could not create PostgreSQL database" }
    }
    Write-Host "Set the password for the application role. psql will prompt without putting it in command-line arguments."
    & $Psql -h $HostName -p $Port -U $AdminUser -d postgres -v ON_ERROR_STOP=1 -c "\password $Role"
    if ($LASTEXITCODE -ne 0) { throw "Could not set application role password" }
} finally {
    Remove-Item Env:\PGPASSWORD -ErrorAction SilentlyContinue
    [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($BSTR)
    $SecurePassword.Dispose()
}
Write-Host ("Database is ready. Set KEYPOOL_DATABASE_URL to postgres://{0}:<password>@{1}:{2}/{3}?sslmode=disable" -f $Role, $HostName, $Port, $Database)
