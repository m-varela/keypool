# Auditoria final F10 — KeyPool sem banco externo

## Escopo entregue

- Execução única por `keypoold serve`; bootstrap automático na primeira execução, com `net.Listen` antes da primeira escrita.
- `memstore.Open` lê `state.json`; mutações duráveis usam checkpoint temporário, `fsync`, rename e publicação de memória somente após sucesso.
- Credenciais são cifradas com AES-256-GCM; a API key é persistida somente por hash SHA-256.
- Uso/eventos/saúde têm rings limitados; contadores diários e snapshots de limites são persistidos.
- Configuração, presets e `keypoolctl project apply` usam JSON; removidos parser YAML e módulos Go externos.
- Removidos os diretórios e scripts do backend anterior, os writers de usage/events e os comandos auxiliares de inicialização/migração.

## Evidência de testes

O E2E offline `TestAppBootstrapRestartAndAdminKeyPersistence` cobre primeiro bootstrap, resposta do fake upstream, bind exclusivo, criação de chave via admin, restauração no reinício, contadores diários, snapshot de limite, rejeição de API key errada e ausência de segredos em texto claro no checkpoint.

A regressão dos defaults confirmou o problema anterior: serializar strings YAML e inserir o resultado diretamente como `json.RawMessage` poderia produzir JSON inválido. `json.Marshal` gera agora o valor JSON válido e o teste correspondente permanece na suíte.

## Limitações conhecidas desta entrega

O ambiente fornece Go 1.23.2, não Go 1.26. A tentativa de baixar Go 1.26 falhou por indisponibilidade de DNS/rede. Os testes e vet foram executados com um modfile temporário externo que declara Go 1.23, sem alterar o `go.mod` final; isso não comprova compatibilidade/aceite com Go 1.26. PowerShell, execução nativa Windows/DPAPI e branch/commits Git não puderam ser verificados: o ZIP não contém metadados `.git`.

A aceitação de integração real com provedores, Mathlab e Windows continua externa. A cópia integral da especificação foi mantida em `docs/`; `docs/PLANO.md` também é mantido como plano histórico, conforme solicitado.

Veja `docs/VERIFY.md` para o resultado discriminado de cada comando.
