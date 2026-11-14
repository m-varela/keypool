# Verificação — diagnóstico estruturado de falha de rota

Data: 2026-11-13. Os comandos são executados offline e não chamam provedores reais.

## Ambiente

- Toolchain disponível: Go 1.23.2.
- Manifesto original: `go 1.26` (preservado sem alteração).
- Para diagnóstico, foi usado `/tmp/keypool-dev.mod`, modfile temporário externo com diretiva `go 1.23`. Esse arquivo não é entregue no repositório.

## Resultados executados

- `gofmt -l` nos arquivos Go tocados — sem saída; arquivos modificados formatados.
- `GOTOOLCHAIN=local go test -modfile=/tmp/keypool-dev.mod ./...` — aprovado em todas as packages.
- `GOTOOLCHAIN=local go vet -modfile=/tmp/keypool-dev.mod ./...` — aprovado.
- `GOTOOLCHAIN=local go build -modfile=/tmp/keypool-dev.mod ./...` — aprovado.
- `GOTOOLCHAIN=local go test -race -modfile=/tmp/keypool-dev.mod ./internal/router ./internal/gateway ./cmd/keypoold` — aprovado.
- `GOTOOLCHAIN=local go test ./...` com o `go.mod` original — não executável: `go.mod requires go >= 1.26 (running go 1.23.2)`.

O `gofmt -l .` global ainda lista `cmd/keypoolctl/main.go` e `internal/admin/admin.go`, dois arquivos preexistentes não tocados nesta alteração, por causa de terminações CRLF. Eles não foram reformatados para evitar um diff não relacionado que mudaria o arquivo inteiro.

## Limites

A aprovação com modfile temporário detecta regressões nas packages e no build sob Go 1.23, mas não substitui a matriz oficial em Go 1.26. Testes Windows/DPAPI e integração com provedores reais não fazem parte desta execução. Não houve chamadas de rede nem consumo de créditos.


## Verificação 0.4.2 — fallback e timeout por upstream

Data: 2026-11-14. O host disponibiliza Go 1.23.2; `go.mod` original declara Go 1.26 e foi preservado. Com modfile externo temporário `/tmp/keypool-final.mod` declarando Go 1.23.2:

- `GOTOOLCHAIN=local go test -modfile=/tmp/keypool-final.mod ./...` — aprovado em todas as packages.
- `GOTOOLCHAIN=local go vet -modfile=/tmp/keypool-final.mod ./...` — aprovado.
- `GOTOOLCHAIN=local go build -modfile=/tmp/keypool-final.mod ./...` — aprovado.
- `GOTOOLCHAIN=local go test -race -modfile=/tmp/keypool-final.mod ./internal/router ./internal/config` — aprovado.

Os testes novos simulam fila saturada e um upstream que excede `attempt_timeout`; não fazem chamadas externas. Esta verificação com Go 1.23.2 não substitui execução oficial com Go 1.26.


## Validação das correções de recuperação — 14/11/2026

Toolchain nativo Go 1.26.0, linux/amd64; `go.mod` preservado.

- `gofmt` nos arquivos alterados; `git diff --check`: aprovados.
- `go test ./...`, `go vet ./...`, `go build ./...`: aprovados.
- `go test -race ./internal/router ./internal/gateway ./internal/sched`: aprovado.
- `go test ./internal/sched -run '^$' -bench . -benchtime=100ms`: aprovado; medição local sem comparação de baseline e sem representar latência de provedores.
- Regressões: upstream lento com duas chaves deve ceder ao fallback em duas tentativas totais; timeout de fila preserva estimativa conhecida; teste com `clock.Fake` confirma 17s restantes após cooldown de 20s e avanço de 3s, e ausência de estimativa para concorrência ocupada sem prazo conhecido.

Não executados: integração com provedores reais, Windows/DPAPI e carga end-to-end. `docs/PLANO.md` não está presente no ZIP recebido; as mudanças corrigem o comportamento existente sem redesenhar a arquitetura nem criar campos de API.
