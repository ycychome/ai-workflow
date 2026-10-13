"""离线准备与汇总的回归测试；只供集中验收阶段执行。"""
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
CLI = ROOT / "eval/cli.py"

class OfflineEvaluationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="issuedesk-eval-tests-")
        self.addCleanup(self.temp.cleanup)
        self.dir = Path(self.temp.name)

    def cli(self, *args, success=True):
        result = subprocess.run([sys.executable, str(CLI), *map(str,args)], capture_output=True, text=True)
        self.assertEqual(result.returncode == 0, success, result.stderr)
        return result

    def config(self):
        # 数字仅为回归测试输入；生产矩阵必须读取运行器有效配置。
        config = {"model_profile":"synthetic-test-profile","budget":{"issue_calls":7,"daily_calls":29,"issue_minutes":11,"max_attempts":2,"max_fix_rounds":2,"input_price_per_million":0,"output_price_per_million":0},"runner":{"attempt_minutes":3,"agent_concurrency":2,"workflow_agent_concurrency":2,"max_tasks_per_workflow":9,"max_evidence_requests":8}}
        path = self.dir / "effective.json"
        path.write_text(json.dumps(config))
        return path,config

    def matrix(self):
        path,config = self.config()
        target = self.dir / "matrix.json"
        self.cli("matrix","--execution-config",path,"--output",target)
        plan=json.loads(target.read_text())
        self.assertEqual(plan["config"],config)
        self.assertEqual(len(plan["runs"]),90)
        return target,plan

    def record(self,plan,**changes):
        evidence=self.dir / "evidence.txt"
        evidence.write_text("合成可信执行器证据：测试通过\n")
        record={"run_key":plan["runs"][0]["run_key"],"config_sha256":plan["config_sha256"],"outcome":"success","tests_passed":True,"model_calls":2,"input_tokens":30,"output_tokens":15,"duration_seconds":20,"evidence":{"path":evidence.name,"sha256":hashlib.sha256(evidence.read_bytes()).hexdigest()}}
        record.update(changes)
        return record

    def write_records(self,records):
        path=self.dir / "results.jsonl"
        path.write_text("".join(json.dumps(r)+"\n" for r in records))
        return path

    def test_prepare_is_deterministic_and_excludes_private_material(self):
        for task in [f"T{i:02d}" for i in range(1,11)]:
            first=self.dir/(task+"-one"); second=self.dir/(task+"-two")
            a=json.loads(self.cli("prepare","--task",task,"--candidate","defect","--output",first).stdout)
            b=json.loads(self.cli("prepare","--task",task,"--candidate","defect","--output",second).stdout)
            self.assertEqual(a["snapshot_sha256"],b["snapshot_sha256"])
            self.assertFalse((first/"eval").exists())
            self.assertFalse(any("hidden" in str(p) or "variants" in str(p) or "labels" in str(p) for p in first.rglob("*")))
            self.assertTrue((first/"ISSUE.md").exists())
        self.cli("prepare","--task","T01","--candidate","fix","--output",self.dir/"T01-one",success=False)

    def test_fix_matches_baseline(self):
        baseline=self.dir/"base";fix=self.dir/"fix"
        a=json.loads(self.cli("prepare","--task","T03","--candidate","baseline","--output",baseline).stdout)
        b=json.loads(self.cli("prepare","--task","T03","--candidate","fix","--output",fix).stdout)
        self.assertEqual(a["snapshot_sha256"],b["snapshot_sha256"])

    def test_zero_fix_rounds_still_allows_evaluation(self):
        path,config = self.config()
        config["budget"]["max_fix_rounds"] = 0
        path.write_text(json.dumps(config))
        target = self.dir / "zero-fix-matrix.json"
        self.cli("matrix","--execution-config",path,"--output",target)
        plan = json.loads(target.read_text())
        self.assertEqual(plan["config"]["budget"]["max_fix_rounds"],0)
        self.assertEqual(len(plan["runs"]),90)

    def test_missing_results_and_unknown_prices_not_success(self):
        matrix,plan=self.matrix();results=self.write_records([self.record(plan)])
        out=self.dir/"summary.json";self.cli("summarize","--matrix",matrix,"--results",results,"--output",out)
        summary=json.loads(out.read_text())
        self.assertFalse(summary["complete"]);self.assertEqual(len(summary["missing_runs"]),89)
        self.assertEqual(summary["arms"]["A"]["verified_success"],1)
        self.assertIsNone(summary["arms"]["A"]["estimated_cost"])

    def test_duplicate_results_rejected(self):
        matrix,plan=self.matrix();record=self.record(plan);results=self.write_records([record,record])
        self.cli("summarize","--matrix",matrix,"--results",results,"--output",self.dir/"out.json",success=False)

    def test_budget_overrun_not_success(self):
        matrix,plan=self.matrix();results=self.write_records([self.record(plan,model_calls=plan["config"]["budget"]["issue_calls"]+1)])
        out=self.dir/"summary.json";self.cli("summarize","--matrix",matrix,"--results",results,"--output",out)
        arm=json.loads(out.read_text())["arms"]["A"]
        self.assertEqual(arm["verified_success"],0);self.assertEqual(arm["budget_violations"],1)

    def test_config_drift_and_evidence_tamper_rejected(self):
        matrix,plan=self.matrix();record=self.record(plan,config_sha256="different")
        results=self.write_records([record]);self.cli("summarize","--matrix",matrix,"--results",results,"--output",self.dir/"out.json",success=False)
        record=self.record(plan);record["evidence"]["sha256"]="wrong";results=self.write_records([record])
        self.cli("summarize","--matrix",matrix,"--results",results,"--output",self.dir/"out2.json",success=False)

    def test_pending_labels_excluded_from_accuracy(self):
        matrix,plan=self.matrix();results=self.write_records([{"kind":"review","candidate_id":"T01-defect","reported_defect":True}])
        out=self.dir/"summary.json";self.cli("summarize","--matrix",matrix,"--results",results,"--output",out)
        review=json.loads(out.read_text())["review"]
        self.assertEqual(review["pending_human_review"],1)
        self.assertEqual(sum(review["confusion_matrix_approved_only"].values()),0)

if __name__ == "__main__":
    unittest.main()
