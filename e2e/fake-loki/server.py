"""E2E 用の偽 Loki（標準ライブラリだけ）。

コレクタの push（POST /loki/api/v1/push）を本物の Loki と同じく 204 で受けて、受けたものをそのまま記録する。
記録は GET /pushes で取り出せる（[{"content_type": ..., "body": <受けた JSON>}, ...]）。
中身の検証は e2e/smoke.sh が行う。ここは受けて記録するだけにして、判定を1か所に保つ。
"""

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PUSH_PATH = "/loki/api/v1/push"

_lock = threading.Lock()
_pushes = []


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path != PUSH_PATH:
            self.send_error(404)
            return
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length)
        try:
            body = json.loads(raw)
        except ValueError:
            # 本物の Loki も JSON として読めない push は 400 で断る
            self.send_error(400, "body is not JSON")
            return
        with _lock:
            _pushes.append({"content_type": self.headers.get("Content-Type", ""), "body": body})
        self.send_response(204)
        self.end_headers()

    def do_GET(self):
        if self.path == "/ready":
            self._reply(200, b"ready\n", "text/plain")
        elif self.path == "/pushes":
            with _lock:
                data = json.dumps(_pushes).encode()
            self._reply(200, data, "application/json")
        else:
            self.send_error(404)

    def _reply(self, status, data, content_type):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, fmt, *args):
        # 受けたリクエストは docker compose logs で見られるように標準エラーへ（既定のまま）
        super().log_message(fmt, *args)


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 3100), Handler).serve_forever()
