"""仅编写；统一验收阶段再运行。"""
import importlib.util
from pathlib import Path
import tempfile
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
