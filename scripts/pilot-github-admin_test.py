"""管理员适配器恢复保护回归；统一测试阶段再执行。"""
import copy
import importlib.util
from pathlib import Path
import unittest
from unittest.mock import Mock

spec=importlib.util.spec_from_file_location("pilot_admin",Path(__file__).with_name("pilot-github-admin.py"))
admin=importlib.util.module_from_spec(spec)
spec.loader.exec_module(admin)

class ProtectionRecoveryTests(unittest.TestCase):
    def test_unknown_ruleset_change_is_rejected_before_write(self):
        original={"branch":None,"rulesets":[{"id":1,"enforcement":"active","name":"main","rules":[]}],"other_branches":{}}
        observed=copy.deepcopy(original)
        observed["rulesets"][0]["rules"]=[{"type":"required_signatures"}]
        adapter=object.__new__(admin.Adapter)
        adapter.protection=Mock(return_value=observed)
        adapter.gh=Mock()
        with self.assertRaises(ValueError):
            adapter.restore_protection({"branch_protection":original},[],lambda *args:None)
        adapter.gh.assert_not_called()

    def test_disabled_rule_requires_preexisting_intent(self):
        original={"branch":None,"rulesets":[{"id":1,"enforcement":"active","name":"main","rules":[]}],"other_branches":{}}
        observed=copy.deepcopy(original)
        observed["rulesets"][0]["enforcement"]="disabled"
        adapter=object.__new__(admin.Adapter)
        adapter.protection=Mock(return_value=observed)
        adapter.gh=Mock()
        with self.assertRaises(ValueError):
            adapter.restore_protection({"branch_protection":original},[],lambda *args:None)
        adapter.gh.assert_not_called()

    def test_recorded_disable_can_recover_after_crash(self):
        original={"branch":None,"rulesets":[{"id":1,"enforcement":"active","name":"main","rules":[]}],"other_branches":{}}
        observed=copy.deepcopy(original)
        observed["rulesets"][0]["enforcement"]="disabled"
        adapter=object.__new__(admin.Adapter)
        adapter.protection=Mock(side_effect=[observed,original])
        adapter.gh=Mock()
        events=[{"kind":"intent_ruleset_disable","detail":{"id":1}}]
        adapter.restore_protection({"branch_protection":original},events,lambda kind,detail:events.append({"kind":kind,"detail":detail}))
        adapter.gh.assert_called_once_with("PUT","/repos/ycychome/ai-workflow-pilot/rulesets/1",{"enforcement":"active"})
        self.assertEqual(events[-1]["kind"],"protection_recovered")


class MaintenanceRecoveryTests(unittest.TestCase):
    def test_same_durable_token_survives_unknown_failure(self):
        import os,tempfile
        from unittest.mock import patch
        with tempfile.TemporaryDirectory() as directory,patch.dict(os.environ,{"PILOT_MAINTENANCE_STATE_DIR":directory}):
            adapter=object.__new__(admin.Adapter)
            requests=[]
            def ctl(operation,body):
                requests.append((operation,copy.deepcopy(body)))
                return {"intake_stopped":True,"active_agents":0,"stopping_agents":0,"active_workflow_leases":0,"inflight_outbox":0}
            adapter.ctl=ctl
            identity={"repository":"ycychome/ai-workflow-pilot","repository_id":1,"installation_id":2}
            key="a"*64
            with self.assertRaises(RuntimeError):
                with adapter.fence(identity,"old",key):
                    raise RuntimeError("模拟远端结果未知")
            self.assertTrue((Path(directory)/(key+".json")).is_file())
            with adapter.fence(identity,"old",key):pass
            acquired=[body for op,body in requests if op=="acquire"]
            self.assertEqual(acquired[0],acquired[1])
            self.assertGreaterEqual(len(acquired[0]["maintenance_token"]),32)
            self.assertTrue(all(body["keep_intake_stopped"] for op,body in requests if op=="release"))

    def test_unknown_acquire_retains_token_for_retry(self):
        import os,tempfile
        from unittest.mock import patch
        with tempfile.TemporaryDirectory() as directory,patch.dict(os.environ,{"PILOT_MAINTENANCE_STATE_DIR":directory}):
            adapter=object.__new__(admin.Adapter)
            adapter.ctl=Mock(side_effect=RuntimeError("模拟网络超时但服务端可能已提交"))
            with self.assertRaises(RuntimeError):
                with adapter.fence({"repository_id":1},"old","b"*64):pass
            self.assertTrue((Path(directory)/("b"*64+".json")).is_file())
            self.assertEqual(adapter.ctl.call_count,1)


class ResumeIntakeTests(unittest.TestCase):
    def test_completed_reset_accepts_only_protection_recovery_after_verification(self):
        import json,os,tempfile
        from unittest.mock import patch
        for tail,allowed in [([{"kind":"protection_recovered"}],True),([{"kind":"intent_main_update"}],False)]:
            with self.subTest(tail=tail), tempfile.TemporaryDirectory() as directory, patch.dict(os.environ,{"PILOT_BACKUP_DIRECTORY":directory,"PILOT_MAINTENANCE_STATE_DIR":directory}):
                root=Path(directory)
                plan={"identity":{"repository":admin.REPO,"repository_id":1},"root_commit":"a"*40,"branch_protection":{"branch":None,"rulesets":[],"other_branches":{}}}
                (root/"plan.json").write_text(json.dumps(plan))
                key=admin.digest(root/"plan.json")
                (root/("apply-log-"+key+".json")).write_text(json.dumps([{"kind":"verified"}]+tail))
                (root/(key+".json")).write_text(json.dumps({"maintenance_token":"test-token"}))
                adapter=object.__new__(admin.Adapter)
                adapter.inspect=Mock(return_value={**plan["identity"],"refs":{"refs/heads/main":plan["root_commit"]},"branch_protection":plan["branch_protection"]})
                adapter.ctl=Mock(return_value={"intake_stopped":False,"repository_epoch":"new"})
                if allowed:
                    self.assertFalse(adapter.resume_intake({"plan_sha256":key})["intake_stopped"])
                    adapter.ctl.assert_called_once()
                else:
                    with self.assertRaises(ValueError):adapter.resume_intake({"plan_sha256":key})
                    adapter.ctl.assert_not_called()


class ApplyRecoveryTests(unittest.TestCase):
    def test_unknown_successful_main_push_resumes_without_second_force(self):
        import contextlib,hashlib,json,os,tempfile
        from unittest.mock import patch
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);identity={"repository":admin.REPO,"repository_id":1,"installation_id":2}
            files=[]
            for role in ("git_bundle","refs","issues","pull_requests","branch_protection","repository_identity","control_database","outbox","control_artifacts"):
                path=root/(role+".fixture");path.write_bytes(b"offline mocked backup")
                files.append({"role":role,"path":path.name,"sha256":admin.digest(path),"bytes":path.stat().st_size})
            verification={k:True for k in ("isolated","database_restored","outbox_matches","bundle_verified","artifacts_verified")}
            verification["files_digest"]=hashlib.sha256(admin.canonical(files)).hexdigest()
            old="1"*40;new="2"*40;branch="eval/old/base";old_branch="3"*40
            protection={"branch":None,"rulesets":[],"other_branches":{}}
            plan={"repository":admin.REPO,"identity":identity,"repository_epoch":"old","root_commit":new,"refs":{"refs/heads/main":old,"refs/heads/"+branch:old_branch},"branch_protection":protection,"backup_files":files,"restore_verification":verification,"actions":{"close_issues":[],"close_pull_requests":[],"delete_branches":[branch],"force_update":{"ref":"refs/heads/main","old_sha":old,"new_sha":new}}}
            (root/"plan.json").write_bytes(admin.canonical(plan));key=admin.digest(root/"plan.json")
            adapter=object.__new__(admin.Adapter);adapter.identity=lambda:dict(identity)
            current_refs=dict(plan["refs"]);epoch=["old"];acquires=[];pushes=[];fail_once=[True]
            adapter.refs=lambda:dict(current_refs)
            adapter.protection=lambda:copy.deepcopy(protection)
            def status():return {"intake_stopped":True,"active_agents":0,"stopping_agents":0,"active_workflow_leases":0,"inflight_outbox":0,"repository_epoch":epoch[0]}
            def ctl(op,body=None):
                if op=="acquire":acquires.append(copy.deepcopy(body))
                if op=="retire":epoch[0]=body["new_epoch"]
                return status()
            adapter.ctl=ctl
            def inspect():return {**identity,**status(),"refs":dict(current_refs),"branch_protection":copy.deepcopy(protection),"reset_actions":{"close_issues":[],"close_pull_requests":[],"delete_branches":[branch] if "refs/heads/"+branch in current_refs else []}}
            adapter.inspect=inspect
            adapter.git_env=lambda:(contextlib.nullcontext(),{})
            def command(argv,**kwargs):
                if argv[:2]==["git","push"]:
                    pushes.append(argv)
                    if argv[-1]==new+":refs/heads/main":
                        current_refs["refs/heads/main"]=new
                        if fail_once[0]:fail_once[0]=False;raise ValueError("模拟push已成功但响应丢失")
                    else:current_refs.pop(argv[-1][1:],None)
                return b""
            req={"plan":plan,"plan_sha256":key,"forbid_repository_recreation":True,"require_force_with_lease":True}
            env={"PILOT_BACKUP_DIRECTORY":str(root),"PILOT_MAINTENANCE_STATE_DIR":str(root/"state")}
            with patch.dict(os.environ,env),patch.object(admin,"command",command):
                with self.assertRaises(ValueError):adapter.apply_reset(req)
                result=adapter.apply_reset({**req,"resume":True})
            self.assertTrue(result["all_actions_verified"])
            self.assertEqual(current_refs,{"refs/heads/main":new})
            self.assertEqual(sum(p[-1]==new+":refs/heads/main" for p in pushes),1)
            self.assertEqual(acquires[0],acquires[1])
            journal=json.loads((root/("apply-log-"+key+".json")).read_text())
            self.assertEqual(journal[-1]["kind"],"verified")
            self.assertIn("intent_main_update",[e["kind"] for e in journal])

if __name__=="__main__":
    unittest.main()
