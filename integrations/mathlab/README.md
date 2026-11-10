# Kit de integracao Mathlab -> KeyPool

Este diretorio contem os artefatos do lado KeyPool para conectar um Mathlab que ja use `OpenAICompatibleProvider`:

- `providers.fragment.yaml`: contrato de referência do provider KeyPool; a configuração correspondente já está aplicada em `configs/providers.yaml` no ZIP Mathlab integrado.
- `models.fragment.yaml`: referência de mapeamento de papéis para aliases; os seis papéis já estão apontados aos aliases no `configs/models.yaml` entregue separadamente.
- `keypool-project.yaml`: perfil do projeto `mathlab` no KeyPool (copia do exemplo mantido em `configs/examples/`).
- `.env.keypool.example`: nomes das variaveis do processo Mathlab; nao contem credencial real.
- `smoke.py`: preflight HTTP, listagem de aliases, capacidade, completion e SSE. Usa apenas a biblioteca padrao do Python.
- `test_smoke.py`: testes offline do verificador com servidor HTTP falso.

O código-fonte Mathlab não é embutido neste ZIP do KeyPool: a integração real foi aplicada e entregue separadamente no repositório Mathlab. Este diretório mantém o perfil/contrato do lado KeyPool, os fragments como referência (não reaplicar sem comparar), smoke test e verificações offline. Consulte `docs/INTEGRACAO_MATHLAB.md` para o estado compartilhado e os passos de aceitação E2E.

## Teste local do kit

Na raiz do repositorio KeyPool:

```powershell
python -m unittest discover -s integrations/mathlab -p "test_*.py" -v
python -m py_compile integrations/mathlab/smoke.py
```

## Smoke test contra um KeyPool em execucao

Defina as variaveis no processo Mathlab (a chave `kp_...` emitida pelo KeyPool):

```powershell
$env:KEYPOOL_URL = "http://127.0.0.1:8787"
$env:KEYPOOL_API_KEY = "kp_sua_chave_do_keypool"
$env:KEYPOOL_PROJECT = "mathlab"
$env:KEYPOOL_ALIAS = "prover"
python integrations/mathlab/smoke.py
```

O smoke test le uma unica credencial do ambiente (`KEYPOOL_API_KEY`); nao aceita listas nem segredo em argumento de processo. O modo padrao faz apenas consultas de health/readiness/models/capacity. Para enviar uma chamada real ao provedor, opte explicitamente por `--chat` ou `--stream`:

```powershell
python integrations/mathlab/smoke.py --chat
python integrations/mathlab/smoke.py --stream
```

O script nao imprime a API key, prompts, respostas nem corpos de erro upstream. Um HTTP 429 acompanhado de `Retry-After` e reportado como limite temporario esperado, nao como falha do contrato.
