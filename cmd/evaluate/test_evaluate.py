"""仅编写；统一验收阶段再运行。"""
import importlib.util
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import tempfile
import threading
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("executor", Path(__file__).with_name("main.py"))
executor = importlib.util.module_from_spec(spec)
spec.loader.exec_module(executor)

class EvaluationExecutorTests(unittest.TestCase):
    def test_remote_plain_http_rejected_before_reading_token(self):
        with self.assertRaises(ValueError):
            executor.Control("http://control.example", "/must-not-read")

    def test_credential_urls_rejected_before_reading_token(self):
        with self.assertRaises(ValueError):
            executor.Control("https://token@control.example", "/must-not-read")

    def test_login_204_cookie_authenticates_followup_json_request(self):
        class Handler(BaseHTTPRequestHandler):
            login_bodies = []
            authenticated_gets = []

            def log_message(self, format, *args):
                pass

            def do_POST(self):
                if self.path != "/api/login":
                    self.send_error(404)
                    return
                self.login_bodies.append(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
                self.send_response(204)
                self.send_header("Set-Cookie", "session=local-test-session; Path=/; HttpOnly")
                self.send_header("Content-Length", "0")
                self.end_headers()

            def do_GET(self):
                if self.path != "/api/evaluation/config":
                    self.send_error(404)
                    return
                self.authenticated_gets.append(self.headers.get("Cookie", ""))
                data = json.dumps({"protocol": "alpha-evaluation/v1", "config_sha256": "frozen-config"}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

        Handler.login_bodies = []
        Handler.authenticated_gets = []
        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory() as directory:
                token_file = Path(directory) / "token"
                token_file.write_text("local-test-token\n")
                control = executor.Control("http://127.0.0.1:%d" % server.server_port, token_file)
                result = control.request("GET", "/api/evaluation/config")
            self.assertEqual(json.loads(Handler.login_bodies[0]), {"token": "local-test-token"})
            self.assertEqual(Handler.authenticated_gets, ["session=local-test-session"])
            self.assertEqual(result, {"protocol": "alpha-evaluation/v1", "config_sha256": "frozen-config"})
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)

    def test_atomic_checkpoint_survives_update(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "checkpoint.json"
            executor.atomic(path, {"completed": ["T01-A-1"], "pending": "T01-A-2"})
            executor.atomic(path, {"completed": ["T01-A-1", "T01-A-2"], "pending": None})
            self.assertEqual(executor.read(path)["completed"], ["T01-A-1", "T01-A-2"])
            self.assertFalse(path.with_name(path.name + ".tmp").exists())

    def test_missing_reviewer_is_not_exported_as_negative(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            trial = {"trial_id": "review-T01-defect", "kind": "review"}
            executor.atomic(root / "records/review-T01-defect.json", {"reported_defect": None, "outcome": "failed"})
            executor.export_records({"trials": [trial]}, root)
            self.assertEqual((root / "review.jsonl").read_text(), "")

    def test_execution_flag_required_before_network_or_credentials(self):
        class Args:
            execute = False
        with patch.object(executor, "Control") as control:
            with self.assertRaises(ValueError):
                executor.run_plan(Args())
            control.assert_not_called()

if __name__ == "__main__":
    unittest.main()
