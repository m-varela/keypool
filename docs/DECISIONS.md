# Decisoes e desvios

## Decisoes iniciais

- Modulo `keypool`: o prompt permite esse caminho quando nao ha usuario Git conhecido.
- `go 1.26`; dependencias de execucao `gopkg.in/yaml.v3 v3.0.1` e `github.com/jackc/pgx/v5 v5.12.0`. O codigo que importa `pgx` fica restrito a `internal/store/pgstore`.
- Servidor dev escuta somente em loopback. Uma unica API key KeyPool e validada por SHA-256 + comparacao em tempo constante.
- Configuracao construida em `config.Snapshot`; segredos encapsulados em `secret.Secret`; store isolado por interface.
- Python do Mathlab e a especificacao de comportamento para `key_pool.py`, `rate_limiter.py` e `ProviderLimits.resolve`.

## Semanticas portadas

- `limits.Resolve`: cada dimensao herda independentemente na ordem `per_model` > primeira regra correspondente que defina a dimensao > default. A primeira regra que casa fixa `bucket`/`free`, mesmo se nao definir aquela dimensao. Confirmado em `ProviderLimits.resolve` do Python.
- Glob: `*` atravessa `/`, diferentemente de `path.Match`, para IDs como `openai/gpt-oss-120b` e `meta/x:free`.
- `sliding.Window`: entradas permanecem validas quando `At == now-span`; RPM/RPD contam entradas e nao soma de tokens.
- TPM com `need >= limit` e impossivel. TPD usa `max(1, floor(tpd*(1-reserve)))` e preserva a peculiaridade Python: `need >= limit` nao gera espera na janela; a decisao de impossibilidade pertence ao pool.
- `per_model` aceita tanto mapeamento por dimensao quanto inteiro legado (interpretado como TPM), reproduzindo `ProviderLimits.resolve` Python sem importar YAML em `limits`.
- `from_env`: concatena `PREFIX_API_KEYS`, `PREFIX_API_KEY`, depois `PREFIX_API_KEY_n` em ordem numerica e remove duplicatas.
- `Secret.Mask`: comprimento ate 8 vira a mesma quantidade de `*`; acima disso usa quatro caracteres iniciais, reticencias Unicode e quatro finais. Redaction inclui tambem `kp_` conforme o contrato do prompt.

## Desvios/limitacoes desta sessao

1. **F3 concluída no código:** as limitações iniciais de streaming, spill, `requires`, filtros, timeout, recarga de snapshot e relógio injetável foram tratadas na implementação atual. A validação nativa com Go 1.26 e dependências reais permanece pendente por restrição do ambiente.
2. `/v1/capacity` ainda é uma estimativa operacional baseada nas pools e limites carregados; não reproduz integralmente todas as dimensões de `compute_capacity` do Mathlab.
3. A configuração dev permanece volátil entre reinicializações; o modo PostgreSQL persiste configuração e estado.
4. `key test`, `warm/run` e `warm/status` são conectados ao `internal/warm.Manager`; endpoints sem um Manager conectado respondem 501 explicitamente, sem simular sucesso.
5. `stats` inclui diagnóstico do aquecedor (saúde, prontidão, filas, catálogo, hints, conexões e EWMA); métricas Prometheus e painel continuam na F8.
6. Chaves carregadas do ambiente recebem tag fixa `mathlab`; isso serve ao exemplo inicial mas precisa generalizacao para multiplos projetos.
7. Os adaptadores usam `rate_headers` configuráveis por dimensão; hints aprendidos são associados aos buckets do catálogo e ao bucket global quando chegam antes da primeira lista de modelos.
8. Os benchmarks locais mediram `Acquire` em aproximadamente 1,1 us/op no caminho de sucesso e 1,6 us/op no caminho contendido (medicao curta de 100 iteracoes), porem 12-16 alocacoes/op; a meta de <=2 alocacoes nao foi atingida. `WindowAdd` ficou em aproximadamente 231 ns/op e 0 alocacoes/op.
9. O ambiente tinha Go 1.23.2, sem PowerShell e sem o modulo YAML em cache; nao foi possivel executar com Go 1.26 nem com o `yaml.v3` real. Para checar sintaxe/compilacao/testes offline foi usado um `-modfile` temporario e um stub local minimo de YAML, fora do repositorio. Portanto, parsing YAML e presets reais ainda exigem verificacao no ambiente do usuario.
10. Nao foi gerado `go.sum`, pois nao foi possivel resolver/baixar a dependencia real e o prompt proibe inventar esse arquivo.

## Seguranca

- Nenhuma chave real foi fornecida ou usada. Nenhum provedor real foi acessado.
- O fake upstream usa `httptest` local. Logs/respostas sao redigidos e o gateway nao deve registrar corpos ou Authorization.
- `Secret.Reveal` aparece nos adaptadores ao montar a requisicao HTTP. Nao use fora desse ponto sem registrar uma decisao de seguranca.

## F4 — PostgreSQL, cifra e persistencia

- O envelope AES-256-GCM usa `versao(1) || nonce(12) || ciphertext+tag`; AAD e `id || "||" || provider`.
- A master key aceita `KEYPOOL_MASTER_KEY` como base64 padrao/raw/URL; sem variavel, `LoadOrCreateMasterKey` cria/carrega arquivo exclusivo com permissao Unix 0600 e recusa arquivo existente com permissoes POSIX mais abertas. No Windows, a ACL e aplicada por `acl-master-key.ps1`. `master_key_check` e validado antes de decifrar as chaves persistidas.
- `internal/store/migrations` apenas embute/lista SQL e calcula SHA-256; a execucao fica isolada em `pgstore`. Cada arquivo e aplicado em transacao propria sob advisory xact lock, serializando execucoes concorrentes. `0001_init.sql` nao foi reescrito; campos de configuracao foram adicionados em `0002_config_runtime.sql`. O verificador rejeita checksums divergentes, migracoes pendentes e versoes desconhecidas.
- `pgstore.Store` implementa `store.Store`; as configuracoes sao normalizadas em providers/projects/rules/routes e as chaves ficam exclusivamente em `keys.secret_enc`. O segredo da API local e armazenado apenas como SHA-256.
- IDs de chave legados sao mapeados para UUID estavel; quando uma credencial/rotulo ja existe, o seletor usa o UUID canonico que de fato foi persistido.
- `usage.Writer` usa canal limitado, INSERT multi-row em lotes, timeout e contador de descartes. `events.Writer` persiste eventos do scheduler por uma fila limitada independente. `store.Snapshotter` restaura antes do listener, persiste apenas mudancas e tenta flush no encerramento.
- `PoolAdvisoryLock` fixa uma conexao `pgxpool.Conn` para o ciclo inteiro da trava de sessao; se nao consegue destravar, fecha a conexao fisica para nao devolver uma sessao travada ao pool.
- `PrunePool` agrega eventos antigos em `usage_daily` por dia UTC e apaga os registros brutos na mesma transacao; tambem limpa `health_checks` e `events`. `keypoold doctor` estima o desvio entre o relogio local e `clock_timestamp()` do PostgreSQL.
- A integracao real depende de PostgreSQL 16+ e e coberta pelo build tag `integration`; o ambiente de preparacao nao tinha servidor nem as dependencias Go baixadas, portanto o teste so foi compilado com stubs locais, sem afirmar sucesso contra banco real.


## F5 — API administrativa e CLI

- `internal/admin` é montado em `/admin/v1/` pelo gateway e reutiliza sua autenticação Bearer/`X-API-Key`; não existe autenticação paralela.
- `apply` aceita tanto o documento completo de configuração quanto um perfil único `ProjectRaw`, como `configs/examples/mathlab.project.yaml`. JSON usa `DisallowUnknownFields`; YAML usa `KnownFields(true)`. `config.Build` valida antes da persistência e `pgstore.SaveConfig` aplica a alteração numa transação.
- As credenciais existentes são carregadas do store e preservadas quando o provedor permanece no `apply`. Segredos não são devolvidos nas respostas administrativas; há redator de resposta baseado nas credenciais carregadas. A rotação da API key é a única resposta que intencionalmente retorna uma credencial nova uma vez.
- `keypoolctl key add` exige `--stdin`; `key import-env` chama `config.KeysFromEnv`, a mesma função usada pelo bootstrap dev. `key reveal` é operação local com banco/master key e confirmação, sem rota HTTP.
- O `memstore` mantém a semântica de chaves desabilitadas ao salvar configurações; isso evita que um `apply` de projeto apague chaves desabilitadas no modo dev.
- `key test` e endpoints `warm` são implementados na F6; `stats` agrega diagnóstico F6, enquanto `/metrics` e painel são da F8.

## F6 — decisões de aquecimento

- A ausência de `probe.kind` é normalizada para `models` (L1 GET `/models`); apenas `probe.kind: none` desabilita explicitamente L1. Isso evita marcar uma credencial válida sem executar uma sonda.
- L0 compartilha o cliente/transport HTTP do gateway. A reserva `BeginProbe` impede tráfego real simultâneo na mesma chave; além disso L0 só aquece o host quando todas as chaves daquele provider estão ociosas.
- O limite efetivo de workers é `min(config.warm.workers, 4)`. A fila e as chamadas administrativas de sonda compartilham o mesmo orçamento global.
- L2 permanece opt-in. O orçamento diário é reconstruído de `usage_events` com `source='warmup'` e alias `warmup-L2`; só é debitado após `httptrace.WroteRequest` confirmar envio pelo transporte.
- Catálogo de uma credencial substituída não pode sobreviver ao reload: a geração da credencial é incrementada, validação/catalog/EWMA são limpos, e a linha persistida é invalidada. No boot, snapshots preservam janelas de uso/cota, mas estado derivado de credencial (saúde, validação, catálogo e EWMA) é invalidado; cada chave elegível passa por L1 com a credencial atualmente carregada. Se L1 estiver explicitamente desabilitada, o catálogo é esquecido sem bloquear o roteamento.
- `Snapshotter` aceita fonte dinâmica de pools porque `Router.SetSnapshot` substitui as instâncias de pool durante `apply`. Isso evita continuar salvando/restaurando objetos antigos após reload.
- A execução de L0/L1/L2 grava `health_checks` e `usage_events` com `source='warmup'`; L2 registra no máximo um token solicitado por execução.
- Prontidão considera somente chaves com L1 habilitada e probe configurada; o timeout libera `/readyz` com motivo explícito, sem declarar que a validação foi bem-sucedida.


## F7 — leases, passthrough e orçamento por projeto

- Um lease direto mantém o `Router` em read-lock até `report`, watchdog ou shutdown. Assim, o `apply` não troca snapshot, pool ou credencial enquanto um consumidor ainda usa o material devolvido. O TTL é limitado a 120 s; no shutdown, `Gateway.CloseLeases` encerra todos como `leaked` antes de drenar o writer de uso.
- `allow_lease` controla somente a devolução explícita do segredo em `/v1/leases`. `generic_http` passthrough não exige essa permissão porque a chave nunca sai do processo; ela é inserida no request upstream. `generic_http` não pode ser usado como rota de chat OpenAI.
- Passthrough cobra uma unidade de orçamento por tentativa HTTP, mantém a reserva da chave durante o streaming da resposta e não segue redirects. Remove credenciais fornecidas pelo cliente, cabeçalhos internos `X-Keypool-*` e cabeçalhos hop-by-hop, incluindo os nomeados dinamicamente em `Connection`.
- `max_inflight` é uma vaga por projeto compartilhada pelo caminho de chat, lease e passthrough. `max_wait` limita a admissão de concorrência e o scheduler da chave; isso evita espera ilimitada por vaga quando o cliente não envia deadline. Uma resposta HTTP `client_error`/`not_found` no passthrough conta como requisição efetivamente enviada, preserva RPM/RPD e não abre o circuito.
- `daily_token_budget` é contado em dia UTC. O router reserva a estimativa antes do envio e reconcilia com uso real ou estimativa conservadora no término. O startup restaura totais do dia a partir de `usage_events`, excluindo `warmup`; o estado corrente de reserva permanece em memória.
- `usage_events.actual_tokens` foi introduzido na migração aditiva `0003_f7_usage_actual_tokens.sql`, pois uma lease pode reportar total de tokens sem conhecer a divisão prompt/completion. O rollup para `usage_daily.total_tokens` mantém o total real/estimado.
- Calibração de estimativa é local ao processo e por (provedor, modelo): `EWMA(α=0,2)` de `actual_prompt / ceil(messages_bytes/4)`, amostra/fator limitados a [0,5; 2,0]. Não persiste porque é uma estatística derivada e deve reaprender com a combinação corrente de credencial/modelo.
- Estratégia `latency` usa TTFB EWMA por modelo (α=0,2) nas duas opções do P2C, não a duração total por bucket. Chat e passthrough registram TTFB no momento em que os headers upstream chegam; duração total continua em `totalEWMA` para diagnóstico.
- O benchmark de ponta a ponta `BenchmarkGatewayPassthrough` mede gateway + autenticação + admissão + scheduler + injeção de credencial + cópia de resposta contra upstream local. Não foi possível estabelecer comparação numérica confiável com F1/F3 neste ambiente; deve ser rodado com o toolchain e ambiente de aceitação.


## F8 — observabilidade, painel e operação Windows

- Métricas são implementadas em registry próprio, sem cliente Prometheus externo; histograms usam buckets cumulativos fixos e as labels são serializadas de forma determinística e escapadas no formato 0.0.4. Isso reduz dependências e mantém o scrape sob demanda.
- `/metrics` e `/` reutilizam autenticação do gateway. O painel é recurso estático via `go:embed`, somente leitura, sem JavaScript nem recurso externo; CSP proíbe scripts e conexões de origem externa.
- Os gauges de saúde, janelas, cooldown e idade warm são coletados no scrape usando snapshots/dados já existentes. As métricas de requisição/tentativa/queue/TTFB são atualizadas no gateway, e os contadores de descarte/leak refletem os writers e o watchdog.
- O TTFB é propagado no `router.Response` e `PassthroughResponse` como `time.Duration`; o gateway não precisa inferir TTFB a partir da duração total da resposta.
- DPAPI foi implementado via chamadas Win32 build-tagged (`CryptProtectData`/`CryptUnprotectData`) sem nova dependência Go. É opt-in e user-scoped; o modo de arquivo e `KEYPOOL_MASTER_KEY` permanecem padrões alternativos. O blob DPAPI exige o mesmo perfil de usuário Windows.
- A tarefa agendada usa `AtLogOn`, usuário atual, `Limited`, `Hidden` e `IgnoreNew`; variáveis de ambiente não são copiadas para argumentos de processo.
- O guia de integração Mathlab começou como proposta na F8 e, na F9, foi aplicado nos dois repositórios: o KeyPool mantém perfil/contrato/smoke test; o Mathlab configura provider compatível OpenAI, seis aliases, headers de política, capacidade remota e streaming opt-in. Testes offline passam; E2E real permanece pendente.


## F8 — decisões de acabamento

- `/metrics` usa registry próprio, sem client Prometheus; saída é text exposition 0.0.4, `HELP`/`TYPE`, labels determinísticas escapadas e histogramas cumulativos com buckets fixos. Amostras negativas/NaN/Inf são descartadas para manter counters monotônicos e tempos válidos.
- O painel `GET /` é estático via `go:embed`, read-only e CSP sem scripts/recursos externos. Para permitir navegação real em browser, o gateway aceita HTTP Basic com usuário fixo `keypool` e API key como senha; o challenge `WWW-Authenticate` habilita o diálogo nativo. Bearer e `X-API-Key` seguem como modos para clientes.
- O contador `keypool_attempts_total` registra apenas tentativas conhecidas em resposta bem-sucedida ou passthrough com headers; falhas anteriores à obtenção de uma resposta não são inferidas como envio ao provedor.
- DPAPI é opt-in, user-scoped, implementado via Win32 com build tag `windows` e sem nova dependência. É recusado em `serve --dev`, que não usa master key persistente.
- A tarefa agendada usa AtLogOn, Hidden, Limited, IgnoreNew e reinícios limitados; o diretório de trabalho padrão é a raiz do repositório e pode ser substituído.


## F9 — integração Mathlab

- A integração de código foi aplicada em dois repositórios: o KeyPool mantém o gateway/perfil e o Mathlab configura `OpenAICompatibleProvider`, aliases por papel, headers de política, consulta remota de capacidade e streaming SSE opt-in.
- O Mathlab mantém exatamente uma credencial cliente KeyPool (`KEYPOOL_API_KEY`). Mais de uma é rejeitada para um provider com `capacity_path`; as chaves Groq/OpenRouter são importadas e rotacionadas somente no KeyPool.
- `key_pool.py`, `rate_limiter.py` e os provedores diretos antigos permanecem no repositório Mathlab para rollback e compatibilidade. Não removê-los antes de aceitação E2E real, incluindo 429, concorrência, reinicialização e cancelamento upstream.
- A configuração e a suíte offline foram validadas com mocks; nenhuma credencial real nem chamada de rede a upstream foi usada. A validação ponta a ponta e a tarefa Lean real continuam pendentes do ambiente implantado.
