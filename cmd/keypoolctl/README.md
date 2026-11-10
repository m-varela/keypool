# keypoolctl

Cliente da API administrativa KeyPool. Configure `KEYPOOL_URL` (padrão `http://127.0.0.1:8787`) e `KEYPOOL_API_KEY`.

Comandos: `status`, `provider list|show|add|enable|disable`, `key list|add|import-env|enable|disable|tag|rm|test|reveal`, `project apply|list|show`, `apikey info|rotate`, `usage`, `events`, `warm run|status` e `capacity`.

`key add` lê o segredo somente com `--stdin`; `key import-env` importa `.env` segundo a mesma ordem/deduplicação do bootstrap; `key reveal` lê o `state.json` local com a master key e exige confirmação interativa; a operação não usa a API. `key test` força uma L1; `warm run` executa L0/L1 e `warm status` mostra prontidão, fila e saúde por chave.


A saída padrão é tabular para listagens/estatísticas e chave-valor para objetos.
Acrescente `--json` antes ou depois do comando para obter JSON estruturado, por exemplo
`keypoolctl --json status`, `keypoolctl key list --json` ou
`keypoolctl usage --since 24h --json`. O status em JSON é um único objeto com
`stats`, `providers`, `keys` e `projects`. Operações que devolvem uma credencial
de uso único (como `apikey rotate`) preservam a resposta JSON mesmo sem `--json`;
trate essa saída como segredo e não a registre em logs.

O filtro `key list --provider NOME` restringe a listagem ao provedor solicitado. Em saída tabular, campos de credencial/segredo são omitidos; `--json` deve ser usado com cuidado porque mostra a resposta integral.
