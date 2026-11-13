# KeyPool — documentação consolidada

> Documento único e incremental do projeto. Esta é a referência operacional e técnica vigente. Ao alterar o sistema, atualize a seção correspondente e acrescente uma entrada em **Histórico de alterações**. Não recrie documentos separados em `docs/` sem necessidade explícita.
>
> **Estado da documentação:** consolidada em 10/11/2026 a partir dos documentos anteriores de API, decisões, estado local, auditoria, operação Windows, plano, provedores, segurança e verificação. Informações históricas incompatíveis com a implementação atual foram descartadas ou identificadas como históricas.

## 1. Visão geral

KeyPool é um gateway local em Go que centraliza credenciais de provedores de IA e expõe uma API compatível com OpenAI. O daemon executa em um único modo (`keypoold serve`), escuta somente em loopback e mantém o estado operacional em memória. Um único arquivo `state.json` preserva os dados necessários à correção após reinício.

Requisitos: Go 1.26 ou superior; somente biblioteca padrão; nenhum banco ou módulo Go de terceiros. Configurações, perfis de projeto, presets e operações `apply` usam JSON estrito. Não há suporte operacional a PostgreSQL, YAML, `--dev`, `init` ou `migrate`.

Componentes principais:

- `cmd/keypoold`: daemon, bootstrap e diagnóstico (`serve`, `doctor`, `version`).
- `cmd/keypoolctl`: CLI administrativa e operações locais, incluindo `key reveal`.
- `internal/router`, `internal/gateway`, `internal/sched`: roteamento, HTTP, limites, seleção e leases.
- `internal/store/memstore`: estado em memória, checkpoint e histórico limitado.
- `internal/provider`, `internal/classify`, `internal/sse`, `internal/warm`: adaptadores, classificação, streaming e aquecimento.

## 2. Execução e configuração

### Primeira execução

1. Prepare um arquivo JSON, normalmente `configs/examples/keypool.json`, e os perfis de projeto referenciados por ele.
2. Defina as variáveis de credenciais dos provedores (`<PREFIXO>_API_KEYS`, `<PREFIXO>_API_KEY` ou `<PREFIXO>_API_KEY_N`). Opcionalmente, defina `KEYPOOL_API_KEY` antes do primeiro boot.
3. Execute em terminal visível:

```powershell
go run ./cmd/keypoold serve --config ./configs/examples/keypool.json
```

4. Se `KEYPOOL_API_KEY` não foi fornecida, copie e armazene a chave `kp_...` impressa uma única vez.

O daemon prepara a configuração e o snapshot, obtém o listener TCP loopback e só então grava o estado inicial. Assim, uma porta ocupada não deixa um checkpoint parcialmente inicializado.

### Execuções seguintes

Defina `KEYPOOL_API_KEY` com a chave inicial ou rotacionada e execute `keypoold serve` sem `--config`. A chave é validada contra o hash persistido. A configuração inicial não é relida; alterações são aplicadas pela CLI/API administrativa. Variáveis de ambiente de provedores também não substituem automaticamente as credenciais já importadas; use `keypoolctl key import-env` para importar novas credenciais.

### Variáveis de ambiente

| Variável | Finalidade |
|---|---|
| `KEYPOOL_DATA_DIR` | Diretório de dados. |
| `KEYPOOL_API_KEY` | API key local; opcional no primeiro boot, obrigatória depois. |
| `KEYPOOL_MASTER_KEY` | Master key fornecida pelo ambiente. |
| `KEYPOOL_MASTER_KEY_FILE` | Caminho alternativo do arquivo da master key. |
| `KEYPOOL_URL` | URL do daemon usada por `keypoolctl`. |
| `<PREFIXO>_API_KEYS`, `<PREFIXO>_API_KEY`, `<PREFIXO>_API_KEY_N` | Credenciais importadas no bootstrap ou por `key import-env`. |

## 3. Configuração de provedores e projetos

Um provedor `openai_compatible` pode ser configurado sem código quando implementa o protocolo compatível. Exemplo mínimo:

```json
{
  "providers": {
    "meu-provedor": {
      "type": "openai_compatible",
      "enabled": true,
      "base_url": "https://api.example.invalid/v1",
      "auth": { "scheme": "bearer" },
      "paths": { "chat": "/chat/completions", "models": "/models" },
      "timeout": "90s",
      "env_prefix": "MEU_PROVEDOR",
      "key_strategy": "round_robin",
      "limits": { "default_tpm": 60000, "default_rpm": 60, "rules": [] },
      "keys": []
    }
  }
}
```

Para encaminhamento HTTP sem conversão de payload, use `type: "generic_http"`, configure `base_url`, autenticação, `env_prefix`, timeout e permita explicitamente o provedor no projeto. O cliente chama `/p/{provider}/{path...}`. Não envie segredos em query strings.

Exemplo de perfil de projeto:

```json
{
  "projects": {
    "meu-projeto": {
      "keys": {
        "meu-provedor": { "include": { "tags_any": ["meu-projeto"] } }
      },
      "routes": {
        "assistente": {
          "priority": 2,
          "chain": ["meu-provedor:modelo-principal"]
        }
      }
    }
  }
}
```

A configuração global não concede acesso automaticamente: o projeto precisa declarar os provedores/chaves permitidos e suas rotas. `rate_headers` pode mapear cabeçalhos upstream para `tpm`, `tpd`, `rpm` e `rpd`. Use limites conservadores e revise presets/cabeçalhos quando o fornecedor alterar seu contrato.

`project_file` é resolvido relativamente ao arquivo JSON que o declara. É usado no bootstrap, mas não é serializado em `state.json`.

## 4. Integração Google AI Studio (Gemini API)

O KeyPool integra a **Gemini API** com uma chave criada no Google AI Studio por meio do adaptador `openai_compatible` existente. Isso não integra a interface web do AI Studio, histórico ou prompts salvos e não é uma integração com Vertex AI.

Contrato OpenAI-compatible utilizado:

- Base URL: `https://generativelanguage.googleapis.com/v1beta/openai`
- `POST /chat/completions` e `GET /models`
- `Authorization: Bearer <GEMINI_API_KEY>`
- Preset genérico embutido: `gemini` (`internal/provider/presets/gemini.json`)

O preset usa timeout de 180s, sonda `models`, `limit_scope: provider` e rotação round-robin. Não configura limites numéricos, cabeçalhos de quota ou `stream_include_usage` sem confirmação específica. O campo `reasoning_effort` é encaminhado apenas para modelos `gemini-3.*`, conforme o preset; para outros modelos o adaptador o remove.

### Primeiro bootstrap

A entrada `gemini` já está no exemplo `configs/examples/keypool.json`. Defina `GEMINI_API_KEYS`, `GEMINI_API_KEY` ou `GEMINI_API_KEY_N` no ambiente do daemon antes do primeiro bootstrap. O exemplo usa `default_project: example`, então as chaves importadas pelo bootstrap recebem a tag `example`:

```powershell
$env:GEMINI_API_KEY = Read-Host 'Gemini API key'
go run ./cmd/keypoold serve --config ./configs/examples/keypool.json
```

### KeyPool já inicializado

Com o daemon ativo, cadastre o provedor e a chave pela CLI administrativa:

```powershell
go run ./cmd/keypoolctl provider add -f ./integrations/google-ai-studio/provider.json --name gemini
$env:GEMINI_API_KEY | go run ./cmd/keypoolctl key add --provider gemini --label gemini-main --tag example --stdin
go run ./cmd/keypoolctl key list --provider gemini
go run ./cmd/keypoolctl key test <ID_DA_CHAVE>
```

A chave é lida por stdin; nunca a passe como argumento, não a registre no repositório e não a envie ao cliente. `key test` consulta `/models`; selecione um ID retornado que também aceite chat completions.

### Perfil de projeto e cliente

O KeyPool não mantém perfis de integração para aplicações específicas. Cada repositório consumidor deve guardar e versionar o próprio arquivo JSON de projeto, escolher seu identificador e tags e aplicar seu perfil com `keypoolctl project apply -f <perfil-do-projeto.json>`. O arquivo `configs/examples/example.project.json` serve somente como exemplo genérico.

O cliente usa apenas a chave local `kp_...`, o header `X-Keypool-Project` correspondente ao identificador do projeto e um alias definido nesse perfil. A credencial Google permanece dentro do KeyPool.

### Quotas e limitações

As quotas upstream da Gemini API são aplicadas por projeto Google Cloud, não por chave individual. Várias chaves do mesmo projeto não multiplicam a quota. `limit_scope: provider` compartilha a contabilização local entre as chaves cadastradas nesse provedor; se houver projetos Google distintos, cadastre provedores distintos e configure os limites locais com os valores atuais exibidos no AI Studio. Não presuma que todo ID listado em `/models` aceite chat completions.

Esta integração cobre chat completions OpenAI-compatible e streaming SSE suportado pelo gateway. Não declara suporte a `generateContent` nativo, embeddings, imagem, áudio, vídeo, Live API, Batch ou a todos os parâmetros exclusivos Gemini. Referências: https://ai.google.dev/gemini-api/docs/openai, https://ai.google.dev/gemini-api/docs/models, https://ai.google.dev/gemini-api/docs/rate-limits e https://ai.google.dev/gemini-api/docs/api-key.

## 5. API HTTP e administração

O listener padrão é `127.0.0.1:8787`. Todas as rotas, exceto `GET /healthz`, exigem `Authorization: Bearer <KEYPOOL_API_KEY>` ou `X-API-Key`. O painel também aceita HTTP Basic com usuário literal `keypool` e a API key como senha. Não exponha a porta diretamente à internet.

### Gateway

| Método e rota | Autenticação/condição | Finalidade |
|---|---|---|
| `GET /healthz` | Pública | Liveness; não significa prontidão. |
| `GET /readyz` | API key | Prontidão após L1 inicial ou timeout configurado. |
| `GET /` | API key | Painel HTML local somente leitura. |
| `GET /metrics` | API key | Métricas Prometheus. |
| `POST /v1/chat/completions` | API key | Chat compatível com OpenAI, alias e fallback. |
| `GET /v1/models` | API key | Aliases/modelos autorizados ao projeto. |
| `GET /v1/capacity` | API key | Capacidade estimada para alias/projeto. |
| `POST /v1/leases` | API key + `allow_lease` | Reserva uma chave e devolve dados temporários da lease. |
| `POST /v1/leases/{id}/report` | API key + projeto proprietário | Reporta resultado, tokens e cabeçalhos; encerra lease. |
| `ANY /p/{provider}/{path...}` | API key + `generic_http` autorizado | Passthrough HTTP. |

Para selecionar projeto, envie `X-Keypool-Project`; se ausente, usa-se `default_project`. Cabeçalhos opcionais: `X-Keypool-Priority`, `X-Keypool-Timeout-Ms`, `X-Keypool-Queue-Timeout-Ms`, `X-Keypool-Exclude` e `X-Keypool-Avoid-Weights`. `X-Keypool-Timeout-Ms` limita a chamada inteira (espera na fila + geração upstream); `X-Keypool-Queue-Timeout-Ms` limita somente a espera por uma chave/capacidade e não encurta a geração. Se somente o orçamento de fila expirar, a API responde HTTP `429` com código `keypool_queue_timeout`; se o prazo total expirar, responde HTTP `408`. Respostas do gateway podem incluir `X-Keypool-Request-Id`, `X-Keypool-Target`, `X-Keypool-Key` (rótulo mascarado), `X-Keypool-Attempts`, `X-Keypool-Queue-Ms` e `X-Keypool-Fallback`.

### Administração e CLI

As rotas `/admin/v1/*` usam a mesma autenticação. Incluem `apply`, CRUD de provedores/projetos/regras/rotas/chaves, teste de chave, aquecimento, status, estatísticas, uso, eventos e rotação da API key. `apply` aceita JSON completo ou perfil de projeto. Nenhuma resposta administrativa revela segredos de provedores.

Exemplos de CLI:

```powershell
$env:KEYPOOL_URL = "http://127.0.0.1:8787"
$env:KEYPOOL_API_KEY = "kp_sua_chave"
go run ./cmd/keypoolctl status
go run ./cmd/keypoolctl provider list
go run ./cmd/keypoolctl key list
go run ./cmd/keypoolctl project apply -f ./configs/examples/example.project.json
go run ./cmd/keypoolctl usage --project example --since 24h
go run ./cmd/keypoolctl events --since 1h
```

Importe segredos por `keypoolctl key add --stdin` ou `keypoolctl key import-env`; não passe segredos como argumentos de processo. `keypoolctl key reveal <label>` é estritamente local: abre o checkpoint com a master key, pede confirmação e revela a credencial selecionada.

## 6. Estado local, persistência e recuperação

### Diretório e arquivos

`KEYPOOL_DATA_DIR` define o diretório. Padrões: `%LOCALAPPDATA%\keypool` no Windows, `$XDG_CONFIG_HOME/keypool` quando definido, ou `~/.keypool`.

- `master.key`: master key em base64 (ou blob DPAPI no Windows, quando selecionado).
- `state.json`: checkpoint versionado.
- `state.json.tmp`: arquivo temporário durante gravação atômica.

`memstore.Open` apenas lê; não cria nem altera arquivos. Na primeira execução, o daemon só inicializa o checkpoint depois de obter o listener.

### Conteúdo e protocolo de gravação

O checkpoint guarda configuração, credenciais cifradas com AES-256-GCM, hash SHA-256 e metadados da API key, snapshots das janelas de limites e contadores diários. Não guarda texto claro de credenciais nem da API key. Histórico detalhado de uso, eventos e saúde permanece em memória e se perde no reinício.

A gravação usa `state.json.tmp` → sync/fsync → rename. Mutação só é publicada na memória depois da gravação bem-sucedida; falha de I/O mantém o estado anterior. `commitMu` serializa mutações duráveis e o I/O do checkpoint. `mu` e `usageMu` não são mantidos durante I/O; o caminho quente e as leituras não adquirem `commitMu`.

### API key e master key

A API key é obrigatória depois do bootstrap e é comparada pelo hash persistido. A master key pode vir de `KEYPOOL_MASTER_KEY`, de `KEYPOOL_MASTER_KEY_FILE` ou do arquivo padrão. Em Unix, o arquivo é criado com permissão `0600`. No Windows, aplique ACL restritiva com `scripts/windows/acl-master-key.ps1`. A opção `--master-key-dpapi` usa o perfil do usuário Windows atual; DPAPI e `KEYPOOL_MASTER_KEY` são mutuamente exclusivos.

### Backup, restauração e reset

1. Pare o daemon.
2. Copie juntos `state.json` e a master key correspondente, protegendo o backup.
3. Restaure ambos no diretório configurado, defina `KEYPOOL_API_KEY` e execute `keypoold doctor` antes de iniciar.

Sem a master key, credenciais cifradas são irrecuperáveis. Com DPAPI, a restauração depende do mesmo contexto de usuário capaz de descriptografar o blob. Não há migração automática de instalações antigas; exporte credenciais usando a versão antiga antes de substituí-la.

Para zerar: pare o daemon, faça backup se necessário e remova `state.json`/`state.json.tmp`. Preserve `master.key` para reutilizá-la ou remova-a conscientemente para gerar outra no próximo bootstrap. O reset remove configuração, chaves cadastradas, API key, snapshots e contadores.

### Retenção do histórico

| Série | Limite por quantidade | Limite por idade | Persistência |
|---|---:|---:|---|
| Uso | 50.000 | 7 dias | Volátil; contadores diários são persistidos. |
| Eventos | 10.000 | 30 dias | Volátil. |
| Saúde | 1.000 | 7 dias | Volátil. |

`keypool_usage_dropped_total` conta eventos de uso descartados pelo limite de retenção em memória, não falhas de fila ou gravação em banco.

## 7. Segurança

- Mantenha o listener em loopback; não configure port-forward, firewall ou proxy público sem autenticação e TLS adequadas.
- Não versionar credenciais, colocá-las em argumentos de processo, tickets ou logs.
- Nunca registrar `Authorization`, chaves, prompts, completions ou corpos brutos.
- `Secret.Reveal` deve permanecer restrito aos pontos que realmente precisam montar uma chamada ao provedor ou executar `key reveal` local.
- O passthrough só alcança provedores configurados e autorizados; a autenticação recebida do cliente é removida/substituída pela credencial selecionada.
- Métricas usam labels operacionais; não devem conter segredos, conteúdo de prompt ou identificadores ilimitados.
- Em suspeita de vazamento, rotacione a API key, desabilite/remova a credencial afetada, revalide as chaves e preserve logs/eventos operacionais sem coletar corpos por padrão.

## 8. Operação no Windows

Requisitos: Windows 10/11 ou Windows Server suportado pelo Go usado no build; Go 1.26+; PowerShell 5.1+ e módulo `ScheduledTasks` para tarefa de logon.

```powershell
Set-ExecutionPolicy -Scope Process Bypass
.\scripts\windows\build.ps1
.\scripts\windows\test.ps1
.\scripts\windows\test.ps1 -Race
.\scripts\windows\acl-master-key.ps1
```

Para tarefa agendada, defina `KEYPOOL_API_KEY` e, se aplicável, `KEYPOOL_MASTER_KEY` no ambiente do usuário e execute:

```powershell
.\scripts\windows\install-task.ps1 -Executable .\bin\keypoold.exe -Arguments "serve"
```

A tarefa é criada para o usuário atual, em `AtLogOn`, nível `Limited`, `Hidden` e `IgnoreNew`, com tentativas limitadas de reinício. Faça o primeiro bootstrap em terminal visível para capturar a API key gerada; não dependa de uma tarefa oculta para isso. Remoção:

```powershell
.\scripts\windows\install-task.ps1 -TaskName KeyPool -Remove
```

O painel padrão fica em `http://127.0.0.1:8787/`. Use usuário `keypool` e a API key como senha. `/metrics` exige Bearer ou `X-API-Key`.

## 9. Verificação, testes e estado validado

Execute na raiz com Go 1.26+:

```powershell
gofmt -l .
go vet ./...
go build ./...
go test ./...
go test -race ./internal/sched/... ./internal/router/... ./internal/gateway/... ./internal/store/... ./cmd/...
$env:GOPROXY = "off"
go build ./...
go test ./...
go list -m all
grep -rn "yaml" --include=*.go .
```

`gofmt -l .` e a busca de YAML em arquivos Go devem ficar vazios. `go list -m all` deve listar somente `keypool`. O `go.mod` deve conter apenas `module keypool` e `go 1.26`; não deve existir `go.sum`. Os testes são offline e usam `httptest`, fake upstream, `clock.Fake` e diretórios temporários. Em Windows, execute também os scripts PowerShell e valide nativamente DPAPI, ACLs e Agendador de Tarefas.

### Evidência disponível da consolidação F10

- E2E offline cobre bootstrap, autenticação HTTP, fake upstream, bind exclusivo, criação administrativa de chave, reinício, restauração de contadores/snapshot e rejeição de API key incorreta.
- O teste de regressão confirmou que serializar strings como YAML e inserir o resultado em `json.RawMessage` podia gerar JSON inválido; `json.Marshal` corrigiu o problema.
- O ring de 50.000 eventos mediu 11,10 MiB, abaixo do orçamento aproximado de 25 MiB.
- Os testes Go, `go vet`, `go build` e `-race` passaram com modfile temporário externo usando Go 1.23.2, porque Go 1.26 não estava instalado e a tentativa de download falhou por indisponibilidade de DNS/rede. Isso não substitui validação oficial em Go 1.26.
- Benchmarks históricos do host AMD EPYC 9V74 (Go 1.23.2): `AcquireSuccess` 8.362 ns/12 allocs; `AcquireContended` 5.665 ns/15 allocs; `WindowAdd` 171,5 ns/0 allocs; `WaitForRoom` 399,3 ns/0 allocs; `SSEForward` 917,1 ns/0 allocs; `GatewayPassthrough` 134.137 ns/282 allocs. Há também números históricos de outro host (Intel Xeon Platinum 8573C); hosts/toolchains diferentes impedem comparação controlada.
- Seis testes Python da integração exemplo passaram. A validação nativa Windows/DPAPI e a aceitação com provedores reais não foram executadas.
- O ZIP de origem não continha `.git`; portanto, não foi possível criar branch ou commits por fase. Benchmarks medidos em hosts/toolchains diferentes não permitem afirmar ausência quantitativa de regressão.

Para consultar os comandos e resultados detalhados da entrega F10, veja `RELATORIO_REFATORACAO_F10.md` na raiz do repositório.

## 10. Decisões arquiteturais vigentes (F10)

1. **Checkpoint único:** memória é a fonte da verdade durante a execução; `state.json` persiste somente o necessário à correção após reinício.
2. **Modo único:** bootstrap automático em `serve`; sem `init`, `migrate`, `--dev`, `--database` ou `KEYPOOL_DATABASE_URL`.
3. **JSON e biblioteca padrão:** nenhuma dependência Go de terceiros; configuração/presets/apply em JSON.
4. **Segredos cifrados:** AES-256-GCM e master key/DPAPI mantidos.
5. **API key por hash:** SHA-256 em disco; texto da chave só em memória/ambiente e emissão inicial única.
6. **Commit atômico:** tmp, sync, rename; falha não altera o estado publicado em memória.
7. **Instância única:** bind TCP loopback antes da primeira escrita durável.
8. **Histórico limitado e volátil:** limites definidos na seção 5; contadores diários persistidos.
9. **Sinks diretos:** append em memória; métrica de descartes reflete retenção.
10. **Memstore único:** `New` para testes sem disco; daemon usa `Open`.
11. **Sem `Ping`:** removido da interface do store.
12. **IDs de chave string:** sem conversão para UUID.

## 11. Histórico de alterações

- **2026-11-12 — Integração Gemini API:** adicionado preset `gemini` OpenAI-compatible, exemplos de bootstrap e exemplo genérico de projeto Gemini, guia operacional e testes offline de contrato Bearer/modelos/chat. Quotas permanecem por projeto Google Cloud; nenhuma chave real foi incluída.
- **2026-11-10 — Consolidação inicial:** unificação dos documentos de API, decisões, estado local, auditoria, operação Windows, plano, provedores, segurança e verificação. Esta seção deve receber uma nova entrada em cada atualização relevante, com data, resumo e impacto operacional.
