# Operacao do KeyPool no Windows

Este guia descreve a instalacao local, execucao, tarefa agendada, observabilidade e backup. O KeyPool foi projetado para escutar somente em loopback (`127.0.0.1`); nao publique a porta do gateway na rede.

## 1. Requisitos

- Windows 10/11 ou Windows Server suportado pelo Go usado para compilar.
- Go 1.26 ou superior para compilar a versao atual.
- PostgreSQL 16 ou superior para modo persistente. O modo `serve --dev` usa memoria e nao sobrevive ao encerramento.
- PowerShell 5.1 ou superior e modulo `ScheduledTasks` para a tarefa de logon.

## 2. Build e testes

Na raiz do repositório:

```powershell
Set-ExecutionPolicy -Scope Process Bypass
.\scripts\windows\build.ps1
.\scripts\windows\test.ps1
.\scripts\windows\test.ps1 -Integration
```

A integração PostgreSQL exige `KEYPOOL_TEST_DATABASE_URL` apontando para uma base de teste descartável. Execute `setup-db.ps1` apenas após revisar os parâmetros do ambiente.

## 3. Preparar a instalação persistente

1. Defina `KEYPOOL_DATABASE_URL` para a base KeyPool e `KEYPOOL_API_KEY` para o token administrativo gerado no init. Use variáveis de ambiente do usuário ou um mecanismo corporativo de segredos; não coloque tokens em argumentos de linha de comando ou no YAML.
2. Execute `keypoold migrate`.
3. Execute `keypoold init`. O comando cria o arquivo de master key se ele não existir e exibe a API key apenas uma vez.
4. Guarde a API key e faça backup separado da master key. Sem a master key, as credenciais cifradas no PostgreSQL não podem ser recuperadas.
5. Execute `keypoold serve` e verifique `http://127.0.0.1:8787/healthz` e `/readyz` com autenticação.

Comandos de referência:

```powershell
.\bin\keypoold.exe migrate
.\bin\keypoold.exe init
.\bin\keypoold.exe doctor
.\bin\keypoold.exe serve
```

A porta e o endereço são definidos pela configuração; o daemon rejeita listen fora de loopback.

## 4. Master key com DPAPI (opcional)

No Windows, a master key pode ser protegida pelo DPAPI do **usuário atual**. O arquivo contém o blob cifrado, não a chave em texto base64. O modo DPAPI e `KEYPOOL_MASTER_KEY` são mutuamente exclusivos. A opção DPAPI vale somente para o modo persistente PostgreSQL, não para `serve --dev`.

```powershell
.\bin\keypoold.exe init --master-key-dpapi
.\bin\keypoold.exe doctor --master-key-dpapi
.\bin\keypoold.exe serve --master-key-dpapi
```

Use o mesmo usuário Windows para init, doctor e execução agendada. DPAPI de usuário vincula o material ao perfil/credenciais desse usuário; mover somente o arquivo para outro usuário ou máquina não é uma estratégia de recuperação. Faça backup coordenado do banco e mantenha um plano de recuperação testado. O modo padrão continua sendo arquivo base64 com ACL restrita ou `KEYPOOL_MASTER_KEY`.

## 5. Tarefa agendada ao fazer logon

Compile e coloque `keypoold.exe` em `keypool\bin`. Defina as variáveis necessárias no ambiente do usuário e instale a tarefa:

```powershell
.\scripts\windows\install-task.ps1 -Executable .\bin\keypoold.exe -Arguments "serve"
```

Para iniciar usando uma configuração importada no primeiro boot do banco:

```powershell
.\scripts\windows\install-task.ps1 -Executable .\bin\keypoold.exe -Arguments 'serve --config "C:\KeyPool\keypool.yaml"'
```

A tarefa usa o gatilho `AtLogOn`, contexto interativo do usuário atual, nível `Limited`, sem elevação e opção `Hidden`. `IgnoreNew` evita múltiplas instâncias concorrentes; falhas de inicialização têm até cinco reinícios com intervalo de um minuto. O diretório de trabalho padrão é a raiz do repositório e pode ser alterado por `-WorkingDirectory`. A tarefa não armazena a API key ou senha do banco nos argumentos; ela depende do ambiente do usuário. Após instalar, confira em Agendador de Tarefas se a ação, diretório de trabalho e usuário estão corretos.

Remover:

```powershell
.\scripts\windows\install-task.ps1 -TaskName KeyPool -Remove
```

Para executar manualmente durante diagnóstico, use `keypoold serve` em um terminal e acompanhe os logs.

## 6. Painel e métricas

Com o serviço ativo, abra `http://127.0.0.1:8787/` no navegador. O painel responde ao desafio HTTP Basic: use usuário `keypool` e a API key como senha. O navegador mantém a autenticação para os links do mesmo host. Clientes de API e coletores devem preferir `Authorization: Bearer <KEYPOOL_API_KEY>` ou `X-API-Key`. O painel é somente leitura, embutido no binário e não baixa recursos externos. `GET /metrics` usa o formato Prometheus 0.0.4 e requer a mesma autenticação.

Exemplo de coleta local:

```powershell
curl.exe -H "Authorization: Bearer $env:KEYPOOL_API_KEY" http://127.0.0.1:8787/metrics
# Painel no browser: usuario "keypool" e KEYPOOL_API_KEY como senha no prompt Basic.
```

As métricas são agregadas em memória e reiniciam com o processo. `usage_dropped_total` espelha a fila de escrita local; contadores de requisição, tentativa e lease são contadores de processo. As séries por chave usam o ID/label interno, nunca o segredo.

## 7. Backup e recuperação

- Faça backup consistente do PostgreSQL e da master key correspondente.
- Proteja os backups com ACL e criptografia fora do KeyPool.
- Não versione `.env`, arquivos de master key, dumps com dados reais ou logs contendo tokens.
- Teste restauração em ambiente isolado, incluindo `keypoold doctor` e uma chamada de modelo não sensível.
- Em DPAPI, preserve o contexto de usuário Windows que pode descriptografar o blob.

## 8. Diagnóstico rápido

- `/healthz` confirma que o processo responde; não garante prontidão.
- `/readyz` indica se a primeira passada L1 terminou ou se o timeout de prontidão liberou o gateway com aviso.
- `keypoold doctor` verifica PostgreSQL, migrações, master key, API key e endereço.
- `GET /admin/v1/stats` detalha provedores, aquecimento e filas.
- `GET /metrics` fornece séries para Prometheus.

Não desabilite autenticação nem altere o listener para `0.0.0.0` para contornar um problema local. Investigue a configuração, credenciais e permissões.
