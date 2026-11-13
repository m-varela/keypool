# KeyPool

KeyPool é um gateway local em Go que centraliza chaves de provedores de IA e expõe uma API compatível com OpenAI. O daemon roda em um único modo (`keypoold serve`), escuta somente em loopback e mantém a configuração e os segredos operacionais em memória. Um único checkpoint local, `state.json`, restaura o que afeta a correção do serviço após reinício.

Versão atual do daemon: `0.4.0`. A referência técnica completa está em [`docs/KEYPOOL.md`](docs/KEYPOOL.md).

## Requisitos

- Go 1.26 ou superior.
- Apenas o toolchain Go e a biblioteca padrão: não há banco de dados nem módulo Go de terceiros.
- Windows 10/11 com PowerShell 5.1 para os scripts opcionais de operação. O daemon e a CLI também compilam fora do Windows.
- Uma chave válida do provedor que será usado. Nenhuma credencial real está incluída no repositório.
- Python 3 (opcional), para executar os smoke tests dos kits de integração.

## Início rápido no Windows

O `bootstrap.ps1` prepara o ambiente e sobe o daemon em primeiro plano:

```powershell
Set-ExecutionPolicy -Scope Process Bypass
$env:GROQ_API_KEYS = "gsk_sua_chave_1,gsk_sua_chave_2"
$env:OPENROUTER_API_KEY = "sk-or-sua_chave"
.\scripts\windows\bootstrap.ps1
```

Na **primeira execução** (quando não existe `state.json`), o script:

1. valida o Go, o JSON de configuração, o `project_file` e a porta loopback;
2. compila `bin\keypoold.exe` e `bin\keypoolctl.exe`;
3. coleta as chaves dos provedores pelo ambiente ou por prompt oculto;
4. usa a `KEYPOOL_API_KEY` existente ou gera uma chave local `kp_...` e a exibe uma única vez;
5. restringe a ACL do diretório de dados ao usuário atual;
6. executa `keypoold serve --config <arquivo>`.

Nas **execuções seguintes**, o script exige `KEYPOOL_API_KEY` (no processo ou no ambiente do usuário), roda `keypoold doctor` e executa `keypoold serve`.

Parâmetros: `-Config` (padrão `configs/examples/keypool.json`), `-Dpapi`, `-SkipBuild`, `-CheckOnly` (só valida e relata, sem criar nada nem iniciar o daemon), `-PersistApiKey` (grava a API key local no ambiente do usuário) e `-NonInteractive`.

Nenhum segredo é passado como argumento de processo nem gravado em log. As variáveis definidas pelo script existem só no processo.

## Diagnosticos de erro para clientes

As respostas de erro mantêm o envelope JSON legado (`error.type`, `error.message`, `error.code`) e também expõem `X-Keypool-Error-Code` e `X-Keypool-Request-Id`. Falhas tipadas de rota podem acrescentar `X-Keypool-Failure-Phase`, `X-Keypool-Upstream-Target`, `X-Keypool-Total-Attempts` e `X-Keypool-Fallback`; `X-Keypool-Retryable` e `Retry-After` só aparecem quando a informação é conhecida. O envelope `error` espelha esses dados como `phase`, `upstream_target`, `attempts`, `fallback_attempted`, `retryable` e `retry_after_seconds`. Timeout de fila continua distinto (`429 keypool_queue_timeout`, fase `queue`) do timeout total/upstream (`408 keypool_timeout`, fase `upstream` quando houve tentativa). Valores de alvo são redigidos e não incluem chave, prompt nem corpo upstream. O KeyPool não define política de retry do consumidor: preserva seu próprio fallback/retry interno e publica o diagnóstico técnico.

## Compilar e testar

Na raiz do repositório:

```powershell
gofmt -l .
go vet ./...
go build ./...
go test ./...
go test -race ./internal/sched/... ./internal/router/... ./internal/gateway/... ./internal/store/... ./cmd/...
```

`gofmt -l .` deve ficar vazio. Os testes são offline: usam `httptest`, fake upstream, `clock.Fake` e diretórios temporários.

Atalhos para Windows:

```powershell
.\scripts\windows\build.ps1          # gofmt -w, go vet, go build e gera bin\keypoold.exe e bin\keypoolctl.exe
.\scripts\windows\test.ps1           # go test ./...
.\scripts\windows\test.ps1 -Race     # inclui go test -race nos pacotes críticos
```

Atenção: `build.ps1` executa `gofmt -w .`, ou seja, reformata os arquivos do repositório.

## Primeira execução manual

A configuração inicial é JSON. O exemplo referencia `example.project.json` por caminho relativo:

```powershell
$env:GROQ_API_KEYS = "gsk_sua_chave_1,gsk_sua_chave_2"
$env:OPENROUTER_API_KEY = "sk-or-sua_chave"
# Opcional: defina uma chave local conhecida antes do primeiro boot.
$env:KEYPOOL_API_KEY = "kp_sua_chave_local"

go run ./cmd/keypoold serve --config ./configs/examples/keypool.json
```

Se `KEYPOOL_API_KEY` não estiver definida na primeira execução, o daemon gera uma chave `kp_...` e a imprime **uma única vez** (`KEYPOOL_API_KEY=kp_...`). Guarde-a e configure-a no ambiente do usuário. A primeira execução exige `--config`.

O daemon prepara a configuração, obtém o listener loopback e só então grava o estado inicial. Assim, uma porta ocupada não deixa um checkpoint parcialmente inicializado.

### Execuções seguintes

```powershell
$env:KEYPOOL_API_KEY = "kp_a_chave_emitida_no_primeiro_boot"
go run ./cmd/keypoold serve
```

- `KEYPOOL_API_KEY` passa a ser obrigatória e é validada contra o hash persistido.
- `--config` é ignorado depois do bootstrap; alterações posteriores são feitas pela API administrativa ou pela CLI.
- A primeira inicialização importa as chaves `<PREFIXO>_API_KEYS`, `<PREFIXO>_API_KEY` e `<PREFIXO>_API_KEY_N`. Variáveis posteriores **não** substituem credenciais já cadastradas; use `keypoolctl key import-env` para importar credenciais adicionais.

Comandos do daemon: `keypoold serve [--config arquivo.json] [--master-key-dpapi]`, `keypoold doctor [--master-key-dpapi]` e `keypoold version`.

O endereço de escuta precisa ser loopback (`127.0.0.1` ou `localhost`). O padrão e o exemplo usam `127.0.0.1:8787`. Não exponha a porta diretamente à internet.

## Estado local e segurança

O diretório de dados é resolvido nesta ordem: `KEYPOOL_DATA_DIR`, `%LOCALAPPDATA%\keypool`, `$XDG_CONFIG_HOME/keypool` ou `~/.keypool`. Os arquivos são:

- `master.key`: master key em base64 (ou blob DPAPI no Windows, quando selecionado).
- `state.json`: checkpoint versionado com a configuração, as credenciais de provedor cifradas com AES-256-GCM, o hash SHA-256 e os metadados da API key, os snapshots das janelas de limites e os contadores diários.
- `state.json.tmp`: arquivo temporário durante a gravação atômica (tmp → fsync → rename).

O `state.json` nunca armazena texto claro de credenciais de provedor nem da API key. O histórico detalhado de uso, eventos e saúde é volátil e limitado em memória (50.000 registros de uso por até 7 dias, 10.000 eventos por até 30 dias e 1.000 registros de saúde por até 7 dias); os contadores diários necessários ao orçamento são persistidos.

Faça backup coordenado de `state.json` **e** `master.key`: sem a master key, as credenciais cifradas são irrecuperáveis. Procedimentos de backup, restauração e reset estão na seção 5 de `docs/KEYPOOL.md`.

Variáveis principais:

| Variável | Finalidade |
|---|---|
| `KEYPOOL_API_KEY` | API key local; opcional no primeiro boot, obrigatória depois. |
| `KEYPOOL_DATA_DIR` | Diretório de dados. |
| `KEYPOOL_MASTER_KEY` | Master key (base64 de 32 bytes) fornecida pelo ambiente. |
| `KEYPOOL_MASTER_KEY_FILE` | Caminho alternativo do arquivo da master key. |
| `KEYPOOL_URL` | URL do daemon usada pela `keypoolctl` (padrão `http://127.0.0.1:8787`). |

No Windows, `--master-key-dpapi` é opcional, usa o perfil do usuário atual e é mutuamente exclusivo com `KEYPOOL_MASTER_KEY`. Em Unix, o arquivo da master key é criado com permissão `0600`.

## API local

Toda rota, exceto `GET /healthz`, exige `Authorization: Bearer kp_...` ou `X-API-Key`. O painel (`GET /`) também aceita HTTP Basic com usuário `keypool` e a API key como senha. Para selecionar o projeto, envie `X-Keypool-Project: example`; se omitido, usa-se `default_project`.

| Rota | Condição | Finalidade |
|---|---|---|
| `GET /healthz` | pública | liveness; não indica prontidão |
| `GET /readyz` | API key | prontidão após o aquecimento L1 inicial ou o timeout configurado |
| `GET /` | API key | painel HTML local somente leitura |
| `GET /metrics` | API key | métricas Prometheus |
| `POST /v1/chat/completions` | API key | chat compatível com OpenAI, com alias e fallback |
| `GET /v1/models` | API key | aliases/modelos autorizados ao projeto |
| `GET /v1/capacity` | API key | capacidade estimada por alias/projeto |
| `POST /v1/leases` | API key + `allow_lease` | reserva uma chave e devolve uma lease temporária |
| `POST /v1/leases/{id}/report` | API key + projeto dono | reporta resultado, tokens e cabeçalhos; encerra a lease |
| `ANY /p/{provider}/{path...}` | API key + `generic_http` autorizado | passthrough HTTP |
| `/admin/v1/*` | API key | API administrativa (usada pela `keypoolctl`) |

Cabeçalhos opcionais de requisição: `X-Keypool-Priority`, `X-Keypool-Timeout-Ms` (prazo total da chamada, incluindo upstream), `X-Keypool-Queue-Timeout-Ms` (prazo separado para aguardar uma chave/capacidade), `X-Keypool-Exclude` e `X-Keypool-Avoid-Weights`. Um timeout de fila retorna `429 keypool_queue_timeout`; o timeout total continua retornando `408`. As respostas podem incluir `X-Keypool-Request-Id`, `X-Keypool-Target`, `X-Keypool-Key` (rótulo mascarado), `X-Keypool-Attempts`, `X-Keypool-Queue-Ms` e `X-Keypool-Fallback`.

A configuração global não concede acesso automaticamente: o projeto precisa declarar os provedores/chaves permitidos e suas rotas. Provedores `openai_compatible` podem ser configurados sem código; `generic_http` habilita o passthrough. Exemplos e detalhes estão na seção 3 de `docs/KEYPOOL.md`.

## Administração

Configure a URL e a API key para usar a CLI:

```powershell
$env:KEYPOOL_URL = "http://127.0.0.1:8787"
$env:KEYPOOL_API_KEY = "kp_sua_chave_local"
go run ./cmd/keypoolctl status
go run ./cmd/keypoolctl provider list
go run ./cmd/keypoolctl key list
go run ./cmd/keypoolctl project apply -f ./configs/examples/example.project.json
go run ./cmd/keypoolctl usage --project example --since 24h
go run ./cmd/keypoolctl events --since 1h
```

Comandos disponíveis:

| Comando | Subcomandos / opções |
|---|---|
| `status` | visão geral (estatísticas, provedores, chaves e projetos) |
| `provider` | `list`, `show <nome>`, `add -f <arquivo.json> [--name nome]`, `enable <nome>`, `disable <nome>` |
| `key` | `list [--provider nome]`, `add`, `import-env`, `enable`, `disable`, `rm`, `test`, `tag <id> +tag -tag`, `reveal <label>` |
| `project` | `apply -f <arquivo.json>`, `list`, `show <nome>` |
| `apikey` | `info`, `rotate` |
| `usage` | `[--project nome] [--since 24h] [--group-by key\|model\|project]` |
| `events` | `[--since 1h] [--kind tipo]` |
| `warm` | `run`, `status` |
| `capacity` | `[--project nome] [--alias nome]` |

`keypoolctl --json <comando>` (ou `--json` depois do comando) devolve JSON estruturado; sem a opção, a saída é tabular para listagens e chave-valor para objetos. Tratar como segredo a saída de `apikey rotate`, que devolve uma credencial de uso único mesmo sem `--json`.

Para cadastrar uma chave, passe o segredo por stdin, nunca como argumento:

```powershell
$env:GROQ_API_KEY | go run ./cmd/keypoolctl key add --provider groq --label groq-main --tag example --stdin
```

Para importar chaves de um arquivo `.env` (mesma ordem e deduplicação do bootstrap), informe o arquivo, o prefixo e o provedor de destino:

```powershell
go run ./cmd/keypoolctl key import-env .\.env --prefix GROQ --provider groq --tag example
```

`keypoolctl key reveal <label>` é uma operação estritamente local: abre o `state.json` com a master key, pede confirmação interativa e exibe o segredo correspondente. Ela não usa a API.

`keypoold doctor` verifica a integridade do estado, a master key, a API key do ambiente, o listener loopback e a disponibilidade da porta. Execute-o com o daemon parado:

```powershell
go run ./cmd/keypoold doctor
```

## Operação no Windows

| Script | Finalidade |
|---|---|
| `scripts\windows\bootstrap.ps1` | prepara o ambiente e sobe o daemon (primeira execução e execuções seguintes) |
| `scripts\windows\build.ps1` | formata, valida e gera `bin\keypoold.exe` e `bin\keypoolctl.exe` |
| `scripts\windows\test.ps1` | executa os testes (`-Race` inclui o detector de corridas) |
| `scripts\windows\run.ps1` | atalho para `go run ./cmd/keypoold serve --config <arquivo>` |
| `scripts\windows\acl-master-key.ps1` | restringe a ACL do diretório de dados ao usuário atual |
| `scripts\windows\install-task.ps1` | cria (ou remove, com `-Remove`) a tarefa agendada de logon |

Para iniciar o daemon junto com o logon, defina `KEYPOOL_API_KEY` (e, se aplicável, `KEYPOOL_MASTER_KEY`) no ambiente do usuário e execute:

```powershell
.\scripts\windows\install-task.ps1 -Executable .\bin\keypoold.exe -Arguments "serve"
```

A tarefa é criada para o usuário atual, no logon, sem elevação, oculta e com até 5 reinícios. Faça o primeiro bootstrap em terminal visível para capturar a API key gerada. Para remover: `.\scripts\windows\install-task.ps1 -TaskName KeyPool -Remove`.

## Integração Google AI Studio (Gemini API)

O diretório [`integrations/google-ai-studio`](integrations/google-ai-studio) contém um exemplo genérico de provedor para a Gemini API, com preset `openai_compatible`, variáveis de ambiente e smoke test. Ele usa uma API key criada no Google AI Studio; não integra a interface web do AI Studio.

Consulte [`integrations/google-ai-studio/README.md`](integrations/google-ai-studio/README.md) para cadastrar o provedor e a chave, validar `/models` e testar chat completions/streaming. O perfil de projeto é específico de cada consumidor e deve ficar no repositório desse projeto; `configs/examples/example.project.json` é apenas um exemplo genérico.

## Estrutura do repositório

- `cmd/keypoold`: daemon (`serve`, `doctor`, `version`); `cmd/keypoolctl`: CLI administrativa e operação local de `key reveal`.
- `internal/router`, `internal/gateway`, `internal/sched`: roteamento, API HTTP, limites, seleção e leases.
- `internal/admin`: API administrativa (`/admin/v1/*`).
- `internal/store/memstore`: estado em memória, checkpoint cifrado e histórico limitado.
- `internal/provider`, `internal/classify`, `internal/sse`, `internal/warm`: adaptadores e presets, classificação de erros, streaming e aquecimento.
- `internal/config`, `internal/cryptobox`, `internal/secret`, `internal/metrics`: configuração JSON, criptografia, segredos e métricas/painel.
- `configs/examples`: configuração e perfil genéricos para experimentar o gateway.
- `integrations/google-ai-studio`: preset/provedor e guia genérico da Gemini API.
- `scripts/windows`: scripts PowerShell (ASCII puro) de operação.

## Estado do projeto e documentação

- [`docs/KEYPOOL.md`](docs/KEYPOOL.md): documentação consolidada (execução, configuração, API, estado local, segurança, Windows, provedores, verificação e decisões).
- [`STATUS.md`](STATUS.md): estado das fases e limites objetivos da entrega.
- [`AGENTS.md`](AGENTS.md): convenções para agentes de código.

Pendências registradas em `STATUS.md`: a verificação nativa com Go 1.26 (os testes foram executados com um modfile temporário de Go 1.23.2), a validação nativa de PowerShell/DPAPI/ACL no Windows e a aceitação E2E cliente → KeyPool → provedor real.
