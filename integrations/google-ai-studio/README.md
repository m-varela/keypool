# Google AI Studio (Gemini API) + KeyPool

Este guia integra o KeyPool à **Gemini API** usando uma API key criada no Google AI Studio. Não integra a interface web do AI Studio, o histórico ou os prompts salvos.

## Contrato utilizado

- Base URL: `https://generativelanguage.googleapis.com/v1beta/openai`
- Chat: `POST /chat/completions`
- Modelos: `GET /models`
- Autenticação upstream: `Authorization: Bearer <GEMINI_API_KEY>`
- Adaptador: `openai_compatible` já existente no KeyPool.

## 1. Preparar o projeto Google

1. Crie/seleciona um projeto no Google AI Studio e gere uma Gemini API key.
2. Consulte no AI Studio os modelos disponíveis e as quotas ativas desse projeto.
3. Não compartilhe a chave com clientes nem a inclua em arquivos versionados.

## 2. Primeiro bootstrap

A entrada `gemini` já está no exemplo `configs/examples/keypool.json`. Defina a chave apenas no ambiente local do processo e inicie o daemon: 

```powershell
$env:GEMINI_API_KEY = Read-Host 'Gemini API key'
go run ./cmd/keypoold serve --config ./configs/examples/keypool.json
```

Guarde a `kp_...` emitida pelo KeyPool. O bootstrap aceita `GEMINI_API_KEYS`, `GEMINI_API_KEY` e `GEMINI_API_KEY_N`; valores duplicados são eliminados.

## 3. KeyPool já inicializado

Com o daemon ativo e `KEYPOOL_URL`/`KEYPOOL_API_KEY` configurados para a instância local, registre o provedor e a chave. O JSON em `provider.json` é o payload do provedor, sem segredo: 

```powershell
go run ./cmd/keypoolctl provider add -f ./integrations/google-ai-studio/provider.json --name gemini
$env:GEMINI_API_KEY = Read-Host 'Gemini API key'
$env:GEMINI_API_KEY | go run ./cmd/keypoolctl key add --provider gemini --label gemini-main --tag mathlab --stdin
```

O comando `key add --stdin` lê o segredo da entrada padrão. Não passe a chave como argumento. Para importar um arquivo `.env` local, use `keypoolctl key import-env <arquivo> --prefix GEMINI --provider gemini --tag mathlab` conforme as opções exibidas por `keypoolctl help`/CLI.

## 4. Validar e escolher um modelo

```powershell
go run ./cmd/keypoolctl key list --provider gemini
go run ./cmd/keypoolctl key test <ID_DA_CHAVE>
```

A sonda consulta `/models`. Escolha um `id` retornado que também seja compatível com `chat/completions` na documentação atual do Google. Não presuma que todo modelo listado aceite esse endpoint.

## 5. Aplicar o perfil Mathlab

Abra `mathlab.project.json` e substitua `REPLACE_WITH_MODEL_ID_FROM_KEY_TEST` pelo ID exato retornado na etapa anterior. O perfil é completo e preserva as rotas existentes; revise o diff antes de aplicar. Depois: 

```powershell
go run ./cmd/keypoolctl project apply -f ./integrations/google-ai-studio/mathlab.project.json
```

O perfil autoriza chaves Gemini com a tag `mathlab` e expõe o alias `gemini`. Não aplique o arquivo enquanto o placeholder não tiver sido substituído.

## 6. Chamada pelo gateway

Configure `KEYPOOL_API_KEY` com a chave local `kp_...` e envie: 

```powershell
$headers = @{ Authorization = "Bearer $env:KEYPOOL_API_KEY"; 'X-Keypool-Project' = 'mathlab' }
$body = @{ model = 'gemini'; messages = @(@{ role = 'user'; content = 'Responda apenas: integração funcionando.' }); max_tokens = 128 } | ConvertTo-Json -Depth 8
Invoke-RestMethod -Method Post -Uri 'http://127.0.0.1:8787/v1/chat/completions' -Headers $headers -ContentType 'application/json' -Body $body
```

Para streaming, use um cliente HTTP/SSE que leia incrementalmente o mesmo endpoint com `stream: true`; confirme os eventos `data:` e o terminador `data: [DONE]`. A chave Gemini nunca deve ser enviada pelo cliente.

## 7. Quotas e rotação

O preset usa `limit_scope: provider`: todas as chaves registradas nesse provedor compartilham os limites locais do KeyPool. A quota upstream da Gemini API é aplicada por projeto Google Cloud, não por chave. Alternar várias chaves do mesmo projeto não multiplica RPM/TPM/RPD/TPD. Consulte os valores reais no AI Studio; não copie limites de outro projeto. Se operar projetos Google distintos, cadastre provedores distintos para manter suas quotas separadas.

## 8. Limites desta integração

A primeira versão cobre chat completions OpenAI-compatible e streaming SSE conforme suportado pelo gateway. `reasoning_effort` é encaminhado apenas para modelos `gemini-3.*`, conforme o exemplo oficial atual; para os demais modelos o adaptador remove esse campo. Não promete suporte a `generateContent` nativo, embeddings, imagem, áudio, vídeo, Live API, Batch ou a todos os campos exclusivos Gemini. O preset não força `stream_options` nem cabeçalhos de quota não verificados.

## Referências oficiais

- https://ai.google.dev/gemini-api/docs/openai
- https://ai.google.dev/gemini-api/docs/models
- https://ai.google.dev/gemini-api/docs/rate-limits
- https://ai.google.dev/gemini-api/docs/api-key
