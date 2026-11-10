# Convencoes para agentes de codigo

1. Leia `docs/PLANO.md` por inteiro antes de alterar arquitetura ou contratos.
2. Leia `STATUS.md` e `docs/DECISIONS.md`; atualize ambos quando o escopo mudar.
3. Preserve o limite de Go 1.26 e use somente a biblioteca padrao; nao adicione modulos Go externos.
4. Comentarios, mensagens de erro, CLI e documentacao em portugues (pt-BR); identificadores Go em ingles. Scripts PowerShell devem ser ASCII puro.
5. Nunca registre Authorization, segredo, prompt, completion ou corpo bruto de provedor. Segredos ficam em `secret.Secret`; `Reveal` somente nos adaptadores ao montar requisicoes.
6. Logica temporal recebe `clock.Clock`; nao introduza `time.Sleep`, `time.After` ou `time.Now` diretamente nos pacotes de nucleo.
7. Nao mantenha `mu`/`usageMu` durante I/O, espera ou callback. `commitMu` pode serializar mutacoes duraveis e gravacao do checkpoint, conforme D6 em `docs/DECISIONS.md`.
8. Adicione testes offline com `clock.Fake` e `httptest`; nao acesse provedores reais.
9. Ao portar comportamento Python, cite o modulo/metodo de origem no comentario e preserve a semantica observavel. Registre desvios em `docs/DECISIONS.md`.
10. Antes de concluir: `gofmt`, `go vet ./...`, `go build ./...`, `go test ./...`; para scheduler rode `go test -race` e benchmarks. Consulte `docs/VERIFY.md`.
11. Nao implemente fases posteriores sem atualizar o escopo e o estado em `STATUS.md`.
