# Google AI Studio (Gemini API) + KeyPool

Este kit genérico integra a **Gemini API** usando uma API key criada no Google AI Studio. Não integra a interface web do AI Studio, histórico ou prompts salvos.

## Contrato utilizado

- Base URL: `https://generativelanguage.googleapis.com/v1beta/openai`
- Chat: `POST /chat/completions`
- Catálogo: `GET /models`
- Autenticação upstream: `Authorization: Bearer <GEMINI_API_KEY>`
- Adaptador: `openai_compatible` e preset `gemini` já incluídos no KeyPool.

## 1. Preparar o projeto Google

1. Crie ou selecione um projeto no Google AI Studio e gere uma Gemini API key.
2. Consulte no AI Studio os modelos disponíveis e as quotas ativas desse projeto.
3. Não compartilhe a chave com clientes nem a inclua em arquivos versionados.

## 2. Primeiro bootstrap

O exemplo genérico `configs/examples/keypool.json` já habilita `gemini` junto com Groq e OpenRouter. Defina a chave apenas no ambiente local do processo e inicie o daemon:

```powershell
$env:GEMINI_API_KEY = Read-Host 'Gemini API key'
go run ./cmd/keypoold serve --config ./configs/examples/keypool.json
```

O exemplo usa `default_project: example`; por isso, as credenciais importadas no primeiro bootstrap recebem a tag `example`. O bootstrap aceita `GEMINI_API_KEYS`, `GEMINI_API_KEY` e `GEMINI_API_KEY_N`.

## 3. KeyPool já inicializado

Com o daemon ativo e `KEYPOOL_URL`/`KEYPOOL_API_KEY` configurados para a instância local, registre o provedor e a chave:

```powershell
go run ./cmd/keypoolctl provider add -f ./integrations/google-ai-studio/provider.json --name gemini
$env:GEMINI_API_KEY = Read-Host 'Gemini API key'
$env:GEMINI_API_KEY | go run ./cmd/keypoolctl key add --provider gemini --label gemini-main --tag example --stdin
go run ./cmd/keypoolctl key list --provider gemini
go run ./cmd/keypoolctl key test <ID_DA_CHAVE>
```

O comando `key add --stdin` lê o segredo da entrada padrão. Não passe a chave como argumento. Para importar um arquivo `.env` local, use `keypoolctl key import-env <arquivo> --prefix GEMINI --provider gemini --tag <tag-do-projeto>`.

## 4. Perfil de projeto

Perfis de projeto pertencem aos repositórios consumidores, não a este kit de provedor. O KeyPool contém apenas o preset genérico e o contrato da Gemini API. Para experimentar a configuração genérica:

```powershell
go run ./cmd/keypoolctl project apply -f ./configs/examples/example.project.json
```

Esse perfil autoriza credenciais com a tag `example` e disponibiliza o alias `assistant`. Em outra aplicação, mantenha o perfil JSON e os aliases no repositório dessa aplicação, usando uma tag própria.

## 5. Testar uma chamada pelo gateway

```powershell
$headers = @{ Authorization = "Bearer $env:KEYPOOL_API_KEY"; 'X-Keypool-Project' = 'example' }
$body = @{ model = 'assistant'; messages = @(@{ role = 'user'; content = 'Responda apenas: integração funcionando.' }); max_tokens = 128 } | ConvertTo-Json -Depth 8
Invoke-RestMethod -Method Post -Uri 'http://127.0.0.1:8787/v1/chat/completions' -Headers $headers -ContentType 'application/json' -Body $body
```

Para streaming, use um cliente HTTP/SSE que leia incrementalmente o mesmo endpoint com `stream: true`; confirme os eventos `data:` e o terminador `data: [DONE]`. A chave Gemini nunca deve ser enviada pelo cliente.

O smoke test do kit pode ser executado com `KEYPOOL_API_KEY` definida:

```powershell
python integrations/google-ai-studio/smoke.py --project example --model assistant
```

## 6. Quotas e limitações

O preset usa `limit_scope: provider`: todas as chaves cadastradas nesse provedor compartilham os limites locais do KeyPool. A quota upstream da Gemini API é aplicada por projeto Google Cloud, não por chave. Alternar várias chaves do mesmo projeto não multiplica RPM/TPM/RPD/TPD. Consulte os valores reais no AI Studio; não copie limites de outro projeto. Se operar projetos Google distintos, cadastre provedores distintos para manter as quotas separadas.

A integração cobre chat completions OpenAI-compatible e streaming SSE conforme suportado pelo gateway. Não promete suporte a `generateContent` nativo, embeddings, imagem, áudio, vídeo, Live API, Batch ou a todos os campos exclusivos Gemini. O preset permite `reasoning_effort` somente para modelos `gemini-3.*`, conforme o contrato documentado.

## Referências oficiais

- https://ai.google.dev/gemini-api/docs/openai
- https://ai.google.dev/gemini-api/docs/models
- https://ai.google.dev/gemini-api/docs/rate-limits
- https://ai.google.dev/gemini-api/docs/api-key
