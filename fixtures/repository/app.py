"""A dependency-free greeting service used by the AI Workflow M0 spike."""

import argparse
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlsplit


class GreetingServer(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, address, greeting_file):
        self.greeting_file = Path(greeting_file)
        super().__init__(address, GreetingHandler)


class GreetingHandler(BaseHTTPRequestHandler):
    def respond(self, status, body):
        payload = json.dumps(body, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        target = urlsplit(self.path)
        if target.path == "/healthz":
            self.respond(200, {"status": "ok"})
            return
        if target.path != "/api/greeting":
            self.respond(404, {"error": "not found"})
            return
        try:
            params = parse_qs(target.query, keep_blank_values=True, max_num_fields=5)
        except ValueError:
            self.respond(400, {"error": "invalid query"})
            return
        if set(params) - {"name"} or len(params.get("name", [])) > 1:
            self.respond(400, {"error": "only one name parameter is allowed"})
            return
        name = params.get("name", [None])[0]
        if name is not None:
            name = name.strip()
            if not name or len(name) > 80:
                self.respond(400, {"error": "name must contain 1 to 80 characters"})
                return
        try:
            greeting = self.server.greeting_file.read_text(encoding="utf-8").strip()
        except (OSError, UnicodeError):
            self.respond(503, {"error": "greeting unavailable"})
            return
        if not greeting:
            self.respond(503, {"error": "greeting unavailable"})
            return
        self.respond(200, {"message": greeting if name is None else f"{greeting}, {name}!"})

    def do_POST(self):
        self.respond(405, {"error": "method not allowed"})

    def log_message(self, *args):
        # Names and query strings need not appear in a public request log.
        pass


def main():
    parser = argparse.ArgumentParser(description="A tiny JSON greeting service")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8000)
    parser.add_argument("--greeting-file", type=Path, default=Path(__file__).with_name("greeting.txt"))
    args = parser.parse_args()
    with GreetingServer((args.host, args.port), args.greeting_file) as server:
        print(f"Listening on http://{args.host}:{server.server_port}", flush=True)
        try:
            server.serve_forever()
        except KeyboardInterrupt:
            pass


if __name__ == "__main__":
    main()
