"""Helpers opcionais para adaptar o LLMClient/capacity.py do Mathlab ao KeyPool.

Nao substitui OpenAICompatibleProvider: fornece somente headers por chamada e
consulta autenticada de capacidade, que sao as partes especificas do KeyPool.
"""
from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Iterable


class KeyPoolUnavailable(RuntimeError):
    """A consulta de capacidade falhou; o chamador deve aplicar fallback conservador."""


def _csv(values: Iterable[str] | None) -> str:
    if isinstance(values, str):
        values = values.split(",")
    cleaned = []
    for value in values or ():
        text = str(value).strip()
        if text and "," not in text and "\r" not in text and "\n" not in text:
            cleaned.append(text)
    # Preserve ordem, mas evita duplicar exclusoes recebidas de camadas diferentes.
    return ",".join(dict.fromkeys(cleaned))


def keypool_headers(
    project: str,
    *,
    priority: int | None = None,
    exclude: Iterable[str] | None = None,
    avoid_weights: Iterable[str] | None = None,
    timeout_ms: int | None = None,
) -> dict[str, str]:
    """Construi somente os headers de politica; autenticacao continua no provider."""
    project = project.strip()
    if not project or any(c in project for c in "\r\n"):
        raise ValueError("project invalido")
    headers = {"X-Keypool-Project": project}
    if priority is not None:
        if not 0 <= priority <= 9:
            raise ValueError("priority deve estar entre 0 e 9")
        headers["X-Keypool-Priority"] = str(priority)
    excluded = _csv(exclude)
    if excluded:
        headers["X-Keypool-Exclude"] = excluded
    avoided = _csv(avoid_weights)
    if avoided:
        headers["X-Keypool-Avoid-Weights"] = avoided
    if timeout_ms is not None:
        if timeout_ms < 0:
            raise ValueError("timeout_ms nao pode ser negativo")
        headers["X-Keypool-Timeout-Ms"] = str(timeout_ms)
    return headers


def fetch_capacity(
    base_url: str,
    api_key: str,
    project: str,
    alias: str,
    *,
    timeout: float = 5.0,
) -> dict:
    """Consulta capacidade real do alias; lanca KeyPoolUnavailable em falha.

    Retorna o objeto do alias (live_keys, buckets, lanes, parallelism). O chamador
    deve usar parallelism como estimativa operacional e manter fallback conservador.
    """
    if not api_key or not project or not alias:
        raise ValueError("api_key, project e alias sao obrigatorios")
    query = urllib.parse.urlencode({"alias": alias})
    url = base_url.rstrip("/") + "/v1/capacity?" + query
    req = urllib.request.Request(
        url,
        headers={
            "Authorization": "Bearer " + api_key,
            "X-Keypool-Project": project,
            "Accept": "application/json",
        },
        method="GET",
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as response:
            if response.status < 200 or response.status >= 300:
                raise KeyPoolUnavailable("KeyPool capacity HTTP %d" % response.status)
            payload = json.loads(response.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        raise KeyPoolUnavailable("KeyPool capacity HTTP %d" % exc.code) from None
    except (urllib.error.URLError, TimeoutError, OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise KeyPoolUnavailable("KeyPool capacity indisponivel (%s)" % type(exc).__name__) from None
    if not isinstance(payload, dict) or payload.get("project") != project:
        raise KeyPoolUnavailable("resposta de capacidade nao corresponde ao projeto")
    capacities = payload.get("capacity")
    item = capacities.get(alias) if isinstance(capacities, dict) else None
    fields = ("live_keys", "buckets", "lanes", "parallelism")
    if not isinstance(item, dict) or any(type(item.get(field)) is not int or item[field] < 0 for field in fields):
        raise KeyPoolUnavailable("resposta de capacidade incompleta para o alias")
    return {field: item[field] for field in fields}
