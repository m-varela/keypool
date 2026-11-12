#!/usr/bin/env python3
"""Smoke test local do alias Gemini via gateway KeyPool; usa somente stdlib."""
from __future__ import annotations
import argparse
import json
import os
import sys
import urllib.error
import urllib.request


def request(url: str, key: str, project: str, payload: dict | None = None):
    headers = {
        "Authorization": f"Bearer {key}",
        "X-Keypool-Project": project,
        "Accept": "application/json",
    }
    data = None
    if payload is not None:
        headers["Content-Type"] = "application/json"
        data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(url, data=data, headers=headers, method="POST" if data else "GET")
    try:
        with urllib.request.urlopen(req, timeout=90) as response:
            # Itera linhas à medida que chegam; em SSE cada evento é delimitado por linha.
            chunks = []
            for line in response:
                chunks.append(line)
            return response.status, dict(response.headers.items()), b"".join(chunks)
    except urllib.error.HTTPError as exc:
        # Não imprime corpo upstream: ele pode conter conteúdo privado.
        raise RuntimeError(f"KeyPool respondeu HTTP {exc.code}") from None
    except (urllib.error.URLError, TimeoutError, OSError) as exc:
        raise RuntimeError(f"falha de transporte: {type(exc).__name__}") from None


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default=os.getenv("KEYPOOL_URL", "http://127.0.0.1:8787/v1"))
    parser.add_argument("--project", default="mathlab")
    parser.add_argument("--model", default="gemini")
    parser.add_argument("--stream", action="store_true", help="solicita SSE e valida status/content-type")
    args = parser.parse_args()
    key = os.getenv("KEYPOOL_API_KEY", "").strip()
    if not key:
        print("Defina KEYPOOL_API_KEY com a chave local kp_... do KeyPool.", file=sys.stderr)
        return 2
    payload = {
        "model": args.model,
        "messages": [{"role": "user", "content": "Responda apenas: integração funcionando."}],
        "max_tokens": 128,
    }
    if args.stream:
        payload["stream"] = True
    try:
        status, headers, body = request(args.base_url.rstrip("/") + "/chat/completions", key, args.project, payload)
        if args.stream:
            content_type = next((v for k, v in headers.items() if k.lower() == "content-type"), "")
            if "text/event-stream" not in content_type.lower():
                raise RuntimeError("resposta stream não declarou Content-Type text/event-stream")
            if b"data:" not in body or b"[DONE]" not in body:
                raise RuntimeError("stream não contém eventos data: e terminador [DONE]")
            print(f"HTTP {status}: SSE OK; corpo recebido em linhas ({len(body)} bytes).")
        else:
            doc = json.loads(body.decode("utf-8"))
            if not isinstance(doc, dict) or not isinstance(doc.get("choices"), list):
                raise RuntimeError("resposta não corresponde ao formato chat.completion esperado")
            print(f"HTTP {status}: chat completion OK; choices={len(doc['choices'])}")
        return 0
    except (RuntimeError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        print(str(exc), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
