# Changelog

## Unreleased — diagnóstico genérico de erros

- Respostas de erro do gateway agora incluem `X-Keypool-Error-Code`, espelhando o código estável do envelope JSON; `X-Keypool-Request-Id` permanece disponível para correlação.
- Nenhuma política de retry/circuit breaker do consumidor foi adicionada ao KeyPool.
