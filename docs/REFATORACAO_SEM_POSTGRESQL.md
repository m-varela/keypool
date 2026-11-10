# Plano de refatoração — KeyPool sem PostgreSQL

> Data: 10/11/2026 · Base: `keypool.zip` (keypoold 0.4.0, módulo `keypool`, Go 1.26)
>
> **Método e limite deste plano.** Foi escrito a partir da leitura do código e da documentação do ZIP. O ambiente em que foi redigido não tinha Go instalado, então **nada foi compilado nem executado**. Toda afirmação sobre comportamento vem da leitura e precisa ser confirmada pelos testes das Fases 0 e 7.

---

## 1. Objetivo

Remover por completo o PostgreSQL do KeyPool e deixá-lo dependente apenas do toolchain Go, mantendo todas as funcionalidades F0–F9 e reduzindo o projeto a **um único modo de execução**.

| Requisito do pedido | Critério verificável |
|---|---|
| Nenhum PostgreSQL | `grep -ril "postgres\|pgx\|KEYPOOL_DATABASE_URL"` só encontra menções históricas em `docs/DECISIONS.md` e `STATUS.md` |
| Nenhum requisito externo além do Go | `go.mod` sem `require`; nenhum `go.sum`; `GOPROXY=off go build ./...` funciona com cache de módulos vazio |
| Conteúdo em memória local | store único em memória (ver D1 para o único arquivo de checkpoint) |
| Um único modo | existe só `keypoold serve`; não existem `--dev`, `init`, `migrate`, `--database` |
| Todas as funcionalidades | matriz de paridade da seção 7 |
| Sem complexidade acessória | seção 9 lista o que **não** será feito |

---

## 2. Diagnóstico do código atual

- **Dois modos hoje.** `keypoold serve --dev <yaml>` usa `memstore` (volátil). `keypoold serve` usa `pgstore` (PostgreSQL). O resto do sistema fala com ambos pela interface `store.Store` (`internal/store/store.go`).
- **PostgreSQL está isolado.** Só `internal/store/pgstore` importa `pgx` (cerca de 1,9 mil linhas de Go mais 3 migrações SQL). O README e o `go.mod` confirmam: `pgx/v5` e `yaml.v3` são as duas únicas dependências de terceiros.
- **O caminho quente nunca toca o banco** (decisão D5 do `docs/PLANO.md`). A configuração vive num snapshot imutável em memória; uso e eventos saem por filas assíncronas e limitadas. Logo, remover o PostgreSQL **não muda a latência das requisições**; o ganho é operacional.
- **O `memstore` já implementa** `store.Store`, `WarmStore`, `AdminReader` e `ProjectDailyUsageReader`. Hoje é volátil e **sem limite de crescimento**: os slices `usage`, `events` e `health` só recebem `append`. Num daemon que fica semanas ligado isso é um vazamento de memória.
- **O que o PostgreSQL faz de fato hoje**, e para onde cada responsabilidade vai:

| Responsabilidade atual (PostgreSQL) | Destino no alvo |
|---|---|
| Configuração normalizada (`providers`, `projects`, `routes`, `project_key_rules`, `app_settings.runtime_config`) | `config.Raw` em memória + checkpoint |
| Chaves de provedor cifradas (`keys.secret_enc`) | `secret.Secret` em memória + checkpoint cifrado (AES-256-GCM) |
| Hash da API key, `master_key_check` (`app_settings`) | memória + checkpoint |
| Janelas de limite (`limit_group_state`) | pools em memória + checkpoint (já gerido pelo `Snapshotter`) |
| `usage_events` e `usage_daily` | ring em memória (limite por contagem e idade) + contadores diários no checkpoint |
| `events` | ring em memória |
| `health_checks` (só escrita; nenhum código lê) | ring pequeno em memória |
| `model_catalog` | memória; já é invalidado e re-sondado por L1 a cada boot (DECISIONS F6) |
| Trava consultiva (`pg_try_advisory_lock`) | `net.Listen` antes de qualquer escrita (D7) |
| Migrações e checksums | campo `version` no checkpoint |
| Retenção e rollup (`PrunePool`) | limites por contagem/idade nos rings; sem job |
| Filas `usage.Writer` e `events.Writer` | removidas: `append` em memória é O(1) |
| `doctor`: desvio de relógio do banco | removido |

- **Achado paralelo (provável bug latente).** `config.Build` serializa os `defaults`/`force` de cada rota com `yaml.Marshal` (`marshalValue`, `internal/config/config.go`), e `router.mergePayload` cola esses bytes diretamente no corpo JSON como `json.RawMessage`. Números (`0.7`, `7000`) coincidem em YAML e JSON, mas uma string como `reasoning_effort: medium` vira `medium\n`, que **não é JSON válido**. O perfil `mathlab.project.yaml` usa exatamente isso em todas as rotas. Não executei o código, então trate como hipótese a confirmar com um teste (Fase 2). A migração para JSON elimina a causa.

---

## 3. Decisões

Cada decisão deve ser registrada em `docs/DECISIONS.md` (seção F10). As marcadas **[CONFIRMAR]** mudam algo visível para o dono do projeto; o plano segue o padrão indicado, e a alternativa está descrita.

**D1 — Memória é a fonte da verdade; um único arquivo de checkpoint guarda só o que afeta correção. [CONFIRMAR]**
O pedido diz "conteúdo salvo em memória local". Memória pura, porém, perderia a cada reinício:
(a) chaves e configuração criadas por `keypoolctl`/API admin; (b) a API key rotacionada; (c) as janelas RPM/RPD/TPM/TPD, o que leva a estourar a cota diária do provedor sem o KeyPool saber; (d) o orçamento diário de tokens por projeto.
Esses itens são funcionalidades existentes (F4/F5), então o alvo mantém **um** arquivo `state.json` (JSON, escrita atômica, só com a biblioteca padrão). Ele **não é um modo**: é sempre usado. O histórico de uso/eventos/saúde **não** vai para o arquivo.
*Alternativa:* volatilidade total. Basta `Open` não ler nem gravar o arquivo; custa os itens (a)–(d) acima.

**D2 — Um modo só: o fluxo do `serve` persistente, sem PostgreSQL.**
`--dev`, `init`, `migrate`, `--database` e `KEYPOOL_DATABASE_URL` deixam de existir. Na primeira execução, `serve` faz o bootstrap sozinho (cria master key, API key e importa o JSON de configuração).

**D3 — Zero módulos de terceiros: YAML vira JSON. [CONFIRMAR]**
"Nenhum requerimento externo, apenas o Go" é lido literalmente: `yaml.v3` também é módulo externo (e hoje exige rede na primeira compilação, pois não há `go.sum`). A biblioteca padrão não tem YAML, e escrever um parser YAML seria complexidade acessória. Configs, presets e o `apply` passam a ser JSON, sem comentários.
*Alternativa:* manter `yaml.v3` como única dependência. Nesse caso **pule a Fase 2** (o resto do plano não depende dela), mantenha a correção do bug dos `defaults` (trocar `yaml.Marshal` por `json.Marshal` é independente) e ajuste o critério "go.mod sem require".

**D4 — Segredos continuam cifrados em repouso.**
`internal/cryptobox` (AES-256-GCM, AAD `id||provider`), master key (arquivo, `KEYPOOL_MASTER_KEY` ou DPAPI opcional) e `master_key_check` permanecem como estão. Os arquivos ficam com permissão 0600 (Windows: ACL por script).

**D5 — API key do KeyPool só existe como SHA-256 em disco.**
Comportamento atual preservado: após a primeira execução, `KEYPOOL_API_KEY` é obrigatória e validada contra o hash (o `config.Build` e o `admin` precisam do texto em memória para derivar o hash do snapshot). Na primeira execução, ela é a variável de ambiente, ou é gerada e impressa uma única vez.

**D6 — Gravação atômica e protocolo de commit.**
Gravar em `state.json.tmp`, `fsync`, depois `os.Rename` sobre `state.json` (em Windows, `os.Rename` substitui o destino). Um mutex dedicado (`commitMu`) serializa **somente** mutações duráveis (admin, snapshot de pools) e a gravação do arquivo; leituras e o caminho quente nunca o adquirem. Isso é uma exceção consciente à regra 7 do `AGENTS.md` ("sem mutex durante I/O") e deve ser registrada como desvio em `DECISIONS.md`. O mutex de estado (`mu`) nunca é mantido durante I/O.

**D7 — Instância única por bind TCP, não por lock de arquivo.**
O `net.Listen` no endereço loopback é feito **antes de qualquer escrita em disco**. Duas instâncias com o mesmo diretório de dados têm o mesmo `listen` (vem do estado), então a segunda falha com "endereço em uso". Substitui o advisory lock do PostgreSQL sem lock file, sem lock stale e sem pacote externo (a biblioteca padrão não tem `flock` portátil).

**D8 — Histórico volátil e limitado. [CONFIRMAR]**
Uso, eventos e saúde ficam em rings em memória (seção 4.5). Não sobrevivem a reinício e têm retenção menor que a do PostgreSQL (30/7/90 dias com rollup diário). O orçamento diário de tokens, que depende desse histórico, passa a ser guardado em **contadores diários** próprios, persistidos no checkpoint.
*Alternativa:* persistir o histórico em JSONL com rotação; rejeitada por ser a parte mais cara em código e de menor valor para uso pessoal.

**D9 — Filas de escrita removidas.**
`internal/usage` (`Writer`) e `internal/events` (`Writer`) existem para não bloquear requisições em I/O de banco. Com `append` em memória isso não existe. Os sinks do gateway/router/warm chamam `AppendUsage`/`AppendEvent` direto (mutex próprio e curto, `usageMu`). A métrica `keypool_usage_dropped_total` é **mantida** (dashboards não quebram), com novo significado: eventos descartados por limite de retenção.

**D10 — Menor churn possível.**
O pacote `internal/store/memstore` evolui para ser a implementação única; `store.Store` permanece. Assim `admin_test.go`, `f7_test.go` e `warm_test.go` continuam usando `memstore.New(raw)`. `New` é só um construtor de teste (sem arquivo); o daemon usa apenas `Open`.

**D11 — `store.Store` perde `Ping`.** Não há chamadores fora das implementações; sai junto com o PostgreSQL.

**D12 — IDs de chave permanecem strings (`sched.KeyID`).** O mapeamento para UUID estável do `pgstore` (`stableKeyUUID`) não é portado.

---

## 4. Arquitetura alvo

### 4.1 Diretório de dados

`KEYPOOL_DATA_DIR` (padrão já existente em `dataDir()`: `%LOCALAPPDATA%\keypool`, ou `$XDG_CONFIG_HOME/keypool`, ou `~/.keypool`):

```
<datadir>/
  master.key      # já existe hoje (ou KEYPOOL_MASTER_KEY, ou DPAPI)
  state.json      # novo: checkpoint
  state.json.tmp  # transitório durante a gravação
```

### 4.2 Ciclo de vida do `keypoold serve`

Único comando de execução: `keypoold serve [--config <arquivo.json>] [--master-key-dpapi]`.

1. Resolver diretório e master key: `LoadOrCreateMasterKey` (cria na 1ª execução; carrega depois) ou o equivalente DPAPI; `cryptobox.New`.
2. `memstore.Open(ctx, Options{Dir, Box, Clock})`: lê `state.json` se existir (valida `version`, `master_key_check`); se não existir, devolve store **não inicializado**. `Open` nunca cria nem altera arquivos.
3. **Se não inicializado:** exigir `--config`; `config.LoadFile` (JSON + perfil de projeto + chaves do ambiente via `KeysFromEnv`); API key = `KEYPOOL_API_KEY` ou gerada; validar com `config.Build`. **Nada é gravado ainda.**
   **Se inicializado:** exigir `KEYPOOL_API_KEY` e `VerifyAPIKey`; `--config` é ignorado com aviso (igual ao modo PostgreSQL de hoje).
4. Montar o snapshot (config + chaves habilitadas) e `router.New`.
5. **`net.Listen` no endereço do snapshot** (precisa ser loopback). Falha → erro claro "porta ocupada ou outra instância em execução" (D7).
6. Somente agora, se não inicializado: `Initialize(ctx, apiKey, raw)` grava o primeiro `state.json`; imprime `KEYPOOL_API_KEY=kp_...` uma única vez quando a chave foi gerada.
7. `SetProjectDailyUsage` com os contadores do dia; `Snapshotter.Restore` e `Start`; warm; gateway; admin; sinks diretos para o store.
8. `srv.Serve(ln)`.
9. Encerramento (ordem explícita, não por `defer` espalhado): `srv.Shutdown` → `warm.Close` → `gateway.CloseLeases` → `snapshotter.Close` (flush final) → `store.Close` (grava se houver contadores sujos).

Para ser testável, extrair `newApp(ctx, opts) (*app, error)` (passos 1–6), `app.Addr()` e `app.Run(ctx) error` (passos 7–9).

> Na primeira execução rode `serve` num terminal para capturar a chave impressa. Se a primeira execução for pela Tarefa Agendada oculta, a chave gerada se perde; nesse caso defina `KEYPOOL_API_KEY` antes.

### 4.3 Formato do `state.json`

```json
{
  "version": 1,
  "saved_at": "2026-11-10T21:00:00Z",
  "api_key": { "hash": "<sha256 hex>", "prefix": "kp_ab12", "created_at": "...", "rotated_at": "..." },
  "master_key_check": "<base64>",
  "config": { "listen": "127.0.0.1:8787", "default_project": "mathlab", "providers": {}, "projects": {}, "warm": {} },
  "keys": [
    { "provider": "groq", "id": "groq-1", "label": "gsk_...abcd", "group": "", "tags": ["mathlab"], "enabled": true, "secret_enc": "<base64 AES-GCM>" }
  ],
  "pools": { "groq": { "saved_at_ms": 0, "merged": false, "keys": [], "groups": {} } },
  "daily": { "day": "2026-11-10", "project_tokens": { "mathlab": 12345 }, "warm_l2_tokens": { "groq/groq-1": 0 } }
}
```

- `config` é o `config.Raw` serializado em JSON (já é o que o `memstore` faz em `cloneRaw`). `Raw.Keys`, `Raw.APIKey` e `Raw.ProjectFile` têm `json:"-"` e **não** entram aqui.
- `keys[].secret_enc` usa `cryptobox.Encrypt(id, provider, plaintext)`. O texto da chave nunca aparece no arquivo (testar).
- `pools` guarda `sched.PoolSnapshot` por provedor (o `Snapshotter` já faz hash e só salva quando muda).
- Arquivo ilegível, JSON inválido ou `version` desconhecida → **erro explícito**; nunca recriar o estado em silêncio.

### 4.4 API do `memstore` após a mudança

```go
// Construtor só para testes (sem arquivo).
func New(raw *config.Raw) *Store

// Único construtor do daemon.
func Open(ctx context.Context, o Options) (*Store, error) // Options{Dir string; Box *cryptobox.Box; Clock clock.Clock}

func (s *Store) Initialized() bool
func (s *Store) Initialize(ctx context.Context, apiKey string, raw *config.Raw) error
func (s *Store) VerifyAPIKey(ctx context.Context, token string) (bool, error) // comparação em tempo constante
func (s *Store) APIKeyInfo(ctx context.Context) (prefix string, created, rotated time.Time, err error)
func (s *Store) UsageEvicted() uint64
func (s *Store) Close() error // grava se houver contadores sujos; idempotente
```

Mais todos os métodos já existentes de `store.Store`, `WarmStore`, `AdminReader` e `ProjectDailyUsageReader`, sem mudar assinaturas (exceto a remoção de `Ping`).

Arquivos sugeridos em `internal/store/memstore/`: `memstore.go` (existente, evoluído), `ring.go`, `retention.go` (constantes), `daily.go`, `checkpoint.go` (DTO, `Open`, gravação atômica, `Close`), `apikey.go`.

### 4.5 Rings e limites de retenção

`ring[T]` genérico, capacidade fixa, descarta o mais antigo. Poda por idade a partir da cabeça a cada `append` (as séries são quase ordenadas no tempo). Cada descarte incrementa `UsageEvicted()` (só para `usage`).

| Série | Limite por contagem | Limite por idade | Observação |
|---|---|---|---|
| `usage` | 50 000 | 7 dias | pior caso estimado em 15–20 MB (≈300–400 B/evento); **medir na Fase 1** e ajustar |
| `events` | 10 000 | 30 dias | volume baixo |
| `health` | 1 000 | 7 dias | hoje é só escrita; nenhum código lê |

Constantes num único arquivo (`retention.go`), para ajuste fácil. `ReadUsage`, `ReadEvents` e `CountWarmL2Tokens` (fora do dia corrente) leem do ring.

### 4.6 Contadores diários (substituem `usage_events` no boot)

`daily{Day, ProjectTokens map[string]int, WarmL2Tokens map[string]int}`, dia UTC.

- `AppendUsage` incrementa os contadores com **a mesma regra de normalização** que o `ReadProjectDailyTokens` do `memstore` já tem: `source` vazio/desconhecido vira `request`; `warmup` não conta no projeto; total = `ActualTokens` se > 0, senão `Usage.TotalTokens`, senão prompt+completion, senão `EstimatedTokens`.
- Eventos com `Source=="warmup"` e `Alias=="warmup-L2"` incrementam `WarmL2Tokens["provider/keyID"]` por `EstimatedTokens` (regra atual de `CountWarmL2Tokens`).
- Mudança de dia UTC zera os contadores.
- `ReadProjectDailyTokens(from)` responde pelos contadores se `Day` for o dia de `from`; senão, mapa vazio.
- `CountWarmL2Tokens(from,to)` responde pelo contador se o intervalo for o dia dos contadores; senão varre o ring.
- Persistência: marcados como sujos em `AppendUsage` (sem I/O) e gravados junto ao próximo `SavePoolState` (≤ 10 s, pois toda requisição altera as janelas) e no `Close`.

### 4.7 Protocolo de concorrência

| Mutex | Protege | Quem usa | I/O sob o mutex? |
|---|---|---|---|
| `mu` (RWMutex) | config, chaves, pools, API key | leitores (admin, boot) | **nunca** |
| `usageMu` (Mutex) | rings e contadores diários | caminho quente (`AppendUsage`/`AppendEvent`), leitores | **nunca** |
| `commitMu` (Mutex) | serializa mutações duráveis e a gravação do arquivo | `SaveConfig`, `SaveKeys`, `DeleteKey`, `RotateAPIKey`, `SavePoolState`, `Initialize`, `Close` | **sim** (exceção D6); caminho quente e leituras **nunca** o adquirem |

Mutação durável (`commit`):
1. `commitMu.Lock()`.
2. `mu.RLock()` → clonar o estado atual → `mu.RUnlock()`.
3. Aplicar a mutação no clone; serializar o clone (para os contadores, copiar sob `usageMu`).
4. Gravar atomicamente (tmp → fsync → rename), com retry curto para falhas transitórias do Windows (antivírus segurando o arquivo). A espera usa `clock.Clock.Wait`, nunca `time.Sleep` direto (regra 6).
5. **Somente se gravou**: `mu.Lock()` → trocar o estado → `mu.Unlock()`.
6. `commitMu.Unlock()`.

Efeito: falha de gravação devolve erro e **deixa a memória intacta** (paridade com a transação do `pgstore.SaveConfig`), e não há atualização perdida porque toda mutação passa por `commitMu`. `New` (testes) pula o passo 4.

### 4.8 `memstore` em testes vs. produção

`New(raw)` não tem `path` nem `Box`; o passo 4 vira no-op. Não é um modo de execução: é a mesma lógica sem disco, usada só por testes.

---

## 5. Fases de execução

Ordem obrigatória. Cada fase termina com `gofmt -l .` vazio, `go vet ./...`, `go build ./...`, `go test ./...` verdes e **um commit** (`refactor(F10): <fase>`). Não avance com a fase anterior vermelha.

### Fase 0 — Baseline e rede de segurança

1. Confirmar `go version` ≥ 1.26. Criar a branch `refactor/sem-postgresql`.
2. Com rede (única vez): `go mod tidy`, depois `gofmt -l .`, `go vet ./...`, `go build ./...`, `go test ./...` e `go test -race ./internal/sched/... ./internal/router/... ./internal/gateway/... ./internal/store/... ./internal/usage/...`. Registrar o que já falha **antes** de qualquer mudança. Não commitar o `go.sum` gerado.
3. **Ler `internal/store/pgstore/integration_test.go` e `store_test.go`**: são a especificação de comportamento do store (API key, rotação, exclusão, snapshots, semântica de `SaveConfig`/`SaveKeys`). Listar cada asserção que `memstore_test.go` ainda não cobre; elas serão portadas na Fase 1.
4. Copiar este plano para `docs/REFATORACAO_SEM_POSTGRESQL.md`.

**Aceite:** baseline registrado (o que passa, o que falha); lista de asserções a portar.

### Fase 1 — Store único: memória + checkpoint (`internal/store/memstore`)

Implementar as seções 4.3 a 4.7.

1. `ring.go` (genérico), `retention.go` (constantes da tabela 4.5), `daily.go` (4.6).
2. `checkpoint.go`: DTO do `state.json`, `Open` (sem efeitos colaterais), `Initialize`, gravação atômica (tmp → fsync → rename, retry curto no Windows com `clock.Clock.Wait`), `Close` idempotente.
3. `apikey.go`: SHA-256 hex, `VerifyAPIKey` com `subtle.ConstantTimeCompare`, `APIKeyInfo`, `RotateAPIKey` persistente.
4. Adaptar `SaveConfig`, `SaveKeys`, `DeleteKey`, `RotateAPIKey`, `SavePoolState` ao protocolo `commit` (4.7). **Preservar a semântica já testada**: `SaveConfig` não apaga chaves desabilitadas de provedores que continuam existindo; `LoadKeys` devolve cópias.
5. `AppendUsage`, `AppendEvent`, `SaveHealthCheck`: rings + contadores; `ReadUsage`, `ReadEvents`, `ReadProjectDailyTokens`, `CountWarmL2Tokens` conforme 4.5/4.6.
6. `internal/store/store.go`: remover `Ping` e os comentários que citam `pgstore`.
7. `Store.Clock`: usar `clock.Clock` para idade dos rings e dia UTC (regra 6 do `AGENTS.md`); `clock.Fake` nos testes.

**Testes (offline, `t.TempDir()`, `clock.Fake`):**
- ida e volta: `Initialize` → fechar → `Open` → config, chaves, hash e `master_key_check` idênticos;
- `state.json` **não contém** o texto de nenhuma chave de provedor nem a API key (buscar os bytes do arquivo);
- master key errada → `Open` falha com erro claro; JSON corrompido e `version` desconhecida → erro, sem sobrescrever o arquivo;
- diretório somente leitura → mutação devolve erro **e a memória continua igual**;
- `RotateAPIKey` persiste; `VerifyAPIKey` aceita a nova e rejeita a antiga após reabrir;
- pools: `SavePoolState` → reabrir → `LoadPoolState` igual; `SavedAtMS==0` quando ausente (contrato do `Snapshotter.Restore`);
- contadores diários: sobrevivem ao reinício no mesmo dia; zeram na virada UTC; `warmup` não entra no projeto; `warmup-L2` entra em `WarmL2Tokens`;
- rings: limite por contagem, limite por idade, `UsageEvicted` correto;
- permissões `0600` em Unix (`runtime.GOOS != "windows"`);
- `go test -race`: goroutines concorrentes de `AppendUsage`, `SaveConfig`, `SavePoolState` e leitores;
- asserções portadas do `pgstore` (Fase 0, item 3);
- medição: inserir 50 000 eventos e registrar `runtime.MemStats` no relatório; se passar de ~25 MB, reduzir a constante.

**Aceite:** `go test -race ./internal/store/...` verde; relatório de memória anexado.

### Fase 2 — Configuração em JSON, zero módulos de terceiros (somente se D3 confirmada)

1. `internal/config/config.go`: remover `yaml.v3`; `LoadDev` → `LoadFile` (JSON; `project_file` relativo ao diretório do arquivo); `decodeYAML` → `decodeJSON` (`DisallowUnknownFields` e rejeição de conteúdo extra); `RawModelLimits.UnmarshalJSON` (inteiro = TPM legado, objeto = `limits.ModelLimits`); remover `UnmarshalYAML` de `RawModelLimits` e `Policy` (`Policy` já tem `UnmarshalJSON`/`MarshalJSON`); `marshalValue` → `json.Marshal`.
   `Raw.ProjectFile` hoje é `json:"-"`. O arquivo de configuração precisa lê-lo, mas ele **não** deve ir para o `state.json`: decodificar numa struct local que embute `Raw` e declara `project_file`, ou outra solução igualmente simples.
2. Remover as tags `yaml:"..."` de `config.go`, `internal/limits/limits.go` e `internal/provider/provider.go`.
3. `internal/provider/presets.go`: `//go:embed presets/*.json`, decoder JSON estrito; converter `groq.yaml` e `openrouter.yaml`. **Verificar onde o nome do preset vira nome de arquivo** (`Preset: "groq"` → `groq.<ext>`) e ajustar.
4. `internal/admin/admin.go`: `decodeStrict` e `isProjectDocument` só com JSON (remover os ramos YAML).
5. `cmd/keypoolctl/main.go`: `Content-Type: application/json`; se o arquivo de `apply -f` não começar com `{`, erro em pt-BR dizendo que o formato agora é JSON.
6. Converter exemplos: `configs/examples/keypool.dev.yaml` → `keypool.json` e `mathlab.project.yaml` → `mathlab.project.json` (atualizar `project_file`); `integrations/mathlab/keypool-project.yaml` → `.json`. **Não tocar** em `providers.fragment.yaml` e `models.fragment.yaml`: pertencem ao repositório Mathlab (Python).
7. Testes: reescrever `TestRawModelLimitsAcceptsLegacyScalarTPM` (hoje usa `yaml.Node`); **teste de regressão dos defaults** — rota com `"defaults": {"reasoning_effort":"medium","temperature":0.7}` → `config.Build` → `json.Marshal(mergePayload(route.Defaults, nil, nil))` deve ser JSON válido contendo `"reasoning_effort":"medium"` (escrever o teste antes da troca para confirmar ou descartar a hipótese do bug); presets carregam e validam; `LoadFile` com `project_file`; campo desconhecido rejeitado; `apply` aceita configuração completa e perfil de projeto em JSON.

**Aceite:** `grep -rn "yaml" --include=*.go .` vazio; testes verdes; teste de regressão dos defaults presente.

> Se D3 for recusada: pule os itens 1–6, mantenha `yaml.v3`, e faça apenas a troca de `marshalValue` por `json.Marshal` + o teste de regressão (itens 1 parcial e 7).

### Fase 3 — Daemon de modo único (`cmd/keypoold`)

1. Extrair `newApp`/`app.Addr`/`app.Run` conforme 4.2. Remover `runMigrate`, `runInit`, `serveDev`, `retentionLoop`, o fluxo persistente com `pgstore`, as flags `--dev`/`--database` e `loadExistingMasterKey` em favor de `LoadOrCreateMasterKey`.
2. Comandos restantes: `serve`, `doctor`, `version`. Mensagem de uso atualizada.
3. Sinks: `gw.SetUsageSink`, `wm.SetUsageSink`, `gw.SetEventSink`, `wm.SetEventSink` e `rt.SetOnEvent` chamam `store.AppendUsage`/`AppendEvent` direto, **sem** `usage.Writer`/`events.Writer`. Manter o prefixo de `Subject` por provedor que o fluxo persistente aplica hoje.
4. `cmd/keypoold/metrics.go`: `collectRuntimeMetrics` troca o parâmetro `*usage.Writer` por `func() uint64` (`store.UsageEvicted`). Em `internal/metrics/metrics.go` o texto de ajuda de `keypool_usage_dropped_total` passa a "Eventos de uso descartados por limite de retenção em memória."
5. `doctor`: diretório de dados gravável; `state.json` legível, `version` e `master_key_check` válidos; master key (env, arquivo com permissão 0600 em Unix, DPAPI); API key confere com o hash; `listen` é loopback; porta livre. Sem checagem de relógio do banco.
6. Manter `isLoopbackAddress`, `newAPIKey`, `dataDir`, `masterKeyPath`, `--master-key-dpapi`.

**Testes:**
- `isLoopbackAddress`;
- **E2E offline** (`cmd/keypoold/e2e_test.go`), com `internal/testutil/fakeupstream` e `listen: 127.0.0.1:0`:
  1. 1ª execução: JSON de config em `t.TempDir()` + chave de provedor por ambiente → `newApp` → `Run`; `POST /v1/chat/completions` com a API key → 200;
  2. encerrar; reabrir com o mesmo diretório e `KEYPOOL_API_KEY` → a chave de provedor, a rota e a janela de uso (RPD) **foram restauradas**; sem `--config`;
  3. reabrir com `KEYPOOL_API_KEY` errada → falha;
  4. segunda instância no mesmo diretório enquanto a primeira roda → falha de bind (D7);
  5. `POST /admin/v1/keys` → encerrar → reabrir → a chave existe.

**Aceite:** E2E verde (inclui `-race`); `grep -n "pgstore\|memstore.New(" cmd/keypoold` sem resultados (o daemon só usa `Open`).

### Fase 4 — CLI (`cmd/keypoolctl`)

1. `revealLocal`: abrir o `state.json` somente leitura via `memstore.Open` + master key (env, arquivo ou DPAPI, mesma resolução de hoje) e procurar o `label`; manter a confirmação interativa e a regra "operação local, não usa a API". Mensagem de erro quando o estado não existe.
2. Remover a importação de `pgstore` e qualquer menção a `KEYPOOL_DATABASE_URL`.
3. Atualizar `cmd/keypoolctl/README.md`.

**Testes:** `output_test.go` e `main_test.go` seguem verdes; novo teste de `reveal` contra um `state.json` temporário (rótulo existente, inexistente, master key errada).

**Aceite:** `go build ./cmd/...` sem `pgx` no grafo (`go list -deps ./cmd/... | grep -i pgx` vazio).

### Fase 5 — Remoções e `go.mod`

1. Remover: `internal/store/pgstore/`, `internal/store/migrations/`, `internal/usage/`, `internal/events/`, `scripts/windows/setup-db.ps1`, `scripts/windows/run-dev.ps1`, `docs/POSTGRESQL_F4.md`, e (se D3) todos os `*.yaml` substituídos.
2. `go.mod`: deixar apenas `module keypool` e `go 1.26`; apagar o `go.sum` se existir; **não inventar `go.sum`** (não há dependências).
3. `GOPROXY=off go build ./... && GOPROXY=off go test ./...` com cache de módulos vazio (`GOFLAGS=-modcacherw` e `GOMODCACHE` apontando para um diretório novo).

**Aceite:** build e testes offline passam; `go list -m all` mostra só `keypool`.

### Fase 6 — Scripts e documentação

**Scripts (`scripts/windows`, ASCII puro, regra 4):**
- `run-dev.ps1` → `run.ps1`: `go run ./cmd/keypoold serve --config configs/examples/keypool.json`;
- `test.ps1`: remover `-Integration` e as referências a `KEYPOOL_TEST_DATABASE_URL`; ajustar a lista do `-Race` (sem `./internal/usage/...`);
- `install-task.ps1`: trocar o aviso final por "Confirme que KEYPOOL_API_KEY (e, se usar, KEYPOOL_MASTER_KEY) estão no ambiente do usuário"; sem `KEYPOOL_DATABASE_URL`;
- `acl-master-key.ps1`: cobrir também `state.json` (ou o diretório de dados);
- `build.ps1`: conferir se algo cita PostgreSQL.

**Documentação (pt-BR):**
- `README.md`: requisitos (só Go); fluxo único (1ª execução, execuções seguintes, `KEYPOOL_API_KEY`); remover seções de modo dev e PostgreSQL; config em JSON.
- `AGENTS.md`: item 3 vira "somente biblioteca padrão" (ou "padrão + `yaml.v3`" se D3 recusada); remover a menção a `pgx`.
- `docs/ESTADO_LOCAL.md` (novo, substitui `POSTGRESQL_F4.md`): diretório de dados, `state.json`, primeira execução, backup (**guardar `master.key` junto**: sem ela o estado cifrado é irrecuperável), restauração, como "zerar" (apagar `state.json`), limites de retenção.
- `docs/DECISIONS.md`: seção **F10** com D1–D12, a exceção D6 à regra 7, a mudança de significado da métrica de descarte, e o resultado do teste de regressão dos defaults.
- `docs/PLANO.md` (84 KB): **não reescrever**. Adicionar um adendo no topo ("F10: a persistência PostgreSQL deste plano foi substituída; ver `docs/DECISIONS.md#F10` e `docs/ESTADO_LOCAL.md`") e marcar a seção 7 (modelo de dados) como superada.
- `STATUS.md`: nova linha **F10 — remoção do PostgreSQL**; reescrever a linha F4 como histórica; atualizar as notas que citam PostgreSQL.
- `docs/VERIFY.md`, `docs/OPERACAO_WINDOWS.md`, `docs/SECURITY.md`, `docs/API.md`, `docs/FINAL_AUDIT.md`, `docs/INTEGRACAO_MATHLAB.md`, `integrations/mathlab/README.md`: remover PostgreSQL/`--dev`; `SECURITY.md` descreve o `state.json`; `API.md` documenta os limites de `usage`/`events` e a métrica.
- `integrations/mathlab/smoke.py` e `test_smoke.py`: conferir se leem `keypool-project.yaml`; atualizar para `.json`.

**Aceite:** `grep -ril "postgres\|pgx\|KEYPOOL_DATABASE_URL\|serve --dev" .` só retorna `docs/DECISIONS.md`, `STATUS.md` e o adendo do `docs/PLANO.md`.

### Fase 7 — Verificação final

1. `gofmt -l .` (vazio), `go vet ./...`, `go build ./...`, `go test ./...`.
2. `go test -race ./internal/sched/... ./internal/router/... ./internal/gateway/... ./internal/store/... ./cmd/...`.
3. Benchmarks existentes do scheduler/gateway (`go test -bench . ./internal/sched ./internal/gateway`): **sem regressão** frente à Fase 0 (o caminho quente não foi tocado).
4. Greps de resíduos (critérios da seção 1).
5. Checklist manual da matriz de paridade (seção 7), com o fake upstream ou chaves reais.
6. Relatório final (formato no prompt).

---

## 6. O que muda para quem usa

1. `init`, `migrate`, `serve --dev`, `--database` e `KEYPOOL_DATABASE_URL` **deixam de existir**. Um único `keypoold serve [--config arquivo.json]`.
2. Configuração em **JSON** (se D3): sem comentários; mesmos campos de antes.
3. As chaves de provedor lidas de `<PREFIXO>_API_KEY(S)` entram **só na primeira execução** (ou por `keypoolctl key import-env`). Mudar a variável depois não altera o estado. É a semântica do modo PostgreSQL de hoje; difere do `--dev`, que relia o ambiente a cada início.
4. Depois da primeira execução, `KEYPOOL_API_KEY` é obrigatória (como hoje no modo persistente).
5. Histórico de `usage`/`events` não sobrevive a reinício e tem retenção menor (4.5); some o rollup `usage_daily`.
6. Queda abrupta perde até ~10 s de janelas e contadores (igual ao modo PostgreSQL).
7. **Sem migração de dados.** O STATUS informa que a aceitação E2E com PostgreSQL real nunca foi feita. Se houver chaves num banco existente, extraia-as **antes** de atualizar, com a versão atual (`keypoolctl key reveal <label>`), e recadastre com `key add --stdin`.

---

## 7. Matriz de paridade funcional

| Funcionalidade | Como é mantida | Prova |
|---|---|---|
| Roteamento, spill, retries, fallback, SSE, leases, passthrough, headroom/latency, calibração EWMA | **código não tocado** (`router`, `gateway`, `sched`, `sse`, `classify`, `provider`) | suíte existente + benchmarks sem regressão |
| Configuração persistente e `apply` | `config` no `state.json` via `commit` | teste de ida e volta; E2E (5) |
| Chaves cifradas em repouso | AES-256-GCM no `state.json` | teste "sem texto claro no arquivo" |
| CRUD de chaves (`add`, `enable`, `disable`, `tag`, `rm`, `import-env`, `test`) | `SaveKeys`/`DeleteKey` via `commit` | `admin_test.go` adaptado; E2E (5) |
| `key reveal` local | lê `state.json` + master key, com confirmação | teste da Fase 4 |
| API key só como hash; rotação | `apikey.go`, hash no estado | testes da Fase 1; E2E (3) |
| Restauração das janelas RPM/RPD/TPM/TPD | `pools` no estado + `Snapshotter` | teste da Fase 1; E2E (2) |
| `daily_token_budget` por projeto | contadores diários | teste da Fase 1 |
| Warm L0/L1/L2 e orçamento diário de L2 | `WarmStore` em memória + contador | `warm_test.go` + teste de contadores |
| `usage` e `events` (CLI/API, `group_by`) | rings | `admin_test.go` adaptado |
| Métricas Prometheus e painel | inalterados; `keypool_usage_dropped_total` = descartes por retenção | `metrics_test.go` |
| Instância única | bind TCP antes de gravar (D7) | E2E (4) |
| Retenção | limites por contagem/idade | testes de ring |
| DPAPI, master key por env/arquivo | `cryptobox` inalterado | `cryptobox_test.go` |
| Loopback obrigatório | inalterado | teste de `isLoopbackAddress` + E2E |
| `doctor` | adaptado (sem relógio do banco) | execução manual |
| Mathlab (F9) | contrato HTTP inalterado | `integrations/mathlab/test_smoke.py` |

---

## 8. Plano de testes (resumo)

- **Novos:** `memstore` (ring, checkpoint, contadores, API key, concorrência, memória); regressão dos defaults; presets e `LoadFile` JSON; E2E do daemon; `reveal`.
- **Adaptados:** `admin_test.go` (corpos de `apply` em JSON), `config_test.go` (`RawModelLimits` por JSON), `memstore_test.go`, `f7_test.go` e `warm_test.go` (só se tocarem `Ping`).
- **Removidos:** `pgstore/*_test.go`, `migrations/migrate_test.go`, `usage/writer_test.go`, `events/writer_test.go`, `-tags integration`.
- Todos offline, com `clock.Fake`, `httptest` e `fakeupstream` (regra 8). Nada acessa provedor real.

---

## 9. Fora de escopo (para não criar complexidade acessória)

- Nenhum banco embutido ou biblioteca de terceiros (SQLite, bbolt, etc.).
- Nenhuma persistência do histórico de uso/eventos (JSONL, rotação, rollup).
- Nenhum modo `--dev`, flag de "modo volátil" ou shim de compatibilidade com `--database`.
- Nenhum parser YAML próprio.
- Nenhum lock file, recarga automática do `state.json` ou multi-instância.
- Nenhuma mudança de lógica em `router`, `gateway`, `sched`, `warm`, nem nas rotas HTTP públicas (apenas mensagens que citem PostgreSQL).
- Nenhuma ferramenta de migração de dados PostgreSQL → JSON.
- Nenhuma remoção de `SaveHealthCheck` (sink só de escrita) nesta refatoração; fica como limpeza futura.

---

## 10. Riscos e mitigação

| Risco | Mitigação |
|---|---|
| Arquivo corrompido por queda no meio da gravação | tmp + fsync + rename atômico; erro explícito ao abrir; nunca recriar em silêncio |
| `os.Rename` falha no Windows (antivírus segurando o arquivo) | retry curto com `clock.Clock.Wait`; erro claro após esgotar |
| Perda da `master.key` | documentar backup junto do estado; `doctor` avisa se ausente |
| Primeira execução pela Tarefa Agendada oculta perde a API key gerada | documentar: 1ª execução em terminal ou definir `KEYPOOL_API_KEY` antes |
| Crescimento de memória | rings limitados; medição na Fase 1; métrica de descarte |
| Hot path afetado por mutex novo | `usageMu` curto e separado; `commitMu` nunca no caminho quente; benchmarks da Fase 7 |
| Usuário do `--dev` estranha o import único do ambiente | seção 6 e `docs/ESTADO_LOCAL.md`; `key import-env` |
| Troca YAML→JSON desagrada | D3 é isolada na Fase 2; alternativa descrita |
| Bug dos `defaults` muda comportamento | é correção (JSON válido); teste de regressão registra |
| Ambiente do plano sem Go | Fases 0 e 7 obrigam executar tudo; relatório não pode declarar sucesso sem rodar |

---

## 11. Critérios de aceite globais

- [ ] `go.mod` só com `module` e `go 1.26`; sem `go.sum`; `GOPROXY=off go build ./... && go test ./...` passam com cache vazio.
- [ ] `go vet ./...`, `gofmt -l .` limpos; `go test -race` verde nos pacotes listados.
- [ ] Nenhuma referência a PostgreSQL/`pgx`/`KEYPOOL_DATABASE_URL`/`serve --dev` fora de `DECISIONS.md`, `STATUS.md` e do adendo do `PLANO.md`.
- [ ] Um único comando de execução (`serve`); `init`/`migrate`/`--dev`/`--database` inexistentes.
- [ ] E2E do daemon cobre: 1ª execução, reinício com restauração, API key errada, instância duplicada, chave via admin.
- [ ] `state.json` sem texto claro de segredos; permissões corretas.
- [ ] Memória de 50 000 eventos medida e dentro do orçamento.
- [ ] Benchmarks do scheduler/gateway sem regressão.
- [ ] `DECISIONS.md` (F10), `STATUS.md`, `README.md`, `AGENTS.md`, `docs/ESTADO_LOCAL.md` atualizados; scripts em ASCII puro.

---

## Apêndice A — Variáveis de ambiente

| Variável | Antes | Depois |
|---|---|---|
| `KEYPOOL_DATABASE_URL` | obrigatória no modo persistente | **removida** |
| `KEYPOOL_TEST_DATABASE_URL` | testes de integração | **removida** |
| `KEYPOOL_API_KEY` | obrigatória (persistente) / opcional (dev) | opcional na 1ª execução; **obrigatória depois** |
| `KEYPOOL_MASTER_KEY`, `KEYPOOL_MASTER_KEY_FILE` | mantidas | mantidas |
| `KEYPOOL_DATA_DIR` | diretório da master key | diretório da master key **e** do `state.json` |
| `<PREFIXO>_API_KEY(S)`, `<PREFIXO>_API_KEY_N` | lidas no `--dev` e na 1ª importação | lidas só na 1ª execução e por `key import-env` |
| `KEYPOOL_URL` | CLI | CLI |

## Apêndice B — Arquivos

**Criar:** `internal/store/memstore/{ring,retention,daily,checkpoint,apikey}.go` e testes; `cmd/keypoold/app.go` e `e2e_test.go`; `configs/examples/keypool.json`, `mathlab.project.json`; `internal/provider/presets/{groq,openrouter}.json`; `docs/ESTADO_LOCAL.md`, `docs/REFATORACAO_SEM_POSTGRESQL.md`; `scripts/windows/run.ps1`.

**Alterar:** `go.mod`, `AGENTS.md`, `README.md`, `STATUS.md`, `docs/{DECISIONS,PLANO,VERIFY,OPERACAO_WINDOWS,SECURITY,API,FINAL_AUDIT,INTEGRACAO_MATHLAB}.md`, `cmd/keypoold/{main,metrics}.go`, `cmd/keypoolctl/{main.go,README.md}`, `internal/config/{config,config_test}.go`, `internal/admin/{admin,admin_test}.go`, `internal/provider/{presets,provider}.go`, `internal/limits/limits.go`, `internal/metrics/metrics.go`, `internal/store/store.go`, `internal/store/memstore/{memstore,memstore_test}.go`, `scripts/windows/{test,install-task,acl-master-key}.ps1`, `integrations/mathlab/{README.md,smoke.py,test_smoke.py}`.

**Remover:** `internal/store/pgstore/`, `internal/store/migrations/`, `internal/usage/`, `internal/events/`, `scripts/windows/{setup-db,run-dev}.ps1`, `docs/POSTGRESQL_F4.md`, `configs/examples/*.yaml`, `internal/provider/presets/*.yaml`, `integrations/mathlab/keypool-project.yaml`.
