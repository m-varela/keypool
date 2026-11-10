# Modelo de segurança e operação segura

## Fronteiras de confiança

- O KeyPool é um serviço local. O listener deve permanecer em loopback; `keypoold` rejeita endereços que não sejam loopback.
- A API key `kp_...` autentica clientes e administração. O checkpoint guarda apenas seu hash SHA-256 e metadados não secretos.
- Credenciais de provedores são cifradas com AES-256-GCM, com AAD vinculada ao ID e ao provedor. A master key deve ser protegida e guardada junto do backup de `state.json`.
- O passthrough `generic_http` só alcança provedores configurados e autorizados para o projeto. Credenciais e cabeçalhos internos recebidos do cliente são removidos/substituídos.

## Segredos e logs

- Não coloque credenciais em arquivos versionados, argumentos de processo, tickets ou logs.
- Prefira `keypoolctl key import-env` e `keypoolctl key add --stdin` para importar segredos sem argumentos.
- Prompts, respostas e corpos brutos de provedores não são gravados. Logs e eventos não devem conter corpo de requisição, corpo de resposta, Authorization ou segredo.
- `/metrics`, painel e `/readyz` exigem API key. O painel aceita HTTP Basic (usuário literal `keypool`, API key como senha). Não exponha esses endpoints sem camada de autenticação equivalente.
- Métricas usam labels operacionais (projeto/provedor/modelo/ID/estado), nunca valores de segredo, conteúdo de prompt ou identificadores ilimitados.

## Master key e checkpoint

A master key pode vir de `KEYPOOL_MASTER_KEY` ou do arquivo indicado por `KEYPOOL_MASTER_KEY_FILE` (padrão `master.key` dentro de `KEYPOOL_DATA_DIR`). Arquivos Unix são criados com permissão `0600`; no Windows aplique ACL restritiva ao diretório de dados com `scripts/windows/acl-master-key.ps1`. `--master-key-dpapi` é opcional no Windows e usa o perfil do usuário atual. DPAPI e `KEYPOOL_MASTER_KEY` são mutuamente exclusivos.

`state.json` contém a configuração, chaves cifradas, hash da API key, snapshots e contadores diários. O histórico detalhado é volátil. A gravação é atômica e uma falha não publica a nova versão na memória. O `commitMu` serializa mutações duráveis e o I/O do checkpoint; leituras e caminho quente não o adquirem.

Faça backup consistente de `state.json` **e** da master key. Perder a master key torna as credenciais cifradas irrecuperáveis. No modo DPAPI, o arquivo só pode ser aberto pelo contexto de usuário compatível.

## Rede e acesso

- Escute somente `127.0.0.1`/loopback.
- Não crie port-forward, regra de firewall ou proxy público sem autenticação e TLS apropriadas.
- A tarefa agendada é criada para o usuário atual, com nível `Limited` e sem elevação.
- Revise scripts PowerShell antes de executá-los.

## Resposta a incidentes

1. Revogue/rotacione a API key se houver suspeita de vazamento.
2. Desabilite ou remova a credencial do provedor afetado e execute L1 de revalidação após a rotação.
3. Preserve eventos e logs operacionais; não colete prompts ou corpos por padrão.
4. Se a master key for perdida, recadastre as credenciais de provedores.
5. Preserve e teste a restauração de `state.json` e da master key em ambiente isolado.
