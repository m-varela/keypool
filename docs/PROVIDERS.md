# Provedores: configuração somente por YAML

O KeyPool permite adicionar configurações de provedor sem alterar código quando o protocolo é OpenAI-compatible ou passthrough HTTP genérico (`generic_http`). Um protocolo proprietário novo exige um adaptador Go; YAML não converte automaticamente uma API arbitrária em chat OpenAI.

## 1. Provedor OpenAI-compatible

```yaml
providers:
  meu-provedor:
    type: openai_compatible
    enabled: true
    base_url: https://api.example.invalid/v1
    auth:
      scheme: bearer
    paths:
      chat: /chat/completions
      models: /models
    timeout: 90s
    env_prefix: MEU_PROVEDOR
    key_strategy: round_robin
    limit_scope: auto
    limits:
      default_tpm: 60000
      default_rpm: 60
      rules:
        - match: "modelo-*"
          bucket: "modelos-gerais"
          tpm: 30000
          rpm: 30
          rpd: 1000
    keys: [] # segredos são importados via ambiente/CLI, não versionados no YAML
```

As chaves podem ser lidas do ambiente com `MEU_PROVEDOR_API_KEYS` no fluxo de configuração dev, ou importadas no banco com `keypoolctl key import-env`. Configure `auth` e `paths` de acordo com a documentação do fornecedor.

## 2. Passthrough generic_http

Use `generic_http` para encaminhar caminhos HTTP específicos sem conversão de payload. A chave continua sendo escolhida pelo pool, e o KeyPool substitui a autenticação do cliente pela configurada para a chave selecionada.

```yaml
providers:
  meu-http:
    type: generic_http
    enabled: true
    base_url: https://api.example.invalid
    auth:
      scheme: bearer
    env_prefix: MEU_HTTP
    timeout: 120s
    keys: []
```

O projeto deve permitir o provedor e configurar os limites/políticas correspondentes. Clientes chamam `/{provider}/{path}` no prefixo `/p`, por exemplo `POST /p/meu-http/v1/jobs`. Não use query string para enviar segredos no cliente.

## 3. Habilitar no projeto

```yaml
projects:
  meu-projeto:
    keys:
      meu-provedor:
        include:
          tags_any: [meu-projeto]
    routes:
      assistente:
        priority: 2
        chain:
          - meu-provedor:modelo-principal
```

Para `generic_http`, `allow_lease` controla se o projeto pode receber leases diretas. A seleção do provedor deve ser explícita em `keys`; a configuração global não concede acesso automaticamente.

## 4. Limites e cabeçalhos

`rate_headers` mapeia cabeçalhos específicos a dimensões `tpm`, `tpd`, `rpm` e `rpd`, com padrão de reset/remaining conforme `provider.RateHeaderSpec`. Use valores conservadores em `limits`; hints em runtime corrigem estimativas. Revise presets e rate headers sempre que o fornecedor mudar o contrato.

## 5. Verificação

1. Valide YAML com `keypoold doctor`/`config.Build` antes de aplicar.
2. Importe uma chave de teste com privilégio mínimo.
3. Execute `keypoolctl key test <id>` e consulte `keypoolctl warm status`.
4. Faça uma requisição de teste pelo alias do projeto.
5. Verifique `/metrics` e eventos sem imprimir segredos ou corpos.

Evite usar credenciais de produção em ambientes de desenvolvimento ou em testes offline.
