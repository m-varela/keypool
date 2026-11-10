# Verificação do KeyPool

Execute os comandos na raiz do repositório com Go 1.26 ou superior. O projeto não usa módulos externos; `go.mod` deve conter somente o módulo e a versão do Go, e não deve existir `go.sum`.

## Verificação local

```powershell
gofmt -l .
go vet ./...
go build ./...
go test ./...
go test -race ./internal/sched/... ./internal/router/... ./internal/gateway/... ./internal/store/... ./cmd/...
$env:GOPROXY = "off"
go build ./...
go test ./...
go list -m all
grep -rn "yaml" --include=*.go .
```

`gofmt -l .` e o grep por YAML no código Go devem produzir saída vazia. `go list -m all` deve listar somente `keypool`. A verificação de resíduos deve apontar somente os documentos históricos de decisões/status/plano e a cópia integral da especificação; não devem restar referências operacionais no código, scripts ou documentação de uso.

## Execução e bootstrap

Na primeira execução, defina a configuração JSON e execute `keypoold serve --config <arquivo.json>`. O processo faz bind TCP antes da primeira gravação, inicializa `state.json` e, se `KEYPOOL_API_KEY` não foi definida, imprime a chave gerada uma única vez. Nas execuções seguintes, configure `KEYPOOL_API_KEY`; o daemon valida o hash persistido e não relê a configuração. Consulte `docs/ESTADO_LOCAL.md` para backup e restauração.

## Testes offline e integração

A suíte usa `httptest`, fake upstream, `clock.Fake` e diretórios temporários. Nenhum teste deve acessar provedores reais. O E2E de `cmd/keypoold` cobre bootstrap, autenticação HTTP, bind exclusivo, gravação administrativa, reinício e restauração do checkpoint.

No Windows, execute `scripts/windows/test.ps1`; acrescente `-Race` quando o ambiente tiver CGO e compilador C. Os scripts PowerShell devem permanecer ASCII puro. A validação nativa de DPAPI, ACLs e Agendador de Tarefas precisa ser feita em Windows.

## Resultado observado em 10/11/2026

O ambiente de implementação disponibilizou Go 1.23.2, inferior ao `go 1.26` declarado pelo projeto, e não conseguiu baixar a toolchain por indisponibilidade de DNS/rede. Para testar o código sem alterar `go.mod`, foi usado `/tmp/keypool-check.mod`, externo ao repositório, contendo somente `module keypool` e `go 1.23`. Com `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off`, `go test ./...`, `go vet ./...`, `go build ./...` e a suíte `-race` dos pacotes indicados passaram usando esse modfile temporário. Isso valida o código com o Go disponível, mas não substitui a execução oficial em Go 1.26.

A medição do ring de 50.000 eventos foi de 11,10 MiB, abaixo do limite aproximado de 25 MiB. Os benchmarks curtos e as limitações de comparação estão detalhados em `RELATORIO_REFATORACAO_F10.md`. A validação Windows/DPAPI e uma aceitação com provedores reais não foram executadas.
