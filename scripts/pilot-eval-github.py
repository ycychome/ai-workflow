#!/usr/bin/env python3
"""pilot-eval-prepare/v1 的真实 GitHub Git Data API 适配器。只接受显式 stdin 请求。"""
import base64
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import sys

spec = importlib.util.spec_from_file_location("pilot_admin", Path(__file__).with_name("pilot-github-admin.py"))
admin = importlib.util.module_from_spec(spec)
spec.loader.exec_module(admin)
target_spec = importlib.util.spec_from_file_location("evaluation_target", Path(__file__).with_name("pilot-eval-target.py"))
target_policy = importlib.util.module_from_spec(target_spec)
target_spec.loader.exec_module(target_policy)

def evaluation_operator_token():
    if os.environ.get("PILOT_EVAL_WRITE_TOKEN") or os.environ.get("PILOT_EVAL_WRITE_TOKEN_FILE"):
        return admin.secret("PILOT_EVAL_WRITE_TOKEN")
    env = os.environ.copy()
    env["GIT_TERMINAL_PROMPT"] = "0"
    try:
        result = subprocess.run(["git", "credential", "fill"], input=b"protocol=https\nhost=github.com\n\n", stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, env=env, timeout=15, check=False)
    except (OSError, subprocess.SubprocessError):
        raise ValueError("缺少评估操作员凭据；设置 PILOT_EVAL_WRITE_TOKEN(_FILE) 或配置 github.com Git credential") from None
    if result.returncode != 0:
        raise ValueError("无法从 github.com Git credential 取得评估操作员凭据")
    fields = {}
    for line in result.stdout.decode("utf-8", errors="strict").splitlines():
        if "=" in line:
            key, value = line.split("=", 1)
            fields[key] = value
    if fields.get("host") != "github.com" or not fields.get("username") or not fields.get("password"):
        raise ValueError("github.com Git credential 未返回有效操作员身份")
    return fields["password"]

class EvaluationGitHub:
    def __init__(self):
        # Evaluation uses a dedicated GitHub client: constructing the destructive
        # admin adapter would also log into the maintenance control plane.
        self.http = admin.HTTP()
        self.operator_token = evaluation_operator_token()
        self.installation_token = None
        self.prefix = "/repos/" + target_policy.TARGET_REPOSITORY

    def gh(self, method, path, body=None, missing=False):
        # The App token is deliberately read-only. Hash-pinned workflow files
        # and frozen Git objects are written with the already verified operator.
        token = self.installation_token if method == "GET" and self.installation_token else self.operator_token
        return self.http.call(method, admin.API + self.prefix + path, body, token=token, missing=missing)

    def identity(self, request):
        target_policy.validate_identity(request.get("repository"), request.get("repository_id"), request.get("installation_id"), request.get("app_id"))
        configured_app_id = int(os.environ["PILOT_GITHUB_APP_ID"])
        if configured_app_id != request["app_id"]:
            raise ValueError("显式 App ID 与本机配置不匹配")
        user = self.http.call("GET", admin.API + "/user", token=self.operator_token)
        repo = self.http.call("GET", admin.API + "/repos/" + target_policy.TARGET_REPOSITORY, token=self.operator_token)
        if (repo.get("full_name") != target_policy.TARGET_REPOSITORY or repo.get("id") != target_policy.TARGET_REPOSITORY_ID
                or not repo.get("permissions", {}).get("admin") or user.get("type") != "User"
                or user.get("login") != target_policy.TARGET_OWNER_LOGIN or user.get("id") != target_policy.TARGET_OWNER_ID):
            raise ValueError("当次操作员不是已核验的仓库所有者，或测试仓库数字 ID 不匹配")
        app_token = admin.app_jwt()
        installation = self.http.call("GET", admin.API + "/repos/" + target_policy.TARGET_REPOSITORY + "/installation", token=app_token)
        if installation.get("id") != request["installation_id"] or installation.get("app_id") != request["app_id"]:
            raise ValueError("测试仓库的真实 App 安装身份不匹配")
        return {"repository": target_policy.TARGET_REPOSITORY, "repository_id": repo["id"], "installation_id": installation["id"], "app_id": installation["app_id"], "actor": {"login": user["login"], "id": user["id"], "admin": True}}, app_token

    def authorize_installation_token(self, identity, app_token):
        required = {"actions": "read", "contents": "read", "pull_requests": "read"}
        result = self.http.call("POST", admin.API + "/app/installations/" + str(identity["installation_id"]) + "/access_tokens", {"repository_ids": [target_policy.TARGET_REPOSITORY_ID], "permissions": required}, token=app_token)
        permissions = result.get("permissions", {})
        repositories = result.get("repositories", [])
        unrequested_writes = [name for name, level in permissions.items() if level == "write" and required.get(name) != "write"]
        if (any(permissions.get(name) != level for name, level in required.items())
                or unrequested_writes
                or len(repositories) != 1 or repositories[0].get("id") != target_policy.TARGET_REPOSITORY_ID
                or not result.get("token")):
            raise ValueError("测试仓库的 App 安装缺少评估所需的单仓库只读权限")
        self.installation_token = result["token"]

    def tree_files(self, sha):
        result = self.gh("GET", "/git/trees/" + sha + "?recursive=1")
        if result.get("truncated"):
            raise ValueError("远端 Git 树被截断，无法验证完整源码")
        files = {}
        for item in result["tree"]:
            if item["type"] == "blob":
                blob = self.gh("GET", "/git/blobs/" + item["sha"])
                if blob["encoding"] != "base64":
                    raise ValueError("Git blob 编码不受支持")
                files[item["path"]] = base64.b64decode(blob["content"])
            elif item["type"] != "tree":
                raise ValueError("评估快照不得含 submodule")
        return files

    def commit(self, files, message, parent=None):
        entries = []
        for path, data in sorted(files.items()):
            blob = self.gh("POST", "/git/blobs", {"encoding": "base64", "content": base64.b64encode(data).decode()})
            entries.append({"path": path, "mode": "100644", "type": "blob", "sha": blob["sha"]})
        tree = self.gh("POST", "/git/trees", {"tree": entries})
        return self.gh("POST", "/git/commits", {"message": message, "tree": tree["sha"], "parents": [parent] if parent else []})["sha"]

    def ensure_ref(self, name, sha):
        old = self.gh("GET", "/git/ref/heads/" + name, missing=True)
        if old is not None:
            if old["object"]["sha"] != sha:
                raise ValueError("既有试验引用绑定另一提交；拒绝覆盖")
            return
        self.gh("POST", "/git/refs", {"ref": "refs/heads/" + name, "sha": sha})
        if self.gh("GET", "/git/ref/heads/" + name)["object"]["sha"] != sha:
            raise ValueError("试验引用读回不匹配")

    def prepare(self, request):
        identity, app_token = self.identity(request)
        self.authorize_installation_token(identity, app_token)
        trial = request["trial"]
        trial_id = trial["trial_id"]
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,100}", trial_id) or request["idempotency_key"] != trial_id:
            raise ValueError("试验 ID 无效")
        root = Path(request["workspace"]).resolve(strict=True)
        files = {}
        for path in root.rglob("*"):
            if path.is_symlink():
                raise ValueError("评估候选包含符号链接")
            if path.is_file():
                relative = path.relative_to(root).as_posix()
                if relative not in request["source_files"] and relative not in ("ISSUE.md", "AGENTS.md"):
                    raise ValueError("候选中存在未冻结文件：" + relative)
                files[relative] = path.read_bytes()
        hashes = {path: hashlib.sha256(files[path]).hexdigest() for path in request["source_files"]}
        actual = hashlib.sha256(json.dumps(hashes, sort_keys=True).encode()).hexdigest()
        if hashes != request["source_files"] or actual != request["source_sha256"]:
            raise ValueError("候选实际文件与冻结摘要不匹配")
        # CI definitions are operator-installed, hash-pinned trusted additions.
        # Hidden test sources and labels never enter the candidate repository.
        trusted_path = Path(os.environ["PILOT_EVAL_TRUSTED_FILES"]).resolve(strict=True)
        trusted = json.loads(trusted_path.read_text())
        if not trusted:
            raise ValueError("没有配置受信 CI 文件，不能准备不可验证的候选")
        ci_hashes = {}
        for target, item in trusted.items():
            if not re.fullmatch(r"\.github/(workflows|actions)/[A-Za-z0-9_./-]+", target) or ".." in target.split("/"):
                raise ValueError("受信附加文件必须限定 CI 配置路径")
            path = (trusted_path.parent / item["path"]).resolve(strict=True)
            data = path.read_bytes()
            if hashlib.sha256(data).hexdigest() != item["sha256"]:
                raise ValueError("受信 CI 文件摘要变化")
            files[target] = data
            ci_hashes[target] = item["sha256"]
        state_root = Path(os.environ["PILOT_EVAL_STATE_DIRECTORY"]).resolve()
        state_root.mkdir(parents=True, exist_ok=True)
        state_root.chmod(0o700)
        state_path = state_root / (trial_id + ".json")
        request_digest = hashlib.sha256(admin.canonical({"request": request, "trusted_ci": ci_hashes})).hexdigest()
        import fcntl
        lock = (state_root / (trial_id + ".lock")).open("a")
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            state = json.loads(state_path.read_text()) if state_path.exists() else {"request_digest": request_digest, "base_branch": "eval/" + trial_id + "/base", "candidate_branch": "eval/" + trial_id + "/candidate"}
            if state["request_digest"] != request_digest:
                raise ValueError("既有试验预留绑定不同请求")
            def checkpoint():
                temp = state_path.with_suffix(".tmp")
                with temp.open("wb") as output:
                    output.write(admin.canonical(state))
                    output.flush()
                    os.fsync(output.fileno())
                os.replace(temp, state_path)
                state_path.chmod(0o600)
            checkpoint()
            baseline_files = files
            if trial["kind"] == "review":
                baseline_root = Path(os.environ["PILOT_EVAL_BASELINE_DIRECTORY"]).resolve(strict=True)
                baseline_files = dict(files)
                for path in request["source_files"]:
                    source = (baseline_root / path).resolve(strict=True)
                    if not source.is_relative_to(baseline_root):
                        raise ValueError("基线文件路径越界")
                    baseline_files[path] = source.read_bytes()
                baseline_hashes = {p: hashlib.sha256(baseline_files[p]).hexdigest() for p in request["source_files"]}
                if hashlib.sha256(json.dumps(baseline_hashes, sort_keys=True).encode()).hexdigest() != request["baseline_source_sha256"]:
                    raise ValueError("审查对照基线与冻结 manifest 不一致")
            if not state.get("base_sha"):
                state["base_sha"] = self.commit(baseline_files, "独立评估基线 " + trial_id)
                checkpoint()
            self.ensure_ref(state["base_branch"], state["base_sha"])
            if not state.get("candidate_sha"):
                state["candidate_sha"] = self.commit(files, "固定候选 " + trial_id, state["base_sha"]) if trial["kind"] == "review" else state["base_sha"]
                checkpoint()
            if trial["kind"] == "review":
                self.ensure_ref(state["candidate_branch"], state["candidate_sha"])
            # Re-read every remote blob, not just a ref's SHA supplied by the client.
            remote = self.tree_files(state["candidate_sha"])
            if remote != files:
                raise ValueError("远端候选树与实际冻结文件不完全相等")
            remote_base = self.tree_files(state["base_sha"])
            if remote_base != baseline_files:
                raise ValueError("远端基线树与实际冻结基线不完全相等")
            if trial["kind"] == "review" and not state.get("pull_request"):
                head = target_policy.TARGET_REPOSITORY.split("/")[0] + ":" + state["candidate_branch"]
                query = "/pulls?state=all&head=" + admin.urllib.parse.quote(head, safe="") + "&base=" + admin.urllib.parse.quote(state["base_branch"], safe="")
                matches = self.gh("GET", query)
                if len(matches) > 1:
                    raise ValueError("同一候选存在多个 PR，需人工处理")
                if matches:
                    pr = matches[0]
                else:
                    if state.get("pr_creating"):
                        raise ValueError("候选 PR 创建结果未知；尚未找到原 PR，禁止重复创建")
                    state["pr_creating"] = True
                    checkpoint()
                    pr = self.gh("POST", "/pulls", {"title": trial["task_id"] + " 固定候选审查", "body": "仅审查冻结候选，不修改代码；实际结论以独立验证和审查证据为准。", "head": state["candidate_branch"], "base": state["base_branch"], "draft": True})
                state["pull_request"] = pr["number"]
                checkpoint()
            if trial["kind"] == "review":
                pr = self.gh("GET", "/pulls/" + str(state["pull_request"]))
                if pr["state"] != "open" or pr["head"]["sha"] != state["candidate_sha"] or pr["base"]["sha"] != state["base_sha"] or pr["head"]["repo"]["id"] != identity["repository_id"] or pr["base"]["repo"]["id"] != identity["repository_id"]:
                    raise ValueError("真实审查 PR 身份或 head/base 已变化")
            base_hashes = {p: hashlib.sha256(baseline_files[p]).hexdigest() for p in request["source_files"]}
            return {**identity, **{k: state[k] for k in ("base_sha", "candidate_sha", "base_branch", "candidate_branch")}, "pull_request": state.get("pull_request", 0), "source_sha256": actual, "base_source_sha256": hashlib.sha256(json.dumps(base_hashes, sort_keys=True).encode()).hexdigest(), "trusted_ci_files": ci_hashes, "independently_rebuilt": True}
        finally:
            lock.close()

def main():
    request = json.loads(sys.stdin.buffer.read(32 << 20))
    if request.get("protocol") != "pilot-eval-prepare/v1" or request.get("operation") != "prepare_independent_trial":
        raise ValueError("不支持的评估准备请求")
    target_policy.validate_identity(request.get("repository"), request.get("repository_id"), request.get("installation_id"), request.get("app_id"))
    result = EvaluationGitHub().prepare(request)
    print(json.dumps({"protocol": "pilot-eval-prepare/v1", "ok": True, **result}, ensure_ascii=False))

if __name__ == "__main__":
    try:
        main()
    except Exception:
        print("评估引用准备失败；保留持久回执，未输出凭据。", file=sys.stderr)
        raise SystemExit(1)
