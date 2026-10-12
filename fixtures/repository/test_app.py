import json
import tempfile
import threading
import unittest
from pathlib import Path
from urllib.error import HTTPError
from urllib.parse import urlencode
from urllib.request import Request, urlopen

from app import GreetingServer


class GreetingTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.greeting = Path(self.directory.name) / "greeting.txt"
        self.greeting.write_text("hello\n", encoding="utf-8")
        self.server = GreetingServer(("127.0.0.1", 0), self.greeting)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.stop_server)
        self.base = f"http://127.0.0.1:{self.server.server_port}"

    def stop_server(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)

    def request(self, path, method="GET"):
        req = Request(self.base + path, method=method)
        try:
            response = urlopen(req, timeout=2)
        except HTTPError as error:
            response = error
        with response:
            self.assertEqual(response.headers.get_content_type(), "application/json")
            return response.status, json.loads(response.read().decode("utf-8"))

    def test_health(self):
        self.assertEqual(self.request("/healthz"), (200, {"status": "ok"}))

    def test_default_greeting(self):
        self.assertEqual(self.request("/api/greeting"), (200, {"message": "hello"}))

    def test_fixed_patch_changes_live_response(self):
        # Verify the M0 change without replacing source or restarting the server.
        self.greeting.write_text("hello M0\n", encoding="utf-8")
        self.assertEqual(self.request("/api/greeting"), (200, {"message": "hello M0"}))
        self.assertEqual(self.request("/api/greeting?name=Codex"), (200, {"message": "hello M0, Codex!"}))

    def test_unicode_name_and_whitespace(self):
        query = urlencode({"name": "  小明  "})
        self.assertEqual(self.request("/api/greeting?" + query), (200, {"message": "hello, 小明!"}))

    def test_invalid_name_and_unknown_parameters(self):
        for query in ("name=", "name=%20", "name=" + "a" * 81, "name=a&name=b", "secret=x", "name=a&x=1&y=2&z=3&p=4&q=5"):
            with self.subTest(query=query):
                self.assertEqual(self.request("/api/greeting?" + query)[0], 400)

    def test_unknown_route_and_method(self):
        self.assertEqual(self.request("/missing")[0], 404)
        self.assertEqual(self.request("/api/greeting", method="POST")[0], 405)

    def test_missing_empty_and_invalid_config(self):
        self.greeting.unlink()
        self.assertEqual(self.request("/api/greeting")[0], 503)
        self.greeting.write_text("\n", encoding="utf-8")
        self.assertEqual(self.request("/api/greeting")[0], 503)
        self.greeting.write_bytes(b"\xff")
        self.assertEqual(self.request("/api/greeting")[0], 503)


if __name__ == "__main__":
    unittest.main()
