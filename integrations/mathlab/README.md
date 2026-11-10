# Kit de integração Mathlab -> KeyPool

Este diretório contém os artefatos do lado KeyPool para conectar um Mathlab que use `OpenAICompatibleProvider`:

- `providers.fragment.yaml` e `models.fragment.yaml`: fragments ilustrativos do repositório Mathlab; não fazem parte da configuração JSON do KeyPool e não devem ser alterados por esta refatoração.
- `keypool-project.json`: perfil declarativo do projeto `mathlab` no KeyPool.
- `.env.keypool.example`: nomes das variáveis do processo Mathlab; não contém credencial real.
- `smoke.py`: preflight HTTP, aliases, capacidade, completion e SSE usando a biblioteca padrão Python.
- `test_smoke.py`: testes offline do verificador com servidor HTTP falso.

A configuração correspondente do KeyPool é `configs/examples/keypool.json`, que referencia `mathlab.project.json`. O código-fonte Mathlab é entregue em repositório separado. Consulte `docs/INTEGRACAO_MATHLAB.md` para o contrato compartilhado e os passos de aceitação E2E.

## Teste local

```powershell
python -m unittest discover -s integrations/mathlab -p "test_*.py" -v
python -m py_compile integrations/mathlab/smoke.py
```

## Smoke test contra um KeyPool em execução

```powershell
$env:KEYPOOL_URL = "http://127.0.0.1:8787"
$env:KEYPOOL_API_KEY = "kp_sua_chave_do_keypool"
$env:KEYPOOL_PROJECT = "mathlab"
$env:KEYPOOL_ALIAS = "prover"
python integrations/mathlab/smoke.py
```

O smoke test lê uma única credencial cliente `KEYPOOL_API_KEY`, não aceita listas nem segredo em argumento de processo. O modo padrão consulta health/readiness/models/capacity. Para enviar uma chamada ao provedor, opte explicitamente por `--chat` ou `--stream`. O script não imprime a API key, prompts, respostas nem corpos de erro upstream.
