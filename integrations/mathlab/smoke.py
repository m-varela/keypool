#!/usr/bin/env python3
"""Verificador sem dependencias externas para o contrato Mathlab -> KeyPool."""
from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from typing import Mapping


@dataclass
class HTTPResult:
    status: int
    headers: Mapping[str, str]
    body: bytes


class CheckFailed(RuntimeError):
    pass


def _url(base: str, path: str) -> str:
    return base.rstrip("/") + path


def _request(
    url: str,
    *,
    api_key: str | None = None,
    project: str | None = None,
    method: str = "GET",
    payload: dict | None = None,
    headers: dict[str, str] | None = None,
    timeout: float = 20,
) -> HTTPResult:
    request_headers = {"Accept": "application/json"}
    if api_key:
        request_headers["Authorization"] = "Bearer " + api_key
    if project:
        request_headers["X-Keypool-Project"] = project
    if payload is not None:
        request_headers["Content-Type"] = "application/json"
    if headers:
        request_headers.update(headers)
    data = json.dumps(payload, ensure_ascii=False).encode("utf-8") if payload is not None else None
    req = urllib.request.Request(url, data=data, headers=request_headers, method=method)
    try:
        response = urllib.request.urlopen(req, timeout=timeout)
    except urllib.error.HTTPError as exc:
        # Mantem o corpo apenas em memoria. Nunca imprime mensagens upstream ou credenciais.
        try:
            return HTTPResult(exc.code, dict(exc.headers.items()), exc.read())
        finally:
            exc.close()
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        reason = getattr(exc, "reason", None)
        detail = type(reason).__name__ if reason is not None else type(exc).__name__
        raise CheckFailed(f"falha de transporte em {urllib.parse.urlsplit(url).path}: {detail}") from None
    try:
        body = response.read()
        return HTTPResult(response.status, dict(response.headers.items()), body)
    finally:
        response.close()


def _json(result: HTTPResult, endpoint: str) -> dict:
    if result.status < 200 or result.status >= 300:
        raise CheckFailed(f"{endpoint}: HTTP {result.status}")
    try:
        value = json.loads(result.body.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        raise CheckFailed(f"{endpoint}: resposta nao e JSON valido") from None
    if not isinstance(value, dict):
        raise CheckFailed(f"{endpoint}: resposta JSON deve ser objeto")
    return value


def preflight(base: str, api_key: str, project: str, alias: str, timeout: float = 20) -> list[str]:
    results: list[str] = []

    health = _request(_url(base, "/healthz"), timeout=timeout)
    health_doc = _json(health, "GET /healthz")
    if health_doc.get("status") != "ok":
        raise CheckFailed("GET /healthz: status inesperado")
    results.append("GET /healthz: OK")

    ready = _request(_url(base, "/readyz"), api_key=api_key, timeout=timeout)
    ready_doc = _json(ready, "GET /readyz")
    if ready_doc.get("status") != "ready":
        raise CheckFailed("GET /readyz: KeyPool ainda nao esta pronto")
    results.append("GET /readyz: autenticacao e prontidao OK")

    models = _json(
        _request(_url(base, "/v1/models"), api_key=api_key, project=project, timeout=timeout),
        "GET /v1/models",
    )
    entries = models.get("data")
    if models.get("object") != "list" or not isinstance(entries, list):
        raise CheckFailed("GET /v1/models: formato OpenAI list esperado")
    aliases = {item.get("id") for item in entries if isinstance(item, dict)}
    if alias not in aliases:
        raise CheckFailed(f"GET /v1/models: alias configurado '{alias}' nao foi publicado")
    results.append(f"GET /v1/models: alias '{alias}' publicado")

    query = urllib.parse.urlencode({"alias": alias})
    capacity = _json(
        _request(_url(base, "/v1/capacity?" + query), api_key=api_key, project=project, timeout=timeout),
        "GET /v1/capacity",
    )
    if capacity.get("project") != project or not isinstance(capacity.get("capacity"), dict):
        raise CheckFailed("GET /v1/capacity: formato de capacidade invalido")
    alias_capacity = capacity["capacity"].get(alias)
    if not isinstance(alias_capacity, dict) or not isinstance(alias_capacity.get("parallelism"), int):
        raise CheckFailed("GET /v1/capacity: alias sem campo parallelism inteiro")
    results.append(f"GET /v1/capacity: parallelism={alias_capacity['parallelism']}")
    return results


def _chat_payload(alias: str, message: str, stream: bool) -> dict:
    return {
        "model": alias,
        "messages": [{"role": "user", "content": message}],
        "max_tokens": 16,
        "stream": stream,
    }


def chat_smoke(base: str, api_key: str, project: str, alias: str, message: str, timeout: float = 300) -> str:
    result = _request(
        _url(base, "/v1/chat/completions"),
        api_key=api_key,
        project=project,
        method="POST",
        payload=_chat_payload(alias, message, False),
        timeout=timeout,
    )
    if result.status == 429:
        retry = next((v for k, v in result.headers.items() if k.lower() == "retry-after"), "")
        if not retry:
            raise CheckFailed("POST /v1/chat/completions: HTTP 429 sem Retry-After")
        return f"POST /v1/chat/completions: KeyPool retornou 429 com Retry-After={retry} (capacidade temporariamente esgotada)"
    doc = _json(result, "POST /v1/chat/completions")
    if not isinstance(doc.get("choices"), list) or not doc["choices"]:
        raise CheckFailed("POST /v1/chat/completions: resposta sem choices")
    return "POST /v1/chat/completions: completion nao-streaming OK"


def _stream_request(base: str, api_key: str, project: str, alias: str, message: str, timeout: float) -> str:
    headers = {
        "Authorization": "Bearer " + api_key,
        "X-Keypool-Project": project,
        "Accept": "text/event-stream",
        "Content-Type": "application/json",
    }
    data = json.dumps(_chat_payload(alias, message, True), ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(_url(base, "/v1/chat/completions"), data=data, headers=headers, method="POST")
    try:
        response = urllib.request.urlopen(req, timeout=timeout)
    except urllib.error.HTTPError as exc:
        try:
            if exc.code == 429:
                retry = exc.headers.get("Retry-After", "")
                if retry:
                    return f"POST /v1/chat/completions (stream): 429 com Retry-After={retry}"
                raise CheckFailed("stream: HTTP 429 sem Retry-After") from None
            raise CheckFailed(f"POST /v1/chat/completions (stream): HTTP {exc.code}") from None
        finally:
            exc.close()
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        raise CheckFailed(f"stream: falha de transporte ({type(exc).__name__})") from None
    done = False
    try:
        if "text/event-stream" not in response.headers.get("Content-Type", "").lower():
            raise CheckFailed("stream: Content-Type nao e text/event-stream")
        for raw_line in response:
            if raw_line.strip() == b"data: [DONE]":
                done = True
                break
        if not done:
            raise CheckFailed("stream: fluxo terminou sem data: [DONE]")
    finally:
        response.close()
    return "POST /v1/chat/completions (stream): SSE e [DONE] OK"


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default=os.getenv("KEYPOOL_URL", "http://127.0.0.1:8787"))
    parser.add_argument("--project", default=os.getenv("KEYPOOL_PROJECT", "mathlab"))
    parser.add_argument("--alias", default=os.getenv("KEYPOOL_ALIAS", "prover"))
    parser.add_argument("--timeout", type=float, default=20)
    parser.add_argument("--chat", action="store_true", help="envia uma chamada real nao-streaming")
    parser.add_argument("--stream", action="store_true", help="envia uma chamada real SSE")
    parser.add_argument("--message", default=os.getenv("KEYPOOL_TEST_MESSAGE", "Responda apenas: OK"))
    args = parser.parse_args(argv)
    api_key = os.getenv("KEYPOOL_API_KEY", "").strip()
    if not api_key:
        parser.error("defina KEYPOOL_API_KEY no ambiente; a credencial nao pode ser argumento")
    if args.timeout <= 0:
        parser.error("--timeout deve ser maior que zero")
    try:
        for line in preflight(args.url, api_key, args.project, args.alias, args.timeout):
            print("[OK] " + line)
        if args.chat:
            print("[OK] " + chat_smoke(args.url, api_key, args.project, args.alias, args.message, max(args.timeout, 60)))
        if args.stream:
            print("[OK] " + _stream_request(args.url, api_key, args.project, args.alias, args.message, max(args.timeout, 60)))
    except CheckFailed as exc:
        print("[FALHA] " + str(exc), file=sys.stderr)
        return 1
    print("Contrato Mathlab -> KeyPool: verificacoes solicitadas concluidas.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
