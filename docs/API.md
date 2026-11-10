# API do KeyPool

O gateway é local e escuta em loopback. Exceto `GET /healthz`, os endpoints exigem `Authorization: Bearer <KEYPOOL_API_KEY>` ou `X-API-Key: <KEYPOOL_API_KEY>`. O painel também aceita HTTP Basic com usuário literal `keypool` e a API key como senha. Não exponha a porta diretamente à internet.

## Endpoints do gateway

| Método e rota | Auth | Descrição |
|---|---|---|
| `GET /healthz` | Não | Liveness do processo; não representa prontidão. |
| `GET /readyz` | Sim | Readiness após primeira passada L1 ou timeout configurado. |
| `GET /` | Sim | Painel HTML local somente leitura, embutido no executável. |
| `GET /metrics` | Sim | Métricas Prometheus 0.0.4. |
| `POST /v1/chat/completions` | Sim | Gateway compatível com OpenAI, com alias de projeto e fallback. |
| `GET /v1/models` | Sim | Aliases/modelos autorizados pelo projeto. |
| `GET /v1/capacity` | Sim | Capacidade estimada para o alias solicitado. |
| `POST /v1/leases` | Sim + `allow_lease` | Reserva uma chave e devolve detalhes temporários da lease. |
| `POST /v1/leases/{id}/report` | Sim + projeto proprietário | Reporta outcome, tokens e cabeçalhos de limites; fecha a lease. |
| `ANY /p/{provider}/{path...}` | Sim + `generic_http` autorizado | Passthrough autenticado para endpoint permitido. |

As respostas administrativas não revelam segredos de provedores. O `state.json` local guarda as credenciais cifradas e somente o hash da API key.

## Exemplo de chat

```http
POST /v1/chat/completions HTTP/1.1
Host: 127.0.0.1:8787
Authorization: Bearer kp_...
X-Keypool-Project: mathlab
Content-Type: application/json

{"model":"prover","messages":[{"role":"user","content":"Olá"}]}
```

Cabeçalhos opcionais incluem `X-Keypool-Priority`, `X-Keypool-Timeout-Ms`, `X-Keypool-Exclude` e `X-Keypool-Avoid-Weights`. O gateway devolve `X-Keypool-Target`, `X-Keypool-Key` (rótulo mascarado), `X-Keypool-Attempts`, `X-Keypool-Queue-Ms` e `X-Keypool-Fallback`.

## Administração

As rotas `/admin/v1/*` são protegidas pela mesma autenticação. Incluem `apply`, CRUD de provedores/projetos/regras/rotas/chaves, `keys/{id}/test`, `warm/run`, `warm/status`, `stats`, `usage`, `events` e rotação da API key. `keypoolctl project apply -f` aceita documentos JSON e rejeita YAML.

## Histórico e retenção

`GET /admin/v1/usage` e `GET /admin/v1/events` leem rings voláteis. Uso mantém até 50.000 eventos por 7 dias; eventos mantêm até 10.000 por 30 dias; saúde mantém até 1.000 por 7 dias. Os registros detalhados desaparecem no reinício. Contadores diários usados pelo orçamento de projeto e warm L2 ficam no `state.json`.

A métrica `keypool_usage_dropped_total` conta eventos de uso descartados por limite de retenção em memória; não representa falha de fila ou perda de escrita em banco.

## Limites operacionais

- Corpo de POST limitado a 8 MiB no gateway.
- IDs de request devolvidos em `X-Keypool-Request-Id`.
- Prompts e respostas não são persistidos nem expostos em métricas.
- Endpoints de observabilidade exigem credencial; não configure scraping remoto sem um túnel/controlador confiável.
