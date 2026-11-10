# Verificacao

Execute na raiz do repositorio com Go 1.26+ e acesso aos modulos declarados em `go.mod`.

## Preparacao e verificacao geral

```powershell
go mod tidy
gofmt -l .
go vet ./...
go build ./...
go test ./...
```

Resultado esperado: `go mod tidy` gera `go.sum` autentico; `gofmt -l .` nao lista arquivos; `vet`, `build` e todos os testes terminam sem erro.

## Concorrencia e benchmarks

```powershell
go test -race ./...
go test -bench . ./internal/sched/... ./internal/sliding/...
```

No Windows, o race detector exige `CGO_ENABLED=1` e um compilador C compativel.

## Integracao PostgreSQL F4

Configure `KEYPOOL_TEST_DATABASE_URL` para um banco descartavel, com permissao de criar/remover schemas, e execute:

```powershell
$env:KEYPOOL_TEST_DATABASE_URL = "postgres://keypool:senha@127.0.0.1:5432/keypool_test?sslmode=disable"
go test -tags integration ./internal/store/pgstore
```

O teste cria um schema temporario, aplica e verifica checksums, testa inicializacao e autenticacao, cifra/decifra, configuracao, snapshot/restore, eventos, lock consultivo e rollup/retencao. O schema e removido no cleanup. Nunca aponte essa variavel para producao.

Scripts PowerShell equivalentes:

```powershell
.\scripts\windows\build.ps1
.\scripts\windows\test.ps1
.\scripts\windows\test.ps1 -Race
.\scripts\windows\test.ps1 -Integration
```

O setup de banco e a protecao da chave mestra estao documentados em `docs/POSTGRESQL_F4.md`.

## Resultado observado na revisão F6 (10/11/2026)

Ambiente disponível: Go 1.23.2 Linux/amd64. O `go.mod` requer Go 1.26. Não há PostgreSQL, `psql`, `initdb` ou PowerShell; `pgx` e `yaml.v3` reais não estão no cache e a rede/DNS não permite baixá-los. O `go.mod` do projeto não foi alterado e nenhum `go.sum` foi inventado.

Para testar os pacotes internos independentes de `pgx`, foi usado um modfile temporário externo (`/tmp/keypool-check.mod`) com um stub mínimo local para `yaml.v3`. Isso não substitui a validação com os módulos reais. Os comandos executados foram:

```sh
export GOTOOLCHAIN=local
pkgs=$(find ./internal -name '*.go' -not -path './internal/store/pgstore/*' -printf '%h\n' | sort -u | tr '\n' ' ')
go test -modfile=/tmp/keypool-check.mod $pkgs
go test -race -modfile=/tmp/keypool-check.mod $pkgs
go vet -modfile=/tmp/keypool-check.mod $pkgs
gofmt -l $(find . -name '*.go' -not -path './vendor/*')
```

Resultados: todos os testes dos pacotes `internal` exceto `internal/store/pgstore` passaram; o mesmo conjunto passou com `-race` e `go vet`; `gofmt` não listou arquivos. Os testes recentes de startup restore, catalog invalidation, router reload e warm também passaram.

Não foi confirmada a compilação integrada de `cmd/keypoold`, `cmd/keypoolctl` e `internal/store/pgstore`, pois dependem do driver `pgx` ausente. O stub YAML não valida integralmente `yaml.v3` (incluindo `yaml.Node.Decode` e `KnownFields`). Nenhum teste executou contra PostgreSQL real. A aceitação completa ainda precisa ser repetida com Go 1.26+, dependências reais e PostgreSQL 16+ descartável.

## F5 — API administrativa e CLI

Com o serviço de desenvolvimento em execução (`keypoold serve --dev ...`) e `KEYPOOL_API_KEY` configurada:

```powershell
$env:KEYPOOL_URL = "http://127.0.0.1:8787"
$env:KEYPOOL_API_KEY = "kp_sua_chave_local"
go run ./cmd/keypoolctl status
go run ./cmd/keypoolctl provider list
go run ./cmd/keypoolctl key list
go run ./cmd/keypoolctl project apply -f ./configs/examples/mathlab.project.yaml
go run ./cmd/keypoolctl usage --project mathlab --since 24h
go run ./cmd/keypoolctl events --since 1h
```

Testes offline específicos da F5:

```powershell
go test ./internal/admin ./cmd/keypoolctl ./internal/gateway ./internal/store/memstore
```

O teste de integração PostgreSQL deve ser ampliado/rodado com `KEYPOOL_TEST_DATABASE_URL` para cobrir `SaveKeys`, `DeleteKey`, rotação da API key e consultas administrativas. `key test`, `warm run` e `warm status` estão implementados e conectados ao gerenciador F6; os comandos abaixo exercitam a API em modo dev.

## F6 — aquecimento, saúde e catálogo

Em modo `serve --dev`, com `KEYPOOL_API_KEY` e um provedor de teste configurados:

```powershell
$env:KEYPOOL_URL = "http://127.0.0.1:8787"
$env:KEYPOOL_API_KEY = "kp_sua_chave_local"
go run ./cmd/keypoolctl warm status
go run ./cmd/keypoolctl warm run --provider groq
go run ./cmd/keypoolctl warm run --provider groq --key ID_DA_CHAVE
go run ./cmd/keypoolctl key test ID_DA_CHAVE
```

Ou pela API autenticada:

```powershell
Invoke-RestMethod -Method Get -Uri "$env:KEYPOOL_URL/admin/v1/warm/status" -Headers @{ Authorization = "Bearer $env:KEYPOOL_API_KEY" }
Invoke-RestMethod -Method Post -Uri "$env:KEYPOOL_URL/admin/v1/warm/run" -Headers @{ Authorization = "Bearer $env:KEYPOOL_API_KEY" } -ContentType "application/json" -Body '{"provider":"groq"}'
Invoke-RestMethod -Method Get -Uri "$env:KEYPOOL_URL/readyz"
```

Cobertura automatizada da F6:

```powershell
go test ./internal/warm ./internal/sched ./internal/router ./internal/admin ./internal/gateway ./internal/store ./internal/store/memstore
go test -race ./internal/warm ./internal/sched ./internal/router ./internal/gateway
```

Confirme ainda os seguintes comportamentos no provedor/fake upstream: 401 invalida a chave; 429 não abre circuito; catálogos fixos só autorizam modelos listados enquanto o cache está válido; `probe.kind: none` desabilita L1; L2 permanece desligada por padrão e, quando ligada, não chama modelo free/RPD baixo nem ultrapassa o orçamento diário. No boot, validação e catálogo antigos não são confiados antes da primeira L1 da credencial atual.

## Resultado observado desta entrega F6 (10/11/2026)

Ambiente: Go 1.23.2 Linux/amd64; projeto exige Go 1.26. Não há PostgreSQL/PowerShell, módulos reais `pgx`/`yaml.v3` em cache ou rede/DNS funcional. O `go.mod` do repositório não foi alterado.

Com o modfile temporário externo `/tmp/keypool-check.mod` (stub mínimo apenas para `yaml.v3`):

- `gofmt` em todos os arquivos Go: limpo.
- `go test` em todos os pacotes `internal` exceto `internal/store/pgstore`: passou.
- `go test -race` no mesmo conjunto: passou.
- `go vet` no mesmo conjunto: passou.
- Testes F6 e regressões de recarga/snapshot: passaram.

Não foi possível confirmar a compilação integrada de `cmd/keypoold`, `cmd/keypoolctl` e `internal/store/pgstore`, pois dependem do driver `pgx` ausente. O stub YAML também não substitui a validação com `yaml.v3` real. Não foi executado teste PostgreSQL. Para a aceitação final, execute as verificações gerais e os testes de integração F4 acima com Go 1.26+, módulos reais e PostgreSQL 16+ descartável.


## F7 — leases, passthrough e políticas do projeto

Configure um projeto de teste com limites e permissão explícitos:

```yaml
projects:
  integration:
    allow_lease: true
    policy:
      max_inflight: 8
      max_wait: 3s
      daily_token_budget: 100000
```

Para `generic_http`, configure um provedor separado e selecione a chave em `projects.integration.keys`. Esse tipo não pode aparecer em `routes` de chat. O passthrough não devolve o segredo e não requer `allow_lease`.

Crie e feche uma lease:

```powershell
$headers = @{ Authorization = "Bearer $env:KEYPOOL_API_KEY"; "X-Keypool-Project" = "integration" }
$lease = Invoke-RestMethod -Method Post -Uri "$env:KEYPOOL_URL/v1/leases" -Headers $headers -ContentType "application/json" -Body '{"target":"openrouter:openrouter/free","est_tokens":1500,"priority":2,"timeout_ms":30000,"ttl_s":120}'
# Use $lease.base_url, $lease.auth_header e $lease.secret diretamente no cliente do projeto.
Invoke-RestMethod -Method Post -Uri "$env:KEYPOOL_URL/v1/leases/$($lease.lease_id)/report" -Headers $headers -ContentType "application/json" -Body '{"outcome":"success","tokens":1432}'
```

O segredo é retornado somente na criação e somente para projetos com `allow_lease: true`. Reportar uma segunda vez deve retornar 404. Sem relatório, watchdog fecha a lease no TTL como `leaked`; no encerramento do daemon as leases ainda abertas também são concluídas antes do flush dos writers.

Teste um caminho `generic_http` (o método e o corpo são repassados; a chave do provedor substitui a credencial do cliente):

```powershell
Invoke-RestMethod -Method Post -Uri "$env:KEYPOOL_URL/p/search/v1/search?q=abc" -Headers $headers -ContentType "application/json" -Body '{"q":"abc"}'
```

Não inclua `generic_http` em rotas de chat. O passthrough rejeita traversal (`..` e `\`), não segue redirects e remove `Authorization`, `X-API-Key`, `X-Keypool-*` e cabeçalhos hop-by-hop, inclusive os indicados por `Connection`.

Verificações automatizadas específicas:

```powershell
go test ./internal/router ./internal/gateway ./internal/sched ./internal/store/memstore
go test -race ./internal/router ./internal/gateway ./internal/sched
go test -run '^$' -bench '^BenchmarkGatewayPassthrough$' -benchmem ./internal/gateway
```

A política `max_inflight` é compartilhada por chat, lease e passthrough; `max_wait` deve limitar tanto a espera por uma vaga quanto a aquisição no scheduler. `daily_token_budget` é diário UTC, reserva a estimativa antes do envio e reconcilia com uso reportado/estimado. Em PostgreSQL, a inicialização recompõe o total do dia a partir de `usage_events`; migração `0003_f7_usage_actual_tokens.sql` adiciona `actual_tokens` e `usage_daily.total_tokens`.

O seletor `headroom` deve preferir a chave com maior folga relativa; `latency` compara duas escolhas com EWMA de TTFB. O calibrador de tokens mantém EWMA α=0,2 por provedor/modelo, limitado a fator [0,5; 2,0].

## Resultado observado da implementação F7 (10/11/2026)

Ambiente: Linux/amd64, Go 1.23.2, projeto declara Go 1.26. Para verificar os pacotes que não importam o driver PostgreSQL foi utilizado `/tmp/keypool-check.mod`, fora do repositório, com stub mínimo de `yaml.v3`; o `go.mod` e o `go.sum` do projeto não foram alterados.

- `go test -count=1` em todos os 25 pacotes `internal` exceto `internal/store/pgstore`: passou.
- `go test -race -count=1` em todos esses pacotes, dividido em dois grupos para caber no limite do ambiente: passou. Durante a primeira execução, o detector encontrou corridas apenas nos slices de callback dos testes; os harnesses foram sincronizados com canais e a execução foi repetida com sucesso.
- `go vet` no mesmo conjunto: passou.
- `gofmt -l` em todos os arquivos Go: sem saída.
- Benchmarks em execução curta (250 ms, máquina desta sessão, Intel Xeon Platinum 8573C): `BenchmarkGatewayPassthrough` 158.046 ns/op, 54.121 B/op, 282 allocs/op; `BenchmarkAcquireSuccess` 24.459 ns/op, 959 B/op, 12 allocs/op; `BenchmarkAcquireContended` 19.970 ns/op, 1.026 B/op, 15 allocs/op; `BenchmarkWindowAdd` 282,4 ns/op, 319 B/op, 0 allocs/op.
- Comparação curta do scheduler com o ZIP F6, na mesma máquina, toolchain, modfile e `-benchtime=250ms`: F6 `AcquireSuccess` 26.257 ns/op (918 B, 12 allocs) e contended 19.405 ns/op (1.082 B, 15 allocs); F7 24.459 ns/op (959 B, 12 allocs) e contended 19.970 ns/op (1.026 B, 15 allocs). A diferença é pequena e compatível com variação de benchmark curto; não houve aumento de alocações. Essa comparação usa o estado F6 recebido e não substitui uma comparação controlada com a medição histórica da F1/F3.
- `go test ./...` com o modfile de verificação foi tentado e parou antes de compilar porque `github.com/jackc/pgx/v5` e `pgxpool` não estão disponíveis no cache (`GOPROXY=off`). O projeto declara Go 1.26, enquanto o toolchain instalado é Go 1.23.2.

Não foram validados `cmd/keypoold`, `cmd/keypoolctl` e `internal/store/pgstore` em build integrado nem o schema/consultas contra PostgreSQL 16+. A aceitação final continua exigindo Go 1.26+, dependências reais (`pgx` e `yaml.v3`) e PostgreSQL descartável, conforme os comandos gerais desta documentação. O benchmark F7 foi executado contra upstream `httptest` local e não acessou provedores externos.


## F8 — métricas, painel, DPAPI e tarefa Windows

Testes offline do registry, endpoint autenticado e formatação Prometheus:

```powershell
go test ./internal/metrics ./internal/gateway ./internal/router ./internal/sched
go test -race ./internal/metrics ./internal/gateway ./internal/router ./internal/sched
```

Após subir `keypoold serve --dev` ou modo persistente:

```powershell
$headers = @{ Authorization = "Bearer $env:KEYPOOL_API_KEY" }
Invoke-WebRequest -Uri "http://127.0.0.1:8787/metrics" -Headers $headers
Invoke-RestMethod -Uri "http://127.0.0.1:8787/readyz" -Headers $headers
```

Abra `http://127.0.0.1:8787/` em navegador e informe usuário `keypool` e `KEYPOOL_API_KEY` como senha no diálogo Basic Auth. O browser reutiliza a autenticação nos links read-only do painel.

Critérios: `/metrics` retorna `text/plain; version=0.0.4`, `# HELP`/`# TYPE`, histogramas `bucket/sum/count`; chamadas sem API key retornam 401; `/` exige auth, retorna HTML embutido e CSP; nenhum conteúdo de prompt, resposta ou segredo aparece em labels. Confirmar `keypool_requests_total`, `keypool_attempts_total`, `keypool_queue_seconds`, `keypool_upstream_ttfb_seconds`, `keypool_key_state`, `keypool_window_used_ratio`, `keypool_cooldown_seconds`, `keypool_warm_age_seconds`, `keypool_usage_dropped_total` e `keypool_leases_leaked_total`.

Em Windows, compile o binário e verifique DPAPI no perfil usuário que será usado pelo serviço:

```powershell
.\scripts\windows\build.ps1
.\bin\keypoold.exe init --master-key-dpapi
.\bin\keypoold.exe doctor --master-key-dpapi
.\bin\keypoold.exe serve --master-key-dpapi
.\scripts\windows\install-task.ps1 -Executable .\bin\keypoold.exe -Arguments "serve"
.\scripts\windows\install-task.ps1 -TaskName KeyPool -Remove
```

Não misture `--master-key-dpapi` com `KEYPOOL_MASTER_KEY`. Confirme que init/doctor/serve são executados no mesmo perfil Windows; não tente restaurar DPAPI em perfil diferente sem um procedimento explícito de migração. Confirme no Agendador de Tarefas que a tarefa é oculta, usuário atual, nível `Limited`, diretório de trabalho correto e política de reinício limitada.

## Resultado observado da F8 nesta sessão (10/11/2026)

Ambiente de verificação: Linux/amd64, Go 1.23.2. O módulo declara Go 1.26 e depende de `pgx/v5`; ambos os requisitos impediram o build integrado do daemon nesta sessão. Para isolar os pacotes internos sem `pgstore`, usei `/tmp/keypool-check.mod` fora do repositório, com stub mínimo de `yaml.v3`; não alterei `go.mod` nem `go.sum` do projeto.

- `go test -count=1` em todos os 25 pacotes internos que não importam `internal/store/pgstore`: passou.
- `go test -race -count=1` nos mesmos pacotes, divididos em dois grupos: passou. Isso inclui métricas, gateway, router, scheduler, warm e stores em memória.
- `go vet` nos mesmos pacotes: passou.
- `gofmt -l` em todos os arquivos Go: sem saída.
- Compilação cruzada `GOOS=windows GOARCH=amd64 go test -c` de `internal/cryptobox`, `internal/metrics`, `internal/gateway`, `internal/router` e `internal/sched`: passou. Isso verifica compilação dos caminhos DPAPI/HTTP centrais, mas não substitui execução no Windows.
- `install-task.ps1` verificado como ASCII; o cmdlet `New-ScheduledTaskSettingsSet -Hidden` foi conferido na documentação Microsoft. O script não pôde ser executado porque PowerShell/Agendador de Tarefas não existem neste ambiente.
- A autenticação do painel tem testes para Bearer, `X-API-Key`, HTTP Basic (usuário `keypool`) e desafio `WWW-Authenticate`; o endpoint Prometheus tem teste de `Content-Type`, amostras e bloqueio sem credencial.
- `cmd/keypoold`, `cmd/keypoolctl`, `internal/store/pgstore`, integração real de banco, migration e `doctor` não foram compilados/executados: `github.com/jackc/pgx/v5`/`pgxpool` não estão no cache, e a rede para resolver dependências/toolchain não está disponível.

Aceitação restante em máquina Windows com Go 1.26+ e PostgreSQL 16+: `go mod tidy`; `scripts\windows\build.ps1`; `scripts\windows\test.ps1`; `scripts\windows\test.ps1 -Integration`; teste manual de DPAPI no mesmo perfil de usuário; instalação/remoção da tarefa agendada; scrape autenticado de `/metrics`; navegação no painel com Basic Auth. O ambiente desta sessão não permite declarar essa aceitação nativa concluída.


## Rodada final de consistência e desempenho (10/11/2026)

Esta rodada incluiu a CLI e o caminho SSE. A saída padrão de `keypoolctl` é tabular para listas e chave-valor para objetos; `--json` pode aparecer antes ou depois do comando. `status --json` retorna um único objeto; `key list --provider NOME` filtra localmente o envelope retornado pela API. Respostas de rotação que contêm `api_key` não mascarada são preservadas em JSON para exibição única, enquanto colunas sensíveis são omitidas da tabela. Testes unitários da camada de saída passaram isoladamente; o build integrado do comando segue pendente por depender de `pgx` e Go 1.26.

O teste de watchdog em `internal/gateway/f7_test.go` usa `clock.Fake` e sincronização por evento, sem `time.Sleep`. O reader SSE reutiliza buffers via `NextRaw`/`Reset`, sem cópia por evento no caminho do roteador. Medições curtas locais com Go 1.23.2/Linux (não representam SLO de produção):

| Benchmark | Tempo observado | Bytes/op | Allocs/op |
|---|---:|---:|---:|
| `BenchmarkAcquireSuccess` | 8,362 ns | 917 B | 12 |
| `BenchmarkAcquireContended` | 5,665 ns | 933 B | 15 |
| `BenchmarkWindowAdd` | 171.5 ns | 319 B | 0 |
| `BenchmarkWaitForRoom` | 399.3 ns | 0 B | 0 |
| `BenchmarkSSEForward` | 917.1 ns | 0 B | 0 |
| `BenchmarkGatewayPassthrough` | 134,137 ns | 54,072 B | 282 |

A meta da seção 17 para `Pool.TryAcquire` (`<5 µs`, `≤2 alocações`) **não foi atingida**: os benchmarks de sucesso/contendido continuam com 12/15 alocações e o tempo observado deve ser reduzido. Não se introduziu otimização arriscada sem perfil. O `BenchmarkSSEForward` sintético mede 0 alocações por fluxo após reutilização de buffers; isso não implica 0 alocações no gateway completo.

Mathlab usa uma única `KEYPOOL_API_KEY`; a rotação das credenciais upstream permanece no KeyPool. A variável de mesmo nome usada pelo daemon KeyPool para sua própria API key é um segredo diferente e deve ficar em ambiente separado por processo.

## F9 — contrato de integração Mathlab

A integração de código foi aplicada no repositório Mathlab externo. O Mathlab usa exatamente uma credencial cliente `KEYPOOL_API_KEY`; os seis papéis apontam para aliases KeyPool. Para conferir localmente, no repositório Mathlab:

```powershell
python -m pytest -q tests/test_keypool_gateway.py
python -m pytest -q --ignore=tests/test_z3_check.py
python -m compileall -q src tests scripts
```

Os testes locais usam `httpx.MockTransport`; não comprovam a aceitação operacional. Após configurar KeyPool, PostgreSQL, a chave `kp_...` e uma chave upstream importada no KeyPool, execute `integrations/keypool/smoke.py` e uma tarefa curta do Mathlab. Em seguida valide 429/`Retry-After`, concorrência, reinicialização, SSE/cancelamento e uma execução Lean real.

Pendências ambientais conhecidas: compilação Go completa com Go 1.26+ e dependências reais; testes PostgreSQL 16+; DPAPI/Task Scheduler em Windows; integração Mathlab→KeyPool→upstream com segredos/serviços reais. Não remova os módulos locais de compatibilidade antes da aceitação E2E.


## Resultado da rodada final (10/11/2026)

- `go test` passou para os pacotes internos sem `pgstore` e sem o teste de configuração que exige semântica real de `yaml.Node.Decode`. O teste de `internal/config` que usa esse recurso não pode ser validado pelo stub; o pacote compila e `go vet` passa, mas esse teste precisa ser repetido com `gopkg.in/yaml.v3` real.
- `go test` dos arquivos `cmd/keypoolctl/output.go` + `output_test.go`: passou; cobre `--json` antes/depois, tabela sem segredos, JSON estruturado, preservação única de `api_key` e filtro de provedor. O build do comando inteiro ainda não é verificável sem `pgx`/Go 1.26.
- `go test -race`: passou para `internal/sse`, `sliding`, `sched`, `router`, `gateway`, `warm` e `store/memstore`. `go vet` passou em todos os pacotes `internal` exceto `pgstore`; `gofmt -l` não listou arquivos.
- Kit offline `integrations/mathlab`: 6 testes passaram.
- Limite do harness: ao executar todos os testes internos com o stub YAML, 1 teste de `internal/config` falhou especificamente porque o stub retorna erro para `yaml.Node.Decode`; não classificar isso como falha funcional nem como aprovação real do parser.
- O Go original do projeto declara 1.26, mas o host fornece 1.23.2. O ambiente também não possui `pgx`, PostgreSQL 16+, PowerShell ou acesso funcional para baixar dependências/toolchain. Build integrado, teste PG, DPAPI e Task Scheduler seguem sem validação nativa.
