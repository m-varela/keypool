## 0.4.2 - 2026-11-14

- Adiciona `policy.attempt_timeout`, limite genérico por tentativa upstream não-stream, mantendo o prazo global da requisição disponível para fallback.
- O roteador conserva destinos sem capacidade imediata após os disponíveis; falha de fila ou timeout de uma tentativa não remove os demais provedores da cadeia.
- Em rotas com spill, uma falha transitória em candidato imediato pode avançar para candidato que precise aguardar capacidade, dentro do orçamento de fila/requisição.
- Expõe o parse validado de `attempt_timeout` por projeto e cobre timeout individual, fallback após saturação de fila e fallback de upstream lento.
- Não adiciona política específica de MathLab, escolha de modelo ou conhecimento de tarefas/domínios ao KeyPool.

## 0.4.1 - 2026-11-13

- Adiciona suporte genérico a `limits.rpd_reset_timezone` para quotas diárias alinhadas a fuso IANA.
- Ajusta a expiração de RPD à próxima meia-noite local, respeitando transições de horário de verão.
- Inclui referência de cotas do Gemini 3.8 Flash no nível gratuito padrão (5 RPM, 250.000 TPM, 20 RPD), com o contador diário em `America/Los_Angeles`.
- A atualização do provedor Gemini preserva as credenciais já registradas.

## Unreleased — adaptadores para modelos gratuitos OpenRouter do MathLab

- Preset OpenRouter reconhece os padrões Nemotron 3 Ultra, Nemotron 3.5 Super VL e ERNIE 5.5 Flash usados nas cadeias gratuitas do MathLab.
- Adicionado teste que verifica a conversão de `reasoning_effort` para `reasoning.effort` nos IDs `:free` dessas famílias.
- A seleção estrita de modelos gratuitos vive no perfil do consumidor MathLab (`integrations/keypool/project.json`); os presets globais continuam reutilizáveis por outros projetos.

# Changelog

## Unreleased — diagnóstico genérico de erros

- O prazo local de espera por capacidade agora é distinguido do timeout total mesmo quando `Pool.Acquire` observa o contexto filho já expirado e retorna `context.DeadlineExceeded`; a classificação de fila permanece HTTP 429 desde que o contexto global ainda esteja ativo.
- Regressão coberta por teste HTTP, repetido 10 vezes sem falha e executado também sob detector de corrida.
- `Router.Do` preserva falhas tipadas com fase, alvo upstream conhecido, total de tentativas e fallback; o gateway acrescenta campos/headers opcionais e seguros sem remover o contrato legado.
- Métricas de falhas por fase/código, tentativas preservadas e timeouts por fase, sem labels de alta cardinalidade.

- Respostas de erro do gateway agora incluem `X-Keypool-Error-Code`, espelhando o código estável do envelope JSON; `X-Keypool-Request-Id` permanece disponível para correlação.
- Nenhuma política de retry/circuit breaker do consumidor foi adicionada ao KeyPool.
