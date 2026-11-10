# PostgreSQL local — F4

## Requisitos

- PostgreSQL 16 ou superior em execucao local.
- Go 1.26 ou superior.
- `psql` no `PATH` (somente para `scripts/windows/setup-db.ps1`).

## Preparar banco no Windows

Em PowerShell, a partir da raiz do repositorio:

```powershell
.\scripts\windows\setup-db.ps1
$env:KEYPOOL_DATABASE_URL = "postgres://keypool:SUA_SENHA@127.0.0.1:5432/keypool?sslmode=disable"
```

O script cria o papel e o banco se ainda nao existirem. A senha da conta administrativa e solicitada como `SecureString` e usada apenas para executar `psql`; a senha do papel `keypool` e definida pelo prompt interativo `\password`. Nao passe senhas em argumentos de linha de comando.

## Inicializar KeyPool

```powershell
go mod tidy
go run ./cmd/keypoold migrate
go run ./cmd/keypoold init
```

`init` imprime uma nova API key `kp_...` uma unica vez. Guarde-a em `KEYPOOL_API_KEY`. Por padrao, a master key e criada em `%LOCALAPPDATA%\keypool\master.key`; proteja a ACL com:

```powershell
.\scripts\windows\acl-master-key.ps1
```

Tambem e possivel fornecer `KEYPOOL_MASTER_KEY` como base64 de exatamente 32 bytes; nessa forma, `init` nao cria arquivo. Nunca altere nem perca a master key depois de gravar credenciais cifradas.

## Importar configuracao e executar

Defina as variaveis dos provedores antes de importar o YAML de exemplo. O atalho `scripts/windows/run-dev.ps1` e apenas para modo em memoria (`serve --dev`); o servidor persistente usa o comando abaixo:

```powershell
$env:GROQ_API_KEYS = "gsk_..."
$env:OPENROUTER_API_KEY = "sk-or-..."
go run ./cmd/keypoold serve --config ./configs/examples/keypool.dev.yaml
```

O arquivo YAML e importado apenas se o banco ainda nao tiver configuracao. Depois da importacao, as credenciais sao lidas do PostgreSQL e decifradas somente apos validar `master_key_check`. O daemon exige `KEYPOOL_API_KEY`, mantem o servidor em loopback, restaura snapshots antes de aceitar trafego, salva snapshots alterados aproximadamente a cada 10 segundos, grava uso em INSERT multi-row por lote, persiste eventos operacionais em fila separada e libera a trava consultiva no encerramento.

Comandos operacionais:

```powershell
go run ./cmd/keypoold doctor
go run ./cmd/keypoold migrate
go run ./cmd/keypoold version
```

`doctor` verifica conectividade, desvio estimado do relogio do banco, checksums das migracoes, master key, metadados da API key e endereco local. `migrate` e idempotente e rejeita checksum divergente em migracoes ja aplicadas.

## Teste de integracao PostgreSQL

Use um banco descartavel e uma conta capaz de criar schemas:

```powershell
$env:KEYPOOL_TEST_DATABASE_URL = "postgres://keypool:SUA_SENHA@127.0.0.1:5432/keypool_test?sslmode=disable"
go test -tags integration ./internal/store/pgstore
# ou
.\scripts\windows\test.ps1 -Integration
```

O teste cria um schema aleatorio, aplica as migracoes e remove o schema no cleanup. Nao aponte essa variavel para banco de producao.
