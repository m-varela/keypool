# Status de implementacao

Data de referencia: 10/11/2026.

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
