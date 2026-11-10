# Auditoria final KeyPool — 10/11/2026

## Resultado

F0–F9 têm implementação de código distribuída entre os repositórios KeyPool e Mathlab. A F9 foi aplicada no código dos dois projetos. Isto não equivale à aceitação E2E: nenhum servidor KeyPool, PostgreSQL real, credencial ou provedor upstream estava disponível neste host.

## Corrigido nesta rodada

- CLI `keypoolctl`: saída tabular/chave-valor por padrão; `--json` global antes ou depois do comando; `status --json` agregado; filtro `key list --provider`; colunas sensíveis omitidas da tabela; respostas com `api_key` de rotação preservadas em JSON de uso único.
- SSE: `Reader.NextRaw`/`Reset` reutilizam buffers; o gateway usa o caminho sem cópia por evento.
- Teste de lease/watchdog: sem `time.Sleep`, usa clock fake e sincronização por evento.
- Integração Mathlab: status e decisões sincronizados com a alteração real no repositório externo; contrato de credencial singular documentado; kit/smoke usa `KEYPOOL_API_KEY`.

## Verificações locais

- Go: suíte interna sem `pgstore` passou, exceto um teste YAML que não é suportado pelo stub temporário (`yaml.Node.Decode`); o teste de saída CLI passou; race passou em SSE/sliding/scheduler/router/gateway/warm/memstore; `go vet` e `gofmt` limpos; smoke kit: 6/6.
- Desempenho: `AcquireSuccess` 8,362 ns/12 allocs, `AcquireContended` 5,665 ns/15 allocs, `WaitForRoom` 399.3 ns/0 allocs e `SSEForward` 917.1 ns/0 allocs; a meta Acquire `<5 µs`/`≤2 allocs` continua pendente. Ver `docs/VERIFY.md`.

## Fora da capacidade deste host

Build integrado com Go 1.26, dependências reais (`pgx`, `yaml.v3`), PostgreSQL 16+, execução Windows/PowerShell/DPAPI/Task Scheduler, integração HTTP real Mathlab→KeyPool→upstream e testes com credenciais reais.
