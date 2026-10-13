"""重置栅栏测试，当前阶段不执行。"""
import importlib.util
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("reset", Path(__file__).with_name("pilot-reset.py"))
reset = importlib.util.module_from_spec(spec)
spec.loader.exec_module(reset)

class ResetFenceTests(unittest.TestCase):
    def test_stopping_counts_as_occupied(self):
        state = {"intake_stopped": True, "active_agents": 0, "stopping_agents": 1, "active_workflow_leases": 0, "inflight_outbox": 0, "repository_epoch": "epoch-1", "maintenance_lock_capable": True}
        with self.assertRaises(ValueError):
            reset.quiescent(state)

    def test_inflight_outbox_blocks_reset(self):
        state = {"intake_stopped": True, "active_agents": 0, "stopping_agents": 0, "active_workflow_leases": 0, "inflight_outbox": 1, "repository_epoch": "epoch-1", "maintenance_lock_capable": True}
        with self.assertRaises(ValueError):
            reset.quiescent(state)

    def test_identity_needs_numeric_repository_and_admin(self):
        with self.assertRaises(ValueError):
            reset.identity({"repository": reset.TARGET, "repository_id": 2, "installation_id": 3, "actor": {"login": "user", "id": 4, "admin": False}})

    def test_tampered_backup_cannot_apply(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            data = root / "db.dump"
            data.write_bytes(b"changed")
            with self.assertRaises(ValueError):
                reset.check_files(root, [{"path": "db.dump", "role": "control_database", "sha256": "0" * 64, "bytes": 7}])

if __name__ == "__main__":
    unittest.main()
