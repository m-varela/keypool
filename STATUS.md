# Status de implementacao

Data de referencia: 14/11/2026. Versao do daemon: 0.4.2.

| Fase | Estado | Entrega |
|---|---|---|
| F0 - Fundacao | concluida | modulo Go, dominio, clock, segredos, estrutura e scripts |
| F1 - Nucleo | concluida | janelas, limites, scheduler, leases, snapshots, hints e circuit breaker |
| F2 - Provedores | concluida | classify, SSE, specs/adapters, presets JSON e fake upstream |
| F3 - Gateway/roteador | concluida no codigo | roteamento/spill, filtros, retries, streaming SSE, endpoints e testes offline |
| F4 - Persistencia anterior | historica | esta fase usava PostgreSQL; o backend foi removido na F10 |
| F5 - Admin e CLI | concluida no codigo | API admin/CRUD/apply, key test, warm/run, warm/status, `keypoolctl`, reveal local |
| F6 - Aquecimento e saude | concluida no codigo | L0/L1/L2, scheduler heap, workers limitados, catalogo/TTL, hints, EWMA e readiness |
| F7 - Extras de eficiencia | concluida no codigo | leases HTTP, passthrough generic_http, headroom/latency, calibracao EWMA e politicas por projeto |
| F8 - Acabamento | concluida no codigo | metricas Prometheus, painel embutido, DPAPI opcional e operacao Windows |
| F9 - Integracao de cliente externo | codigo integrado; E2E externo pendente | OpenAICompatibleProvider, aliases, headers de politica, capacidade remota e streaming opt-in |
| F10 - Remocao do PostgreSQL | implementada nesta entrega; verificacao Go 1.26 pendente | store em memoria com checkpoint `state.json`, JSON-only, daemon de modo unico, CLI local e limpeza de docs/scripts |

## F10 — implementacao

- `internal/store/memstore` agora oferece `Open`, `Initialize`, `Close`, validacao do checkpoint e mutacoes duraveis por commit. `New` permanece apenas como construtor sem disco para testes.
- `state.json` guarda a configuracao, credenciais AES-256-GCM, hash SHA-256/metadados da API key, snapshots dos pools e contadores diarios. O historico de uso/eventos/saude e volatil e limitado a 50.000/10.000/1.000 registros com retencao por idade.
- `commitMu` serializa mutacoes duraveis e a gravacao atomica; `mu` e `usageMu` nunca sao mantidos durante I/O.
- `keypoold` tem apenas `serve`, `doctor` e `version`. O bootstrap cria master key/checkpoint apenas depois de obter o listener loopback exclusivo.
- O daemon conecta diretamente sinks de usage/events ao store; `keypool_usage_dropped_total` agora conta descartes por limite de retencao em memoria.
- Configuracao, presets e `keypoolctl project apply` usam JSON; `Raw.ProjectFile` e lido relativamente ao arquivo de configuracao, mas nao entra no checkpoint.
- Foram removidos `pgstore`, migracoes SQL, writers de usage/events, comandos `init`/`migrate`/`serve --dev`, scripts de banco e documentos operacionais obsoletos.
- E2E offline cobre primeiro bootstrap, resposta do fake upstream, bind duplicado, chave criada via admin, reinicio, restauracao de snapshot/contadores, API key incorreta e ausencia de segredos em texto claro no arquivo.

## Limites objetivos desta entrega

O host disponibiliza Go 1.23.2; Go 1.26 nao esta instalado e a tentativa de baixar o toolchain falhou por indisponibilidade de DNS/rede. Por isso os comandos oficiais com Go 1.26 nao foram executados. Para deteccao de erros de codigo foram usados testes/vet com modfile temporario externo que declara Go 1.23; os resultados estao discriminados em `docs/VERIFY.md` e nao substituem a verificacao nativa.

O ZIP original nao contem metadados `.git`. Nao foi possivel criar branch ou registrar um commit por fase. Nenhum historico Git foi inventado.

A execucao nativa de PowerShell/Windows/DPAPI e a integracao real cliente -> KeyPool -> provedor permanecem pendentes. Nenhuma chamada a provedor real foi feita.


## Timeout de fila separado do prazo total

- `X-Keypool-Timeout-Ms` continua sendo o prazo total da requisição, incluindo espera na fila e geração upstream.
- Adicionado `X-Keypool-Queue-Timeout-Ms`, que limita somente a espera por capacidade; sua expiração retorna HTTP 429 com código `keypool_queue_timeout`. O timeout total continua retornando HTTP 408 com código `keypool_timeout`.
- A separação impede que um consumidor configure, por engano, um timeout curto de fila que cancele gerações longas. O contrato é genérico e não contém regras específicas de qualquer consumidor ou domínio.
- Validação: `go test ./...` passou em todas as packages com Go 1.23.2 e modfile temporário externo ajustado para `go 1.23`. O `go.mod` original permanece em Go 1.26; testes nativos com Go 1.26 e `go vet`/`go build` não foram executados devido à indisponibilidade do toolchain e de rede para baixá-lo.


## Diagnostico de erro da API

- Todas as respostas produzidas por `writeError` incluem `X-Keypool-Error-Code`, espelhando o `error.code` do envelope JSON. O cabeçalho complementa, não substitui, o corpo e `X-Keypool-Request-Id`.
- O contrato é genérico: não implementa retry/cooldown no consumidor e não contém qualquer conceito de MathLab.
- Teste offline: o cenário de fila saturada verifica HTTP 429, `error.code=keypool_queue_timeout` e o cabeçalho correspondente.


## 2026-11-13 — Diagnóstico estruturado de falha de rota

- `Router.Do` retorna `RouteFailure` tipado para falhas/expirações de contexto relevantes, preservando fase, último alvo upstream conhecido, total de tentativas e indicação de fallback. A falha de fila é explicitamente classificada como `queue`; timeout após chamada upstream é `upstream`.
- O gateway mantém `X-Keypool-Error-Code`, `X-Keypool-Request-Id` e envelope antigo, adicionando metadados de fase/alvo/tentativas/fallback. `retryable`/`Retry-After` são omitidos quando não conhecidos. O alvo passa por redaction.
- Métricas adicionadas: falhas por fase/código, tentativas preservadas e timeouts por fase; labels não incluem request ID ou texto livre.
- Testes focados: `internal/router`, `internal/gateway` e `internal/metrics` aprovados com `go test` e modfile temporário externo. Testes globais/vet/build/race ainda serão registrados em `docs/VERIFY.md` após execução.
- Limitação de toolchain: o `go.mod` de origem exige Go 1.26; o ambiente possui Go 1.23.2. O manifesto original foi preservado. A execução com modfile temporário é apenas diagnóstico e não substitui validação com Go 1.26.
- O pacote de origem não contém `docs/PLANO.md`, `docs/DECISIONS.md` ou `docs/VERIFY.md`. `docs/PLANO.md` não foi reconstruído como se fosse artefato original; decisões e verificação desta alteração são registradas nos arquivos novos correspondentes.


## F11 — fallback com orçamento por tentativa (0.4.2)

- `policy.attempt_timeout` é uma capacidade genérica opcional por projeto e limita chamadas upstream não-stream sem encerrar o deadline global que ainda pode ser usado para fallback.
- O roteador mantém na cadeia os candidatos sem vaga imediata; tenta primeiro os disponíveis, e depois tenta os demais em janelas de fila limitadas. Uma falha transitória de um provedor não elimina o fallback para um provedor temporariamente ocupado.
- A política não conhece nomes ou papéis do MathLab; o consumidor fornece o timeout no próprio perfil de projeto.
- Testes de regressão cobrem saturação de fila multi-provedor, fallback de upstream lento e validação do campo de política.


## Recuperação de fila e fallback transitório

- Com spill habilitado e outro alvo elegível, falha transitória avança para esse alvo antes de repetir o modelo com outra chave. Sem alternativa, permanece a política anterior de retries.
- Timeout de fila inclui a menor espera restante conhecida entre os candidatos. Estimativas são recalculadas; ocupação sem prazo conhecido mantém `Retry-After` ausente.
- Regressões offline cobrem múltiplas chaves no upstream lento, cooldown conhecido e ocupação indeterminada. Go 1.26.0: testes globais, vet, build e race de router/gateway/sched aprovados; detalhes em `docs/VERIFY.md`.


## Preferências após validação de capacidade

Exclusões e avoid-weights são aplicados aos candidatos viáveis, após validação de
chaves, capacidade, TPM e cotas. Uma rota nominal incapaz de atender a requisição
não elimina a única alternativa viável. Nenhuma cota ou restrição de capacidade é
relaxada. Regressões cobrem exclusão, avoid-weights, preferência válida e TPM impossível.
`docs/PLANO.md` continua ausente no pacote recebido; não houve novo contrato de API.
