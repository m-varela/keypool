<#
.SYNOPSIS
Bootstrap do KeyPool no Windows: prepara o ambiente e sobe o daemon.

.DESCRIPTION
Primeira execucao (nao existe state.json no diretorio de dados):
  1. valida Go, configuracao JSON, project_file e porta loopback;
  2. compila bin\keypoold.exe e bin\keypoolctl.exe;
  3. coleta as chaves dos provedores (variaveis de ambiente ou prompt oculto);
  4. define a API key local kp_... (existente no ambiente ou gerada aqui);
  5. cria o diretorio de dados e restringe a ACL ao usuario atual;
  6. executa "keypoold serve --config <arquivo>" em primeiro plano.

Execucoes seguintes (state.json ja existe):
  exige KEYPOOL_API_KEY, roda "keypoold doctor" e executa "keypoold serve".

Seguranca: nenhum segredo e passado como argumento de processo nem gravado em
log. Variaveis definidas por este script existem apenas no processo e voltam ao
valor original (ou sao removidas) ao final. A unica excecao e -PersistApiKey, que grava somente a API
key local kp_... no ambiente do usuario.

.PARAMETER Config
JSON de configuracao inicial (usado somente no primeiro boot).

.PARAMETER Dpapi
Protege a master key com DPAPI (perfil do usuario atual). Incompativel com
KEYPOOL_MASTER_KEY.

.PARAMETER SkipBuild
Nao recompila os binarios se bin\keypoold.exe ja existir.

.PARAMETER CheckOnly
Apenas valida e relata. Nao cria diretorios, nao pergunta nada e nao inicia o
daemon.

.PARAMETER PersistApiKey
Grava KEYPOOL_API_KEY no ambiente do usuario (nivel User) sem perguntar.

.PARAMETER NonInteractive
Nunca pergunta nada; falha se faltar alguma informacao.

.EXAMPLE
Set-ExecutionPolicy -Scope Process Bypass
.\scripts\windows\bootstrap.ps1
#>
param(
    [string]$Config = "configs/examples/keypool.json",
    [switch]$Dpapi,
    [switch]$SkipBuild,
    [switch]$CheckOnly,
    [switch]$PersistApiKey,
    [switch]$NonInteractive
)

$ErrorActionPreference = "Stop"
$Root = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
$script:OriginalEnv = @{}

# ---------------------------------------------------------------- utilitarios

function Write-Step([string]$Text) { Write-Host ""; Write-Host "== $Text" -ForegroundColor Cyan }
function Write-Ok([string]$Text)   { Write-Host "  [OK]    $Text" -ForegroundColor Green }
function Write-Info([string]$Text) { Write-Host "  [INFO]  $Text" }
function Write-Warn([string]$Text) { Write-Host "  [AVISO] $Text" -ForegroundColor Yellow }

# Guarda o valor original (do processo) para restaura-lo ao final.
function Set-ProcessEnv([string]$Name, [string]$Value) {
    if (-not $script:OriginalEnv.ContainsKey($Name)) {
        $script:OriginalEnv[$Name] = [Environment]::GetEnvironmentVariable($Name, "Process")
    }
    [Environment]::SetEnvironmentVariable($Name, $Value, "Process")
}

function Read-SecretText([string]$Prompt) {
    $secure = Read-Host -Prompt $Prompt -AsSecureString
    $bstr = [System.Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure)
    try {
        return ([System.Runtime.InteropServices.Marshal]::PtrToStringBSTR($bstr)).Trim()
    } finally {
        [System.Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr)
    }
}

# Mesma regra do daemon (cmd/keypoold/app.go, dataDirFor).
function Get-KeyPoolDataDir {
    if ($env:KEYPOOL_DATA_DIR) { return $env:KEYPOOL_DATA_DIR }
    if ($env:LOCALAPPDATA) { return (Join-Path $env:LOCALAPPDATA "keypool") }
    if ($env:XDG_CONFIG_HOME) { return (Join-Path $env:XDG_CONFIG_HOME "keypool") }
    return (Join-Path $HOME ".keypool")
}

# Mesmo formato do daemon (cmd/keypoold/app.go, newAPIKey):
# "kp_" + base32 minusculo, sem padding, de 32 bytes aleatorios.
function New-KeyPoolApiKey {
    $raw = New-Object byte[] 32
    $rng = New-Object System.Security.Cryptography.RNGCryptoServiceProvider
    try { $rng.GetBytes($raw) } finally { $rng.Dispose() }
    $alphabet = "abcdefghijklmnopqrstuvwxyz234567"
    $sb = New-Object System.Text.StringBuilder
    $buffer = 0
    $bits = 0
    foreach ($b in $raw) {
        $buffer = ($buffer -shl 8) -bor [int]$b
        $bits += 8
        while ($bits -ge 5) {
            $idx = ($buffer -shr ($bits - 5)) -band 31
            [void]$sb.Append($alphabet[$idx])
            $bits -= 5
        }
        $buffer = $buffer -band ((1 -shl $bits) - 1)
    }
    if ($bits -gt 0) {
        $idx = ($buffer -shl (5 - $bits)) -band 31
        [void]$sb.Append($alphabet[$idx])
    }
    return ("kp_" + $sb.ToString())
}

function Get-ApiKeyFromEnvironment {
    if ($env:KEYPOOL_API_KEY -and $env:KEYPOOL_API_KEY.Trim()) { return $env:KEYPOOL_API_KEY.Trim() }
    $user = [Environment]::GetEnvironmentVariable("KEYPOOL_API_KEY", "User")
    if ($user -and $user.Trim()) { return $user.Trim() }
    return $null
}

# O bootstrap importa <PREFIXO>_API_KEYS, <PREFIXO>_API_KEY e <PREFIXO>_API_KEY_N.
function Test-ProviderKeysInEnvironment([string]$Prefix) {
    $pattern = "^" + [regex]::Escape($Prefix) + "_API_KEY(S|_\d+)?$"
    foreach ($item in (Get-ChildItem Env:)) {
        if (($item.Name -match $pattern) -and (-not [string]::IsNullOrWhiteSpace($item.Value))) {
            return $true
        }
    }
    return $false
}

function Test-ListenAddress([string]$Listen) {
    $idx = $Listen.LastIndexOf(":")
    if ($idx -lt 1) { throw "Valor 'listen' invalido na configuracao: '$Listen'" }
    $hostPart = $Listen.Substring(0, $idx)
    $port = 0
    if (-not [int]::TryParse($Listen.Substring($idx + 1), [ref]$port)) {
        throw "Porta invalida em 'listen': '$Listen'"
    }
    if ($hostPart -ne "127.0.0.1" -and $hostPart -ne "localhost") {
        throw "O KeyPool so escuta em loopback (127.0.0.1 ou localhost); recebido '$hostPart'."
    }
    $listener = New-Object System.Net.Sockets.TcpListener([System.Net.IPAddress]::Loopback, $port)
    try {
        $listener.Start()
        $listener.Stop()
    } catch {
        throw "A porta $port ja esta em uso. Ha outro keypoold em execucao? Altere 'listen' ou encerre o processo."
    }
}

function Invoke-Native([string]$Label, [scriptblock]$Command) {
    & $Command
    if ($LASTEXITCODE -ne 0) { throw "$Label falhou com codigo $LASTEXITCODE" }
}

# ----------------------------------------------------------------- principal

Push-Location $Root
try {
    # 1. Go ------------------------------------------------------------------
    Write-Step "Verificando o toolchain Go"
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        throw "Go nao encontrado no PATH. Instale Go 1.26 ou superior (https://go.dev/dl/) e abra um novo terminal."
    }
    $goVersion = (& go version) -join " "
    if ($goVersion -match "go(\d+)\.(\d+)") {
        $major = [int]$Matches[1]
        $minor = [int]$Matches[2]
        if (($major -lt 1) -or (($major -eq 1) -and ($minor -lt 26))) {
            Write-Warn "go.mod exige Go 1.26+; encontrado: $goVersion. O build pode falhar ou tentar baixar o toolchain."
        } else {
            Write-Ok $goVersion
        }
    } else {
        Write-Warn "Nao foi possivel interpretar a versao do Go: $goVersion"
    }

    if ($Dpapi -and $env:KEYPOOL_MASTER_KEY) {
        throw "-Dpapi e KEYPOOL_MASTER_KEY sao mutuamente exclusivos."
    }

    # 2. Estado local ----------------------------------------------------------
    Write-Step "Verificando o estado local"
    $dataDir = Get-KeyPoolDataDir
    $statePath = Join-Path $dataDir "state.json"
    $firstBoot = -not (Test-Path -LiteralPath $statePath)
    Write-Info "Diretorio de dados: $dataDir"
    if ($firstBoot) {
        Write-Info "state.json nao existe: PRIMEIRA EXECUCAO (bootstrap completo)."
    } else {
        Write-Ok "state.json encontrado: execucao normal (a opcao --config e ignorada)."
    }

    # 3. Configuracao inicial (somente primeiro boot) ----------------------------
    $providers = @()
    $cfg = $null
    $configFull = $null
    if ($firstBoot) {
        Write-Step "Validando a configuracao inicial"
        if ([System.IO.Path]::IsPathRooted($Config)) { $configCandidate = $Config } else { $configCandidate = Join-Path $Root $Config }
        if (-not (Test-Path -LiteralPath $configCandidate)) { throw "Arquivo de configuracao nao encontrado: $configCandidate" }
        $configFull = (Resolve-Path -LiteralPath $configCandidate).Path
        try {
            $cfg = Get-Content -LiteralPath $configFull -Raw -Encoding UTF8 | ConvertFrom-Json
        } catch {
            throw "JSON invalido em ${configFull}: $($_.Exception.Message)"
        }
        Write-Ok "Configuracao: $configFull"

        if ($cfg.project_file) {
            $projectFull = Join-Path (Split-Path -Parent $configFull) $cfg.project_file
            if (-not (Test-Path -LiteralPath $projectFull)) { throw "project_file nao encontrado: $projectFull" }
            Write-Ok "project_file: $projectFull"
        }
        if ($cfg.default_project) { Write-Info "Projeto padrao: $($cfg.default_project)" }

        $listen = "127.0.0.1:8787"
        if ($cfg.listen) { $listen = [string]$cfg.listen }
        Test-ListenAddress $listen
        Write-Ok "Porta livre e em loopback: $listen"

        if ($cfg.providers) {
            foreach ($prop in $cfg.providers.PSObject.Properties) {
                $p = $prop.Value
                if ($p.enabled -eq $false) { continue }
                if (-not $p.env_prefix) {
                    Write-Warn "Provedor '$($prop.Name)' sem env_prefix: nenhuma chave sera importada do ambiente."
                    continue
                }
                $providers += [pscustomobject]@{ Name = $prop.Name; Prefix = ([string]$p.env_prefix).ToUpper() }
            }
        }
        if ($providers.Count -eq 0) { throw "Nenhum provedor habilitado com env_prefix na configuracao." }
    }

    # 4. Build -------------------------------------------------------------------
    Write-Step "Compilando os binarios"
    $daemon = Join-Path $Root "bin\keypoold.exe"
    $ctl = Join-Path $Root "bin\keypoolctl.exe"
    if ($SkipBuild -and (Test-Path -LiteralPath $daemon)) {
        Write-Info "Build ignorado (-SkipBuild); usando bin\keypoold.exe existente."
    } else {
        New-Item -ItemType Directory -Force -Path "bin" | Out-Null
        Invoke-Native "go build keypoold" { go build -o bin/keypoold.exe ./cmd/keypoold }
        Invoke-Native "go build keypoolctl" { go build -o bin/keypoolctl.exe ./cmd/keypoolctl }
        Write-Ok "bin\keypoold.exe e bin\keypoolctl.exe prontos."
    }

    # 5a. Execucao normal ------------------------------------------------------------
    if (-not $firstBoot) {
        Write-Step "Verificando a API key local"
        $apiKey = Get-ApiKeyFromEnvironment
        if (-not $apiKey) {
            throw ("KEYPOOL_API_KEY nao esta definida. Defina a chave emitida no primeiro boot " +
                   "(ex.: `$env:KEYPOOL_API_KEY = 'kp_...') e execute novamente. " +
                   "Ela nao pode ser recuperada; se foi perdida, consulte 'Backup, restauracao e reset' em docs\KEYPOOL.md.")
        }
        Set-ProcessEnv "KEYPOOL_API_KEY" $apiKey
        Write-Ok "KEYPOOL_API_KEY definida."

        Write-Step "Executando keypoold doctor"
        $doctorArgs = @("doctor")
        if ($Dpapi) { $doctorArgs += "--master-key-dpapi" }
        Invoke-Native "keypoold doctor" { & $daemon @doctorArgs }

        if ($CheckOnly) { Write-Host ""; Write-Ok "Verificacao concluida (-CheckOnly). Daemon nao iniciado."; return }

        Write-Step "Iniciando o daemon (Ctrl+C para encerrar)"
        $serveArgs = @("serve")
        if ($Dpapi) { $serveArgs += "--master-key-dpapi" }
        Invoke-Native "keypoold serve" { & $daemon @serveArgs }
        return
    }

    # 5b. Primeira execucao ------------------------------------------------------------
    Write-Step "Chaves dos provedores"
    $withKeys = 0
    foreach ($p in $providers) {
        if (Test-ProviderKeysInEnvironment $p.Prefix) {
            Write-Ok "$($p.Name): chaves encontradas no ambiente ($($p.Prefix)_API_KEY*)."
            $withKeys++
            continue
        }
        if ($CheckOnly -or $NonInteractive) {
            Write-Warn "$($p.Name): sem chaves. Defina $($p.Prefix)_API_KEYS antes do bootstrap."
            continue
        }
        $value = Read-SecretText "Chaves de '$($p.Name)', separadas por virgula (Enter para pular)"
        if ($value) {
            Set-ProcessEnv "$($p.Prefix)_API_KEYS" $value
            Write-Ok "$($p.Name): chaves recebidas (somente neste processo)."
            $withKeys++
        } else {
            Write-Info "$($p.Name): ignorado. Use 'keypoolctl key add' depois para cadastrar."
        }
    }
    if ($withKeys -eq 0) {
        if ($CheckOnly) {
            Write-Warn "Nenhuma chave de provedor disponivel. O bootstrap real exigira ao menos uma."
        } else {
            throw "Nenhuma chave de provedor informada. Defina GROQ_API_KEYS / OPENROUTER_API_KEY (ou o env_prefix da sua configuracao) e execute novamente."
        }
    }

    Write-Step "API key local (kp_...)"
    $apiKey = Get-ApiKeyFromEnvironment
    $generated = $false
    if ($apiKey) {
        Write-Ok "Usando KEYPOOL_API_KEY ja definida no ambiente."
    } elseif ($CheckOnly) {
        Write-Info "KEYPOOL_API_KEY nao definida: uma chave seria gerada no bootstrap real."
    } else {
        $apiKey = New-KeyPoolApiKey
        $generated = $true
        Write-Ok "Chave gerada localmente."
    }
    if ($apiKey) { Set-ProcessEnv "KEYPOOL_API_KEY" $apiKey }

    if ($CheckOnly) {
        Write-Host ""
        Write-Ok "Verificacao concluida (-CheckOnly). Nada foi criado e o daemon nao foi iniciado."
        return
    }

    if ($generated) {
        Write-Host ""
        Write-Host "  Sua API key local do KeyPool (guarde agora; nao sera exibida novamente):" -ForegroundColor Yellow
        Write-Host ""
        Write-Host "      KEYPOOL_API_KEY=$apiKey" -ForegroundColor Yellow
        Write-Host ""
        $persist = [bool]$PersistApiKey
        if ((-not $persist) -and (-not $NonInteractive)) {
            $answer = Read-Host "  Salvar no ambiente do usuario (Windows) para as proximas execucoes? [S/n]"
            $persist = ($answer -eq "" -or $answer -match "^[sSyY]")
        }
        if ($persist) {
            [Environment]::SetEnvironmentVariable("KEYPOOL_API_KEY", $apiKey, "User")
            Write-Ok "KEYPOOL_API_KEY salva no ambiente do usuario (valera em novos terminais)."
        } else {
            Write-Warn "A chave NAO foi salva. Anote-a: ela e obrigatoria nas proximas execucoes."
        }
    } elseif ($PersistApiKey -and $apiKey) {
        [Environment]::SetEnvironmentVariable("KEYPOOL_API_KEY", $apiKey, "User")
        Write-Ok "KEYPOOL_API_KEY salva no ambiente do usuario."
    }

    Write-Step "Diretorio de dados e permissoes"
    $dirExists = Test-Path -LiteralPath $dataDir
    $dirEmpty = $true
    if ($dirExists) { $dirEmpty = -not (Get-ChildItem -LiteralPath $dataDir -Force | Select-Object -First 1) }
    if ($dirExists -and (-not $dirEmpty)) {
        Write-Warn "O diretorio de dados ja contem arquivos; a ACL nao foi alterada. Revise com scripts\windows\acl-master-key.ps1."
    } else {
        if (-not $dirExists) { New-Item -ItemType Directory -Force -Path $dataDir | Out-Null }
        try {
            & (Join-Path $PSScriptRoot "acl-master-key.ps1") -Path $dataDir | Out-Null
            Write-Ok "ACL restrita ao usuario atual em $dataDir"
        } catch {
            Write-Warn "Nao foi possivel restringir a ACL: $($_.Exception.Message). Execute scripts\windows\acl-master-key.ps1 manualmente."
        }
    }
    if ($env:KEYPOOL_MASTER_KEY_FILE) {
        Write-Warn "KEYPOOL_MASTER_KEY_FILE aponta para fora do diretorio de dados; proteja esse arquivo manualmente."
    }

    Write-Step "Proximos passos (em OUTRO terminal, com KEYPOOL_API_KEY definida)"
    Write-Info ".\bin\keypoolctl.exe status"
    Write-Info ".\bin\keypoolctl.exe key list"
    Write-Info "python integrations\mathlab\smoke.py   (variaveis em integrations\mathlab\.env.keypool.example)"
    Write-Info "Execucoes futuras: .\scripts\windows\bootstrap.ps1  (ou bin\keypoold.exe serve)"

    Write-Step "Iniciando o daemon no primeiro boot (Ctrl+C para encerrar)"
    $serveArgs = @("serve", "--config", $configFull)
    if ($Dpapi) { $serveArgs += "--master-key-dpapi" }
    Invoke-Native "keypoold serve" { & $daemon @serveArgs }
}
finally {
    foreach ($name in @($script:OriginalEnv.Keys)) {
        [Environment]::SetEnvironmentVariable($name, $script:OriginalEnv[$name], "Process")
    }
    Pop-Location
}
