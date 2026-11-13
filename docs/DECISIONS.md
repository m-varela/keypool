# Decisões de implementação — falhas estruturadas de rota

Data: 2026-11-13.

1. **Contrato aditivo:** `RouteFailure` é um erro genérico de infraestrutura. O envelope antigo permanece; fase, alvo, tentativas, fallback e retryability são campos adicionais opcionais.
2. **Sem retry do consumidor:** KeyPool continua dono dos retries/fallbacks da cadeia upstream. O contrato apenas informa o que ocorreu.
3. **Retryability conservadora:** `retryable` e `Retry-After` só são enviados quando há conhecimento confiável. Queue timeout é conhecido como retryable; timeout upstream, por si só, não autoriza essa conclusão.
4. **Alvo sanitizado:** publicar somente `provider:model`/target conhecido após redaction; nunca incluir chaves, prompts ou corpos upstream.
5. **Métricas com cardinalidade limitada:** labels restritas a fase e código estáveis. Request IDs e mensagens são excluídos.
6. **Compatibilidade:** preservar `X-Keypool-Error-Code`, `X-Keypool-Request-Id`, códigos HTTP existentes e `{error:{type,message,code}}`.
7. **Toolchain:** não alterar `go.mod` para acomodar o ambiente. Diagnósticos com Go 1.23 usam modfile externo e não equivalem à validação requerida em Go 1.26.
