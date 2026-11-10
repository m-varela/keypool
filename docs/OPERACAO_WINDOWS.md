# Operação do KeyPool no Windows

O KeyPool escuta somente em loopback (`127.0.0.1`). O estado é local, cifrado e guardado no diretório de dados do usuário.

## 1. Requisitos

- Windows 10/11 ou Windows Server suportado pelo Go usado para compilar.
- Go 1.26 ou superior.
- PowerShell 5.1 ou superior e módulo `ScheduledTasks` para a tarefa de logon.
- Nenhum banco ou módulo Go externo é necessário.

## 2. Build e testes

Na raiz do repositório:

```powershell
Set-ExecutionPolicy -Scope Process Bypass
.\scripts\windows\build.ps1
.\scripts\windows\test.ps1
.\scripts\windows\test.ps1 -Race
```

## 3. Primeiro bootstrap

Defina as credenciais dos provedores no ambiente do usuário. Opcionalmente, escolha a API key local antes da primeira execução:

```powershell
$env:GROQ_API_KEYS = "gsk_chave_1,gsk_chave_2"
$env:OPENROUTER_API_KEY = "sk-or-chave"
$env:KEYPOOL_API_KEY = "kp_chave_local"
.\bin\keypoold.exe serve --config .\configs\examples\keypool.json
```

Se `KEYPOOL_API_KEY` não for definida, copie a chave gerada e impressa uma única vez. Em execuções seguintes, defina `KEYPOOL_API_KEY` no ambiente e execute `serve` sem `--config`. O `doctor` verifica estado, master key, API key, loopback e porta.

O diretório de dados padrão é `%LOCALAPPDATA%\keypool`; `KEYPOOL_DATA_DIR` pode alterá-lo. `KEYPOOL_MASTER_KEY` e `KEYPOOL_MASTER_KEY_FILE` escolhem a master key por ambiente/arquivo.

## 4. Master key e ACL

A master key padrão é um arquivo base64 `master.key` no diretório de dados, com ACL restritiva. Aplique a ACL após criar o diretório:

```powershell
.\scripts\windows\acl-master-key.ps1
```

Como alternativa, `--master-key-dpapi` protege a master key pelo DPAPI do usuário atual. O arquivo DPAPI depende do mesmo perfil Windows; não basta copiá-lo para outro usuário ou máquina. DPAPI e `KEYPOOL_MASTER_KEY` são mutuamente exclusivos.

```powershell
.\bin\keypoold.exe doctor --master-key-dpapi
.\bin\keypoold.exe serve --master-key-dpapi
```

## 5. Tarefa agendada

Defina no ambiente do usuário `KEYPOOL_API_KEY` e, se usar, `KEYPOOL_MASTER_KEY`. Instale a tarefa:

```powershell
.\scripts\windows\install-task.ps1 -Executable .\bin\keypoold.exe -Arguments "serve"
```

A tarefa usa `AtLogOn`, usuário atual, nível `Limited`, `Hidden` e `IgnoreNew`; falhas de inicialização têm até cinco reinícios com intervalo de um minuto. O diretório de trabalho padrão é a raiz do repositório e pode ser alterado por `-WorkingDirectory`.

Remova a tarefa:

```powershell
.\scripts\windows\install-task.ps1 -TaskName KeyPool -Remove
```

Faça o primeiro bootstrap em um terminal visível para capturar uma API key gerada. Uma tarefa oculta não é um local adequado para a primeira emissão.

## 6. Painel e métricas

Com o serviço ativo, abra `http://127.0.0.1:8787/`. Use usuário `keypool` e a API key como senha no prompt Basic. `GET /metrics` requer Bearer ou `X-API-Key`.

## 7. Backup e recuperação

Pare o daemon e copie juntos `state.json` e `master.key`. Proteja os backups com ACL e armazenamento seguro. Sem a master key as credenciais cifradas não podem ser recuperadas. Em modo DPAPI, mantenha o contexto do usuário capaz de descriptografar o blob.

Para restaurar, recoloque os arquivos no diretório configurado, defina `KEYPOOL_API_KEY` e execute `keypoold doctor`. Para zerar o estado, pare o daemon e remova `state.json`/`state.json.tmp`; consulte `docs/ESTADO_LOCAL.md`.

## 8. Diagnóstico

- `/healthz` confirma que o processo responde; não garante prontidão.
- `/readyz` informa a prontidão L1 ou o timeout configurado.
- `keypoold doctor` verifica checkpoint, master key, API key, listen e disponibilidade da porta.
- `GET /admin/v1/stats` detalha provedores e aquecimento.
- `GET /metrics` fornece séries Prometheus; `keypool_usage_dropped_total` conta descarte pelo limite do ring.
