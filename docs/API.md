# API do KeyPool

O gateway é local e escuta em loopback. Exceto `GET /healthz`, os endpoints do gateway e da administração exigem autenticação por `Authorization: Bearer <KEYPOOL_API_KEY>` ou `X-API-Key: <KEYPOOL_API_KEY>`. Para navegação do painel no browser, também é aceito HTTP Basic com usuário literal `keypool` e a API key como senha; o 401 envia `WWW-Authenticate` para abrir o diálogo nativo do navegador. Não exponha a porta diretamente à internet.

## Endpoints do gateway

| Método e rota | Auth | Descrição |
|---|---|---|
| `GET /healthz` | Não | Liveness do processo; não representa prontidão. |
| `GET /readyz` | Sim | Readiness após primeira passada L1 ou `ready_timeout`; pode retornar warning. |
| `GET /` | Sim | Painel HTML local somente leitura, embutido no executável. |
| `GET /metrics` | Sim | Métricas Prometheus 0.0.4. |
| `POST /v1/chat/completions` | Sim | Gateway compatível com OpenAI, com alias de projeto e fallback. |
| `GET /v1/models` | Sim | Aliases/modelos autorizados pelo projeto. |
| `GET /v1/capacity` | Sim | Capacidade estimada para o alias solicitado. |
| `POST /v1/leases` | Sim + `allow_lease` | Reserva uma chave e devolve detalhes temporários da lease. |
| `POST /v1/leases/{id}/report` | Sim + projeto proprietário | Reporta outcome, tokens e cabeçalhos de limites; fecha a lease. |
| `ANY /p/{provider}/{path...}` | Sim + `generic_http` autorizado | Passthrough autenticado para endpoint arbitrário permitido. |

As respostas administrativas não revelam segredos de provedores. A API key do KeyPool é um segredo separado, armazenado apenas como hash no banco.

## Exemplo de chat

```http
POST /v1/chat/completions HTTP/1.1
Host: 127.0.0.1:8787
Authorization: Bearer kp_...
X-Keypool-Project: mathlab
Content-Type: application/json

{"model":"prover","messages":[{"role":"user","content":"Olá"}]}
```

Cabeçalhos opcionais incluem `X-Keypool-Priority`, `X-Keypool-Timeout-Ms`, `X-Keypool-Exclude` e `X-Keypool-Avoid-Weights`. O gateway devolve `X-Keypool-Target`, `X-Keypool-Key` (label mascarado), `X-Keypool-Attempts`, `X-Keypool-Queue-Ms` e `X-Keypool-Fallback`.

## Exemplo de métricas

```powershell
curl.exe -H "X-API-Key: $env:KEYPOOL_API_KEY" http://127.0.0.1:8787/metrics
```

O endpoint utiliza `Content-Type: text/plain; version=0.0.4; charset=utf-8` e `Cache-Control: no-store`. Não há endpoint de escrita no painel HTML.

## Administração

As rotas `/admin/v1/*` são protegidas pela mesma autenticação. Incluem `apply`, CRUD de provedores/projetos/regras/rotas/chaves, `keys/{id}/test`, `warm/run`, `warm/status`, `stats`, `usage`, `events` e rotação da API key. Consulte `internal/admin/admin.go` para os paths e payloads exatos da versão compilada.

## Limites operacionais

- Corpo de POST limitado a 8 MiB no gateway.
- IDs de request devolvidos em `X-Keypool-Request-Id`.
- O conteúdo de prompts e respostas não é persistido nem exposto em métricas.
- Endpoints de observabilidade exigem credencial; não configure scraping remoto sem um túnel/controlador confiável.
