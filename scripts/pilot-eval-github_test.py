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
            identity={"repository":"zymooll/ai-workflow-alpha-test","repository_id":1416790756,"installation_id":22,"app_id":5285791}
            request={"protocol":"pilot-eval-prepare/v1","operation":"prepare_independent_trial",**identity,"trial":{"trial_id":"review-opaque","task_id":"T01","kind":"review"},"idempotency_key":"review-opaque","workspace":str(workspace),"source_files":hashes,"source_sha256":source_hash(hashes),"baseline_source_sha256":source_hash({"app.go":hashlib.sha256(b"correct").hexdigest()})}
            adapter=object.__new__(eval_github.EvaluationGitHub);adapter.identity=Mock(return_value=(identity,"app-jwt"));adapter.authorize_installation_token=Mock()
            refs={};trees={};prs=[];post_calls=[];commits=[]
            def commit(files,message,parent=None):
                sha=hashlib.sha1((str(len(trees))+message).encode()).hexdigest();trees[sha]=copy.deepcopy(files);commits.append(sha);return sha
            adapter.commit=commit;adapter.tree_files=lambda sha:copy.deepcopy(trees[sha])
            def gh(method,path,body=None,missing=False):
                if path.startswith("/git/ref/heads/"):return {"object":{"sha":refs[path[len('/git/ref/heads/'):]]}} if path[len('/git/ref/heads/'):] in refs else None
                if path=="/git/refs":refs[body["ref"].removeprefix("refs/heads/")]=body["sha"];return {}
                if path.startswith("/pulls?"):return copy.deepcopy(prs)
                if path=="/pulls" and method=="POST":
                    post_calls.append(body);pr={"number":7,"state":"open","head":{"sha":refs[body['head']],"repo":{"id":1416790756}},"base":{"sha":refs[body['base']],"repo":{"id":1416790756}}};prs.append(pr)
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

class EvaluationTargetBoundaryTests(unittest.TestCase):
    def test_operator_token_uses_separate_evaluation_environment_name(self):
        with patch.dict(os.environ,{"PILOT_EVAL_WRITE_TOKEN":"provided","PILOT_EVAL_WRITE_TOKEN_FILE":""}),patch.object(eval_github.admin,"secret",return_value="operator-token") as secret,patch.object(eval_github.subprocess,"run") as run:
            self.assertEqual(eval_github.evaluation_operator_token(),"operator-token")
        secret.assert_called_once_with("PILOT_EVAL_WRITE_TOKEN")
        run.assert_not_called()

    def test_operator_token_falls_back_to_github_git_credential(self):
        output=b"protocol=https\nhost=github.com\nusername=zymooll\npassword=helper-token\n\n"
        completed=eval_github.subprocess.CompletedProcess(["git","credential","fill"],0,stdout=output,stderr=b"")
        with patch.dict(os.environ,{"PILOT_EVAL_WRITE_TOKEN":"","PILOT_EVAL_WRITE_TOKEN_FILE":""}),patch.object(eval_github.subprocess,"run",return_value=completed) as run:
            self.assertEqual(eval_github.evaluation_operator_token(),"helper-token")
        self.assertEqual(run.call_args.args[0],["git","credential","fill"])
        self.assertEqual(run.call_args.kwargs["input"],b"protocol=https\nhost=github.com\n\n")

    def test_evaluation_client_is_separate_from_maintenance_admin(self):
        http=Mock()
        with patch.object(eval_github.admin,"HTTP",return_value=http),patch.object(eval_github,"evaluation_operator_token",return_value="operator-token"),patch.object(eval_github.admin,"Adapter",side_effect=AssertionError("maintenance adapter must not be constructed")):
            adapter=eval_github.EvaluationGitHub()
        self.assertEqual(adapter.prefix,"/repos/zymooll/ai-workflow-alpha-test")
        self.assertEqual(adapter.installation_token,None)

    def test_old_maintenance_repository_is_rejected_before_api_access(self):
        adapter=object.__new__(eval_github.EvaluationGitHub)
        adapter.http=Mock()
        request={"repository":"ycychome/ai-workflow-pilot","repository_id":1416790756,"installation_id":22,"app_id":5285791}
        with self.assertRaises(ValueError):adapter.identity(request)
        adapter.http.call.assert_not_called()

    def test_only_the_pinned_repository_and_numeric_id_pass_target_gate(self):
        target=eval_github.target_policy
        target.validate_identity("zymooll/ai-workflow-alpha-test",1416790756,22,5285791)
        for repository,repository_id in (("ycychome/ai-workflow-pilot",1416790756),("ycychome/test-repo-for-ai-test",1415305337),("zymooll/ai-workflow-alpha-test",1)):
            with self.subTest(repository=repository,repository_id=repository_id),self.assertRaises(ValueError):
                target.validate_identity(repository,repository_id,22,5285791)
        with self.assertRaises(ValueError):
            target.validate_identity("zymooll/ai-workflow-alpha-test",1416790756,22,33)

    def test_identity_checks_actor_repo_installation_and_app(self):
        adapter=object.__new__(eval_github.EvaluationGitHub)
        adapter.operator_token="personal-admin"
        responses={
            "https://api.github.com/user":{"login":"zymooll","id":51841832,"type":"User"},
            "https://api.github.com/repos/zymooll/ai-workflow-alpha-test":{"full_name":"zymooll/ai-workflow-alpha-test","id":1416790756,"permissions":{"admin":True}},
            "https://api.github.com/repos/zymooll/ai-workflow-alpha-test/installation":{"id":22,"app_id":5285791},
        }
        adapter.http=Mock()
        adapter.http.call.side_effect=lambda method,url,body=None,token=None,missing=False:responses[url]
        request={"repository":"zymooll/ai-workflow-alpha-test","repository_id":1416790756,"installation_id":22,"app_id":5285791}
        with patch.dict(os.environ,{"PILOT_GITHUB_APP_ID":"5285791"}),patch.object(eval_github.admin,"app_jwt",return_value="jwt"):
            identity,jwt=adapter.identity(request)
        self.assertEqual((identity["repository_id"],identity["installation_id"],identity["app_id"]),(1416790756,22,5285791))
        self.assertEqual(identity["actor"],{"login":"zymooll","id":51841832,"admin":True})
        self.assertEqual(jwt,"jwt")
        self.assertTrue(all("ai-workflow-pilot" not in call.args[1] and "test-repo-for-ai-test" not in call.args[1] for call in adapter.http.call.call_args_list))

    def test_identity_rejects_non_admin_before_app_installation_lookup(self):
        adapter=object.__new__(eval_github.EvaluationGitHub)
        adapter.operator_token="personal-admin"
        responses={
            "https://api.github.com/user":{"login":"zymooll","id":9,"type":"User"},
            "https://api.github.com/repos/zymooll/ai-workflow-alpha-test":{"full_name":"zymooll/ai-workflow-alpha-test","id":1416790756,"permissions":{"admin":False}},
        }
        adapter.http=Mock()
        adapter.http.call.side_effect=lambda method,url,body=None,token=None,missing=False:responses[url]
        request={"repository":"zymooll/ai-workflow-alpha-test","repository_id":1416790756,"installation_id":22,"app_id":5285791}
        with patch.dict(os.environ,{"PILOT_GITHUB_APP_ID":"5285791"}),patch.object(eval_github.admin,"app_jwt",return_value="jwt"),self.assertRaises(ValueError):
            adapter.identity(request)
        self.assertEqual(adapter.http.call.call_count,2)

    def test_identity_rejects_wrong_installation_or_app_id(self):
        adapter=object.__new__(eval_github.EvaluationGitHub)
        adapter.operator_token="personal-admin"
        responses={
            "https://api.github.com/user":{"login":"zymooll","id":51841832,"type":"User"},
            "https://api.github.com/repos/zymooll/ai-workflow-alpha-test":{"full_name":"zymooll/ai-workflow-alpha-test","id":1416790756,"permissions":{"admin":True}},
            "https://api.github.com/repos/zymooll/ai-workflow-alpha-test/installation":{"id":23,"app_id":5285791},
        }
        adapter.http=Mock()
        adapter.http.call.side_effect=lambda method,url,body=None,token=None,missing=False:responses[url]
        request={"repository":"zymooll/ai-workflow-alpha-test","repository_id":1416790756,"installation_id":22,"app_id":5285791}
        with patch.dict(os.environ,{"PILOT_GITHUB_APP_ID":"5285791"}),patch.object(eval_github.admin,"app_jwt",return_value="jwt"),self.assertRaises(ValueError):
            adapter.identity(request)
        self.assertEqual(adapter.http.call.call_count,3)

        with patch.dict(os.environ,{"PILOT_GITHUB_APP_ID":"5285792"}),self.assertRaises(ValueError):
            adapter.identity(request)
        self.assertEqual(adapter.http.call.call_count,3)

    def test_installation_token_is_scoped_to_target_and_minimum_permissions(self):
        adapter=object.__new__(eval_github.EvaluationGitHub)
        adapter.http=Mock()
        adapter.http.call.return_value={"token":"scoped-token","permissions":{"actions":"read","contents":"read","pull_requests":"read","metadata":"read"},"repositories":[{"id":1416790756}]}
        identity={"installation_id":22}
        adapter.authorize_installation_token(identity,"jwt")
        call=adapter.http.call.call_args
        self.assertEqual(call.args[0:2],("POST","https://api.github.com/app/installations/22/access_tokens"))
        self.assertEqual(call.args[2],{"repository_ids":[1416790756],"permissions":{"actions":"read","contents":"read","pull_requests":"read"}})
        self.assertEqual(adapter.installation_token,"scoped-token")

    def test_installation_token_rejects_any_unrequested_write_permission(self):
        adapter=object.__new__(eval_github.EvaluationGitHub)
        adapter.http=Mock()
        adapter.http.call.return_value={"token":"too-powerful","permissions":{"actions":"read","contents":"read","pull_requests":"read","checks":"write","metadata":"read"},"repositories":[{"id":1416790756}]}
        with self.assertRaises(ValueError):
            adapter.authorize_installation_token({"installation_id":22},"jwt")
        self.assertIsNone(getattr(adapter,"installation_token",None))

    def test_reads_use_scoped_app_token_and_all_writes_use_verified_operator(self):
        adapter=object.__new__(eval_github.EvaluationGitHub)
        adapter.http=Mock()
        adapter.prefix="/repos/zymooll/ai-workflow-alpha-test"
        adapter.operator_token="verified-operator"
        adapter.installation_token="scoped-read-only-app-token"
        adapter.gh("GET","/git/trees/abc")
        adapter.gh("POST","/git/blobs",{"encoding":"base64","content":"eA=="})
        get_call,post_call=adapter.http.call.call_args_list
        self.assertEqual(get_call.kwargs["token"],"scoped-read-only-app-token")
        self.assertEqual(post_call.kwargs["token"],"verified-operator")

if __name__=="__main__":unittest.main()
