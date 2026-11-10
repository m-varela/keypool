import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import smoke
import client_helpers


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_args):
        pass

    def _send(self, status, body, content_type="application/json", extra=None):
        data = body if isinstance(body, bytes) else json.dumps(body).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(data)))
        for name, value in (extra or {}).items():
            self.send_header(name, value)
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path == "/healthz":
            self._send(200, {"status": "ok"})
        elif self.path == "/readyz":
            if self.headers.get("Authorization") != "Bearer kp_test" :
                self._send(401, {"error": {"type": "auth"}})
            else:
                self._send(200, {"status": "ready"})
        elif self.path == "/v1/models":
            if self.headers.get("X-Keypool-Project") != "mathlab":
                self._send(400, {"error": {"type": "project"}})
            else:
                self._send(200, {"object": "list", "data": [{"id": "prover", "object": "model"}]})
        elif self.path == "/v1/capacity?alias=prover":
            if self.headers.get("Authorization") != "Bearer kp_test" or self.headers.get("X-Keypool-Project") != "mathlab":
                self._send(401, {"error": {"type": "auth"}})
            else:
                self._send(200, {"project": "mathlab", "capacity": {"prover": {"live_keys": 2, "buckets": 1, "parallelism": 2, "lanes": 2}}})
        else:
            self._send(404, {"error": {"type": "not_found"}})

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
        if self.path != "/v1/chat/completions":
            self._send(404, {})
            return
        if self.headers.get("Authorization") != "Bearer kp_test" or self.headers.get("X-Keypool-Project") != "mathlab":
            self._send(401, {"error": {"type": "auth"}})
            return
        message = body.get("messages", [{}])[0].get("content")
        if message == "rate-limit":
            self._send(429, {"error": {"type": "keypool_capacity_exhausted"}}, extra={"Retry-After": "7"})
        elif body.get("stream"):
            payload = b'data: {"choices":[]}\n\ndata: [DONE]\n\n'
            self._send(200, payload, "text/event-stream", {"Cache-Control": "no-store"})
        else:
            self._send(200, {"id": "chatcmpl-test", "choices": [{"message": {"content": "OK"}}]})


class SmokeTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()
        cls.base = "http://127.0.0.1:%d" % cls.server.server_address[1]

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.thread.join(timeout=2)

    def test_preflight_contract(self):
        result = smoke.preflight(self.base, "kp_test", "mathlab", "prover")
        self.assertEqual(len(result), 4)
        self.assertIn("parallelism=2", result[-1])

    def test_non_streaming_chat(self):
        result = smoke.chat_smoke(self.base, "kp_test", "mathlab", "prover", "ping")
        self.assertIn("completion", result)

    def test_streaming_chat(self):
        result = smoke._stream_request(self.base, "kp_test", "mathlab", "prover", "ping", 5)
        self.assertIn("[DONE]", result)

    def test_capacity_helper(self):
        result = client_helpers.fetch_capacity(self.base, "kp_test", "mathlab", "prover")
        self.assertEqual(result["parallelism"], 2)
        self.assertEqual(result["live_keys"], 2)

    def test_retry_after_is_preserved(self):
        result = smoke.chat_smoke(self.base, "kp_test", "mathlab", "prover", "rate-limit")
        self.assertIn("Retry-After=7", result)
        streamed = smoke._stream_request(self.base, "kp_test", "mathlab", "prover", "rate-limit", 5)
        self.assertIn("Retry-After=7", streamed)

    def test_policy_headers(self):
        headers = client_helpers.keypool_headers(
            "mathlab", priority=1, exclude=["groq:model-a", "groq:model-a"],
            avoid_weights=["model-b"], timeout_ms=1200,
        )
        self.assertEqual(headers["X-Keypool-Project"], "mathlab")
        self.assertEqual(headers["X-Keypool-Exclude"], "groq:model-a")
        self.assertEqual(headers["X-Keypool-Priority"], "1")
        with self.assertRaises(ValueError):
            client_helpers.keypool_headers("mathlab", priority=10)


if __name__ == "__main__":
    unittest.main()
