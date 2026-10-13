#!/usr/bin/env python3
"""Local allowlist proxy: only GitHub's webhook POST reaches control."""

from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from http.client import HTTPConnection

HOST = "127.0.0.1"
PORT = 8082
WEBHOOK_PATH = "/webhooks/github"
MAX_BODY = 25 * 1024 * 1024


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self):
        if self.path != WEBHOOK_PATH:
            self.send_error(404)
            return
        try:
            length = int(self.headers.get("Content-Length", "-1"))
        except ValueError:
            self.send_error(400)
            return
        if length < 0 or length > MAX_BODY:
            self.send_error(413 if length > MAX_BODY else 411)
            return
        body = self.rfile.read(length)
        headers = {
            "Content-Length": str(len(body)),
            "Connection": "close",
        }
        for key in (
            "Content-Type",
            "User-Agent",
            "X-GitHub-Event",
            "X-GitHub-Delivery",
            "X-Hub-Signature",
            "X-Hub-Signature-256",
        ):
            if key in self.headers:
                headers[key] = self.headers[key]
        conn = HTTPConnection("127.0.0.1", 8080, timeout=30)
        try:
            conn.request("POST", WEBHOOK_PATH, body=body, headers=headers)
            response = conn.getresponse()
            response_body = response.read(1024 * 1024)
            self.send_response(response.status)
            self.send_header("Content-Length", str(len(response_body)))
            self.send_header("Connection", "close")
            if response.getheader("Content-Type"):
                self.send_header("Content-Type", response.getheader("Content-Type"))
            self.end_headers()
            self.wfile.write(response_body)
        except OSError:
            self.send_error(502)
        finally:
            conn.close()

    def do_GET(self):
        self.send_error(404)

    def do_PUT(self):
        self.send_error(404)

    def do_DELETE(self):
        self.send_error(404)

    def log_message(self, fmt, *args):
        print("webhook-proxy: " + (fmt % args), flush=True)


if __name__ == "__main__":
    ThreadingHTTPServer((HOST, PORT), Handler).serve_forever()
