# Integração Mathlab -> KeyPool

A integração usa o gateway OpenAI-compatible do KeyPool e a implementação `OpenAICompatibleProvider` no repositório Mathlab. O contrato HTTP permanece: uma API key cliente `KEYPOOL_API_KEY`, aliases de projeto, headers de política, consulta `/v1/capacity` e streaming SSE opt-in.

## Perfil KeyPool

O perfil do projeto fica em `integrations/mathlab/keypool-project.json` e o exemplo do daemon em `configs/examples/mathlab.project.json`. A configuração principal `configs/examples/keypool.json` carrega o perfil pelo campo `project_file`, resolvido relativamente ao arquivo JSON que o declara.

Os arquivos `providers.fragment.yaml` e `models.fragment.yaml` são fragments de referência destinados ao repositório Python Mathlab; não são lidos pelo KeyPool e foram mantidos fora da migração de formato.

## Contrato operacional

- O Mathlab usa exatamente uma credencial cliente `KEYPOOL_API_KEY` (`kp_...`).
- Chaves Groq/OpenRouter são importadas e rotacionadas no KeyPool, não no processo Mathlab.
- O Mathlab encaminha prioridade, exclusões, timeout e política por headers `X-Keypool-*`.
- A consulta de capacidade é feita por alias e pode usar cache/fallback conservador do lado cliente.
- Streaming é incremental e respeita cancelamento do cliente.

## Verificações offline

Na raiz do repositório KeyPool:

```powershell
python -m unittest discover -s integrations/mathlab -p "test_*.py" -v
python -m py_compile integrations/mathlab/smoke.py
```

Para preflight contra um daemon local, configure `KEYPOOL_URL`, `KEYPOOL_API_KEY`, `KEYPOOL_PROJECT` e `KEYPOOL_ALIAS` e execute `python integrations/mathlab/smoke.py`. `--chat` e `--stream` enviam requisições reais ao provedor configurado e devem ser usados somente de forma deliberada.

A suíte offline não substitui a aceitação E2E no ambiente implantado: continuam necessários testes com credenciais, serviço Mathlab, limites upstream, 429/Retry-After, concorrência, reinicialização e cancelamento.
