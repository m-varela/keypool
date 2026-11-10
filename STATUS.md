# Status de implementacao

Data de referencia: 10/11/2026.

| Fase | Estado | Entrega |
|---|---|---|
| F0 - Fundacao | feita na entrega-base | modulo, dominio, clock, segredo, estrutura, docs e scripts |
| F1 - Nucleo | feita na entrega-base | sliding, limits, sched, leases, snapshots, hints, circuit breaker |
| F2 - Provedores | feita na entrega-base | classify, SSE, specs/adapters, presets e fake upstream |
| F3 - Gateway e roteador dev | implementada no código; validação nativa pendente | snapshot sincronizado, roteamento/spill, filtros, retries, SSE incremental, endpoints, memstore e testes E2E; ver `docs/VERIFY.md` para limites do ambiente |
| F4 - PostgreSQL | implementada; teste real pendente | `pgxpool.Store`, persistencia normalizada, credenciais AES-256-GCM, migrações imutaveis 0001/0002/0003, migrador com checksum, `keypoold migrate/init/serve/doctor` (inclui estimativa de desvio de relogio PostgreSQL/local), lock consultivo por sessao, snapshots periodicos e flush final, writer de uso limitado/em lote, retencao e testes de integracao condicionais |
| F5 - Admin e CLI | implementada no código; PostgreSQL real pendente | API admin/CRUD/apply, `key test`, `warm/run`, `warm/status`, filtro `key list --provider`, tabelas por padrão e `--json` global (inclui status agregado); testes unitários da camada de saída aprovados |
| F6 - Aquecimento e saude | implementada no código; validação nativa externa pendente | L0/L1/L2, agenda heap, workers limitados, catálogo/TTL, hints, EWMA, circuit breaker, persistência e readiness integrados; testes locais passam com os limites descritos abaixo |
| F7 - Extras de eficiencia | implementada no código; PostgreSQL/build nativo pendentes | leases HTTP, passthrough generic_http, headroom/latency, calibração EWMA e políticas por projeto; veja VERIFY.md |
| F8 - Acabamento | implementada no código; validação Windows/DPAPI nativa pendente | /metrics Prometheus, painel embutido/read-only com Basic Auth para browser, tarefa agendada com reinício limitado, DPAPI opcional via Win32 e documentação operacional/API/segurança/provedores |
| F9 - Integracao Mathlab | integração de código aplicada nos dois repositórios; aceitação E2E externa pendente | Mathlab usa `OpenAICompatibleProvider`, seis aliases, headers de política, capacidade remota e streaming opt-in; usa uma única `KEYPOOL_API_KEY`; testes offline aprovados; serviços/credenciais reais ainda não disponíveis |

## F3 — gateway e roteador dev

- `internal/config`: snapshot validado, leitura YAML + variáveis de ambiente, seleção de chaves por projeto, defaults/force/requires e política de spill com padrão habilitado sem perder `false` explícito após JSON.
- `internal/router`: resolução de alias/alvo literal autorizado, aplicação de `defaults`/`force`, filtro de capacidades `requires`, `X-Keypool-Exclude` e `X-Keypool-Avoid-Weights`, ajuste do orçamento TPM, escolha por menor espera e spill, limite `max_inflight`, timeout/cancelamento, prioridade, dead-target TTL e políticas de retry/fallback para 401/402/403/404/429/5xx.
- Streaming: o primeiro evento SSE é validado antes dos headers; depois disso os bytes são encaminhados incrementalmente. `usage` final, término inesperado e cancelamento atualizam a lease; o cancelamento e o encerramento liberam a vaga do projeto e o bloqueio de recarga do snapshot.
- Recarga: `Gateway.SetSnapshot` troca router/pools e snapshot autenticado como uma operação coordenada. Pools preservam estado por ID de chave e a recarga aguarda streams ativos concluírem. Relógio injetável disponível em router e gateway.
- `internal/gateway`: `/healthz`, `/readyz`, `/v1/chat/completions`, `/v1/models` (aliases ordenados) e `/v1/capacity`; autenticação Bearer/`X-API-Key`, IDs, limites do corpo, cabeçalhos de política, flush SSE e log de uso sem prompt/resposta.
- `internal/store/memstore` e `keypoold serve --dev`: configuração e chaves do YAML/ambiente são carregadas no store em memória; snapshotter restaura/persiste o estado durante a execução; eventos do scheduler e uso são conectados ao store. O modo dev permanece volátil entre reinicializações.
- Testes E2E offline ampliados: streaming incremental, fallback do primeiro evento SSE, cancelamento, spill, reload concorrente, limites/capacidade, requisitos de provider, filtros, quota, 403, 404, 5xx, timeout, retry-after e orçamento TPM. Scheduler mantém testes de prioridade e watchdog de lease.

## F4 — implementacao

- `internal/cryptobox`: AES-256-GCM versionado, nonce de 12 bytes, AAD vinculada a ID/provedor, parsing base64 de master key, criacao exclusiva do arquivo e verificacao autenticada de master key.
- `internal/store/migrations`: SQL embutido, tabela de controle, checksums SHA-256, transacao por migracao e verificacao de divergencias. `0001_init.sql` permanece imutavel; campos adicionados posteriormente ficam em `0002_config_runtime.sql`.
- `internal/store/pgstore`: implementa `store.Store`; configura providers/projetos/regras/rotas; cifra e decifra chaves; persiste e restaura snapshots; grava usage e eventos; controla API key por hash SHA-256 e comparacao em tempo constante; verifica `master_key_check` antes de decifrar.
- `internal/usage`: fila limitada e nao bloqueante, INSERT multi-row em lotes, contador de descartes e drenagem ao encerrar.
- `internal/events`: fila propria limitada para eventos do scheduler, com descarte contabilizado e flush no encerramento.
- `internal/store/snapshotter.go`: restaura antes do listener, persiste somente quando o estado muda, executa ciclo periodico de 10 s e faz flush no encerramento.
- `pgstore.AcquirePoolAdvisoryLock`: mantem a sessao que adquiriu a trava ate o unlock; em falha de unlock, fecha a conexao fisica antes de devolve-la ao pool. O migrador serializa cada transacao com `pg_advisory_xact_lock` e rejeita checksums divergentes ou versoes desconhecidas.
- `pgstore.PrunePool`: rollup UTC de eventos antigos para `usage_daily` e limpeza transacional de uso, saude e eventos.
- `keypoold`: `migrate`, `init`, `serve` com PostgreSQL, `serve --dev` em memoria, `doctor` e `version`; importacao YAML inicial atomica com insercao das chaves cifradas.
- `scripts/windows`: setup idempotente do banco, `run-dev.ps1`, ACL da master key e `test.ps1 -Integration` para executar os testes de PostgreSQL.
- `internal/store/pgstore/integration_test.go`: teste com schema temporario cobre migracoes/checksums, init, API key, master key, SaveConfig/LoadConfig, cifra ida-e-volta, Build, snapshots, uso, eventos, lock, exclusao e rollup.


## F5 — admin e CLI (integração com F6 concluída)

- `internal/admin`: API administrativa montada sob a mesma autenticação `kp_` do gateway. Rotas para apply, provedores, projetos, regras de chaves, rotas, chaves, estatísticas, uso, eventos e rotação da API key.
- `POST /admin/v1/apply`: aceita configuração completa `config.Raw` ou perfil único `ProjectRaw`, rejeita campos desconhecidos e valida com `config.Build` antes de persistir. Credenciais existentes são preservadas quando o provedor permanece no documento.
- Chaves: criação, listagem mascarada, edição de metadados, habilitar/desabilitar e remoção. `POST /admin/v1/keys/{id}/test` executa L1; `key enable` solicita revalidação e limpa o estado inválido do scheduler.
- `POST /admin/v1/warm/run` e `GET /admin/v1/warm/status` estão conectados ao gerenciador F6. O CLI oferece `warm run --provider NAME --key ID` e `warm status`.
- `GET /admin/v1/stats` inclui `warm` com prontidão, filas, workers e diagnóstico por chave (saúde, catálogo, sonda recente, conexão, hints e EWMA).
- `apikey rotate` grava apenas o hash SHA-256 e atualiza o token em execução. `usage` e `events` têm leitura no pgstore e no memstore.
- `cmd/keypoolctl`: cliente HTTP com `status`, `provider`, `key`, `project`, `apikey`, `usage`, `events`, `warm` e `capacity`. `key add` aceita segredo somente via `--stdin`; `key import-env` reutiliza `config.KeysFromEnv`; `key reveal` é leitura local do PostgreSQL com confirmação.

Pendências que permanecem fora do critério F6:

- `cmd/keypoolctl`: saída tabular por padrão, `--json` global, status agregado e filtro `key list --provider` implementados; testes unitários da formatação passaram. A compilação integrada ainda depende do driver `pgx` e do Go 1.26+.
- Ampliar e executar a matriz de integração da API administrativa contra PostgreSQL real (CRUD, rotação e consultas), com Go 1.26+ e dependências reais.

## F6 — aquecimento, saúde e catálogo (implementada no código)

- `internal/warm`: scheduler por heap de próximos horários, worker pool com máximo de quatro execuções, limite dinâmico `warm.workers`, exclusão de sondas simultâneas por chave, fila limitada, jitter determinístico ±10–20%, backoff e respeito a `Retry-After`. Sondas manuais compartilham o mesmo orçamento de workers das tarefas agendadas.
- **L0 — conexão:** GET autenticado no endpoint de modelos usando o mesmo cliente/transport do gateway; `httptrace` registra DNS, connect, TLS, TTFB, reuso e estado de conexão. Só é executado quando o conjunto de chaves do host está ocioso; `MaxIdleConnsPerHost` é limitado a `min(4, número de chaves)`. Não gasta tokens; `warm.counts_against_limits` pode reservar RPM local explicitamente.
- **L1 — validação:** adaptador executa `/models` ou endpoint de chave (`probe.kind: key`), registra credencial inválida, mantém catálogo por chave, obtém account info e consome cabeçalhos de rate limit. Catálogo tem TTL configurável (padrão 1 h) e é persistido; no boot, a confiança no catálogo é reestabelecida por L1 com a credencial atualmente carregada. `probe.kind` omitido vira `models`; `probe.kind: none` desabilita L1 explicitamente.
- **L2 — profunda:** completion de no máximo um token, desligada por padrão. Quando habilitada exige credencial validada por L1, modelo presente/autorizado, RPD conhecido e acima do mínimo, orçamento diário disponível, conta não free e chave sem tráfego real pelo intervalo configurado. O orçamento só é debitado quando o transporte confirma escrita da requisição; orçamento consumido é reconstruído por `usage_events` persistidos.
- Saúde e circuito: sondas e tráfego real atualizam estado; três falhas consecutivas abrem circuito a partir de 30 s com backoff exponencial limitado a 10 min; meia-abertura permite uma requisição; 429 é neutro e não invalida credenciais nem abre circuito. Chaves inválidas só voltam após L1 bem-sucedida ou `key enable`.
- Hints: cabeçalhos configurados de limite são agregados por grupo/bucket/dimensão, incluindo hints globais aprendidos antes de o catálogo estar disponível. L2 considera limite RPD aprendido tanto no bucket do modelo quanto no bucket global.
- Diagnóstico: EWMA de TTFB/tempo total por chave/modelo, horários e idade de L0/L1/L2, conexão, catálogo, account info, saúde, versão da credencial e hints são expostos no `warm/status` e em `admin/stats`.
- Persistência: `health_checks` registra cada tentativa; `model_catalog` registra a lista por chave; `usage_events.source='warmup'` registra sondas. Catálogo é removido ao detectar rotação de credencial e também invalidado no boot, até uma L1 bem-sucedida com a credencial atualmente carregada; health checks têm retenção padrão de sete dias.
- Prontidão: `/readyz` aguarda a primeira passada L1 nas chaves configuradas até `ready_timeout` (padrão 20 s), então libera com motivo de aviso; camadas/provedores desabilitados e `probe.kind: none` não bloqueiam indefinidamente.
- Recarga: pools/transportes usados por sondas são fixados durante a operação; reload aguarda probes concluírem. Snapshotter dinâmico acompanha pools novos após `SetSnapshot`, evitando persistir instâncias antigas. Rotação de credencial limpa catálogo, validade e EWMA anterior.
- Testes adicionados/cobertos: chave inválida na L1; 429 neutro; L0/L1 e catálogo; readiness e timeout; chave ocupada; L2 orçamento/free/RPD e escrita de request; expiração de catálogo; rotação de credencial; limite global dos workers manuais; snapshotter após reload; invalidação de saúde/catálogo no boot; L1 explicitamente desabilitada sem bloquear o roteamento.

## F7 — extras de eficiência (implementada no código)

- `POST /v1/leases` reserva uma chave/modelo para uso direto e devolve credencial apenas quando `project.allow_lease: true`. `POST /v1/leases/{id}/report` fecha a reserva com `success`, `rate_limited`, `quota`, `auth`, `forbidden`, `transient` ou `cancel`; os relatórios são auditados em `events` (projeto, lease, `key_id`, `key_label`, sem segredo) e `usage_events` (`source='lease'`).
- Cada lease tem TTL limitado a 120 s, watchdog e ID aleatório de 128 bits. Expiração ou desligamento gracioso finaliza como `leaked`, contabiliza a estimativa conservadora e libera `max_inflight`, a vaga da chave e o bloqueio de reload. O shutdown finaliza leases antes de drenar o writer de uso.
- `ANY /p/{provider}/{path...}` implementa passthrough para `generic_http`. A credencial do provedor substitui credenciais do cliente; cabeçalhos hop-by-hop (inclusive os indicados dinamicamente por `Connection`), `Authorization`, `X-API-Key` e `X-Keypool-*` não são encaminhados. O custo é uma unidade por requisição e a chave permanece reservada até terminar a cópia do corpo.
- `max_inflight` compartilha a mesma admissão por projeto entre chat, leases e passthrough. `max_wait` limita tanto a espera pela vaga de concorrência quanto a aquisição da chave.
- `daily_token_budget` usa reservas em memória e reconciliação com tokens reais/estimados. O consumo UTC é reconstruído no boot a partir de eventos `request`, `lease` e `passthrough`; sondas `warmup` ficam excluídas. Migração `0003_f7_usage_actual_tokens.sql` armazena `actual_tokens` e consolida `usage_daily.total_tokens`.
- Estratégias `headroom` e `latency` já estão conectadas ao seletor de chaves: folga relativa por bucket e P2C com EWMA TTFB, respectivamente. O calibrador de estimativa usa EWMA α=0,2 por (provedor, modelo), fator limitado a [0,5; 2,0].
- Testes F7 cobrem criação/relatório/duplicidade de lease, permissão `allow_lease`, watchdog e shutdown, 429 neutro, passthrough com credencial injetada, headers e path traversal, autorização de chave, reserva/reconciliação de orçamento, restauração de `actual_tokens`, `max_wait`, TTFB e calibração. Há `BenchmarkGatewayPassthrough` para aferição no ambiente-alvo.
- Resultado local: `go test` e `go test -race` (em dois grupos), `go vet` e `gofmt` passaram nos 25 pacotes internos exceto `pgstore`; benchmarks curtos estão registrados em `docs/VERIFY.md`. O teste de integração PostgreSQL foi ampliado para validar `ReadProjectDailyTokens` e `usage_daily.total_tokens`, mas não pôde ser executado neste ambiente.
- Limitação: testes que importam `pgstore` e compilação de `cmd/keypoold` continuam precisando de Go 1.26+, `pgx` real e PostgreSQL 16+. Neste ambiente, os pacotes internos sem `pgx` são os que podem ser efetivamente executados; veja a seção de validação abaixo.


## Limites objetivos da validacao

Ambiente desta sessão: Go 1.23.2 Linux/amd64; o projeto declara Go 1.26. Não há PowerShell, PostgreSQL, `psql`, `initdb`, módulos reais `pgx`/`yaml.v3` em cache nem DNS funcional para baixar dependências. O `go.mod` do projeto não foi alterado e nenhum `go.sum` foi inventado.

Verificações realizadas nesta entrega, usando um modfile temporário externo ao repositório (`/tmp/keypool-check.mod`) que substitui somente `yaml.v3` por um stub mínimo:

- `gofmt` em todos os arquivos Go: limpo.
- `go test` em todos os pacotes sob `internal/`, exceto `internal/store/pgstore` (que exige `pgx`): passou.
- `go test -race` no mesmo conjunto de pacotes internos: passou.
- `go vet` no mesmo conjunto: passou.
- Testes novos de warm, scheduler, router e snapshotter: passaram, inclusive testes de prontidão, limite de concorrência, catálogo/TTL, rotação de segredo e persistência após reload.

Limitações que permanecem: os comandos `go build ./...`, `go test ./...` e `go test -tags integration ./internal/store/pgstore` não foram confirmados com as dependências reais. O daemon e o CLI importam `pgstore`, portanto sua compilação integrada precisa ser repetida com Go 1.26+ e `pgx` real. Também não foi possível executar migrações/consultas contra PostgreSQL 16+. O stub YAML não valida comportamento completo de `yaml.v3`, em especial `yaml.Node.Decode` e `KnownFields`.

Comandos para aceitação final no ambiente-alvo (Windows/Go 1.26+/PostgreSQL 16+):

```powershell
gofmt -l .
go vet ./...
go build ./...
go test ./...
go test -race ./...
$env:KEYPOOL_TEST_DATABASE_URL = "postgres://keypool:senha@127.0.0.1:5432/keypool_test?sslmode=disable"
go test -tags integration ./internal/store/pgstore
```

**Conclusão de status:** F6 está implementada no código e passou nas verificações que este ambiente suporta. A aceitação externa de compilação integrada e PostgreSQL real permanece pendente por indisponibilidade das ferramentas/dependências, não por uma falha de teste observada.

Comandos operacionais e detalhes: `docs/VERIFY.md` e `docs/POSTGRESQL_F4.md`.


## F8 — acabamento e observabilidade (implementada no código)

- `internal/metrics`: registry concorrente sem dependência externa; exposição Prometheus 0.0.4 por `/metrics`, com HELP/TYPE, histogramas cumulativos e escaping de labels. Métricas cobrem requisições/tentativas, fila, TTFB, estado de chave, razão de janela, cooldown, idade de L0/L1/L2, descartes de usage e leases vazados.
- `GET /metrics` e `GET /` exigem a API key do KeyPool. O painel é HTML embutido via `go:embed`, somente leitura, sem JavaScript, CDN, fonte ou recurso externo, com CSP restritiva.
- Gateway registra requisições de chat e passthrough; o roteador fornece TTFB no resultado para histograma, e o scraper lê snapshots de pool/aquecedor sob demanda, fora do caminho de cada requisição.
- `keypoold init|serve|doctor --master-key-dpapi` ativa proteção DPAPI (Windows, escopo do usuário atual) sem dependência Go externa; padrão arquivo base64/ACL continua disponível. DPAPI e `KEYPOOL_MASTER_KEY` são mutuamente exclusivos.
- `scripts/windows/install-task.ps1` instala/remover tarefa `AtLogOn` oculta, usuário atual, nível `Limited`, sem elevação e `IgnoreNew` para evitar instâncias duplicadas.
- Documentação final criada: `docs/OPERACAO_WINDOWS.md`, `docs/API.md`, `docs/SECURITY.md`, `docs/PROVIDERS.md` e guia de integração e aceitação E2E da F9 em `docs/INTEGRACAO_MATHLAB.md`.
- Validação local de endpoint, formato, autenticação, histogramas e labels passa nos testes offline. A compilação do daemon com pgx real e execução Windows/DPAPI/Agendador de Tarefas não puderam ser executadas neste ambiente; consultar `docs/VERIFY.md`.


## F9 — integração de código com o Mathlab

- A integração foi aplicada no repositório Mathlab externo em `mathlab-main/`: `configs/providers.yaml` define o KeyPool como provedor padrão, `configs/models.yaml` direciona os seis papéis a aliases, e `LLMClient` encaminha prioridade/exclusões/timeout por headers.
- `capacity.py` consulta `/v1/capacity` por alias com cache curto e fallback conservador; `OpenAICompatibleProvider` encaminha `reasoning_effort`, preserva `Retry-After` e oferece SSE incremental opt-in com cancelamento.
- O contrato de autenticação do Mathlab é uma única credencial de cliente `KEYPOOL_API_KEY` (`kp_...`). `build_pools` rejeita mais de uma credencial para provedores gerenciados que declaram `capacity_path`; a rotação de credencial ocorre no próprio KeyPool.
- O perfil declarativo `integrations/mathlab/keypool-project.yaml` e o smoke test offline do lado KeyPool foram mantidos. O projeto Mathlab também contém `integrations/keypool/` e testes específicos.
- Testes offline do Mathlab confirmam headers, payload, resposta 429 sem retry local, capacidade remota, fallback, streaming incremental e cancelamento. Testes offline do KeyPool verificam o contrato do smoke test.
- A integração de código está concluída nos dois repositórios. A aceitação ponta a ponta com KeyPool/PostgreSQL/provedor upstream ativos permanece pendente; não remova módulos locais legados até essa aceitação confirmar equivalência e estabilidade.
- Validações externas pendentes: Go 1.26+ com dependências reais, PostgreSQL 16+, DPAPI/Task Scheduler no Windows, chamada real Mathlab→KeyPool→upstream, concorrência, reinicialização, cancelamento upstream e execução de tarefa Lean real.

## Rodada final de consistência (10/11/2026)

- CLI: saída tabular/chave-valor por padrão, `--json` global, `status --json` agregado, filtro `key list --provider` e proteção de saída para campos sensíveis. Respostas com `api_key` de uso único são preservadas em JSON para que a rotação não perca a credencial; não registre essa saída. Testes unitários da camada de formatação passaram.
- SSE: `Reader.NextRaw`/`Reset` reutilizam buffers no caminho do roteador; `BenchmarkSSEForward` mede 0 alocações por stream sintético nesta máquina. `BenchmarkWaitForRoom` também mede 0 alocações.
- Determinismo: o teste de watchdog de lease não usa `time.Sleep`; usa relógio falso e sincronização por evento. Não há `time.Sleep` nos arquivos `*_test.go` do gateway.
- Mathlab: integração de código aplicada nos dois repositórios. A suíte específica F9 passou (19 testes), a suíte offline ampla passou (671 testes, 19 skips, excluindo o módulo Z3 que requer `z3-solver`), `compileall`, YAML e CLI `providers`/`models --no-check` passaram. O cliente usa uma única `KEYPOOL_API_KEY`; rotação upstream fica no KeyPool. O smoke kit KeyPool passou 6/6.
- Metas de desempenho ainda não atingidas: `AcquireSuccess` e `AcquireContended` continuam acima do alvo de `<5 µs` e `≤2 alocações` da seção 17. Valores atuais medidos neste ambiente estão em `docs/VERIFY.md`; não foram feitas otimizações arriscadas sem evidência.
- Não foi possível validar build Go completo, PostgreSQL real, DPAPI/Task Scheduler ou E2E entre serviços neste host. Consulte `docs/VERIFY.md`.

## Medições objetivas da rodada final

Benchmarks curtos locais (Go 1.23.2/Linux, AMD EPYC 9V74): AcquireSuccess 8,362 ns/12 allocs; AcquireContended 5,665 ns/15 allocs; WindowAdd 171.5 ns/0 allocs; WaitForRoom 399.3 ns/0 allocs; SSEForward 917.1 ns/0 allocs; GatewayPassthrough 134,137 ns/282 allocs. As metas de `<5 µs` e `≤2 allocs` de Acquire ainda não foram atingidas. Valores e bytes/op estão detalhados em `docs/VERIFY.md`.
