"""完整计划准备链路的离线测试；网络适配器由明确 mock 替代。"""
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

ROOT=Path(__file__).resolve().parents[2]
def module(name,path):
    spec=importlib.util.spec_from_file_location(name,path)
    result=importlib.util.module_from_spec(spec);spec.loader.exec_module(result);return result
executor=module("executor_plan",ROOT/"cmd/evaluate/main.py")
prepare=module("prepare_plan",ROOT/"scripts/pilot-eval-prepare.py")

class OfflinePlanTests(unittest.TestCase):
    def test_prepare_120_independent_snapshots_and_blind_plan(self):
        with tempfile.TemporaryDirectory(prefix="aiwf-offline-plan-") as directory:
            root=Path(directory)
            cfg={"model_profile":"offline-test-only","budget":{"issue_calls":80,"daily_calls":500,"issue_minutes":120,"max_attempts":2,"max_fix_rounds":2,"input_price_per_million":0,"output_price_per_million":0},"runner":{"attempt_minutes":30,"agent_concurrency":8,"workflow_agent_concurrency":3,"max_tasks_per_workflow":64,"max_evidence_requests":64}}
            (root/"config.json").write_text(json.dumps(cfg))
            subprocess.run([sys.executable,str(ROOT/"eval/cli.py"),"matrix","--execution-config",str(root/"config.json"),"--output",str(root/"matrix.json")],check=True,stdout=subprocess.DEVNULL)
            calls=[]
            def adapter(path,request):
                calls.append(request)
                base=hashlib.sha1((request["trial"]["trial_id"]+":base").encode()).hexdigest()
                review=request["trial"]["kind"]=="review"
                return {"protocol":"pilot-eval-prepare/v1","ok":True,"repository":request["repository"],"repository_id":1,"installation_id":2,"source_sha256":request["source_sha256"],"independently_rebuilt":True,"base_source_sha256":request["baseline_source_sha256"] if review else request["source_sha256"],"base_sha":base,"candidate_sha":hashlib.sha1((base+"head").encode()).hexdigest() if review else base,"base_branch":"eval/"+request["trial"]["trial_id"]+"/base","candidate_branch":"eval/"+request["trial"]["trial_id"]+"/candidate","pull_request":len(calls) if review else 0}
            argv=["prepare","--execute","--manifest",str(ROOT/"eval/manifest.json"),"--matrix",str(root/"matrix.json"),"--adapter",sys.executable,"--output",str(root/"snapshots.json"),"--workspace-root",str(root/"workspaces"),"--repository","ycychome/ai-workflow-pilot","--repository-id","1","--installation-id","2","--seed","20261013"]
            with patch.object(sys,"argv",argv),patch.object(prepare,"call",adapter):
                prepare.main()
                prepare.main() # durable completed records do not invoke adapter again
            self.assertEqual(len(calls),120)
            args=SimpleNamespace(manifest=str(ROOT/"eval/manifest.json"),matrix=str(root/"matrix.json"),snapshots=str(root/"snapshots.json"),seed=20261013,output=str(root/"plan.json"))
            executor.make_plan(args)
            plan=json.loads((root/"plan.json").read_text())
            self.assertEqual([sum(t["kind"]==kind for t in plan["trials"]) for kind in ("smoke","development","review")],[10,90,20])
            reviews=[t for t in plan["trials"] if t["kind"]=="review"]
            self.assertTrue(all("defect" not in t["trial_id"] and "fix" not in t["trial_id"] for t in reviews))
            with self.assertRaises(ValueError):executor.make_plan(args)

if __name__=="__main__":unittest.main()
