# Estado local e recuperação

## Diretório de dados

O KeyPool usa um único diretório de dados, definido por `KEYPOOL_DATA_DIR`. Sem essa variável, o padrão é `%LOCALAPPDATA%\keypool` no Windows, `$XDG_CONFIG_HOME/keypool` quando definida, ou `~/.keypool`.

Arquivos esperados:

- `master.key`: master key em base64 com 32 bytes, salvo com permissão restrita; com `--master-key-dpapi`, contém o blob DPAPI do usuário Windows atual.
- `state.json`: checkpoint versionado e gravado atomicamente.
- `state.json.tmp`: arquivo temporário durante a substituição atômica.

`Open` apenas lê o checkpoint. Não cria nem altera arquivos. Na primeira execução, o daemon prepara a configuração, valida o snapshot e obtém o listener TCP loopback antes de gravar a master key ou inicializar `state.json`. Isso evita deixar um estado parcial quando a porta já está ocupada.

## Conteúdo do checkpoint

`state.json` contém apenas o estado necessário à correção após reinício: configuração, credenciais de provedores cifradas por AES-256-GCM, hash SHA-256 e metadados da API key, snapshots de janelas de limite e contadores diários de tokens.

O arquivo não contém texto claro das credenciais de provedores nem da API key local. `Raw.ProjectFile` é usado na leitura da configuração inicial e não é persistido. Histórico detalhado de uso, eventos e saúde fica em rings limitados em memória e é perdido no reinício.

A gravação segue `state.json.tmp` → `fsync` → rename, com tentativas curtas para falhas transitórias de substituição. Se uma mutação não conseguir gravar o checkpoint, o estado em memória permanece inalterado.

## Primeira execução

1. Defina as variáveis `<PREFIXO>_API_KEY(S)` necessárias e, opcionalmente, `KEYPOOL_API_KEY`.
2. Execute `keypoold serve --config configs/examples/keypool.json` em um terminal.
3. Se a API key não foi fornecida, copie a chave `kp_...` impressa uma única vez e salve-a em local seguro.
4. Nas execuções seguintes, defina `KEYPOOL_API_KEY` com a chave inicial ou a chave rotacionada e execute apenas `keypoold serve`.

Depois do bootstrap, `--config` é ignorado. Faça alterações de configuração por `keypoolctl project apply -f arquivo.json` ou pela API administrativa. Para importar credenciais adicionais use `keypoolctl key import-env` ou `keypoolctl key add --stdin`.

## Backup e restauração

Pare o daemon antes de copiar os arquivos. Faça backup de `state.json` e da master key correspondente, mantendo-os juntos e protegidos. Copiar apenas `state.json` não permite recuperar as credenciais cifradas. No modo DPAPI, a restauração depende do mesmo perfil/usuário Windows capaz de descriptografar o blob.

Para restaurar, coloque os arquivos no diretório definido por `KEYPOOL_DATA_DIR`, restaure a master key compatível, defina `KEYPOOL_API_KEY` e execute `keypoold doctor` antes de iniciar o daemon.

Não existe migração automática dos dados históricos de instalação antiga. Antes de substituir uma instalação que ainda guarda credenciais noutro backend, exporte-as com a versão antiga e cadastre-as na nova instalação.

## Como zerar o estado

1. Pare o daemon.
2. Faça backup dos arquivos caso precise deles depois.
3. Apague `state.json` e `state.json.tmp` do diretório de dados.
4. Preserve `master.key` para reutilizar a mesma master key, ou remova-a conscientemente para gerar outra no próximo bootstrap.
5. Execute `keypoold serve --config <arquivo.json>` para iniciar um novo estado.

Apagar apenas o checkpoint remove configuração, chaves cadastradas, API key, snapshots e contadores diários. A master key antiga não restaura os dados removidos.

## Limites de retenção em memória

- Uso: até 50.000 eventos ou 7 dias.
- Eventos do scheduler/gateway: até 10.000 eventos ou 30 dias.
- Saúde: até 1.000 resultados ou 7 dias.

O histórico não sobrevive ao encerramento. Os contadores diários de orçamento de projeto e tokens warm L2 são persistidos separadamente no checkpoint.
