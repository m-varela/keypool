# KeyPool — Plano de implementação

> Serviço local, em Go, que guarda as chaves de API de provedores de IA (Groq, OpenRouter, e qualquer outro que se adicione depois),
> aplica limites e cotas de forma eficiente, mantém as chaves "aquecidas" e atende os seus projetos (mathlab e futuros) por uma API única.
>
> Data do plano: 09/11/2026. Uso pessoal, uma máquina (Windows), PostgreSQL local, sem Docker.

---

## 1. Resumo executivo

O `mathlab` já tem um sistema de pool de chaves maduro em Python (`src/mathlab/llm/key_pool.py`, `rate_limiter.py`, `client.py`, `capacity.py`).
Este plano transforma essa ideia em um **serviço independente** chamado **KeyPool**:

- **As chaves moram só no KeyPool** (cifradas no PostgreSQL). Os projetos nunca veem as chaves dos provedores: eles guardam apenas **uma** chave do KeyPool (`kp_...`).
- **Os projetos escolhem o que usam**: cada projeto (`mathlab`, etc.) tem um *perfil* no KeyPool dizendo quais chaves pode usar, quais rotas/papéis existem (`prover`, `critic`...), em que ordem de modelos, com que prioridade e reserva de cota.
- **A forma principal de uso é um gateway compatível com a API da OpenAI** (`POST /v1/chat/completions`). O projeto chama o KeyPool como se fosse um provedor; o KeyPool escolhe a chave, respeita os limites, faz retry/fallback/spill e devolve a resposta (inclusive com streaming).
- **As chaves ficam sempre aquecidas**: conexões HTTPS mantidas vivas, chaves validadas periodicamente sem gastar cota, catálogo de modelos em cache, circuit breaker por chave, janelas de limite sincronizadas com os cabeçalhos reais do provedor e latência medida por chave/modelo.
- **Tudo em Go**, um único processo (`keypoold`) + uma CLI (`keypoolctl`), PostgreSQL nativo local, **zero dependências externas além de `pgx` e `yaml.v3`**.

### Decisões já tomadas

| # | Decisão | Motivo |
|---|---|---|
| D1 | **Gateway primeiro** (proxy OpenAI-compatível); *lease* (devolver a chave ao cliente) é opcional e desligado por padrão | As chaves nunca saem do serviço; os projetos ficam finos; qualquer linguagem funciona |
| D2 | **Uma única API key do KeyPool** (`kp_...`), guardada só como hash SHA-256. Sem contas, usuários, papéis ou login | Pedido explícito. Uso pessoal |
| D3 | O projeto se identifica pelo cabeçalho `X-Keypool-Project: mathlab` (rótulo, não segredo) | Permite política por projeto com uma só chave de autenticação |
| D4 | **Contabilidade global por chave/conta, política por projeto** | Dois projetos que usam a mesma chave dividem a mesma cota real; a reserva/prioridade é que muda por projeto |
| D5 | **Caminho quente nunca toca o PostgreSQL**. Config vive em um snapshot imutável em memória; o banco recebe escrita em lote e assíncrona | Latência mínima e previsível |
| D6 | Estado das janelas (TPM/RPM/RPD/TPD), cooldowns e bloqueios é **persistido** periodicamente e restaurado na partida | Corrige uma limitação do Python: a cota diária zerava a cada reinício |
| D7 | Provedores são **dados** (spec declarativa + presets embutidos). Provedor compatível com OpenAI = só config. Provedor com formato próprio = um adaptador Go pequeno | "Qualquer provedor adicionável depois" |
| D8 | Go: diretiva `go 1.26` no `go.mod`; **não usar** recursos exclusivos do Go 1.27 | Funciona nas versões 1.26.x e 1.27.x |
| D9 | Dependências: `github.com/jackc/pgx/v5` (≥ v5.9.2) e `gopkg.in/yaml.v3`. O resto é biblioteca padrão | Menos superfície, menos risco de não compilar sem rede |
| D10 | Sem Docker, WSL ou Make. Scripts `.ps1` **somente ASCII** (PowerShell 5.1) | Mesmo padrão do mathlab (`docs/WINDOWS.md`) |

---

## 2. Objetivos e não objetivos

### Objetivos

1. Guardar chaves de vários provedores com segurança e metadados (rótulo, conta/organização, tags).
2. Servir múltiplos projetos com **política própria** (quais chaves, quais modelos, prioridade, reserva diária, espera máxima).
3. **Nunca estourar limites sem necessidade**: TPM, RPM, RPD, TPD, concorrência; buckets compartilhados (`*:free`); escopo por chave, por conta ou por provedor; aprendizado automático de que duas chaves dividem a mesma conta.
4. **Manter as chaves aquecidas e saudáveis** (seção 11) e detectar chave inválida/morta antes do primeiro pedido real.
5. Retry, fallback entre modelos/provedores e *spill* (usar o próximo alvo quando o preferido está saturado) **dentro do serviço**.
6. Streaming (SSE) sem perda de desempenho.
7. Observabilidade local: `/metrics`, estatísticas por chave, eventos, uso diário — **nunca** registrar o conteúdo dos prompts por padrão.
8. Ser otimizável: caminho quente sem banco, sem alocações supérfluas, benchmarks incluídos.

### Não objetivos (v1)

- Contas, múltiplos usuários, papéis/RBAC, OAuth, cobrança.
- Cache semântico, RAG, orquestração de agentes.
- Alta disponibilidade, cluster, mais de uma instância (uma trava consultiva no PostgreSQL impede duas instâncias ativas).
- Docker, nuvem, Kubernetes.
- Interface web (opcional e só leitura, na fase 8).
- *Hedging* (disparar a mesma requisição em duas chaves): gasta cota.

---

## 3. Arquitetura

```
 Projetos (mathlab, outros)                     KeyPool (um processo Go: keypoold)
 ┌────────────────────────┐   HTTP 127.0.0.1    ┌──────────────────────────────────────────────────────────────┐
 │ Authorization: Bearer  │ ──────────────────▶ │ gateway ── auth(kp_...) ── projeto (X-Keypool-Project)       │
 │   kp_...               │   POST /v1/chat/... │     │                                                         │
 │ X-Keypool-Project: ... │ ◀────────────────── │     ▼                                                         │
 └────────────────────────┘   JSON ou SSE       │ router: rota → cadeia de alvos → estimar tokens → spill       │
                                                │     │        ▲ retry / fallback / espera                      │
                                                │     ▼        │ classificação de erros                         │
                                                │ sched.Pool (1 por provedor, em memória)                       │
                                                │   chaves · janelas · cooldown · cota · prioridade · saúde     │
                                                │     │                                                         │
                                                │     ▼                                                         │
                                                │ provider adapter ── HTTP/2 keep-alive ──▶ Groq / OpenRouter / │
                                                │                                           outros              │
                                                │                                                               │
                                                │ warm: sondas L0/L1/L2, catálogo, circuit breaker, hints       │
                                                │ usage: escritor assíncrono em lote                            │
                                                │ snapshotter: salva janelas a cada ~10 s                       │
                                                └───────────┬──────────────────────────────────────────────────┘
                                                            │ (fora do caminho quente)
                                                            ▼
                                                   PostgreSQL local (config, chaves cifradas, uso, eventos)
```

### Fluxo de uma requisição (resumo)

1. Autentica a `kp_...` (hash + comparação em tempo constante) e resolve o projeto pelo cabeçalho.
2. Resolve o campo `model`: um **alias de rota** do projeto (`prover`) ou um alvo literal `provider:model` (se o projeto permitir).
3. Monta a cadeia de alvos da rota (ex.: `groq:openai/gpt-oss-120b` → `groq:openai/gpt-oss-20b` → `openrouter:openrouter/free`), filtrando alvos mortos, excluídos e sem chave válida/catálogo.
4. Estima tokens (prompt + saída) e ajusta `max_tokens` ao TPM do modelo.
5. Escolhe o alvo (spill: o primeiro com folga agora; senão o que libera mais cedo) e **reserva** uma chave no `sched.Pool` (`Acquire`).
6. Chama o provedor. Streaming é repassado em tempo real.
7. Classifica o resultado → `Lease.Success/RateLimited/QuotaExhausted/AuthFailed/Failed`. Em erro, volta ao passo 5 (outra chave ou outro alvo).
8. Devolve a resposta + cabeçalhos `X-Keypool-*` (qual chave mascarada, qual alvo, tentativas, tempo na fila).
9. Em segundo plano: registra uso, atualiza saúde/latência, aprende com os cabeçalhos de limite.

---

## 4. Modelo de domínio

| Termo | Definição |
|---|---|
| **Provider** | Um serviço de IA (`groq`, `openrouter`...). Tem tipo (adaptador), `base_url`, autenticação, limites, escopo, estratégia, cabeçalhos de limite, capacidades. É dado (tabela `providers`). |
| **Key** | Uma chave de API de um provedor. Tem rótulo humano, `account_group` (organização/conta), tags, estado (`enabled`, `status`). O segredo fica cifrado. |
| **Account group** | Conjunto de chaves que dividem a mesma cota (na Groq o limite vale por **organização**, não por chave). Chaves sem grupo contam como contas separadas. |
| **Bucket** | Janelas de limite compartilhadas entre modelos. Por padrão, o bucket é o próprio modelo; uma regra (`*:free`) pode agrupar vários modelos num bucket (`openrouter-free`). |
| **Limit group** | A unidade que **de fato** possui as janelas: conforme o escopo é a chave, o *account group* ou o provedor inteiro. |
| **Escopo** (`limit_scope`) | `key`, `group`, `provider` ou `auto` (começa separado e **funde** quando duas chaves levam 429 quase juntas; a fusão é lembrada no banco). |
| **Project** | Um consumidor (mathlab). Tem política (reserva diária, espera máxima, spill, concorrência) e regras de quais chaves pode usar. |
| **Route** | Um alias dentro do projeto (papel). Tem prioridade, cadeia de alvos, parâmetros padrão e requisitos (ex.: `reasoning_effort`). |
| **Target** | `provider:model`. Item da cadeia de uma rota. |
| **Lease** | Reserva de uma chave para uma chamada. Precisa ser concluída com `Success`, uma falha classificada ou `Release`. Expira sozinha (guarda contra vazamento). |
| **Hints** | Valores reais lidos dos cabeçalhos do provedor (`x-ratelimit-remaining-*`, `retry-after`...). Corrigem as janelas locais. |
| **Health** | Estado da chave: `unknown → healthy → degraded → quarantined (circuito aberto) → probing (half-open)`; `invalid` quando o provedor recusa a credencial. |
| **Warm** | Chave validada recentemente, com catálogo conhecido, conexão quente, latência-base medida e janelas sincronizadas. |

---

## 5. Stack e decisões técnicas

| Tema | Escolha | Observação |
|---|---|---|
| Linguagem | Go, `go 1.26` | Em novembro/2026 o Go 1.27 já saiu (19/08/2026); 1.26.x continua suportado. Não usar métodos genéricos, `encoding/json/v2`, pacote `uuid` ou `simd` da stdlib 1.27. |
| HTTP servidor | `net/http` (stdlib), `ServeMux` com padrões de método (`"POST /v1/chat/completions"`) | Sem frameworks |
| HTTP cliente | `net/http` com um `Transport` por provedor | `ForceAttemptHTTP2`, `MaxIdleConnsPerHost`, `IdleConnTimeout` longo, `tls.NewLRUClientSessionCache` |
| JSON | `encoding/json` v1 | Repassar o corpo com `map[string]json.RawMessage` para não perder campos desconhecidos |
| Banco | PostgreSQL ≥ 16 (nativo no Windows), `pgx/v5` + `pgxpool` | `COPY`/lote para uso; `LISTEN/NOTIFY` opcional |
| Migrações | SQL embutido (`embed`) + migrador próprio (~100 linhas) | Sem `goose`/`migrate` |
| Config de bootstrap | YAML (`yaml.v3`) + variáveis `KEYPOOL_*` | Só o mínimo: DSN, endereço, caminho da master key |
| Config de domínio | No PostgreSQL; importável/exportável em YAML (`keypoolctl apply`) | Segredos **nunca** vão para YAML |
| Cifra | AES-256-GCM (stdlib `crypto/aes`, `crypto/cipher`) | Master key em arquivo; DPAPI do Windows é opcional na fase 8 |
| Logs | `log/slog` (JSON ou texto) | Tipo `secret.Secret` implementa `LogValuer`: impossível logar a chave inteira por acidente |
| Métricas | Texto no formato Prometheus escrito à mão | Sem cliente Prometheus |
| IDs | `uuid` gerado pelo PostgreSQL (`gen_random_uuid()`); IDs de requisição com `crypto/rand` | |
| Tempo | Interface `clock.Clock` injetada em **todo** código com lógica temporal | Testes determinísticos |

---

## 6. Estrutura do repositório

```
keypool/
├─ go.mod
├─ README.md
├─ AGENTS.md                    # convenções para futuros agentes de IA
├─ STATUS.md                    # progresso por fase (mantido pelo agente)
├─ docs/
│  ├─ PLANO.md                  # este arquivo
│  ├─ ARQUITETURA.md  API.md  SECURITY.md  PROVIDERS.md
│  ├─ OPERACAO_WINDOWS.md  VERIFY.md  DECISIONS.md
├─ cmd/
│  ├─ keypoold/                 # daemon: init | migrate | serve | doctor | version
│  └─ keypoolctl/               # CLI administrativa (fala com a API admin)
├─ internal/
│  ├─ domain/                   # tipos puros compartilhados (IDs, Outcome, Target, erros)
│  ├─ clock/                    # Clock, Real, Fake
│  ├─ secret/                   # Secret (mascara em String/GoString/Marshal/LogValue), Mask, Redact
│  ├─ sliding/                  # janela deslizante (buffer circular + soma corrente)
│  ├─ limits/                   # Limits, Rule, ModelLimits → Resolve(model) → Resolved
│  ├─ sched/                    # Pool, Lease, estados da chave, escopo/fusão, estratégias, fila de prioridade, hints, saúde
│  ├─ classify/                 # status/corpo/erro de rede → Outcome (compartilhado pelos adaptadores)
│  ├─ sse/                      # leitor/repassador SSE, extração de usage, detecção de erro no 1º evento
│  ├─ provider/                 # Spec, Adapter, registro, presets/*.yaml (embed)
│  │   ├─ openaicompat/         # adaptador base (Groq, OpenRouter, Together, DeepSeek, Ollama...)
│  │   ├─ generic/              # passagem crua com injeção de chave (APIs não-LLM)
│  │   └─ presets/              # groq.yaml, openrouter.yaml
│  ├─ router/                   # resolução de rota, cadeia, spill, loop de tentativas, estimativa/ajuste de tokens
│  ├─ gateway/                  # servidor HTTP, middlewares, superfície OpenAI, leases, passthrough, capacidade
│  ├─ admin/                    # API administrativa
│  ├─ config/                   # bootstrap, Snapshot imutável, validação, formato YAML de apply
│  ├─ cryptobox/                # AES-GCM, master key, verificação da master key
│  ├─ store/                    # interface Store
│  │   ├─ memstore/             # implementação em memória (testes e modo dev)
│  │   ├─ pgstore/              # implementação PostgreSQL (único pacote que importa pgx)
│  │   └─ migrations/           # 0001_init.sql ... (embed)
│  ├─ warm/                     # Warmer: agenda, sondas, máquina de saúde, catálogo
│  ├─ usage/                    # escritor assíncrono em lote, rollup, retenção
│  ├─ metrics/                  # contadores/histogramas → texto Prometheus
│  ├─ obs/                      # slog, request id, recover
│  └─ testutil/                 # fakeupstream (httptest programável), fixtures
├─ configs/examples/            # keypool.dev.yaml, mathlab.project.yaml
└─ scripts/windows/             # build.ps1 test.ps1 setup-db.ps1 run-dev.ps1 install-task.ps1 (ASCII)
```

**Regra de dependência (evita ciclos):** `domain`, `clock`, `secret`, `sliding`, `limits` não importam nada interno além de `domain`/`clock`/`secret`.
`sched` importa só esses. `provider`/`classify`/`sse` não importam `sched`. `router` junta `sched` + `provider`. `gateway`/`admin` usam `router` e `store`.
Só `store/pgstore` importa `pgx`. Só `config` e `cmd/*` importam `yaml.v3`.

---

## 7. Modelo de dados PostgreSQL

Regras: `timestamptz` sempre; `uuid` com `gen_random_uuid()`; `jsonb` para configuração que evolui; nada de segredo em claro; migrações em arquivos numerados e imutáveis.

```sql
-- 0001_init.sql  (o migrador cria schema_migrations por conta própria)

CREATE TABLE app_settings (                       -- linha única
  id                  boolean PRIMARY KEY DEFAULT true CHECK (id),
  api_key_hash        bytea       NOT NULL,       -- sha256(kp_...)
  api_key_prefix      text        NOT NULL,       -- "kp_ab12": só para identificar, nunca o segredo
  api_key_created_at  timestamptz NOT NULL DEFAULT now(),
  api_key_rotated_at  timestamptz,
  master_key_check    bytea       NOT NULL,       -- AEAD de um texto conhecido: detecta master key errada
  config_version      bigint      NOT NULL DEFAULT 1
);

CREATE TABLE providers (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name             text NOT NULL UNIQUE CHECK (name ~ '^[a-z][a-z0-9_-]*$'),
  type             text NOT NULL,                 -- openai_compatible | generic_http | (adaptadores futuros)
  enabled          boolean NOT NULL DEFAULT true,
  spec             jsonb   NOT NULL,              -- base_url, auth, headers, paths, timeouts, capabilities, rate_headers, ping, error_rules
  limit_scope      text    NOT NULL DEFAULT 'key' CHECK (limit_scope IN ('key','group','provider','auto')),
  key_strategy     text    NOT NULL DEFAULT 'round_robin' CHECK (key_strategy IN ('round_robin','least_used','headroom','latency')),
  quota_cooldown_s integer NOT NULL DEFAULT 3600 CHECK (quota_cooldown_s >= 0),
  limits           jsonb   NOT NULL DEFAULT '{}', -- default_*, per_model, rules (mesmo formato do providers.yaml do mathlab)
  warm             jsonb   NOT NULL DEFAULT '{}',
  learned          jsonb   NOT NULL DEFAULT '{}', -- ex.: {"merged": true} aprendido pelo escopo auto
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE keys (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  provider_id     uuid NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
  label           text NOT NULL,                  -- "groq-conta-a-1": o que aparece em logs e cabeçalhos
  secret_enc      bytea NOT NULL,                 -- versão(1) | nonce(12) | ciphertext+tag  (AAD = id || provider)
  secret_hash     bytea NOT NULL,                 -- sha256(segredo): deduplicação sem decifrar
  mask            text  NOT NULL,                 -- "gsk_…ab12"
  account_group   text,                           -- chaves com o mesmo valor dividem cota
  tags            text[] NOT NULL DEFAULT '{}',
  enabled         boolean NOT NULL DEFAULT true,  -- interruptor manual
  status          text NOT NULL DEFAULT 'unknown' CHECK (status IN ('unknown','healthy','degraded','quarantined','invalid')),
  status_reason   text,
  status_at       timestamptz,
  last_ok_at      timestamptz,
  last_error_at   timestamptz,
  last_used_at    timestamptz,
  meta            jsonb NOT NULL DEFAULT '{}',
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now(),
  UNIQUE (provider_id, label),
  UNIQUE (provider_id, secret_hash)
);
CREATE INDEX keys_provider_idx ON keys(provider_id) WHERE enabled;

CREATE TABLE projects (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name         text NOT NULL UNIQUE CHECK (name ~ '^[a-z][a-z0-9_-]*$'),
  description  text NOT NULL DEFAULT '',
  enabled      boolean NOT NULL DEFAULT true,
  allow_lease  boolean NOT NULL DEFAULT false,    -- permite devolver a chave ao cliente (modo lease)
  allow_literal_targets boolean NOT NULL DEFAULT false, -- aceita model = "provider:model"
  policy       jsonb NOT NULL DEFAULT '{}',       -- tpd_reserve, reserve_priority_cutoff, spill_on_saturation, max_wait_ms, max_inflight, priority_offset, daily_token_budget
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE project_key_rules (                  -- quais chaves o projeto pode usar. Sem regra 'include' = provedor proibido.
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id   uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  provider_id  uuid NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
  mode         text NOT NULL CHECK (mode IN ('include','exclude')),
  selector     jsonb NOT NULL,                    -- {"key_ids":[], "labels":[], "tags_any":[], "tags_all":[], "account_groups":[]}
  position     integer NOT NULL DEFAULT 0
);
CREATE INDEX project_key_rules_idx ON project_key_rules(project_id, provider_id);

CREATE TABLE routes (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id  uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  alias       text NOT NULL,                      -- "prover", "critic"...
  priority    integer NOT NULL DEFAULT 5 CHECK (priority BETWEEN 0 AND 9),
  chain       jsonb NOT NULL,                     -- [{"provider":"groq","model":"openai/gpt-oss-120b","params":{}}]
  defaults    jsonb NOT NULL DEFAULT '{}',        -- temperature, max_tokens, reasoning_effort...
  requires    jsonb NOT NULL DEFAULT '{}',        -- {"reasoning_effort": true}
  UNIQUE (project_id, alias)
);

-- Estado de execução persistido (restauração após reinício)
CREATE TABLE limit_group_state (
  provider_id uuid NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
  group_key   text NOT NULL,                      -- id da chave | nome do grupo | "*"
  state       jsonb NOT NULL,                     -- janelas por bucket (timestamps em epoch ms de parede), cooldowns, bloqueios de cota
  saved_at    timestamptz NOT NULL,
  PRIMARY KEY (provider_id, group_key)
);

CREATE TABLE model_catalog (
  key_id    uuid NOT NULL REFERENCES keys(id) ON DELETE CASCADE,
  model_id  text NOT NULL,
  seen_at   timestamptz NOT NULL,
  PRIMARY KEY (key_id, model_id)
);

CREATE TABLE health_checks (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  key_id     uuid NOT NULL REFERENCES keys(id) ON DELETE CASCADE,
  probe      text NOT NULL,                       -- conn | validate | deep
  ok         boolean NOT NULL,
  http_status integer,
  latency_ms integer,
  error      text,                                -- já redigido (sem segredos)
  at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX health_checks_key_at ON health_checks(key_id, at DESC);

CREATE TABLE usage_events (
  id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  ts                 timestamptz NOT NULL,
  request_id         text NOT NULL,
  project            text NOT NULL,
  alias              text NOT NULL DEFAULT '',
  provider           text NOT NULL,
  model              text NOT NULL,
  key_id             uuid,
  source             text NOT NULL DEFAULT 'request' CHECK (source IN ('request','warmup','lease','passthrough')),
  outcome            text NOT NULL,               -- success | rate_limited | quota | auth | transient | client_error | canceled ...
  http_status        integer,
  attempt            integer NOT NULL DEFAULT 1,
  queue_ms           integer NOT NULL DEFAULT 0,
  ttfb_ms            integer,
  total_ms           integer,
  prompt_tokens      integer,
  completion_tokens  integer,
  est_tokens         integer,
  stream             boolean NOT NULL DEFAULT false,
  error_class        text
);
CREATE INDEX usage_events_ts_brin ON usage_events USING brin (ts);
CREATE INDEX usage_events_key_ts ON usage_events(key_id, ts DESC);

CREATE TABLE usage_daily (
  day               date NOT NULL,
  project           text NOT NULL,
  provider          text NOT NULL,
  model             text NOT NULL,
  key_id            uuid,
  requests          integer NOT NULL DEFAULT 0,
  errors            integer NOT NULL DEFAULT 0,
  rate_limited      integer NOT NULL DEFAULT 0,
  prompt_tokens     bigint  NOT NULL DEFAULT 0,
  completion_tokens bigint  NOT NULL DEFAULT 0,
  PRIMARY KEY (day, project, provider, model, key_id)
);

CREATE TABLE events (                              -- trilha operacional e de auditoria administrativa
  id       bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  ts       timestamptz NOT NULL DEFAULT now(),
  level    text NOT NULL,                          -- info | warn | error
  kind     text NOT NULL,                          -- key.invalid | scope.merged | config.applied | warm.failed | admin.key_added ...
  subject  text NOT NULL DEFAULT '',               -- rótulo da chave, nome do projeto...
  detail   jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX events_ts ON events(ts DESC);
```

### Uso do banco em tempo de execução

| Operação | Quando | Como |
|---|---|---|
| Carregar configuração | partida e a cada mudança administrativa | uma transação de leitura → `config.Snapshot` imutável → `atomic.Pointer` |
| Salvar janelas/cooldowns | a cada ~10 s **se houver mudança**, e no desligamento | `INSERT ... ON CONFLICT DO UPDATE` em lote |
| Registrar uso | em lote (a cada 1 s ou 200 linhas) | canal limitado; se encher, **descarta** e conta (`usage_dropped_total`); nunca bloqueia a requisição |
| Sondas/eventos | assíncrono | mesmo escritor em lote |
| Retenção | tarefa diária | `usage_events` 30 dias (rollup para `usage_daily` antes), `health_checks` 7 dias, `events` 90 dias |
| Instância única | partida | `pg_try_advisory_lock(<constante>)`; se falhar, `keypoold` encerra com mensagem clara |
| Recarga por SQL direto (opcional) | — | `LISTEN keypool_config`; um gatilho em `app_settings.config_version` faz `NOTIFY` |


---

## 8. Núcleo de agendamento (`internal/sched`)

Este é o coração do projeto. É um **port melhorado** de `src/mathlab/llm/key_pool.py` + `rate_limiter.py`. O comportamento observável deve ser preservado (seção 8.5);
as melhorias estão na seção 8.6.

### 8.1 Conceitos

- Um `sched.Pool` por provedor, 100 % em memória, seguro para uso concorrente.
- Cada chave tem `KeyState` (desativada?, saúde, cooldowns por bucket, bloqueio de cota, contadores, latência EWMA).
- As janelas ficam em **limit groups** (não na chave): `scope=key` → uma por chave; `group` → uma por `account_group` (chave sem grupo = grupo próprio); `provider` → uma só; `auto` → começa como `group` e, se detectar fusão, passa a `provider` (e persiste `learned.merged`).
- Por bucket há cinco dimensões independentes: **TPM** (tokens, 60 s), **TPD** (tokens, 24 h), **RPM** (requisições, 60 s), **RPD** (requisições, 24 h) e **CONC** (requisições em andamento, sem janela — novo).
- Limites resolvidos por `limits.Resolve(model)`: `per_model` > regra (`match` glob) > `default_*`. Retorna `Resolved{TPM, RPM, RPD, TPD, Conc *int; Bucket string; Free bool}` — **struct tipada**, nunca `map`.

### 8.2 Algoritmo de `Acquire` (equivalente ao Python)

Entrada: `Model, Tokens (estimativa), Priority, Reserve, Allow(keyID) bool`.

1. `need = max(0, Tokens)`. `resolved = limits.Resolve(Model)`. `bucket = resolved.Bucket`.
2. Sem chaves → `ErrNoKey`. Todas desativadas (ou nenhuma permitida por `Allow`) → `ErrAllDisabled`.
3. Se `resolved.TPM != nil && need >= *TPM` → `ErrExceedsLimit` (impossível; **não** esperar para sempre).
4. Para cada chave na ordem da estratégia, calcular `wait(key)` = máximo de:
   - cooldown (`all_cooldown_until`, `cooldowns[bucket]`);
   - bloqueio de cota (`quota_blocked_until`; se `!Free`, também `quota_blocked_buckets[bucket]`);
   - `TPM.waitForRoom(need, limit)`;
   - `TPD.waitForRoom(need, floor(tpd * (1 - clamp(Reserve, 0, 0.95))))`, mínimo 1 (como no Python, se `need >= limit` **não** há espera: peculiaridade preservada e registrada em `DECISIONS.md`);
   - `RPM.waitForSlots(limit)` e `RPD.waitForSlots(limit)` (contam **entradas**, não tokens);
   - `CONC`: se `inflight >= limit` → espera "indeterminada" (acorda quando um lease termina);
   - **hint** (novo): se o provedor informou `remaining < need + margem` e o `resetAt` está no futuro → `resetAt - now`;
   - **saúde** (novo): circuito aberto → `openUntil - now`; `invalid` → inelegível.
5. Elegível = `wait == 0`. Se há elegíveis **e** nenhum outro chamador *mais urgente* está bloqueado no mesmo bucket → escolher conforme a estratégia, **reservar** (adiciona `need` em TPM e TPD, `1` em RPM/RPD, `inflight++`), avançar o cursor e devolver o `Lease`.
6. Senão, calcular `wait = min(waits)`. Se `ctx` já expirou → erro: `ErrOutranked` (cedeu a vez) ou `ErrNoCapacity{RetryIn}`. Se `wait > tempo restante do ctx` → `ErrNoCapacity{RetryIn: wait}` imediatamente (não dormir à toa).
7. Entrar na fila do bucket (uma vez): ticket crescente + prioridade. Esperar `min(wait, 5 s, restante)` **ou** um sinal de acordar (`wake`, um canal por bucket fechado/renovado quando a capacidade aumenta: sucesso com delta negativo, `clear_cooldown`, lease liberado, hint melhor) **ou** `ctx.Done()`. Voltar ao passo 4.
8. Ao sair (qualquer caminho): remover o ticket da fila.

**Ordem de passagem na fila** (`outranked`): outro chamador bloqueado no bucket passa na frente se `prioridade menor` (mais urgente) ou `igual e ticket mais antigo`. Chamadores sem espera (`TryAcquire`) também cedem se houver alguém mais urgente esperando.

**Estratégias** (`key_strategy`):

| Nome | Regra |
|---|---|
| `round_robin` (padrão, paridade com Python) | primeiro elegível a partir do cursor; o cursor avança para `índice+1` (um cursor por bucket) |
| `least_used` | elegível com menor uso de TPM na janela (só faz sentido se as janelas não forem compartilhadas) |
| `headroom` (recomendada para Groq) | elegível com maior folga relativa `min(1 - usedTPM/limit, 1 - usedTPD/limit, 1 - usedRPM/limit)`; empate → round robin |
| `latency` | *power of two choices* entre elegíveis, preferindo menor EWMA de TTFB do par (chave, modelo); evita concentrar tudo na mais rápida |

### 8.3 Relatórios do `Lease`

| Método | Efeito |
|---|---|
| `Success(actual, hints)` | **corrige a própria entrada reservada** nas janelas de TPM e TPD (`sliding.Adjust(ref, actual)`, sem criar entradas negativas); se a entrada já saiu da janela (chamada muito longa), acrescenta `actual` em `now`; `used_total += actual`; `errors = 0`; `inflight--`; aplica hints; acorda a fila se liberou capacidade. (O Python soma um delta em `now`, o que pode deixar a janela momentaneamente negativa.) |
| `RateLimited(retryAfter, known, hints)` | `cooldown = retryAfter` (padrão 60 s se desconhecido) **no bucket**; se o escopo for compartilhado, esfria **todas** as chaves do grupo; alimenta a heurística de fusão do `auto` |
| `QuotaExhausted(cooldown)` | cooldown = `quota_cooldown` (padrão 3600 s). Modelo `Free` → bloqueia a chave **inteira**; modelo pago → só os buckets não gratuitos. Não desativa a chave |
| `AuthFailed()` | desativa a chave (`status=invalid`), evento `key.invalid` |
| `Failed()` | `errors++`; cooldown geral `min(30 s, 2^errors)`; conta para o circuit breaker |
| `Release()` | cliente cancelou antes de chegar ao provedor: **devolve** a reserva (tokens, RPM/RPD, inflight) sem contar uso |

**Fusão de escopo (`auto`)**: duas chaves **diferentes** com 429 no **mesmo bucket** em ≤ 20 s, e `retry_after` ≤ 120 s (ou desconhecido) → `merged = true`: as janelas por chave são absorvidas na janela compartilhada, os cooldowns viram o máximo, gera um aviso (`Notice`) e um evento `scope.merged`; a decisão é gravada em `providers.learned`. Constantes configuráveis (`MergeWindow`, `MergeMaxRetry`).

**Lease sem relatório**: um *watchdog* (`lease_ttl`, padrão 15 min; igual ao timeout máximo) conclui o lease como `Success(estimado)` e registra `lease.leaked` (bug do chamador ou do router).

### 8.4 Persistência e restauração

- `Pool.Snapshot()` devolve, por limit group, as entradas das janelas ainda válidas (timestamps de **parede** em epoch ms), cooldowns, bloqueios de cota e saúde. O `Snapshotter` salva a cada ~10 s se algo mudou e no desligamento.
- `Pool.Restore()` converte parede → relógio monotônico: `mono = nowMono - (nowWall - tsWall)`; descarta entradas vencidas e cooldowns passados. Entradas com timestamp no futuro (relógio mudou) são limitadas a `now`.
- Chaves removidas/adicionadas em `SetKeys` (recarga de config) **preservam o estado** por `KeyID`.

### 8.5 Cenários de aceitação (derivados de `tests/test_key_pool.py`)

Cada linha vira um teste Go com `clock.Fake`. Os nomes sugeridos mantêm rastreabilidade.

| Teste Go | Cenário | Esperado |
|---|---|---|
| `TestFromEnvThreeFormsAndDedupe` | `GROQ_API_KEYS="a,b c\na"`, `GROQ_API_KEY=d`, `GROQ_API_KEY_2=b`, `GROQ_API_KEY_1=e` | ordem `a,b,c,d,e` (5 chaves). Ordem: `_API_KEYS` → `_API_KEY` → `_API_KEY_<n>` por n crescente; separadores vírgula/espaço/quebra; sem duplicatas |
| `TestFromEnvPrefixDoesNotMix` | GROQ vs OPENROUTER | cada prefixo só vê as suas chaves |
| `TestRoundRobin` | 2 chaves, 3 acquires | `k1,k2,k1` |
| `TestRateLimitCooldownSwitchesKey` | `RateLimited(30 s)` na k1 (sem modelo) | próximo acquire = k2; `cooldown_remaining(k1) ≈ 30` |
| `TestAuthErrorDisablesKey` | `AuthFailed` na k1, depois na k2 | k1 `disabled`; acquire → k2; após k2, `ErrAllDisabled` |
| `TestTPMReservationWaitsWithFakeClock` | TPM=100, reservas de 60+60 | 2º com timeout 0 falha; com timeout 120 espera e o relógio avança ≥ 60 s |
| `TestTPMIsPerModel` | TPM padrão 100, `special`=50 | modelos têm janelas separadas |
| `TestStatsNeverExposeKey` | stats serializado em JSON | não contém o segredo; contém o prefixo da máscara (`gsk_`) |
| `TestSuccessAdjustsReservation` | reservou 500, real 100 | `used(modelo) == 100` |
| `TestConcurrentAcquireIsRaceFree` | 12 goroutines × 60 acquires em 3 chaves (`-race`) | todas as chaves usadas; nenhum pânico |
| `TestRPMEnforcedAndWaits` | RPM=20 | 21º falha; `EstimateWait ≈ 60 s`; com timeout 70 passa e o relógio ≥ 60 |
| `TestRPDUses24hWindow` | RPD=2 | 3º falha; `EstimateWait > 86000 s` |
| `TestLimitOneIsEnforced` | RPM=1 | 2º falha; com timeout 61 passa |
| `TestFreeBucketSharedBetweenFreeModels` | regra `*:free` → bucket `gratis`, RPM=2 | `a:free`, `b:free` passam; `c:free` falha; `pago` passa |
| `TestProviderScopeSharesWindowsBetweenKeys` | escopo `provider`, RPM=1, 2 chaves | 2º acquire falha em qualquer chave; `EstimateWait ≈ 60` |
| `TestRateLimitCooldownIsPerBucket` | 429 em `a:free` (60 s) | `pago` ainda passa; 429 sem modelo (todos os buckets) → `pago` falha |
| `TestProviderScopeRateLimitCoolsEveryKey` | escopo `provider`, 429 de 30 s | todas as chaves com cooldown ≈ 30 |
| `TestQuotaExhaustedOnFreeBlocksKeyTotally` | 402 em `a:free`, `quota_cooldown=100 s` | `quota_blocked=true`, `disabled=false`; `pago` falha; após 101 s volta |
| `TestQuotaExhaustedOnPaidKeepsFreeUsable` | 402 em `pago` | `a:free` passa; `pago` falha até 101 s |
| `TestEstimateWaitDoesNotReserve` | TPM=1000 | `EstimateWait(100)==0` repetido sem consumir; `EstimateWait(1000)` → impossível |
| `TestEstimateWaitImpossibleWhenAllDisabled` | todas desativadas | impossível |
| `TestLeastUsedPrefersLessUsedKey` | 3 chaves, `least_used`, 100/100/1/1 | `k1,k2,k3,k3` |
| `TestTwoPoolsInParallelAreIndependent` | pools groq e openrouter em paralelo | cada um usa só as suas chaves |

**Novos testes obrigatórios** (sem equivalente em Python): prioridade na fila (urgente passa na frente com limite saturado); reserva de TPD (chamador de baixa prioridade enxerga `1 - reserve`); fusão `auto` (duas chaves, 429 em 20 s → `pooled`, janelas unificadas, aviso uma vez); `Release` devolve a reserva; lease vazado é concluído pelo watchdog; `Snapshot/Restore` com relógio fake; hint de `remaining-tokens` baixo bloqueia até o `reset`; circuito aberto exclui a chave e meia-abertura admite 1 teste; `CONC` bloqueia e libera.

### 8.6 Melhorias sobre o Python (e por quê)

| # | Python hoje | Go |
|---|---|---|
| 1 | `KeyState` é `@dataclass` com `key` no `repr`: qualquer `print(state)` vaza a chave inteira | `secret.Secret` (`String/GoString/MarshalJSON/MarshalText/LogValue` mascarados); revelar só por `Reveal()` dentro do transporte |
| 2 | Espera por *polling* com `sleep` de até 5 s | canal de acordar por bucket + `time.Timer`; respeita `ctx` |
| 3 | Dimensões fixas e dicionário sem tipo (`resolved["bucket"]`) | struct `Resolved` tipada; dimensões como lista interna (`CONC` já incluída; ITPM/OTPM da Groq cabem depois) |
| 4 | Janela `O(n)` por purga/soma | buffer circular com soma corrente (`O(1)` amortizado) e busca do "tempo até liberar" só quando necessário |
| 5 | Estado só em memória (RPD/TPD zeram a cada execução) | snapshot/restore no PostgreSQL |
| 6 | Cursor de round robin único por pool | um cursor por bucket (evita viés entre modelos) |
| 7 | Sem noção de saúde nem de cabeçalhos reais do provedor | saúde + circuit breaker + `Hints` (seção 11) |
| 8 | Aviso de fusão por *polling* (`consume_notice`) | evento + `slog` + callback opcional |
| 9 | `limit_scope: auto` reaprende a cada execução | fusão persistida |
| 10 | `import window_wait_for_slot` sem uso | removido |

---

## 9. Roteador (`internal/router`)

Equivale ao `LLMClient` do mathlab (`complete`, `_pick`, `_run_target`, `_fit_output_budget`, `_reserve_for`), agora dentro do serviço.

### 9.1 Resolução

1. `model` do corpo → **alias** do projeto ou `provider:model` literal (só se `allow_literal_targets`).
2. Cadeia = `route.chain`. Remover alvos:
   - de provedor desabilitado ou sem chave permitida para o projeto;
   - **mortos** (404 recente; TTL padrão 30 min, limpo quando o catálogo L1 volta a listar o modelo — hoje o Python só "morre" pela sessão inteira);
   - ausentes do catálogo das chaves permitidas (modelos fixos); exceção: roteadores dinâmicos (`openrouter/free`), aceitos em chaves cuja validação passou;
   - que não cumprem `route.requires` (ex.: `reasoning_effort` → só `reasoning_effort_models` do provedor);
   - citados em `X-Keypool-Exclude` (lista `provider:model` ou `model`; se não sobrar nenhum, mantém a cadeia toda);
   - com *preferência suave* por outros pesos (`X-Keypool-Avoid-Weights`): tira alvos cujo `weights_id(model)` (ID sem `:free`, `:nitro`...) coincide; se não sobrar nenhum, mantém.
3. Cadeia vazia → erro `keypool_no_target` (HTTP 503) com o motivo por alvo.

### 9.2 Parâmetros

Parâmetros efetivos = `route.defaults` ⊕ corpo da requisição (**a requisição vence**; exceção: chaves em `route.force`). Cabeçalhos aceitos: `X-Keypool-Priority` (inteiro de 0 a 9; substitui a prioridade da rota para aquela chamada), `X-Keypool-Timeout-Ms`, `X-Keypool-Exclude`, `X-Keypool-Avoid-Weights`.
O adaptador converte nomes (`max_completion_tokens` ↔ `max_tokens`, `reasoning_effort` ↔ `reasoning:{effort}`) e **descarta** o que o modelo não aceita.

### 9.3 Estimativa e ajuste de saída (port de `_fit_output_budget`)

- `prompt_est = ceil(len(bytes de "messages") / 4)`, corrigido por um fator por (provedor, modelo) aprendido (EWMA de `usage.prompt_tokens / prompt_est`, limitado a [0,5; 2,0]).
- `want = max_tokens` pedido. Se o modelo tem TPM: `room = TPM - prompt_est - 100`. Se `want <= room` segue. Se `room < 1024` → erro `prompt_too_large_for_tpm` (400) **para esse alvo** (tenta o próximo). Senão `max_tokens = room`.
- `estimate = prompt_est + max_tokens` é o que se reserva.

### 9.4 Escolha do alvo (`pick`) e *spill*

Com `spill_on_saturation` (padrão `true`) e mais de um alvo: percorre a cadeia pulando alvos "esgotados" (429 longo registrado); o primeiro com `EstimateWait == 0` vence; se nenhum, o de **menor espera**. Sem spill, usa sempre o primeiro vivo.

### 9.5 Loop de tentativas

```
rodadas = 0
loop:
  pendentes = cadeia
  enquanto houver pendentes:
     alvo = pick(pendentes); remover de pendentes
     se alvo esgotado: guardar RateLimited(restante); continuar
     resultado = executar(alvo, can_spill = há pendentes && spill)
     se sucesso: devolver
     guardar erro; se foi 429 curto: short_limited = true
  se short_limited && rodadas < 4:
     espera = menor EstimateWait da cadeia; dormir min(max(espera,0.5 s), 65 s) + 0.4 s × prioridade
     rodadas++; repetir
  sair
falha final: 429 com Retry-After (se o último foi 429 longo/esgotado) ou 502/503 conforme o caso
```

`executar(alvo)` (até `max_attempts = 8` tentativas **por alvo**):

1. `Acquire` com `Allow` = chaves permitidas pelo projeto ∩ catálogo do modelo. Timeout `0` se `can_spill`, senão `max_wait` (padrão 600 s, limitado por `X-Keypool-Timeout-Ms` e pelo contexto do cliente). Falhou e `can_spill` → devolver "sem chave livre agora" com a espera estimada.
2. Chamar o provedor. 3. Classificar (tabela 9.6) e relatar ao `Lease`.

### 9.6 Política de erros (única fonte da verdade)

| Resposta do provedor | `Outcome` | Ação no pool | Ação no router |
|---|---|---|---|
| 2xx (sem `error` no corpo) | `Success` | `Success(actual)` | devolver |
| 2xx **com** objeto `error` / `finish_reason=error` (OpenRouter) | conforme `error.code` como status | idem à linha do código | idem |
| 400, 413, 422 | `ClientError` | `Release`-like (sem punir a chave; conta uso estimado) | esse alvo não serve; **próximo alvo**; se todos falharem devolver 400 com a mensagem (redigida) |
| 401 | `AuthInvalid` | `AuthFailed` (desativa) + evento | tenta **outra chave** |
| 403 com termos de orçamento (`budget`, `limit`, `credit`, `spend`) | `QuotaExhausted` | `QuotaExhausted` | outra chave |
| 403 demais | `Forbidden` | bloqueia o par (chave, modelo) por 1 h; 2 modelos distintos → chave `invalid` | outra chave / próximo alvo |
| 402 | `QuotaExhausted` | `QuotaExhausted` (cooldown `quota_cooldown`) | outra chave |
| 404 | `ModelUnavailable` | `Release`-like | marca alvo **morto** (TTL) → próximo alvo |
| 408, 5xx, falha de rede, timeout, JSON inválido | `Transient` | `Failed()` + circuit breaker | *backoff* `min(30 s, 2^(n-1))` e outra chave; após `max_attempts` → próximo alvo |
| 429, `Retry-After < 120 s` (ou ausente) | `RateLimited{short}` | `RateLimited(retryAfter)` | há outra chave livre em ≤ 2 s? tenta outra chave; senão, se `can_spill` → próximo alvo; senão espera |
| 429, `Retry-After ≥ 120 s` | `RateLimited{long}` | `RateLimited`; quando **todas** as chaves (ou 1, se escopo compartilhado) estão em 429 longo → marca o **bucket esgotado até** `retry_after` e limpa os cooldowns individuais | pula o alvo nas chamadas seguintes |
| cliente cancelou | `Canceled` | `Release` | abortar o upstream |

`retry_after` = cabeçalho `Retry-After` (segundos) → senão `x-ratelimit-reset-*` (formato duração do Go: `7.66s`, `2m59.56s`, `14h12m`, `120ms`) → senão `x-ratelimit-reset` (epoch em s **ou** ms; limitar a 86 400 s). Veja `retry_after_from_headers` em `openai_compatible.py`.

### 9.7 Streaming

- Se `stream: true`, abrir a conexão ao provedor e **ler o primeiro evento SSE antes de confirmar** o cabeçalho ao cliente: erro no primeiro evento ainda permite *retry/fallback* transparente.
- Depois do primeiro byte entregue **não há mais retry** (a resposta já começou). Falha no meio do stream → fechar a conexão e registrar `Transient` (sem punir injustamente a chave se o cliente caiu).
- Repassar bytes imediatamente com *flush* (`http.NewResponseController(w).Flush()`); guardar só a **última** linha `data:` não-`[DONE]` para extrair `usage` (sem parsear todos os eventos).
- Garantir `stream_options: {"include_usage": true}` quando o provedor suporta (`capabilities.stream_include_usage`); sem `usage` final → `actual = estimate`.
- Cancelamento do cliente (`r.Context().Done()`) cancela o contexto do upstream e dispara `Release`/`Success(estimado)`.

### 9.8 Capacidade (`GET /v1/capacity`)

Port de `compute_capacity`/`resolve_parallelism` do `capacity.py`: para um projeto/alias, devolve `live_keys`, `buckets`, `lanes` (uma "via" = par chave×bucket com TPM que aceita uma requisição grande agora; bucket sem TPM conta `UNMETERED_LANES = 2`; escopo compartilhado **não** multiplica por chave) e `parallelism`. Serve para o mathlab decidir quantas estratégias/amostras lançar em paralelo.

---

## 10. Provedores (`internal/provider`)

### 10.1 Contrato do adaptador

```go
// Adapter traduz entre o formato canônico (OpenAI chat) e um provedor concreto.
type Adapter interface {
    Type() string
    // BuildRequest monta a requisição HTTP já autenticada (a chave entra aqui e só aqui).
    BuildRequest(ctx context.Context, spec *Spec, key secret.Secret, req *Canonical) (*http.Request, error)
    // Classify transforma resposta/erro em Outcome (e extrai retry-after, mensagem redigida, usage).
    Classify(resp *http.Response, body []byte, err error) Result
    // Hints lê os cabeçalhos de limite (configuráveis em spec.rate_headers).
    Hints(h http.Header, now time.Time) *sched.Hints
    // Probe: validação barata da chave (L1). Pode ser GET /models, GET /key, etc.
    Probe(ctx context.Context, c *http.Client, spec *Spec, key secret.Secret) ProbeResult
    // ListModels: catálogo visível para a chave.
    ListModels(ctx context.Context, c *http.Client, spec *Spec, key secret.Secret) ([]string, error)
    // AdaptPayload: renomeia/descarta campos conforme o modelo.
    AdaptPayload(spec *Spec, model string, payload map[string]json.RawMessage) map[string]json.RawMessage
}
```

`Canonical` guarda o corpo como `map[string]json.RawMessage` (campos desconhecidos passam intactos).

### 10.2 `Spec` declarativa (YAML/JSON, também no banco)

```yaml
name: groq
type: openai_compatible
base_url: https://api.groq.com/openai/v1
auth: { scheme: bearer }                    # bearer | header:NOME | query:NOME
headers: {}
request_extra: {}
timeout: 120s                                # cabeçalhos de resposta; stream não tem timeout total
capabilities:
  max_tokens_field: max_completion_tokens    # max_tokens | max_completion_tokens
  reasoning_style: top_level                 # top_level (reasoning_effort) | nested (reasoning:{effort}) | none
  reasoning_effort_models: ["openai/gpt-oss-*"]
  stream_include_usage: true
rate_headers:                                # nomes configuráveis; Groq (docs): requests=RPD, tokens=TPM
  remaining_tokens:   { header: x-ratelimit-remaining-tokens,   dimension: tpm }
  remaining_requests: { header: x-ratelimit-remaining-requests, dimension: rpd }
  reset_tokens:       { header: x-ratelimit-reset-tokens,       format: duration }
  reset_requests:     { header: x-ratelimit-reset-requests,     format: duration }
  retry_after:        { header: retry-after,                    format: seconds }
paths: { chat: /chat/completions, models: /models }
probe: { kind: models }                      # models | key | completion | none
limit_scope: auto
key_strategy: headroom
quota_cooldown: 1h
limits:
  default_tpm: 8000
  default_rpm: 30
  default_rpd: 1000
  default_tpd: 200000
  per_model:
    openai/gpt-oss-120b: { tpm: 8000, rpm: 30, rpd: 1000, tpd: 200000 }
```

**Presets embutidos** (`presets/groq.yaml`, `presets/openrouter.yaml`) trazem tudo acima já preenchido; o usuário sobrescreve só o que quiser. Os números são do plano gratuito **no momento da escrita** (mathlab `configs/providers.yaml`, 31/10/2026) e **devem ser conferidos** em `console.groq.com/settings/limits`.

OpenRouter (preset): `base_url https://openrouter.ai/api/v1`, `limit_scope: provider` (a capacidade é global por conta; chaves extras não multiplicam), `max_tokens_field: max_tokens`, `reasoning_style: nested`, cabeçalhos opcionais `HTTP-Referer`/`X-Title`, `probe: {kind: key, path: /key}`, regras `*:free` e `openrouter/free` → bucket `openrouter-free`, `free: true`, `rpm: 20`, `rpd: 50` (sobe para 1000 com ≥ 10 créditos comprados na vida da conta; o campo `/key` também informa o contador diário — usar como *hint* quando existir).

### 10.3 Como adicionar um provedor

| Caso | O que fazer |
|---|---|
| Compatível com a API de chat da OpenAI (Together, DeepSeek, Mistral, Cerebras, Fireworks, Ollama/LM Studio local, Gemini no modo OpenAI-compat...) | só config: `keypoolctl provider add --file novo.yaml` (ou `type: openai_compatible` + `base_url` + limites). **Nenhum código.** |
| API que não é de LLM (busca, mapas, TTS...) | `type: generic_http`: o serviço injeta a chave e repassa `ANY /p/{provider}/*`. Regras de bucket por caminho; unidade de custo = 1 requisição |
| Formato próprio (Anthropic Messages, Gemini nativo) | novo pacote `internal/provider/<nome>` que implementa `Adapter`, registrado em `init()` com `provider.Register("<tipo>", factory)`; escreve preset YAML; testes com `fakeupstream`. Backlog |

### 10.4 Classificação compartilhada

`internal/classify` implementa a tabela 9.6 sobre `(status, corpo JSON, erro de rede)`, com **regras sobrescrevíveis** por provedor (`error_rules: [{status: 403, body_matches: "(?i)credit", outcome: quota_exhausted}]`). Mensagens passam por `secret.Redact` (padrões `gsk_…`, `sk-or-…`, `sk-…{20,}`, `Bearer …`, mais a `kp_…`) antes de sair do pacote.

---

## 11. Aquecimento, saúde e catálogo (`internal/warm`)

Objetivo: **a primeira requisição real depois de uma pausa tem a mesma latência de uma no meio de uma rajada**, e uma chave ruim é descoberta **antes** de ser usada.

### 11.1 Camadas de sonda (da mais barata para a mais cara)

| Camada | O que faz | Custo de cota | Padrão |
|---|---|---|---|
| **L0 — conexão** | Mantém `N` conexões TLS/HTTP-2 quentes por host (`GET` autenticado ao endpoint de modelos, corpo descartado). Reaproveita sessões TLS. Mede DNS/connect/TLS com `httptrace` | nenhum token; ver nota abaixo sobre RPM | a cada 45 s **se o host ficou ocioso**, `idle_conns_per_host = min(4, nº de chaves)` |
| **L1 — validação** | `Probe` do provedor por chave (`GET /models`, `GET /key`...). Confirma credencial, **atualiza o catálogo** (`model_catalog`), lê limites reais (OpenRouter `/key`) e cabeçalhos `x-ratelimit-*` | nenhum token | na partida (paralelo limitado) e a cada 10 min ± 20 % de jitter |
| **L2 — profunda** | Completion de 1 token no modelo de sonda | **gasta cota** (1 RPD + poucos tokens) | **desligada**. Se ligada: orçamento diário em tokens, nunca em buckets `free`/RPD baixo (ex.: OpenRouter `:free` = 50/dia), e só para chaves sem tráfego real há > `interval` |

*Nota:* se o provedor contar o endpoint de modelos no RPM, ligar `counts_against_limits: true` — a sonda passa a ocupar uma vaga na janela de RPM, como qualquer requisição. Verificar por provedor; o padrão é `false`.

### 11.2 Saúde passiva (tráfego real)

Toda resposta alimenta a saúde (`sched` + `warm`): sucesso zera falhas; `Transient` e `RateLimited` repetidos degradam. **A sonda só roda em chave ociosa** (sem tráfego real dentro do intervalo): tráfego real já é a melhor sonda.

### 11.3 Máquina de estados e circuit breaker

```
unknown ──L1 ok──▶ healthy ──falhas consecutivas ≥ N──▶ quarantined (circuito ABERTO por T)
   ▲                  ▲                                      │ T expira
   │                  └──── 1 requisição de teste ok ◀── probing (meia-abertura: 1 requisição)
   └── invalid (401/403-credencial) ──nova validação L1 ok (auth_recheck 2 h)──▶ healthy
```

- Parâmetros: `failures_to_open = 3`, `open_duration = 30 s` com *backoff* exponencial até 10 min, `half_open_max = 1`.
- A requisição de teste da meia-abertura é **tráfego real** (não gasta cota extra).
- 429 **não** abre o circuito (é limite, não doença): vai para cooldown. Só `Transient`/timeout/5xx e falhas de conexão contam.
- `invalid` só sai por revalidação L1 bem-sucedida ou por `keypoolctl key enable` — cobre o caso de uma chave "invalidada" por um incidente do provedor.

### 11.4 Sincronização com a verdade do provedor (*hints*)

Todo cabeçalho de limite lido (mesmo em 200) vira um `Hints{Remaining, ResetAt, Dimension, ObservedAt}` por (limit group, bucket, dimensão). A elegibilidade usa `min(folga local, folga informada - margem)` até `ResetAt`. Isso previne 429 que a contabilidade local não prevê (outro processo usando a mesma conta, estimativa de tokens errada, chave compartilhada com outra máquina).
Formatos: números inteiros; durações Go (`time.ParseDuration`: `1.2s`, `2m59.56s`, `14h12m`, `120ms`); epoch s/ms.

### 11.5 Catálogo

`ListModels` por chave, cache de 1 h (TTL configurável) + atualização a cada L1. O router usa o catálogo para que "um modelo fixo só use chaves que o listaram" (equivalente a `validate_model_availability` do mathlab, agora **permanente** e não por execução).
Roteadores dinâmicos (`provider.IsDynamic(model)`, ex.: `openrouter/free`) valem em chaves cujo catálogo respondeu **ou** cuja validação (`/key`) passou.

### 11.6 Latência

EWMA (α = 0,2) de TTFB e de tempo total por (chave, modelo); alimenta a estratégia `latency` e os relatórios. A sonda L0 registra também DNS/connect/TLS por host (diagnóstico de "por que a primeira chamada foi lenta").

### 11.7 Agenda e limites do próprio aquecedor

- Um agendador (heap de próximos horários) + *worker pool* limitado (4). Nunca duas sondas simultâneas na mesma chave. Jitter ± 10–20 %.
- *Backoff* em falhas e respeito a `Retry-After`.
- Interruptores: global, por provedor, por camada. `keypoolctl warm run [--provider X] [--key L]` força uma rodada.
- Toda sonda grava em `health_checks` (retenção 7 dias) e em `usage_events` com `source='warmup'`.

### 11.8 Prontidão

`GET /readyz` só devolve 200 depois da primeira passada L1 em todos os provedores (ou após `ready_timeout`, padrão 20 s, com aviso). `GET /admin/v1/stats` mostra por chave: `status`, `last_ok_at`, catálogo, latência EWMA, conexões quentes, hints atuais e "idade" de cada sonda.


**Nota de recuperação implementada na F6:** o snapshotter preserva janelas de uso/cota e cooldowns, mas, ao iniciar um novo processo, invalida saúde, validação, catálogo e EWMA derivados da credencial. Cada chave elegível é submetida à L1 usando a credencial atualmente carregada; o cache persistido de modelos é invalidado até essa verificação. Em reload dentro do mesmo processo, o estado continua sendo preservado, exceto quando a credencial muda. Se L1 estiver explicitamente desabilitada, o catálogo é esquecido sem transformar essa ausência em um catálogo vazio que bloqueie os modelos.

---

## 12. Superfície HTTP

Escuta em `127.0.0.1:8787` (configurável). **Nunca** em `0.0.0.0` por padrão. Todas as rotas, exceto `/healthz`, exigem `Authorization: Bearer kp_...` (ou `x-api-key: kp_...`).

### 12.1 Gateway (usado pelos projetos)

| Método e caminho | Função |
|---|---|
| `POST /v1/chat/completions` | Chat compatível com OpenAI (JSON ou SSE). `model` = alias da rota ou `provider:model` (se permitido) |
| `GET /v1/models` | Lista os aliases do projeto (no formato `{"object":"list","data":[{"id":"prover",...}]}`) |
| `GET /v1/capacity?alias=prover` | Vias/paralelismo disponíveis (9.8). Sem `alias`: todos |
| `POST /v1/leases` | **Modo lease** (só se `allow_lease`): devolve a chave para o projeto chamar o provedor direto |
| `POST /v1/leases/{id}/report` | Relata o desfecho do lease (`success`, `rate_limited`, `quota`, `auth`, `transient`, `cancel`) |
| `ANY /p/{provider}/{path...}` | Passagem crua com injeção de chave (provedores `generic_http`) |

**Cabeçalhos de entrada:** `X-Keypool-Project` (obrigatório se não houver `default_project`), `X-Keypool-Priority`, `X-Keypool-Timeout-Ms`, `X-Keypool-Exclude`, `X-Keypool-Avoid-Weights`.

**Cabeçalhos de saída** (em todas as respostas do gateway): `X-Keypool-Request-Id`, `X-Keypool-Target` (`groq:openai/gpt-oss-120b`), `X-Keypool-Key` (rótulo, **nunca** o segredo), `X-Keypool-Attempts`, `X-Keypool-Queue-Ms`, `X-Keypool-Fallback: true|false`. Em streaming, os cabeçalhos saem junto com o primeiro byte.

**Erros** (formato OpenAI, com campos extras):

```json
{ "error": { "type": "keypool_capacity_exhausted", "message": "...", "code": "capacity_exhausted",
             "retry_after_s": 540, "targets": [ {"target": "groq:openai/gpt-oss-120b", "reason": "tpd_exhausted", "retry_after_s": 540} ] } }
```

| HTTP | `error.type` | Quando |
|---|---|---|
| 400 | `invalid_request_error` | corpo inválido, `prompt_too_large_for_tpm` em todos os alvos, `X-Keypool-*` malformado |
| 401 | `keypool_auth_error` | chave `kp_` ausente/errada (resposta idêntica nos dois casos) |
| 403 | `keypool_project_forbidden` | projeto desconhecido/desabilitado, alvo literal não permitido, lease não permitido |
| 404 | `keypool_unknown_route` | alias inexistente |
| 429 | `keypool_capacity_exhausted` | toda a cadeia sem capacidade; traz `Retry-After` (segundos) |
| 502 | `keypool_upstream_error` | todos os alvos falharam por erro do provedor (corpo redigido) |
| 503 | `keypool_no_target` / `keypool_no_keys` / `keypool_starting` | cadeia vazia, nenhuma chave permitida, serviço ainda aquecendo |

### 12.2 Lease (opcional)

```json
// POST /v1/leases
{ "target": "openrouter:openrouter/free", "est_tokens": 1500, "priority": 2, "timeout_ms": 30000, "ttl_s": 120 }
// 200
{ "lease_id": "ls_9f2c...", "provider": "openrouter", "base_url": "https://openrouter.ai/api/v1",
  "auth_header": "Authorization", "secret": "sk-or-v1-...", "key_label": "or-conta-b-2", "expires_at": "2026-11-09T18:20:11Z" }
// POST /v1/leases/ls_9f2c.../report
{ "outcome": "success", "tokens": 1432, "rate_headers": { "retry-after": "30" } }
```

O segredo só sai nesse endpoint, só para projetos com `allow_lease`, e o evento é auditado. O `ttl_s` expirado conclui o lease com a estimativa.

### 12.3 Operação

| Caminho | Função |
|---|---|
| `GET /healthz` | vivo (sem autenticação, sem dados) |
| `GET /readyz` | pronto (após aquecimento inicial; 503 antes) |
| `GET /metrics` | texto Prometheus (exige chave) |
| `GET /debug/pprof/*` | só com `--pprof` e somente em loopback |

### 12.4 API administrativa (`/admin/v1`, mesma chave `kp_`)

| Recurso | Operações |
|---|---|
| `providers` | `GET`, `PUT /{name}` (cria/atualiza spec), `DELETE`, `POST /{name}/enable|disable` |
| `keys` | `POST` (corpo contém o segredo; resposta só com máscara), `GET`, `PATCH /{id}` (label, grupo, tags, enabled), `DELETE`, `POST /{id}/test` (força L1), `POST /{id}/enable|disable` |
| `projects` | `GET`, `PUT /{name}`, `DELETE`; `PUT /{name}/key-rules`; `PUT /{name}/routes/{alias}` |
| `apply` | `POST /admin/v1/apply` — aplica um documento YAML/JSON declarativo (provedores, projetos, rotas, regras; **sem segredos**) de forma atômica |
| `stats` | `GET /admin/v1/stats` (por chave/provedor/projeto), `GET /admin/v1/usage?from=&to=&project=&group_by=` |
| `events` | `GET /admin/v1/events?kind=&since=` |
| `warm` | `POST /admin/v1/warm/run`, `GET /admin/v1/warm/status` |
| `apikey` | `POST /admin/v1/apikey/rotate` (devolve a nova chave **uma vez**) |

Nenhuma rota devolve segredo de provedor. `keypoolctl key reveal` existe só como comando **local** que lê o banco e decifra com a master key (não passa pela API).

---

## 13. CLI

`keypoold` (daemon) e `keypoolctl` (cliente HTTP da API admin; lê `KEYPOOL_URL` e `KEYPOOL_API_KEY`).

```
keypoold init                # gera master key + API key kp_ (mostrada UMA vez) e grava o hash; exige banco migrado
keypoold migrate             # aplica migrações
keypoold serve [--config F] [--pprof]
keypoold doctor              # checa banco, migrações, master key, permissões do arquivo, porta, relógio, versão do Go
keypoold version

keypoolctl status                                   # painel: provedores, chaves (status/saúde/folga), filas, aquecimento
keypoolctl provider list|show|add -f arquivo.yaml|enable|disable
keypoolctl key add   --provider groq --label groq-a-1 [--group conta-a] [--tag mathlab] [--stdin]
keypoolctl key import-env .env --prefix GROQ --provider groq [--group conta-a] [--tag t]   # migra do .env do mathlab
keypoolctl key list [--provider P]  |  key test <label>  |  key enable|disable <label>  |  key tag <label> +a -b  |  key rm <label>
keypoolctl key reveal <label>                       # LOCAL: lê o banco e decifra (pede confirmação)
keypoolctl project apply -f mathlab.project.yaml    # perfil do projeto (rotas, regras de chaves, política)
keypoolctl project list|show <nome>
keypoolctl apikey info|rotate
keypoolctl usage [--project P] [--since 24h] [--group-by key|model|project]
keypoolctl events [--since 1h] [--kind key.invalid]
keypoolctl warm run|status
keypoolctl capacity --project mathlab [--alias prover]
```

Regras: o segredo entra **só** por `--stdin`, prompt oculto ou `import-env`; nunca como argumento de linha de comando (aparece no histórico/lista de processos). Saída tabular legível; `--json` em tudo.

---

## 14. Configuração

### 14.1 Bootstrap (`%LOCALAPPDATA%\keypool\keypool.yaml`, variáveis `KEYPOOL_*` vencem)

```yaml
listen: 127.0.0.1:8787
data_dir: ""                       # padrão: %LOCALAPPDATA%\keypool (Windows) | $XDG_CONFIG_HOME/keypool | ~/.keypool
database_url: "postgres://keypool@127.0.0.1:5432/keypool?sslmode=disable"   # preferir KEYPOOL_DATABASE_URL
master_key_file: ""                # padrão: <data_dir>/master.key (32 bytes em base64)
default_project: ""                # vazio = cabeçalho X-Keypool-Project obrigatório
limits:
  max_body_bytes: 8388608
  max_inflight: 256                # concorrência global do gateway
log: { level: info, format: text, file: "" }
usage: { retention_days: 30, queue_size: 8192 }
warm:  { ready_timeout: 20s }
```

### 14.2 Perfil do projeto (documento declarativo, sem segredos) — exemplo do mathlab

```yaml
project: mathlab
description: Provador de teoremas (Lean)
allow_lease: false
allow_literal_targets: false
policy:
  spill_on_saturation: true
  tpd_reserve: 0.25                # fração do TPD diário guardada para prioridades <= cutoff
  reserve_priority_cutoff: 1
  max_wait: 10m
  max_inflight: 12                 # concorrência do projeto
keys:                              # opt-in: o que não está aqui o projeto NÃO usa
  groq:       { include: { tags_any: [mathlab] } }
  openrouter: { include: { tags_any: [mathlab] } }
routes:                            # espelha configs/models.yaml (roles)
  formalizer: { priority: 0, defaults: {temperature: 0.3, max_tokens: 5500, reasoning_effort: medium},
                chain: ["groq:openai/gpt-oss-120b", "groq:openai/gpt-oss-20b", "openrouter:openrouter/free"] }
  critic:     { priority: 1, defaults: {temperature: 0.2, max_tokens: 4096, reasoning_effort: high},
                requires: { reasoning_effort: true }, chain: ["groq:openai/gpt-oss-120b"] }
  prover:     { priority: 2, defaults: {temperature: 0.7, max_tokens: 7000, reasoning_effort: medium},
                chain: ["groq:openai/gpt-oss-120b", "groq:openai/gpt-oss-20b", "openrouter:openrouter/free"] }
  planner:    { priority: 3, defaults: {temperature: 0.6, max_tokens: 4096, reasoning_effort: medium},
                chain: ["groq:openai/gpt-oss-120b", "groq:openai/gpt-oss-20b", "openrouter:openrouter/free", "groq:qwen/qwen3.8-27b"] }
  explorer:   { priority: 3, defaults: {temperature: 0.9, max_tokens: 4096, reasoning_effort: medium},
                chain: ["groq:openai/gpt-oss-120b", "groq:openai/gpt-oss-20b", "openrouter:openrouter/free", "groq:qwen/qwen3.8-27b"] }
  summarizer: { priority: 4, defaults: {temperature: 0.2, max_tokens: 2048, reasoning_effort: low},
                chain: ["groq:openai/gpt-oss-20b", "groq:openai/gpt-oss-120b", "openrouter:openrouter/free", "groq:qwen/qwen3.8-27b"] }
```

Validações do `apply`: provedor existe; modelo não vazio; prioridade 0–9; `include` aponta para ≥ 1 chave existente (aviso se 0); aliases únicos; nenhum campo desconhecido (`KnownFields(true)`); o documento inteiro é aplicado numa transação e gera **uma** nova versão de snapshot.

---

## 15. Segurança

**Modelo de ameaça (uso pessoal, uma máquina):** protege contra vazamento de chaves por dump/backup do banco, por logs, por mensagens de erro, por histórico de comandos e por um projeto com bug. **Não** protege contra malware com os mesmos privilégios do seu usuário Windows.

| Item | Regra |
|---|---|
| API key do KeyPool | `kp_` + 32 bytes aleatórios (`crypto/rand`) em base32 sem padding; exibida **uma vez** em `keypoold init`/`apikey rotate`; guardada só como SHA-256; comparação com `subtle.ConstantTimeCompare`; resposta 401 idêntica para ausente/errada; limitar tentativas falhas por IP (balde simples) |
| Rede | escuta em loopback; sem CORS; `ReadHeaderTimeout`, limite de corpo (8 MiB), sem `WriteTimeout` global (stream) |
| Chaves dos provedores | AES-256-GCM, AAD = `id da chave ‖ nome do provedor`; nonce aleatório de 12 bytes por gravação; versão no primeiro byte para migração futura; `master_key_check` valida a master key na partida |
| Master key | 32 bytes em arquivo (`master.key`) criado por `keypoold init`; ACL só do usuário (script `.ps1` usa `icacls`); alternativa `KEYPOOL_MASTER_KEY`. DPAPI opcional (fase 8). Rotação: `keypoolctl master rotate` (recifra tudo numa transação) — backlog |
| Tipo `secret.Secret` | `String()`/`GoString()`/`MarshalJSON`/`MarshalText`/`LogValue` devolvem a máscara; `Reveal()` só no transporte do adaptador e no `key reveal` |
| Logs/erros | nunca registrar `Authorization`, corpo da requisição, prompts ou respostas (padrão); mensagens do provedor passam por `Redact`; `slog` com `ReplaceAttr` que mascara chaves de nome `authorization|api_key|secret|token` |
| Conteúdo | prompts e respostas **não** são gravados. `log_bodies` não existe na v1 |
| Chave no cliente HTTP | o cabeçalho de autenticação é montado em `BuildRequest`; falhas de rede passam por `Redact` (URLs com `?key=` também) |
| Lease | desligado por padrão; auditado em `events` |
| Backup | documentar: backup do banco **sem** a master key não expõe as chaves; guardar a master key à parte |

---

## 16. Observabilidade

- **Logs** (`slog`): uma linha por requisição (`request_id, project, alias, target, key_label, attempts, queue_ms, ttfb_ms, status, outcome`), mais eventos de saúde. Nunca conteúdo.
- **Métricas** (`/metrics`, texto Prometheus escrito à mão): `keypool_requests_total{project,provider,model,outcome}`, `keypool_attempts_total`, `keypool_queue_seconds` (histograma), `keypool_upstream_ttfb_seconds` (histograma), `keypool_key_state{provider,key,state}`, `keypool_window_used_ratio{provider,key,bucket,dim}`, `keypool_cooldown_seconds{...}`, `keypool_warm_age_seconds{key,probe}`, `keypool_usage_dropped_total`, `keypool_leases_leaked_total`.
- **Estatísticas administrativas**: `keypoolctl status` (tabela viva) e `GET /admin/v1/stats`.
- **Eventos** persistidos (`events`): chave inválida, fusão de escopo, circuito aberto/fechado, config aplicada, falha de sonda, lease vazado.
- **Rastreamento de uma chamada lenta**: cabeçalhos `X-Keypool-Queue-Ms`/`X-Keypool-Attempts` e o log por requisição explicam onde o tempo foi.

---

## 17. Desempenho (metas a **verificar com benchmarks**)

Princípios do caminho quente: sem banco; configuração por `atomic.Pointer[Snapshot]` imutável; um mutex por `Pool` (fragmentar por bucket só se o benchmark mostrar contenção); `sync.Pool` para buffers; repasse do corpo com `json.RawMessage` (não remarshalar mensagens); log de uso por canal limitado que **descarta** em vez de bloquear; transporte HTTP por provedor, reaproveitado.

| Métrica | Meta inicial |
|---|---|
| `Pool.TryAcquire` + `Success` (10 chaves, janelas com 1 000 entradas) | < 5 µs, ≤ 2 alocações |
| Sobrecarga do gateway por requisição (sem contar upstream), não-stream | p99 < 2 ms em máquina comum |
| Sobrecarga por chunk em streaming | < 50 µs, sem alocação por chunk |
| Memória em repouso | < 100 MB com 20 chaves |
| 429 evitáveis (provedor devolveu 429 que o KeyPool poderia ter previsto) | tendência a zero depois dos *hints* |
| Primeira chamada após 10 min ocioso vs mediana | diferença < 50 ms (conexão quente) |

Benchmarks obrigatórios: `BenchmarkAcquireSuccess`, `BenchmarkAcquireContended` (`b.RunParallel`), `BenchmarkWindowAdd`/`WaitForRoom`, `BenchmarkGatewayPassthrough` (com `fakeupstream`), `BenchmarkSSEForward`. `--pprof` para perfil em uso real.

---

## 18. Estratégia de testes (o agente **não** tem chaves nem rede de provedores)

Todo comportamento tem teste **offline**:

- **`clock.Fake`** em tudo que depende de tempo; nenhum `time.Sleep` real em teste do núcleo.
- **`testutil/fakeupstream`**: `httptest.Server` programável por script — resposta por chave (cabeçalho `Authorization`), sequência de status/cabeçalhos/corpos/atrasos, streaming em pedaços, queda no meio do stream, 200 com `error` no corpo, `Retry-After` em vários formatos, cabeçalhos `x-ratelimit-*` no estilo Groq e `X-RateLimit-Reset` no estilo OpenRouter.
- **Tabelas** para classificação (status × corpo × cabeçalhos → `Outcome`), parsing de duração/epoch, resolução de limites, seletores de chaves.
- **Modelo de referência** para janelas: teste aleatório com semente fixa comparando `sliding` com uma implementação ingênua (lista + soma).
- **Fuzz** (`testing.F`): parser de durações/cabeçalhos, parser SSE, `from_env`, `Redact`.
- **Corrida**: `go test -race` em `sched`, `router`, `gateway`, `warm` (no Windows exige `CGO_ENABLED=1` e um compilador C; `scripts/windows/test.ps1 -Race` é opcional).
- **Integração PostgreSQL** (build tag `integration`, `KEYPOOL_TEST_DATABASE_URL`): cada teste cria um schema temporário (`CREATE SCHEMA t_<rand>` + `search_path`) e o remove no fim. Cobrem migrações, criptografia ida-e-volta, snapshot/restore, escritor de uso em lote, trava consultiva.
- **Ponta a ponta sem banco**: `memstore` + `fakeupstream` + gateway real em `httptest` — cenários: sucesso, 429 curto troca de chave, 429 longo esgota bucket, 402 free/pago, 401 desativa, 404 mata alvo, 5xx com retry, spill, prioridade na fila, stream OK, erro no 1º evento do stream com fallback, cancelamento do cliente, lease vazado.
- **Verificação por parte do usuário**: `docs/VERIFY.md` lista, em ordem, os comandos (`go vet`, `go test ./...`, `go test -tags integration ./...`, benchmarks) e o resultado esperado.

---

## 19. Windows (alvo principal)

- Requisitos: Windows 10/11, PowerShell 5.1+, Go 1.26+ (`winget install GoLang.Go`), PostgreSQL ≥ 16 instalado nativamente (instalador oficial; `winget search PostgreSQL` mostra o ID atual). **Sem Docker, WSL ou Make.**
- Scripts em `scripts\windows\*.ps1`, **somente ASCII** (inclusive comentários; PowerShell 5.1 lê UTF-8 sem BOM como ANSI): `build.ps1`, `test.ps1 [-Race] [-Integration]`, `setup-db.ps1` (cria papel e banco `keypool` de forma idempotente via `psql`; senha lida com `Read-Host -AsSecureString`, repassada por `PGPASSWORD` só no processo filho), `run-dev.ps1`, `install-task.ps1` (Agendador de Tarefas "ao fazer logon", oculto, sem admin), `acl-master-key.ps1` (`icacls` só para o usuário).
- Caminho curto para o repositório (`C:\keypool`) e binários em `bin\keypoold.exe`, `bin\keypoolctl.exe`.
- Dados: `%LOCALAPPDATA%\keypool\` (`keypool.yaml`, `master.key`, logs). Parada limpa em `Ctrl+C`/`os.Interrupt` (salva snapshot, drena escritor de uso, fecha o servidor com `Shutdown`).
- Serviço do Windows nativo (`golang.org/x/sys/windows/svc`) fica como opcional da fase 8; o Agendador de Tarefas basta na v1.

---

## 20. Fases e marcos

Cada fase termina compilável, com testes da fase e `STATUS.md` atualizado. **Critérios de aceite** são verificáveis offline.

| Fase | Entrega | Aceite |
|---|---|---|
| **F0 — Fundação** | `go.mod`, estrutura, `domain`, `clock` (Real/Fake), `secret` (Secret, Mask, Redact), `README`, `AGENTS.md`, `docs/VERIFY.md`, scripts `.ps1` | `go vet ./...` limpo; testes de `secret` e `clock`; `Secret` nunca imprime o segredo em `%v`, `%+v`, `%#v`, `json.Marshal`, `slog` |
| **F1 — Núcleo** | `sliding`, `limits`, `sched` completo (escopos, buckets, TPM/TPD/RPM/RPD/CONC, prioridade, reserva, cooldown, cota, fusão, hints, saúde, estratégias, snapshot/restore, `from_env`) | Todos os testes da seção 8.5 + novos; `-race` limpo; benchmarks rodam |
| **F2 — Provedores** | `classify`, `sse`, `provider` (Spec, presets groq/openrouter, `openaicompat`, `generic`), `testutil/fakeupstream` | Tabela de classificação passa; SSE com `usage` final e erro no 1º evento; `retry_after` em todos os formatos; carga de preset válida |
| **F3 — Gateway e roteador** (modo dev, `memstore` + YAML) | `config.Snapshot`, `router`, `gateway` (`/v1/chat/completions`, `/v1/models`, `/v1/capacity`, auth, erros, cabeçalhos), `keypoold serve --dev configs/examples/keypool.dev.yaml` (chaves lidas de variáveis de ambiente) | Cenários ponta a ponta da seção 18 passam; `go build ./cmd/keypoold` |
| **F4 — PostgreSQL** | migrações, `cryptobox`, `pgstore`, `keypoold init/migrate/serve/doctor`, snapshotter, escritor de uso, trava consultiva | Testes de integração (com `KEYPOOL_TEST_DATABASE_URL`) passam; ida-e-volta de cifra; restore de janelas |
| **F5 — Admin e CLI** | API admin, `apply`, `keypoolctl` (incluindo `key import-env`, `status`, `usage`) | `apply` atômico com validações; nenhuma resposta contém segredo (teste que varre JSON); `import-env` reproduz os testes de `from_env` |
| **F6 — Aquecimento e saúde** | `warm` (L0/L1/L2, agenda, jitter, circuit breaker, catálogo, hints ao vivo, readiness) | Testes com `clock.Fake` + `fakeupstream`: chave inválida detectada na partida; circuito abre/fecha; sonda só em chave ociosa; L2 desligada por padrão e respeita orçamento |
| **F7 — Extras de eficiência** | modo lease, passthrough `generic_http`, estratégias `headroom`/`latency`, calibração da estimativa de tokens, `max_inflight`/`daily_token_budget` por projeto | Implementado no código; build/PG integrado pendentes |
| **F8 — Acabamento** | `/metrics`, painel HTML opcional somente leitura, tarefa agendada, DPAPI opcional, documentação final | Implementado no código; validação nativa Windows/PG pendente; checklist em VERIFY |
| **F9 — Integração com o mathlab** (fora deste repositório) | provedor `keypool` no mathlab (seção 21) | Integração aplicada no código dos dois repositórios; testes offline aprovados; aceitação E2E com KeyPool/PostgreSQL/upstream reais pendente |

---

## 21. Integração com o mathlab

> Atualização da entrega F9: a integração de código foi aplicada nos dois repositórios. O Mathlab direciona os seis papéis a aliases KeyPool, usa uma única `KEYPOOL_API_KEY`, envia headers de política, consulta capacidade remota e oferece streaming SSE opt-in; os módulos locais antigos permanecem para rollback. Os testes offline passaram. A aceitação ponta a ponta com PostgreSQL, credenciais, provedor upstream e cancelamento real continua pendente. Consulte `docs/INTEGRACAO_MATHLAB.md`.


O Mathlab recebido já foi atualizado para usar `OpenAICompatibleProvider`, aliases, headers de política e capacidade remota. O conteúdo abaixo registra o contrato e o procedimento de migração original para referência/rollback; não é uma lista de alterações de código ainda pendentes. O Mathlab já sabe falar com provedor compatível com OpenAI (`OpenAICompatibleProvider`: `base_url`, `headers`, chave por variável de ambiente). O **caminho de configuração aplicado** é:

1. `configs/providers.yaml` do mathlab ganha:
   ```yaml
   providers:
     keypool:
       type: openai_compatible
       env_prefix: KEYPOOL            # KEYPOOL_API_KEY=kp_...   (uma única chave cliente do MathLab)
       base_url: http://127.0.0.1:8787/v1
       timeout_s: 600
       headers: { X-Keypool-Project: mathlab }
       limit_scope: provider
       limits: {}                     # sem limites locais: quem limita é o KeyPool
   default_provider: keypool
   ```
2. `configs/models.yaml`: cada papel vira `model: prover`, sem `fallbacks` (a cadeia agora vive na rota do projeto no KeyPool). `priority`, `temperature`, `max_tokens`, `extra` permanecem como padrões do papel no mathlab **ou** migram para `defaults` da rota (escolher uma fonte só; recomendado: no KeyPool).
3. Cabeçalhos por chamada: `X-Keypool-Priority`, `X-Keypool-Exclude` (substitui `exclude_models`), `X-Keypool-Avoid-Weights` (substitui `avoid_weights`).
4. 429 do KeyPool traz `Retry-After`; o `LLMClient` do mathlab já trata `RateLimited(retry_after)`. O `validate_model_availability` consulta `GET /v1/models` (aliases).
5. `capacity.py`/`resolve_parallelism` passa a chamar `GET /v1/capacity?alias=...` para obter as vias.
6. `.env` do mathlab deixa de ter `GROQ_API_KEYS`/`OPENROUTER_API_KEYS`; elas são importadas **uma vez**: `keypoolctl key import-env .env --prefix GROQ --provider groq --group <conta> --tag mathlab`.
7. Remover do mathlab (depois de estável): `key_pool.py`, `rate_limiter.py`, a lógica de spill/espera/`_exhausted` em `client.py` (mantendo apenas a chamada HTTP e o tratamento de `RateLimited` final), `diagnostics.ping_all` (vira `keypoolctl status`).

---

## 22. Riscos e mitigação

| Risco | Mitigação |
|---|---|
| Agente de IA não consegue compilar nem testar | Dependências mínimas; `go.sum` gerado pelo usuário com `go mod tidy`; `VERIFY.md`; testes offline completos; checklist de "compilar de cabeça" no prompt; fases pequenas |
| Limites dos provedores mudam | Presets são **dados** versionados e comentados com data; `rate_headers` leem a verdade em tempo de execução; `keypoolctl provider show` mostra a origem do número |
| Contabilidade local diverge do provedor | *Hints* corrigem; estimativa calibrada; margem de segurança de TPM |
| Estimativa de tokens errada | Reserva conservadora + ajuste por `usage` real + fator por modelo |
| Sondas gastam cota | L0/L1 sem tokens por padrão; L2 desligada, com orçamento e proibida em buckets `free` |
| Perda da master key | Documentar backup; sem ela, reimportar as chaves (`key add`/`import-env`) |
| Banco indisponível | O serviço continua operando com o snapshot em memória; escrita de uso/snapshot acumula e tenta de novo; `/readyz` reporta degradado; partida a frio exige banco |
| Vazamento em log | Tipo `Secret`, `ReplaceAttr`, `Redact`, teste que varre logs e respostas procurando padrões de chave |
| Complexidade do `Acquire` | Porta o Python com testes de paridade (8.5); revisão do documento de decisões; benchmarks |
| Windows sem `-race` | Rodar `go test -race` em outra máquina/CI futura; manter os testes de concorrência determinísticos |

---

## 23. Valores padrão adotados (podem ser trocados em `DECISIONS.md`)

| Tema | Padrão |
|---|---|
| Porta | `127.0.0.1:8787` |
| Idioma | Código e identificadores em inglês; comentários, mensagens de CLI e documentação em português (pt-BR); scripts `.ps1` em ASCII |
| Projeto ausente no cabeçalho | `400` (a menos que `default_project` esteja definido) |
| Prioridade (0–9) | menor = mais urgente; padrão da rota 5 |
| `max_attempts` por alvo | 8 |
| `LONG_RATE_LIMIT` | 120 s |
| `MAX_WAIT_ROUNDS` | 4; `WAKE_STAGGER` 0,4 s × prioridade |
| TPM safety margin | 100 tokens; `MIN_OUTPUT_TOKENS` 1024 |
| `quota_cooldown` | 3600 s |
| TTL de alvo morto | 30 min |
| Retenção | uso 30 d (com rollup), saúde 7 d, eventos 90 d |
| Lease TTL | 15 min (guarda de vazamento) / 120 s (modo lease) |
| Snapshot de janelas | a cada 10 s se houver mudança |

---

## Apêndice A — Mapa Python → Go

| Arquivo do mathlab | Ação | Destino Go |
|---|---|---|
| `src/mathlab/llm/key_pool.py` | **portar** (especificação principal) | `internal/sched` |
| `src/mathlab/llm/rate_limiter.py` (`SlidingWindow`) | portar e melhorar | `internal/sliding` |
| `src/mathlab/config.py` → `ProviderLimits`, `LimitRule`, `ModelLimitValues`, `resolve()` | portar com structs tipadas | `internal/limits` |
| `src/mathlab/config.py` → `ProviderConfig`, `RoutingConfig` | reinterpretar como `provider.Spec` + `project.policy` | `internal/provider`, `internal/config` |
| `src/mathlab/llm/client.py` → `complete`, `_pick`, `_run_target`, `_fit_output_budget`, `_reserve_for`, `_soonest_wait`, `_targets`, `_without`, `_prefer_other_weights`, `validate_model_availability`, `build_pools` | portar | `internal/router`, `internal/warm` (catálogo) |
| `src/mathlab/llm/providers/openai_compatible.py` (`_raise_status`, `_raise_error_object`, `retry_after_from_headers`, `adapt_payload`, `list_models`, `ping`) | portar | `internal/classify`, `internal/provider/openaicompat` |
| `src/mathlab/llm/providers/groq.py` | só como referência de erros/ping (o Go usa HTTP puro) | preset `groq.yaml` |
| `src/mathlab/llm/providers/openrouter.py` (`/key`, `openrouter/free`, cabeçalhos extras) | portar | preset `openrouter.yaml` + `IsDynamic` |
| `src/mathlab/llm/capacity.py` (`compute_capacity`, `lanes`) | portar | `internal/router` (`/v1/capacity`) |
| `src/mathlab/llm/diagnostics.py`, `scripts/check_keys.py` | substituir | `keypoolctl status`, `warm` |
| `src/mathlab/utils/text.py` (`mask_secret`, `redact_secrets`, `SECRET_PATTERNS`, `estimate_tokens`) | portar | `internal/secret`, `internal/router` |
| `tests/test_key_pool.py`, `tests/conftest.py` (`FakeClock`) | traduzir | testes de `sched`, `clock.Fake` |
| `configs/providers.yaml`, `configs/models.yaml` | converter | presets e `configs/examples/mathlab.project.yaml` |
| `docs/PROVIDERS.md`, `docs/GROQ_KEYS.md` | consultar | `docs/PROVIDERS.md` do KeyPool |

## Apêndice B — Fatos dos provedores usados neste plano (conferir de tempos em tempos)

**Groq** (documentação oficial de limites): toda resposta traz `x-ratelimit-limit-requests` e `x-ratelimit-remaining-requests` (**sempre RPD**), `x-ratelimit-limit-tokens` e `x-ratelimit-remaining-tokens` (**sempre TPM**), `x-ratelimit-reset-requests` (ex.: `2m59.56s`) e `x-ratelimit-reset-tokens` (ex.: `7.66s`); `retry-after` (segundos) só aparece em 429. Os limites valem por **organização**. Algumas organizações têm também ITPM/OTPM (entrada/saída separados). Tokens em cache não contam.

**OpenRouter** (documentação oficial de limites): modelos `:free` têm 20 requisições/min e 50/dia (1 000/dia depois de comprar ao menos 10 créditos, de forma permanente); a capacidade é governada globalmente — chaves extras não aumentam o limite; saldo negativo pode gerar 402 até em modelos gratuitos; `GET /api/v1/key` devolve créditos/limites da chave (`limit_remaining`, `is_free_tier`, e, segundo fontes secundárias, `free_model_daily_requests`: tratar como **campo opcional**); 429 traz `X-RateLimit-*` no erro; erros de orçamento (403) não trazem `Retry-After`.

**Versões** (consultadas em 09/11/2026): Go 1.27 lançado em 19/08/2026 (1.26.x ainda suportado); `pgx/v5` na série 5.9.x/5.10.x — o `go.mod` exige `v5.9.2` no mínimo e o usuário roda `go get -u ./... && go mod tidy`.
