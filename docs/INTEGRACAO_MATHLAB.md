# Integracao Mathlab -> KeyPool (F9)

## Estado e limite de escopo

A F9 conecta este gateway ao repositório Mathlab recebido separadamente e efetivamente modificado. O Mathlab configura `OpenAICompatibleProvider`, seis aliases de papel, headers `X-Keypool-*`, consulta remota de capacidade e `LLMClient.stream()` opt-in. O fluxo principal síncrono continua em `complete()`. Os testes offline dos dois repositórios passaram; não houve acesso a servidor KeyPool, PostgreSQL, credenciais nem upstream reais, portanto a aceitação E2E permanece pendente.

Os arquivos copiaveis estao em `integrations/mathlab/`.

## 1. Contrato de configuracao do Mathlab

O Mathlab ja possui `OpenAICompatibleProvider`, portanto deve ser usado o adaptador existente, sem um novo provider HTTP duplicado.

Mescle `integrations/mathlab/providers.fragment.yaml` em `configs/providers.yaml` do Mathlab, preservando todos os demais provedores e evitando uma segunda chave `default_provider`:

```yaml
providers:
  keypool:
    type: openai_compatible
    env_prefix: KEYPOOL
    base_url: http://127.0.0.1:8787/v1
    timeout_s: 600
    headers:
      X-Keypool-Project: mathlab
    limit_scope: provider
    limits: {}

default_provider: keypool
```

O `base_url` termina em `/v1`; o cliente OpenAI compoe `/chat/completions` e `/models`. A autenticacao do cliente deve usar `Authorization: Bearer <kp_...>`. Nao use a chave upstream Groq/OpenRouter como credencial do Mathlab.

`KEYPOOL_API_KEY` e o nome de ambiente esperado pelo `env_prefix: KEYPOOL` do MathLab. O daemon KeyPool também usa `KEYPOOL_API_KEY` para sua credencial administrativa. São valores diferentes em processos diferentes: configure ambientes isolados por processo/serviço (não defina ambos em um ambiente global compartilhado).

## 2. Alias de modelos

O perfil `integrations/mathlab/keypool-project.yaml` define aliases `prover`, `critic`, `formalizer`, `planner`, `explorer` e `summarizer`. O fragmento `models.fragment.yaml` ilustra o mapeamento dos papeis para os mesmos aliases, sem fallback local duplicado.

Antes de mesclar no Mathlab real:

1. Compare os nomes de papeis existentes em `configs/models.yaml` com o fragmento.
2. Mantenha apenas papeis realmente usados pelo Mathlab.
3. Para cada papel, use como `model` o alias correspondente configurado em `keypool-project.yaml`.
4. Remova `fallbacks` locais somente depois de comprovar que a cadeia `routes.<alias>.chain` no KeyPool contem todos os destinos e prioridades desejados.
5. Preserve `temperature`, limites de tokens e campos `extra` em uma unica fonte de verdade. A copia de exemplo atual guarda defaults na rota KeyPool.

O `GET /v1/models` e a fonte de verdade de aliases autorizados para o projeto. Nao envie nomes upstream diretamente quando `allow_literal_targets` estiver desabilitado.

## 3. Migrar credenciais upstream uma unica vez

Faca backup seguro do `.env` do Mathlab antes da migracao. Execute no host onde `keypoolctl` esta configurado para falar com o KeyPool:

```powershell
keypoolctl key import-env .env --prefix GROQ --provider groq --group conta-principal --tag mathlab
keypoolctl key import-env .env --prefix OPENROUTER --provider openrouter --group conta-openrouter --tag mathlab
keypoolctl key list
keypoolctl key test --provider groq
keypoolctl key test --provider openrouter
```

Ajuste os nomes das variaveis e identificadores de grupo ao `.env` real. A importacao deve ser executada uma vez e as chaves precisam aparecer mascaradas no KeyPool. Gere/consulte a API key do cliente KeyPool e defina-a no ambiente do Mathlab:

```powershell
$env:KEYPOOL_API_KEY = "kp_sua_chave_do_keypool"
```

Nao passe segredos em argumentos de linha de comando, nao use `key reveal` para copiar chaves upstream e nao remova `GROQ_API_KEYS`/`OPENROUTER_API_KEYS` do runtime antes do teste completo. Depois de validar e reiniciar ambos os processos, remova as variaveis upstream do ambiente Mathlab e mantenha-as apenas no armazenamento do KeyPool.

## 4. Cabecalhos por chamada

O provider Mathlab deve preservar os headers configurados estaticamente e permitir headers por chamada quando o fluxo interno os suporta:

| Header | Uso |
|---|---|
| `X-Keypool-Project: mathlab` | Seleciona a politica do projeto; deve ser sempre enviado. |
| `X-Keypool-Priority: 0..9` | Prioridade da chamada; pode ser mapeada da prioridade do papel. |
| `X-Keypool-Exclude` | Lista separada por virgulas de `provider:model` ou `model` que o roteador deve evitar. |
| `X-Keypool-Avoid-Weights` | Lista de modelos cujos destinos ponderados devem ser evitados nesta chamada. |
| `X-Keypool-Timeout-Ms` | Timeout opcional por chamada, em milissegundos. |

Nao envie headers `X-Keypool-*` recebidos do usuario final sem validacao. `X-Keypool-Project` deve ser fixado pela configuracao confiavel do Mathlab, nao aceito de entrada arbitraria.

## 5. Rate limit, erros e streaming

O KeyPool responde HTTP 429 com JSON no formato de erro do gateway e `Retry-After` em segundos quando o tempo de espera e conhecido. O cliente Mathlab deve continuar convertendo esse status para `RateLimited(retry_after)` e nao fazer retries identicos em loop. Se nao houver `Retry-After`, use o comportamento de limite desconhecido ja definido pelo Mathlab.

Streaming utiliza `text/event-stream` e termina com `data: [DONE]`. O cliente deve consumir o fluxo incrementalmente, preservar cancelamento/timeouts e fechar a resposta. Nao bufferize o stream completo.

## 6. Integrar capacidade

O endpoint e autenticado:

```http
GET /v1/capacity?alias=prover
Authorization: Bearer kp_...
X-Keypool-Project: mathlab
```

Resposta resumida:

```json
{
  "project": "mathlab",
  "capacity": {
    "prover": {
      "live_keys": 2,
      "buckets": 2,
      "lanes": 4,
      "parallelism": 4
    }
  }
}
```

Os numeros acima sao apenas ilustrativos. O Mathlab deve usar `capacity[alias].parallelism` como limite operacional atual, tratar ausencia/erro/timeout como dado indisponivel e manter um fallback conservador. Nao somar esse valor a estimativas locais nem aplicar novamente o multiplicador por chaves/buckets: o KeyPool ja calcula as vias. Use cache curto, por exemplo 5–15 segundos, para nao consultar o endpoint em cada submissao. Nunca use o valor como garantia de throughput; e uma estimativa do estado observado naquele instante.

## 7. Verificador incluido

`integrations/mathlab/smoke.py` usa somente a biblioteca padrao do Python. Ele le a credencial do ambiente e nao aceita a chave como argumento de linha de comando. O modo padrao testa:

- `/healthz` sem autenticacao;
- `/readyz` com Bearer;
- `/v1/models` e existencia do alias configurado;
- `/v1/capacity?alias=...` e campo `parallelism`.

Com `--chat`, envia uma completion real curta. Com `--stream`, envia uma chamada SSE e espera `data: [DONE]`. Essas duas opcoes podem gerar consumo no provedor e por isso nao sao executadas por padrao. O MathLab usa uma única credencial `kp_...` por processo; não configure uma lista de credenciais cliente.

```powershell
$env:KEYPOOL_URL = "http://127.0.0.1:8787"
$env:KEYPOOL_API_KEY = "kp_sua_chave_do_keypool"
$env:KEYPOOL_PROJECT = "mathlab"
$env:KEYPOOL_ALIAS = "prover"
python integrations/mathlab/smoke.py
python integrations/mathlab/smoke.py --chat
python integrations/mathlab/smoke.py --stream
```

Teste offline do verificador:

```powershell
python -m unittest discover -s integrations/mathlab -p "test_*.py" -v
python -m py_compile integrations/mathlab/smoke.py
```

O script nao imprime a chave, prompt, resposta nem corpo de erro upstream. Um 429 com `Retry-After` e relatado como limite temporario observado.

## 8. Aceitação ponta a ponta no Mathlab real

A integração de código já está aplicada no ZIP Mathlab entregue separadamente. Não reaplique os fragments sem comparar os arquivos atuais. A validação operacional restante é:

1. Inicie KeyPool com PostgreSQL, projeto `mathlab`, provedores upstream importados e API key de cliente provisionada.
2. Confirme `/healthz`, `/readyz`, `/v1/models` e `/v1/capacity` com o smoke test (`KEYPOOL_API_KEY` no processo Mathlab).
3. Rode `keypoolctl status`, `provider list`, `key list`, `warm status` e confira chaves mascaradas/saúde.
4. Confirme no Mathlab que `default_provider: keypool` está ativo e os seis papéis usam aliases; mantenha os provedores locais apenas para rollback.
5. Rode uma chamada real curta via `complete()` usando um alias de baixo risco, sem fallback local para esse papel.
6. Teste `LLMClient.stream()` como API opt-in, incluindo fechamento/cancelamento; o pipeline principal ainda usa `complete()`.
7. Provoque indisponibilidade/429 de maneira controlada e confirme `RateLimited(retry_after)` e ausência de retry cego local.
8. Confirme que `capacity.py` usa `parallelism` do endpoint e que o limite local não é multiplicado novamente.
9. Reinicie Mathlab e KeyPool; confirme persistência e que as chaves upstream não existem no ambiente do Mathlab.
10. Somente após os testes, avalie remover `key_pool.py`, `rate_limiter.py`, spill/espera/`_exhausted` e `diagnostics.ping_all` redundantes. Preserve rollback durante o período de observação.

## 9. Remocoes possiveis no Mathlab depois da estabilizacao

- `key_pool.py` e `rate_limiter.py` (selecao, janelas, fila, reserva e cooldown migrados).
- Spill, espera, selecao de chaves e `_exhausted` em `client.py`; mantenha transporte HTTP, cancelamento, streaming e traducao final de erros.
- Fallbacks duplicados em `configs/models.yaml` depois da validacao das chains KeyPool.
- `diagnostics.ping_all`, substituido por status/warm do KeyPool.

Nao remova antes de provar equivalencia funcional em concorrencia, prioridade, cancelamento, retry-after, streaming e timeout.

## 10. Rollback

1. Restaure `default_provider` para o provider anterior no Mathlab.
2. Reponha `models.yaml` anterior e os fallbacks locais.
3. Restaure temporariamente as variaveis upstream no ambiente Mathlab se ainda necessario.
4. Reinicie Mathlab; mantenha KeyPool ativo para diagnostico, sem apagar chaves importadas.
5. Investigue o evento/usage no KeyPool antes de tentar novamente.

## Status honesto da F9

Concluído no código: configuração KeyPool em `configs/providers.yaml`, aliases dos seis papéis em `configs/models.yaml`, integração em `LLMClient`/`OpenAICompatibleProvider`, headers de política, `reasoning_effort`, capacidade remota, tratamento de `Retry-After`, validação de credencial cliente singular e `LLMClient.stream()` opt-in. O KeyPool mantém perfil, contrato e smoke test. A suíte offline Mathlab passou com 671 testes e 19 skips (excluindo o módulo que requer `z3-solver`); o kit KeyPool passou 6/6.

Pendente: aceitação E2E com KeyPool em execução, PostgreSQL, upstream e credenciais reais; verificação de 429/concorrência/reinicialização/cancelamento upstream; e decidir sobre remoção de módulos locais após equivalência comprovada. O streaming existe como API opt-in, mas o pipeline principal continua usando `complete()`. Consulte `docs/FINAL_AUDIT.md` no KeyPool e `docs/KEYPOOL_FINAL_AUDIT.md` no Mathlab.
