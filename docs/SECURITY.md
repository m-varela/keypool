# Modelo de segurança e operação segura

## Fronteiras de confiança

- O KeyPool é um serviço local. O listener deve permanecer em loopback; `keypoold` rejeita endereços que não sejam loopback.
- A API key `kp_...` autentica clientes e administração. O banco guarda seu hash SHA-256, não o token em claro.
- Credenciais de provedores são cifradas com AES-256-GCM, com AAD vinculada ao ID e ao provedor. A master key precisa ser protegida e mantida separada do backup do banco.
- O passthrough `generic_http` só alcança provedores configurados e autorizados para o projeto. Credenciais e cabeçalhos internos recebidos do cliente são removidos/substituídos.

## Segredos e logs

- Não coloque credenciais em YAML versionado, argumentos de processo, tickets ou logs.
- Prefira `keypoolctl key import-env`/`key add --stdin` para importar segredos sem argumentos.
- Prompts e respostas não são gravados. Os logs e eventos não devem conter corpo de requisição, corpo de resposta ou segredo.
- `/metrics`, painel e `/readyz` exigem API key. O painel aceita HTTP Basic (usuário literal `keypool`, API key como senha) para que o navegador possa autenticar links; Bearer e `X-API-Key` continuam para clientes. Não exponha esses endpoints sem camada de autenticação equivalente.
- Métricas usam labels operacionais (project/provider/model/key ID/state), nunca valores de segredo, conteúdo de prompt ou identificadores de request ilimitados.

## Master key

Modo padrão: arquivo base64 com permissões restritas (`0600` em Unix e ACL restritiva no Windows) ou `KEYPOOL_MASTER_KEY` via ambiente. No Windows, `--master-key-dpapi` é opcional e usa DPAPI no escopo do usuário atual; o blob não pode ser descriptografado por outro usuário. Não misture DPAPI com `KEYPOOL_MASTER_KEY`.

DPAPI não substitui backup nem gestão de identidade. Uma restauração em outro perfil/host pode exigir migração de master key antes de restaurar credenciais. Documente e ensaie recuperação antes de adotar o modo DPAPI em produção.

## Rede e acesso

- Escute somente `127.0.0.1`/loopback.
- Não crie port-forward, regra de firewall ou proxy público sem uma camada de autenticação e TLS apropriada.
- A tarefa agendada é criada para o usuário atual, com nível `Limited` e sem elevação.
- Não execute scripts PowerShell de terceiros sem revisão. Revise `install-task.ps1` antes de registrar a tarefa.

## Operação e resposta a incidentes

1. Revogue/rotacione a API key se houver suspeita de vazamento.
2. Desabilite ou remova a credencial do provedor afetado e faça L1 de revalidação após rotação.
3. Preserve eventos, logs e horário do incidente; não colete prompts ou corpos por padrão.
4. Se a master key for perdida, credenciais cifradas não podem ser recuperadas; será necessário reimportá-las.
5. Faça backup consistente de banco e master key, proteja os artefatos e teste restauração isolada.
