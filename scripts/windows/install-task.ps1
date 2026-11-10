param(
    [string]$Executable = (Join-Path $PSScriptRoot "..\..\bin\keypoold.exe"),
    [string]$Arguments = "serve",
    [string]$TaskName = "KeyPool",
    [string]$WorkingDirectory = (Join-Path $PSScriptRoot "..\.."),
    [switch]$Remove
)

$ErrorActionPreference = "Stop"

if (-not (Get-Command Register-ScheduledTask -ErrorAction SilentlyContinue)) {
    throw "O modulo ScheduledTasks nao esta disponivel nesta instalacao do Windows."
}

if ($Remove) {
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue
    Write-Host "Tarefa '$TaskName' removida (se existia)."
    exit 0
}

$resolvedExe = (Resolve-Path -LiteralPath $Executable -ErrorAction Stop).Path
$resolvedWorkingDirectory = (Resolve-Path -LiteralPath $WorkingDirectory -ErrorAction Stop).Path
$currentUser = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
$action = New-ScheduledTaskAction -Execute $resolvedExe -Argument $Arguments -WorkingDirectory $resolvedWorkingDirectory
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $currentUser
$settings = New-ScheduledTaskSettingsSet -StartWhenAvailable -MultipleInstances IgnoreNew -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 5 -RestartInterval ([TimeSpan]::FromMinutes(1)) -Hidden
$principal = New-ScheduledTaskPrincipal -UserId $currentUser -LogonType Interactive -RunLevel Limited

Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Settings $settings -Principal $principal -Description "KeyPool local gateway" -Force | Out-Null
Write-Host "Tarefa '$TaskName' instalada para '$currentUser'."
Write-Host "A tarefa e executada oculta, no contexto do usuario atual, sem elevacao."
Write-Host "Diretorio de trabalho: $resolvedWorkingDirectory"
Write-Host "Falhas de inicializacao: ate 5 reinicios, com intervalo de 1 minuto."
Write-Host "Confirme que KEYPOOL_DATABASE_URL, KEYPOOL_API_KEY e demais variaveis estao definidas no ambiente do usuario."
