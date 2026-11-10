# KeyPool

KeyPool e um gateway local em Go para centralizar chaves de provedores de IA e expor uma API compativel com OpenAI. O primeiro perfil de projeto e `mathlab`.

> Estado desta árvore: F0-F9 têm implementação de código nos repositórios KeyPool e Mathlab. A CLI oferece saída tabular por padrão e `--json`. Build completo do KeyPool, PostgreSQL, DPAPI/Task Scheduler e aceitação E2E dos dois serviços exigem validação nativa com Go 1.26+, dependências reais, PostgreSQL 16+ e Windows. Consulte `STATUS.md` e `docs/VERIFY.md`.

## Requisitos

- Go 1.26 ou superior.
- Windows 10/11 com PowerShell 5.1 (os scripts nao dependem de modulos externos).
- Acesso a internet apenas na primeira resolucao da dependencia `gopkg.in/yaml.v3` caso ela ainda nao esteja no cache do Go.
- Uma chave de API valida do provedor que sera usado. Este repositorio nao inclui credenciais.

O modo `serve --dev` continua funcionando sem banco. O modo persistente exige PostgreSQL 16+; consulte `docs/POSTGRESQL_F4.md`. Docker, WSL e Make nao sao necessarios.

## Compilar e testar

No PowerShell, a partir da raiz do repositorio:

```powershell
 go mod tidy
 .\scripts\windows\build.ps1
 .\scripts\windows\test.ps1
```

Os comandos de verificacao completos e as variantes de concorrencia/benchmark estao em `docs/VERIFY.md`.

## Executar modo dev

Configure as chaves no ambiente. O modo dev aceita uma lista separada por virgula ou espaco e tambem variaveis numeradas:

```powershell
$env:GROQ_API_KEYS = "gsk_sua_chave_1,gsk_sua_chave_2"
$env:OPENROUTER_API_KEY = "sk-or-sua_chave"
# Opcional: manter a mesma chave de acesso entre reinicios
$env:KEYPOOL_API_KEY = "kp_sua_chave_local"

go run ./cmd/keypoold serve --dev ./configs/examples/keypool.dev.yaml
# Atalho PowerShell 5.1:
.\scripts\windows\run-dev.ps1
```

Sem `KEYPOOL_API_KEY`, o processo gera uma chave `kp_...` aleatoria, imprime-a uma unica vez no stdout e a utiliza ate o processo terminar. Defina a variavel para manter a credencial entre reinicios.

As chaves de provedor sao lidas destas formas (nessa ordem):

1. `<PREFIXO>_API_KEYS` (itens separados por virgula ou espaco);
2. `<PREFIXO>_API_KEY`;
3. `<PREFIXO>_API_KEY_0`, `_1`, `_2` etc. (ordenadas numericamente).

O prefixo e `GROQ` ou `OPENROUTER` nos exemplos. Duplicatas sao removidas. Segredos nao devem ser colocados nos arquivos YAML.

O servidor valida que o endereco de escuta seja loopback (`127.0.0.1` ou `localhost`). A configuracao de exemplo usa `127.0.0.1:8787`.

## API local

Toda rota, exceto `/healthz`, exige a chave do KeyPool por `Authorization: Bearer kp_...` (ou `X-API-Key`). Para abrir o painel no navegador, use HTTP Basic com usuário `keypool` e a API key como senha. Para selecionar o projeto, envie `X-Keypool-Project: mathlab`; se omitido, usa-se `default_project`.

- `GET /healthz`
- `GET /readyz`
- `GET /` (painel somente leitura)
- `GET /metrics` (Prometheus 0.0.4)
- `GET /v1/models`
- `GET /v1/capacity`
- `POST /v1/chat/completions`
- `POST /v1/leases` e `POST /v1/leases/{id}/report` (se `allow_lease` estiver habilitado)
- `ANY /p/{provider}/{path...}` (passthrough para `generic_http`)

Exemplo:

```powershell
$headers = @{
  Authorization = "Bearer $env:KEYPOOL_API_KEY"
  "X-Keypool-Project" = "mathlab"
}
$body = @{
  model = "prover"
  messages = @(@{ role = "user"; content = "Resuma a identidade de Euler." })
  max_tokens = 512
} | ConvertTo-Json -Depth 10
Invoke-RestMethod -Uri "http://127.0.0.1:8787/v1/chat/completions" -Method Post -Headers $headers -ContentType "application/json" -Body $body
```

`model` pode ser um alias configurado, por exemplo `prover` ou `critic`. Alvos literais `provider:model` so funcionam quando `allow_literal_targets` estiver habilitado no projeto.

O gateway inclui os cabecalhos `X-Keypool-Target`, `X-Keypool-Key` (rotulo mascarado), `X-Keypool-Attempts`, `X-Keypool-Queue-Ms` e `X-Keypool-Fallback`.

## Estrutura

- `internal/sliding`, `internal/limits`, `internal/sched`: janelas, resolucao dos limites e pool/leases.
- `internal/provider`, `internal/classify`, `internal/sse`: adaptadores, classificacao e parsing SSE.
- `internal/config`, `internal/store/memstore`: configuracao e armazenamento em memoria.
- `internal/router`, `internal/gateway`: roteamento e API HTTP local.
- `internal/admin`: API administrativa autenticada; `cmd/keypoolctl`: cliente HTTP e operação local de reveal.
- `cmd/keypoold`: processo servidor.
- `configs/examples`: configuracao de desenvolvimento e perfil Mathlab.

O contrato e as decisoes de implementacao estao em `docs/PLANO.md` e `docs/DECISIONS.md`.

Eventos operacionais do scheduler sao persistidos por uma fila assincrona limitada.


## Administração (F5)

Com o daemon em execução, configure `KEYPOOL_URL` (padrão `http://127.0.0.1:8787`) e `KEYPOOL_API_KEY` para usar `keypoolctl`:

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

Para cadastrar uma chave, passe o segredo por stdin (nunca como argumento):

```powershell
$env:GROQ_API_KEY | go run ./cmd/keypoolctl key add --provider groq --label groq-main --tag mathlab --stdin
```

`keypoolctl key reveal <label>` é exclusivamente local e exige `KEYPOOL_DATABASE_URL`, master key e confirmação interativa. `keypoolctl apikey rotate` exibe o token novo uma única vez; atualize `KEYPOOL_API_KEY` no ambiente antes de reiniciar o daemon.

## Aquecimento e saúde (F6)

O serviço warm executa L0 (conexão), L1 (validação sem token) e L2 (sonda de completion de um token, opcional e desligada por padrão). As sondas respeitam ociosidade, workers limitados, circuito, catálogo e limites do provedor. `/readyz` aguarda a primeira rodada L1 até `warm.ready_timeout`.

```powershell
go run ./cmd/keypoolctl warm status
go run ./cmd/keypoolctl warm run --provider groq
go run ./cmd/keypoolctl warm run --provider groq --key ID_DA_CHAVE
go run ./cmd/keypoolctl key test ID_DA_CHAVE
```

Configuração opcional (os padrões podem ser omitidos):

```yaml
warm:
  enabled: true
  workers: 4
  ready_timeout: 20s
  idle_threshold: 45s
  catalog_ttl: 1h
  l0_interval: 45s
  l1_interval: 10m
  l1_jitter: 0.2
  auth_recheck: 2h
  counts_against_limits: false
  l2:
    enabled: false
```

Para detalhes operacionais e limitações de verificação, veja `docs/VERIFY.md` e `STATUS.md`.


## F8 — observabilidade e operação

`GET /metrics` expõe métricas Prometheus 0.0.4 e `GET /` apresenta um painel local estático. Ambos exigem a mesma API key usada pelo gateway (Bearer/`X-API-Key`; o painel também aceita Basic Auth para navegação). O painel não executa operações de escrita, não carrega scripts/CDNs e é embutido no executável.

Métricas principais: requisições/tentativas, histogramas de espera e TTFB, estados de chave, uso relativo de janelas, cooldown, idade das sondas, descartes da fila de usage e leases vazadas.

No Windows, a tarefa ao logon pode ser instalada com `scripts/windows/install-task.ps1`. A master key também pode ser protegida opcionalmente com DPAPI do usuário atual:

```powershell
.\bin\keypoold.exe init --master-key-dpapi
.\bin\keypoold.exe serve --master-key-dpapi
```

O modo DPAPI é mutuamente exclusivo com `KEYPOOL_MASTER_KEY` e requer o mesmo perfil Windows ao iniciar/recuperar. Veja `docs/OPERACAO_WINDOWS.md`, `docs/API.md`, `docs/SECURITY.md` e `docs/PROVIDERS.md`.

## F9 — integração com o mathlab

A integração de código foi aplicada no repositório Mathlab recebido separadamente: aliases por papel, `OpenAICompatibleProvider`, headers de política, consulta remota de capacidade e streaming opt-in. Este diretório mantém o perfil KeyPool, smoke test e contrato offline. A integração usa uma única `KEYPOOL_API_KEY` no processo Mathlab; a rotação upstream pertence ao KeyPool. Aceitação E2E com serviços/credenciais reais continua pendente. Veja `docs/INTEGRACAO_MATHLAB.md` e `docs/VERIFY.md`.
