"""真实评估适配器逻辑的离线故障注入；无网络、模型或GitHub写入。"""
import copy,hashlib,importlib.util,json,os
from pathlib import Path
import tempfile,unittest
from unittest.mock import Mock,patch

spec=importlib.util.spec_from_file_location("eval_github",Path(__file__).with_name("pilot-eval-github.py"))
eval_github=importlib.util.module_from_spec(spec);spec.loader.exec_module(eval_github)

class EvaluationGitHubRecoveryTests(unittest.TestCase):
    def test_existing_ref_never_overwritten(self):
        adapter=object.__new__(eval_github.EvaluationGitHub)
        adapter.gh=Mock(return_value={"object":{"sha":"a"*40}})
        with self.assertRaises(ValueError):adapter.ensure_ref("eval/trial/base","b"*40)
        self.assertEqual(adapter.gh.call_count,1)

    def test_unknown_pr_creation_recovers_existing_pr_without_duplicate(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);workspace=root/"candidate";workspace.mkdir();baseline=root/"baseline";baseline.mkdir()
            (workspace/"app.go").write_bytes(b"defect");(workspace/"ISSUE.md").write_bytes(b"task")
            (baseline/"app.go").write_bytes(b"correct")
            (root/"ci.yml").write_bytes(b"name: test\n")
            (root/"trusted.json").write_text(json.dumps({".github/workflows/test.yml":{"path":"ci.yml","sha256":hashlib.sha256((root/"ci.yml").read_bytes()).hexdigest()}}))
            hashes={"app.go":hashlib.sha256(b"defect").hexdigest()}
            source_hash=lambda h:hashlib.sha256(json.dumps(h,sort_keys=True).encode()).hexdigest()
            identity={"repository":"ycychome/ai-workflow-pilot","repository_id":1,"installation_id":2}
            request={"protocol":"pilot-eval-prepare/v1","operation":"prepare_independent_trial",**identity,"trial":{"trial_id":"review-opaque","task_id":"T01","kind":"review"},"idempotency_key":"review-opaque","workspace":str(workspace),"source_files":hashes,"source_sha256":source_hash(hashes),"baseline_source_sha256":source_hash({"app.go":hashlib.sha256(b"correct").hexdigest()})}
            adapter=object.__new__(eval_github.EvaluationGitHub);adapter.admin=Mock();adapter.admin.identity.return_value=identity
            refs={};trees={};prs=[];post_calls=[];commits=[]
            def commit(files,message,parent=None):
                sha=hashlib.sha1((str(len(trees))+message).encode()).hexdigest();trees[sha]=copy.deepcopy(files);commits.append(sha);return sha
            adapter.commit=commit;adapter.tree_files=lambda sha:copy.deepcopy(trees[sha])
            def gh(method,path,body=None,missing=False):
                if path.startswith("/git/ref/heads/"):return {"object":{"sha":refs[path[len('/git/ref/heads/'):]]}} if path[len('/git/ref/heads/'):] in refs else None
                if path=="/git/refs":refs[body["ref"].removeprefix("refs/heads/")]=body["sha"];return {}
                if path.startswith("/pulls?"):return copy.deepcopy(prs)
                if path=="/pulls" and method=="POST":
                    post_calls.append(body);pr={"number":7,"state":"open","head":{"sha":refs[body['head']],"repo":{"id":1}},"base":{"sha":refs[body['base']],"repo":{"id":1}}};prs.append(pr)
                    raise ValueError("模拟PR已创建但HTTP响应丢失")
                if path=="/pulls/7":return copy.deepcopy(prs[0])
                raise AssertionError((method,path))
            adapter.gh=gh
            env={"PILOT_EVAL_TRUSTED_FILES":str(root/"trusted.json"),"PILOT_EVAL_STATE_DIRECTORY":str(root/"state"),"PILOT_EVAL_BASELINE_DIRECTORY":str(baseline)}
            with patch.dict(os.environ,env):
                with self.assertRaises(ValueError):adapter.prepare(request)
                result=adapter.prepare(request)
                self.assertEqual(result["pull_request"],7)
                self.assertEqual(len(post_calls),1)
                self.assertEqual(len(commits),2)
                self.assertTrue(result["independently_rebuilt"])
                refs[result["base_branch"]]="f"*40
                with self.assertRaises(ValueError):adapter.prepare(request)
                self.assertEqual(len(post_calls),1)

if __name__=="__main__":unittest.main()
