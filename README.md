# KeyPool

KeyPool e um gateway local em Go para centralizar chaves de provedores de IA e expor uma API compativel com OpenAI. A configuracao e os segredos operacionais ficam em memoria; o daemon mantem um unico checkpoint local `state.json` para restaurar o que afeta a correcao do servico.

## Requisitos

- Go 1.26 ou superior.
- Apenas o toolchain Go e a biblioteca padrao; nao ha banco ou modulo Go de terceiros.
- Windows 10/11 com PowerShell 5.1 para os scripts opcionais de operacao.
- Uma chave valida do provedor que sera usado. Nenhuma credencial real esta incluida.

## Compilar e testar

Na raiz do repositorio:

```powershell
gofmt -l .
go vet ./...
go build ./...
go test ./...
go test -race ./internal/sched/... ./internal/router/... ./internal/gateway/... ./internal/store/... ./cmd/...
```

Atalhos para Windows:

```powershell
.\scripts\windows\build.ps1
.\scripts\windows\test.ps1
.\scripts\windows\test.ps1 -Race
```

## Primeira execucao

A configuracao inicial e JSON. O exemplo referencia `mathlab.project.json` por caminho relativo:

```powershell
$env:GROQ_API_KEYS = "gsk_sua_chave_1,gsk_sua_chave_2"
$env:OPENROUTER_API_KEY = "sk-or-sua_chave"
# Opcional: defina uma chave local conhecida antes do primeiro bootstrap.
$env:KEYPOOL_API_KEY = "kp_sua_chave_local"

go run ./cmd/keypoold serve --config ./configs/examples/keypool.json
```

Se `KEYPOOL_API_KEY` nao estiver definida na primeira execucao, o daemon gera uma chave `kp_...` e a imprime uma unica vez. Guarde-a e configure-a no ambiente do usuario. Em execucoes seguintes, `KEYPOOL_API_KEY` e obrigatoria e validada pelo hash persistido. A opcao `--config` e ignorada depois do bootstrap; alteracoes posteriores sao feitas pela API administrativa/CLI.

Execucoes seguintes:

```powershell
$env:KEYPOOL_API_KEY = "kp_a_chave_emitida_no_primeiro_boot"
go run ./cmd/keypoold serve
```

O unico comando de execucao e `keypoold serve`. A primeira inicializacao importa as chaves `<PREFIXO>_API_KEYS`, `<PREFIXO>_API_KEY` e `<PREFIXO>_API_KEY_N`; variaveis posteriores nao substituem credenciais ja cadastradas. Use `keypoolctl key import-env` para importar credenciais adicionais.

O endereco de escuta precisa ser loopback (`127.0.0.1` ou `localhost`). O exemplo usa `127.0.0.1:8787`.

## Estado local e seguranca

O diretorio padrao e `%LOCALAPPDATA%\keypool`, `$XDG_CONFIG_HOME/keypool` ou `~/.keypool`. `KEYPOOL_DATA_DIR` pode altera-lo. Os arquivos sao:

- `master.key`: master key base64 (ou blob DPAPI no Windows, quando selecionado).
- `state.json`: configuracao, chaves de provedor cifradas com AES-256-GCM, hash SHA-256 da API key, snapshots de limites e contadores diarios.
- `state.json.tmp`: arquivo temporario durante gravacao atomica.

O `state.json` nunca armazena texto claro de credenciais de provedor nem da API key. Historico detalhado de uso, eventos e saude e volatil e limitado em memoria. Os contadores diarios necessarios ao orcamento sao persistidos. Faça backup coordenado de `state.json` **e** `master.key`; sem a master key as credenciais cifradas sao irrecuperaveis. Consulte `docs/ESTADO_LOCAL.md`.

Variaveis principais: `KEYPOOL_DATA_DIR`, `KEYPOOL_MASTER_KEY`, `KEYPOOL_MASTER_KEY_FILE` e `KEYPOOL_API_KEY`. A master key pode ser fornecida por ambiente ou arquivo; no Windows, `--master-key-dpapi` e opcional e usa o perfil do usuario atual.

## API local

Toda rota, exceto `/healthz`, exige `Authorization: Bearer kp_...` ou `X-API-Key`. Para selecionar o projeto envie `X-Keypool-Project: mathlab`; se omitido, usa-se `default_project`.

- `GET /healthz`, `GET /readyz`, `GET /` e `GET /metrics`
- `GET /v1/models`, `GET /v1/capacity`, `POST /v1/chat/completions`
- `POST /v1/leases` e `POST /v1/leases/{id}/report` (se `allow_lease` estiver habilitado)
- `ANY /p/{provider}/{path...}` (passthrough `generic_http` autorizado)

## Administracao

Configure a URL e a API key para usar a CLI:

```powershell
$env:KEYPOOL_URL = "http://127.0.0.1:8787"
$env:KEYPOOL_API_KEY = "kp_sua_chave_local"
go run ./cmd/keypoolctl status
go run ./cmd/keypoolctl provider list
go run ./cmd/keypoolctl key list
go run ./cmd/keypoolctl project apply -f ./configs/examples/mathlab.project.json
go run ./cmd/keypoolctl usage --project mathlab --since 24h
go run ./cmd/keypoolctl events --since 1h
```

Para cadastrar uma chave, passe o segredo por stdin, nunca como argumento:

```powershell
$env:GROQ_API_KEY | go run ./cmd/keypoolctl key add --provider groq --label groq-main --tag mathlab --stdin
```

`keypoolctl key reveal <label>` e uma operacao estritamente local: abre `state.json` com a master key, pede confirmacao e exibe o segredo correspondente.

`keypoold doctor` verifica a integridade do estado, a master key, a API key fornecida, o listener loopback e a disponibilidade da porta:

```powershell
go run ./cmd/keypoold doctor
```

## Estrutura e documentacao

- `internal/router`, `internal/gateway`, `internal/sched`: roteamento, API HTTP, limites e leases.
- `internal/store/memstore`: store em memoria, checkpoint cifrado e historico limitado.
- `internal/provider`, `internal/classify`, `internal/sse`: adaptadores, classificacao e streaming.
- `cmd/keypoold`: daemon; `cmd/keypoolctl`: CLI administrativa e operacao local de reveal.
- `configs/examples`: configuracao JSON e perfil Mathlab.

Consulte `docs/API.md`, `docs/SECURITY.md`, `docs/OPERACAO_WINDOWS.md`, `docs/ESTADO_LOCAL.md`, `docs/VERIFY.md` e `STATUS.md`.
