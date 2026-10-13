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
from unittest.mock import Mock, patch

ROOT=Path(__file__).resolve().parents[2]
def module(name,path):
    spec=importlib.util.spec_from_file_location(name,path)
    result=importlib.util.module_from_spec(spec);spec.loader.exec_module(result);return result
executor=module("executor_plan",ROOT/"cmd/evaluate/main.py")
prepare=module("prepare_plan",ROOT/"scripts/pilot-eval-prepare.py")
github_adapter=module("github_adapter_plan",ROOT/"scripts/pilot-eval-github.py")

class OfflinePlanTests(unittest.TestCase):
    def test_batch_ids_are_disjoint_and_development_keys_remain_logical(self):
        manifest,_=executor.checked_manifest(ROOT/"eval/manifest.json")
        with tempfile.TemporaryDirectory(prefix="aiwf-batch-matrix-") as directory:
            root=Path(directory)
            cfg={"model_profile":"offline-test-only","budget":{"issue_calls":80,"daily_calls":500,"issue_minutes":120,"max_attempts":2,"max_fix_rounds":2,"input_price_per_million":0,"output_price_per_million":0},"runner":{"attempt_minutes":30,"agent_concurrency":8,"workflow_agent_concurrency":3,"max_tasks_per_workflow":64,"max_evidence_requests":64,"max_tool_calls_per_run":200}}
            (root/"config.json").write_text(json.dumps(cfg))
            subprocess.run([sys.executable,str(ROOT/"eval/cli.py"),"matrix","--execution-config",str(root/"config.json"),"--output",str(root/"matrix.json")],check=True,stdout=subprocess.DEVNULL)
            matrix=json.loads((root/"matrix.json").read_text())
        original=prepare.build_trials(manifest,matrix,20261013)
        first=prepare.build_trials(manifest,matrix,20261013,"retry-a")
        second=prepare.build_trials(manifest,matrix,20261013,"retry-b")
        self.assertEqual(len({x["trial_id"] for x in first}),120)
        self.assertEqual(len({x["trial_id"] for x in second}),120)
        self.assertTrue({x["trial_id"] for x in first}.isdisjoint({x["trial_id"] for x in second}))
        self.assertEqual([x["trial_id"] for x in original if x["kind"]=="development"],[x["run_key"] for x in original if x["kind"]=="development"])
        for left,right in zip(first,second):
            self.assertEqual(left["trial_id"].split(".",1)[1],right["trial_id"].split(".",1)[1])
            for key in ("kind","task_id","profile","candidate_id","repeat","run_key"):
                if key in left or key in right:
                    self.assertEqual(left.get(key),right.get(key))
            self.assertEqual(left["experiment_id"],"retry-a")
            self.assertTrue(1 <= len(left["trial_id"]) <= 100)
        self.assertEqual(len(prepare.namespaced_trial_id("x" * 24, "t" * 75)), 100)
        with self.assertRaises(ValueError):prepare.namespaced_trial_id("x" * 24, "t" * 76)
        for value in ("", "../batch", "批次", "a"*25, "-bad"):
            if value:
                with self.assertRaises(ValueError):prepare.checked_experiment_id(value)

    def test_resume_rejects_unpinned_or_mixed_identity_before_adapter(self):
        with tempfile.TemporaryDirectory(prefix="aiwf-resume-identity-") as directory:
            root=Path(directory)
            cfg={"model_profile":"offline-test-only","budget":{"issue_calls":80,"daily_calls":500,"issue_minutes":120,"max_attempts":2,"max_fix_rounds":2,"input_price_per_million":0,"output_price_per_million":0},"runner":{"attempt_minutes":30,"agent_concurrency":8,"workflow_agent_concurrency":3,"max_tasks_per_workflow":64,"max_evidence_requests":64,"max_tool_calls_per_run":200}}
            (root/"config.json").write_text(json.dumps(cfg))
            subprocess.run([sys.executable,str(ROOT/"eval/cli.py"),"matrix","--execution-config",str(root/"config.json"),"--output",str(root/"matrix.json")],check=True,stdout=subprocess.DEVNULL)
            matrix=json.loads((root/"matrix.json").read_text())
            pins={"repository":"zymooll/ai-workflow-alpha-test","repository_id":1416790756,"installation_id":22,"app_id":5285791,"manifest_digest":prepare.evaluation.checked_manifest(ROOT/"eval/manifest.json")[1],"config_digest":matrix["config_sha256"],"seed":20261013}
            stale={key:value for key,value in pins.items() if key not in ("repository","repository_id","installation_id","app_id")}
            stale["trials"]=[]
            mixed={**pins,"trials":[{"trial_id":"old-review","repository":"zymooll/ai-workflow-alpha-test","repository_id":1416790756,"installation_id":22,"app_id":5285792}]}
            argv=["prepare","--execute","--manifest",str(ROOT/"eval/manifest.json"),"--matrix",str(root/"matrix.json"),"--adapter",sys.executable,"--output",str(root/"snapshots.json"),"--workspace-root",str(root/"workspaces"),"--repository",pins["repository"],"--repository-id",str(pins["repository_id"]),"--installation-id",str(pins["installation_id"]),"--app-id",str(pins["app_id"]),"--seed",str(pins["seed"])]
            for saved in (stale,mixed):
                (root/"snapshots.json").write_text(json.dumps(saved))
                adapter=Mock(side_effect=AssertionError("resume identity must be checked before adapter invocation"))
                with self.subTest(saved_trial_count=len(saved["trials"])),patch.object(sys,"argv",argv),patch.object(prepare,"call",adapter),self.assertRaises(ValueError):
                    prepare.main()
                adapter.assert_not_called()

    def test_prepare_120_independent_snapshots_and_blind_plan(self):
        with tempfile.TemporaryDirectory(prefix="aiwf-offline-plan-") as directory:
            root=Path(directory)
            cfg={"model_profile":"offline-test-only","budget":{"issue_calls":80,"daily_calls":500,"issue_minutes":120,"max_attempts":2,"max_fix_rounds":2,"input_price_per_million":0,"output_price_per_million":0},"runner":{"attempt_minutes":30,"agent_concurrency":8,"workflow_agent_concurrency":3,"max_tasks_per_workflow":64,"max_evidence_requests":64,"max_tool_calls_per_run":200}}
            (root/"config.json").write_text(json.dumps(cfg))
            subprocess.run([sys.executable,str(ROOT/"eval/cli.py"),"matrix","--execution-config",str(root/"config.json"),"--output",str(root/"matrix.json")],check=True,stdout=subprocess.DEVNULL)
            calls=[]
            def adapter(path,request):
                calls.append(request)
                base=hashlib.sha1((request["trial"]["trial_id"]+":base").encode()).hexdigest()
                review=request["trial"]["kind"]=="review"
                return {"protocol":"pilot-eval-prepare/v1","ok":True,"repository":request["repository"],"repository_id":1416790756,"installation_id":2,"app_id":5285791,"source_sha256":request["source_sha256"],"independently_rebuilt":True,"base_source_sha256":request["baseline_source_sha256"] if review else request["source_sha256"],"base_sha":base,"candidate_sha":hashlib.sha1((base+"head").encode()).hexdigest() if review else base,"base_branch":"eval/"+request["trial"]["trial_id"]+"/base","candidate_branch":"eval/"+request["trial"]["trial_id"]+"/candidate","pull_request":len(calls) if review else 0}
            argv=["prepare","--execute","--manifest",str(ROOT/"eval/manifest.json"),"--matrix",str(root/"matrix.json"),"--adapter",sys.executable,"--output",str(root/"snapshots.json"),"--workspace-root",str(root/"workspaces"),"--repository","zymooll/ai-workflow-alpha-test","--repository-id","1416790756","--installation-id","2","--app-id","5285791","--seed","20261013","--experiment-id","retry-a"]
            with patch.object(sys,"argv",argv),patch.object(prepare,"call",adapter):
                prepare.main()
                prepare.main() # durable completed records do not invoke adapter again
            self.assertEqual(len(calls),120)
            matrix=json.loads((root/"matrix.json").read_text())
            snapshots=json.loads((root/"snapshots.json").read_text())
            self.assertEqual(snapshots["experiment_id"],"retry-a")
            self.assertTrue(all(t["experiment_id"]=="retry-a" for t in snapshots["trials"]))
            dev_snapshots=[t for t in snapshots["trials"] if t["kind"]=="development"]
            self.assertEqual({t["run_key"] for t in dev_snapshots},{r["run_key"] for r in matrix["runs"]})
            mismatched_argv=argv[:-1]+["retry-b"]
            adapter=Mock(side_effect=AssertionError("a different batch must be rejected before adapter invocation"))
            with patch.object(sys,"argv",mismatched_argv),patch.object(prepare,"call",adapter),self.assertRaises(ValueError):
                prepare.main()
            adapter.assert_not_called()
            args=SimpleNamespace(manifest=str(ROOT/"eval/manifest.json"),matrix=str(root/"matrix.json"),snapshots=str(root/"snapshots.json"),seed=20261013,experiment_id="retry-a",output=str(root/"plan.json"))
            executor.make_plan(args)
            plan=json.loads((root/"plan.json").read_text())
            self.assertEqual(plan["experiment_id"],"retry-a")
            self.assertEqual([sum(t["kind"]==kind for t in plan["trials"]) for kind in ("smoke","development","review")],[10,90,20])
            planned_dev=[t for t in plan["trials"] if t["kind"]=="development"]
            self.assertEqual({t["run_key"] for t in planned_dev},{r["run_key"] for r in matrix["runs"]})
            self.assertTrue(all(t["trial_id"]=="retry-a."+t["run_key"] and t["request"]["trial_id"]==t["trial_id"] for t in planned_dev))
            mismatch=SimpleNamespace(**{**vars(args),"experiment_id":"retry-b","output":str(root/"other-plan.json")})
            with self.assertRaises(ValueError):executor.make_plan(mismatch)
            legacy=json.loads(json.dumps(snapshots))
            legacy.pop("experiment_id")
            for item in legacy["trials"]:
                item.pop("experiment_id")
                item["trial_id"]=item["trial_id"].removeprefix("retry-a.")
                if item["kind"]=="development":item.pop("run_key")
            (root/"legacy-snapshots.json").write_text(json.dumps(legacy))
            legacy_args=SimpleNamespace(manifest=str(ROOT/"eval/manifest.json"),matrix=str(root/"matrix.json"),snapshots=str(root/"legacy-snapshots.json"),seed=20261013,output=str(root/"legacy-plan.json"))
            executor.make_plan(legacy_args)
            legacy_plan=json.loads((root/"legacy-plan.json").read_text())
            self.assertEqual(legacy_plan["experiment_id"],"")
            self.assertTrue(all(not t["trial_id"].startswith("retry-a.") for t in legacy_plan["trials"]))
            self.assertTrue(all(t["trial_id"]==t["run_key"] for t in legacy_plan["trials"] if t["kind"]=="development"))
            reviews=[t for t in plan["trials"] if t["kind"]=="review"]
            self.assertTrue(all("defect" not in t["trial_id"] and "fix" not in t["trial_id"] for t in reviews))
            with self.assertRaises(ValueError):executor.make_plan(args)

    def test_default_prepare_preserves_legacy_adapter_reservation_digest(self):
        with tempfile.TemporaryDirectory(prefix="aiwf-legacy-adapter-") as directory:
            root=Path(directory)
            cfg={"model_profile":"offline-test-only","budget":{"issue_calls":80,"daily_calls":500,"issue_minutes":120,"max_attempts":2,"max_fix_rounds":2,"input_price_per_million":0,"output_price_per_million":0},"runner":{"attempt_minutes":30,"agent_concurrency":8,"workflow_agent_concurrency":3,"max_tasks_per_workflow":64,"max_evidence_requests":64,"max_tool_calls_per_run":200}}
            (root/"config.json").write_text(json.dumps(cfg))
            subprocess.run([sys.executable,str(ROOT/"eval/cli.py"),"matrix","--execution-config",str(root/"config.json"),"--output",str(root/"matrix.json")],check=True,stdout=subprocess.DEVNULL)
            matrix=json.loads((root/"matrix.json").read_text())
            manifest,manifest_digest=prepare.evaluation.checked_manifest(ROOT/"eval/manifest.json")
            trials=prepare.build_trials(manifest,matrix,20261013)
            identity={"repository":"zymooll/ai-workflow-alpha-test","repository_id":1416790756,"installation_id":22,"app_id":5285791}
            pins={**identity,"manifest_digest":manifest_digest,"config_digest":matrix["config_sha256"],"seed":20261013,"experiment_id":""}
            saved={**pins,"trials":[{**trial,**identity,"seed":20261013,"manifest_version":manifest["version"],"manifest_digest":manifest_digest,"config_digest":matrix["config_sha256"]} for trial in trials[1:]]}
            (root/"snapshots.json").write_text(json.dumps(saved))
            argv=["prepare","--execute","--manifest",str(ROOT/"eval/manifest.json"),"--matrix",str(root/"matrix.json"),"--adapter",sys.executable,"--output",str(root/"snapshots.json"),"--workspace-root",str(root/"workspaces"),"--repository",identity["repository"],"--repository-id",str(identity["repository_id"]),"--installation-id",str(identity["installation_id"]),"--app-id",str(identity["app_id"]),"--seed","20261013"]
            payloads=[]
            def fake_adapter(_adapter,payload):
                payloads.append(payload)
                return {**identity,"source_sha256":payload["source_sha256"],"base_source_sha256":payload["baseline_source_sha256"],"independently_rebuilt":True,"base_sha":"a"*40,"candidate_sha":"a"*40,"base_branch":"eval/smoke-T01/base","candidate_branch":"eval/smoke-T01/candidate","pull_request":0}
            with patch.object(sys,"argv",argv),patch.object(prepare,"call",side_effect=fake_adapter):
                prepare.main()
            self.assertEqual(len(payloads),1)
            request=payloads[0]
            old_trial={"trial_id":"smoke-T01","task_id":"T01","profile":"full","kind":"smoke","candidate_id":"T01-defect","repeat":1}
            self.assertEqual(request["trial"],old_trial)
            self.assertEqual(request["idempotency_key"],old_trial["trial_id"])
            trusted_ci={".github/workflows/tests.yml":"a"*64}
            expected_request={**request,"trial":old_trial}
            legacy_reservation=hashlib.sha256(github_adapter.admin.canonical({"request":expected_request,"trusted_ci":trusted_ci})).hexdigest()
            emitted_reservation=hashlib.sha256(github_adapter.admin.canonical({"request":request,"trusted_ci":trusted_ci})).hexdigest()
            self.assertEqual(emitted_reservation,legacy_reservation)

    def test_namespaced_run_plan_checkpoint_uses_trial_id_and_logical_record_key(self):
        with tempfile.TemporaryDirectory(prefix="aiwf-run-checkpoint-") as directory:
            root=Path(directory)
            (root/"records").mkdir()
            trial={"trial_id":"retry-a.T01-A-1","experiment_id":"retry-a","run_key":"T01-A-1","kind":"development","task_id":"T01","candidate_id":"T01-defect","profile":"builder_ci","repeat":1,"request":{"trial_id":"retry-a.T01-A-1","config_digest":"cfg","manifest_digest":"manifest"}}
            plan={"protocol":"alpha-evaluation/v1","experiment_id":"retry-a","config_sha256":"cfg","trials":[trial],"human_review":"pending","human_merge":"pending"}
            plan_path=root/"plan.json"
            plan_path.write_text(json.dumps(plan))
            record={"trial_id":trial["trial_id"],"experiment_id":"retry-a","run_key":"T01-A-1","kind":"development","config_sha256":"cfg","outcome":"budget_exhausted","evidence_files":[],"human_review":"pending","human_merge":"pending"}
            (root/"records"/(trial["trial_id"]+".json")).write_text(json.dumps(record))
            args=SimpleNamespace(execute=True,output=str(root),plan=str(plan_path),server="http://offline.invalid",token_file="unused",poll_seconds=0)
            control=Mock()
            control.request.return_value={"protocol":"alpha-evaluation/v1","config_sha256":"cfg","config":{"budget":{"issue_minutes":30}}}
            with patch.object(executor,"Control",return_value=control):
                executor.run_plan(args)
            checkpoint=json.loads((root/"checkpoint.json").read_text())
            self.assertEqual(checkpoint["completed"],[trial["trial_id"]])
            self.assertNotIn(trial["run_key"],checkpoint["completed"])
            control.request.assert_called_once_with("GET","/api/evaluation/config")
            exported=[json.loads(x) for x in (root/"development.jsonl").read_text().splitlines()]
            self.assertEqual([(x["trial_id"],x["run_key"],x["experiment_id"]) for x in exported],[(trial["trial_id"],trial["run_key"],trial["experiment_id"])])

    def test_record_result_keeps_prior_batch_and_exports_logical_run_key(self):
        with tempfile.TemporaryDirectory(prefix="aiwf-batch-records-") as directory:
            out=Path(directory)
            (out/"records").mkdir()
            old=out/"records"/"T01-A-1.json"
            old.write_text('{"outcome":"budget_exhausted","run_key":"T01-A-1","human_review":"pending"}\n')
            original=old.read_bytes()
            snapshot={"workflow":{"id":"workflow-1","evidence":[],"review":{"verdict":"BLOCKED","key":"","tasks":[],"findings":[]},"model_calls":12,"input_tokens":300,"output_tokens":40,"runtime_seconds":70,"last_error":"blocked","control":"ACTIVE"},"trusted_judgment":{"status":"UNVERIFIED"},"config":{"budget":{"issue_calls":200,"issue_minutes":120}},"cost":{"value":0},"models":{"dev":"offline","review":"offline"},"issue_url":"","pull_request_url":""}
            trials=[]
            for experiment in ("retry-a","retry-b"):
                trial={"trial_id":experiment+".T01-A-1","experiment_id":experiment,"run_key":"T01-A-1","kind":"development","task_id":"T01","candidate_id":"T01-defect","profile":"builder_ci","repeat":1,"request":{"trial_id":experiment+".T01-A-1","config_digest":"cfg","manifest_digest":"manifest"}}
                result=executor.record_result(None,trial,snapshot,out)
                self.assertEqual(result["run_key"],"T01-A-1")
                self.assertEqual(result["trial_id"],trial["trial_id"])
                self.assertEqual(result["experiment_id"],experiment)
                trials.append(trial)
            self.assertEqual(old.read_bytes(),original,"new batch overwrote the earlier failed default trial")
            executor.export_records({"trials":trials},out)
            exported=[json.loads(line) for line in (out/"development.jsonl").read_text().splitlines()]
            self.assertEqual([x["run_key"] for x in exported],["T01-A-1","T01-A-1"])
            self.assertEqual({x["experiment_id"] for x in exported},{"retry-a","retry-b"})
            self.assertEqual({x["trial_id"] for x in exported},{"retry-a.T01-A-1","retry-b.T01-A-1"})
            self.assertTrue(all(x["human_review"]=="pending" and x["human_merge"]=="pending" for x in exported))

if __name__=="__main__":unittest.main()
