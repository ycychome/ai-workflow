#!/usr/bin/env python3
"""pilot-admin/v1 真实适配器。仅处理 stdin 显式请求；不打印秘密。"""
import base64
import contextlib
import datetime as dt
import hashlib
import http.cookiejar
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

REPO = "ycychome/ai-workflow-pilot"
API = "https://api.github.com"

def canonical(value):
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()

def digest(path):
    h = hashlib.sha256()
    with Path(path).open("rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()

def secret(name):
    path = os.environ.get(name + "_FILE")
    value = Path(path).read_text().strip() if path else os.environ.get(name, "").strip()
    if not value:
        raise ValueError("缺少明确配置的凭据：" + name)
    return value

def command(argv, *, data=None, env=None, cwd=None):
    result = subprocess.run(argv, input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env, cwd=cwd, timeout=1800, check=False)
    if result.returncode:
        raise ValueError("本地命令失败：" + Path(argv[0]).name + "；详细日志未输出以保护凭据")
    return result.stdout

def save(path, value):
    with Path(path).open("x") as f:
        json.dump(value, f, ensure_ascii=False, indent=2)
        f.write("\n")
        f.flush()
        os.fsync(f.fileno())
    Path(path).chmod(0o600)

def database_env(uri):
    parsed = urllib.parse.urlsplit(uri)
    if parsed.scheme not in ("postgres", "postgresql") or not parsed.hostname:
        raise ValueError("数据库连接必须为明确 PostgreSQL URI")
    env = os.environ.copy()
    env.update({"PGHOST": parsed.hostname, "PGPORT": str(parsed.port or 5432), "PGUSER": urllib.parse.unquote(parsed.username or ""), "PGPASSWORD": urllib.parse.unquote(parsed.password or ""), "PGDATABASE": parsed.path.removeprefix("/")})
    for key, value in urllib.parse.parse_qsl(parsed.query):
        if key not in ("sslmode", "sslrootcert", "sslcert", "sslkey", "connect_timeout"):
            raise ValueError("不支持的数据库连接参数")
        env["PGCONNECT_TIMEOUT" if key == "connect_timeout" else "PG" + key.upper()] = value
    return env

@contextlib.contextmanager
def exported_snapshot(env):
    process = subprocess.Popen(["psql", "--no-psqlrc", "-qAt", "-v", "ON_ERROR_STOP=1"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, env=env, text=True)
    try:
        process.stdin.write("BEGIN ISOLATION LEVEL REPEATABLE READ; SELECT pg_export_snapshot();\n")
        process.stdin.flush()
        snapshot = process.stdout.readline().strip()
        if not snapshot or any(c not in "0123456789ABCDEFabcdef-" for c in snapshot):
            raise ValueError("无法取得一致 PostgreSQL 快照")
        yield snapshot
        process.stdin.write("COMMIT;\n")
        process.stdin.close()
        if process.wait(timeout=30) != 0:
            raise ValueError("一致备份快照事务未确认完成")
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()

def re_full_sha(value):
    return isinstance(value,str) and len(value)==64 and all(c in "0123456789abcdef" for c in value)

def app_jwt():
    app_id = int(os.environ["PILOT_GITHUB_APP_ID"])
    key = Path(os.environ["PILOT_GITHUB_APP_KEY_FILE"]).resolve(strict=True)
    encode = lambda value: base64.urlsafe_b64encode(value).rstrip(b"=")
    now = int(time.time())
    token = encode(canonical({"alg": "RS256", "typ": "JWT"})) + b"." + encode(canonical({"iat": now - 30, "exp": now + 300, "iss": str(app_id)}))
    signature = command(["openssl", "dgst", "-sha256", "-sign", str(key)], data=token)
    return (token + b"." + encode(signature)).decode()

class HTTP:
    def __init__(self):
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, *args, **kwargs):
                return None
        self.opener = urllib.request.build_opener(NoRedirect(), urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

    def call(self, method, url, body=None, token=None, missing=False, origin=None):
        headers = {"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28", "User-Agent": "ai-workflow-pilot-admin"}
        if token:
            headers["Authorization"] = "Bearer " + token
        if origin:
            headers["Origin"] = origin
        if body is not None:
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(url, data=canonical(body) if body is not None else None, headers=headers, method=method)
        try:
            with self.opener.open(req, timeout=90) as response:
                raw = response.read(64 << 20)
                if len(raw) >= 64 << 20:
                    raise ValueError("远端响应过大")
                return json.loads(raw) if raw else None
        except urllib.error.HTTPError as exc:
            if missing and exc.code == 404:
                return None
            raise ValueError("远端请求失败 HTTP " + str(exc.code) + "：" + method + " " + urllib.parse.urlsplit(url).path) from None

class Adapter:
    def __init__(self):
        self.http = HTTP()
        self.token = secret("PILOT_GITHUB_ADMIN_TOKEN")
        self.control = os.environ["PILOT_CONTROL_URL"].rstrip("/")
        parsed = urllib.parse.urlsplit(self.control)
        if parsed.username or parsed.password or parsed.path not in ("", "/") or parsed.query or parsed.fragment or (parsed.scheme != "https" and not (parsed.scheme == "http" and parsed.hostname in ("localhost", "127.0.0.1", "::1"))):
            raise ValueError("控制面 URL 无效")
        self.http.call("POST", self.control + "/api/login", {"token": secret("PILOT_CONTROL_TOKEN")}, origin=self.control)

    def gh(self, method, path, body=None, missing=False, token=None):
        return self.http.call(method, API + path, body, self.token if token is None else token, missing=missing)

    def ctl(self, operation, body=None):
        return self.http.call("GET" if body is None else "POST", self.control + "/api/pilot/maintenance/" + operation, body, origin=self.control)

    def pages(self, path):
        rows = []
        for page in range(1, 1001):
            current = self.gh("GET", path + ("&" if "?" in path else "?") + "per_page=100&page=" + str(page))
            rows += current
            if len(current) < 100:
                return rows
        raise ValueError("远端对象数量超出明确分页上限")

    def identity(self):
        user = self.gh("GET", "/user")
        repo = self.gh("GET", "/repos/" + REPO)
        installation = self.gh("GET", "/repos/" + REPO + "/installation", token=app_jwt())
        expected_id = int(os.environ["PILOT_REPOSITORY_ID"])
        expected_installation = int(os.environ["PILOT_INSTALLATION_ID"])
        if repo["full_name"] != REPO or repo["id"] != expected_id or installation["id"] != expected_installation or installation["app_id"] != int(os.environ["PILOT_GITHUB_APP_ID"]) or not repo.get("permissions", {}).get("admin") or user.get("type") != "User":
            raise ValueError("当次 GitHub 管理员、仓库或 App 安装身份不匹配")
        return {"repository": REPO, "repository_id": repo["id"], "installation_id": installation["id"], "actor": {"login": user["login"], "id": user["id"], "admin": True}}

    def refs(self):
        return {item["ref"]: item["object"]["sha"] for item in self.pages("/repos/" + REPO + "/git/matching-refs/")}

    def protection(self):
        branch = self.gh("GET", "/repos/" + REPO + "/branches/main/protection", missing=True)
        rulesets = self.pages("/repos/" + REPO + "/rulesets?includes_parents=true")
        details = [self.gh("GET", "/repos/" + REPO + "/rulesets/" + str(rule["id"])) for rule in rulesets]
        # Parent rulesets cannot be safely toggled by this repository adapter.
        if any(rule.get("source_type") != "Repository" for rule in details):
            raise ValueError("检测到组织/企业规则集；需要其管理员单独授权，拒绝自动扩大修改范围")
        other = {}
        for item in self.pages("/repos/" + REPO + "/branches"):
            if item["name"] != "main" and item.get("protected"):
                other[item["name"]] = self.gh("GET", "/repos/" + REPO + "/branches/" + urllib.parse.quote(item["name"], safe="") + "/protection", missing=True)
                if item["name"].startswith(("codex/issue-", "eval/", "pilot-test/")) and other[item["name"]] is not None:
                    raise ValueError("待删除测试枝有独立分支保护，需先由管理员明确调整后重新准备计划")
        return {"branch": branch, "rulesets": details, "other_branches": other}

    def inspect(self):
        result = self.identity()
        state = self.ctl("status")
        if state.get("repository_id") != result["repository_id"] or state.get("repository") != REPO:
            raise ValueError("控制面仓库身份不匹配")
        refs = self.refs()
        issues = self.pages("/repos/" + REPO + "/issues?state=open")
        pulls = self.pages("/repos/" + REPO + "/pulls?state=open")
        result.update({key: state[key] for key in ("intake_stopped", "active_agents", "stopping_agents", "active_workflow_leases", "inflight_outbox", "repository_epoch", "maintenance_lock_capable")})
        result.update({"refs": refs, "branch_protection": self.protection(), "reset_actions": {"close_issues": sorted(x["number"] for x in issues if "pull_request" not in x), "close_pull_requests": sorted(x["number"] for x in pulls), "delete_branches": sorted(ref.removeprefix("refs/heads/") for ref in refs if ref.startswith(("refs/heads/codex/issue-", "refs/heads/eval/", "refs/heads/pilot-test/")))}})
        return result

    @contextlib.contextmanager
    def fence(self, expected, epoch, plan_sha256=None, require_quiet=True):
        state_dir = Path(os.environ["PILOT_MAINTENANCE_STATE_DIR"]).resolve()
        state_dir.mkdir(parents=True, exist_ok=True)
        state_dir.chmod(0o700)
        key = plan_sha256 or hashlib.sha256(canonical({"identity": expected, "epoch": epoch, "kind": "backup"})).hexdigest()
        path = state_dir / (key + ".json")
        if path.exists():
            request = json.loads(path.read_text())
            if request["repository_id"] != expected["repository_id"] or request["expected_epoch"] != epoch:
                raise ValueError("持久维护令牌对应另一仓库或代次")
        else:
            request = {"repository": REPO, "repository_id": expected["repository_id"], "expected_epoch": epoch, "new_epoch": "pilot-" + uuid.uuid4().hex, "plan_sha256": key, "maintenance_token": uuid.uuid4().hex + uuid.uuid4().hex}
            save(path, request)
        acquired = self.ctl("acquire", request)
        try:
            if require_quiet:
                self.require_quiet(acquired)
            yield request
        finally:
            self.ctl("release", {**request, "keep_intake_stopped": True})
            if plan_sha256 is None:
                path.unlink()

    def stop_intake(self, request):
        identity = self.identity()
        state = self.ctl("status")
        with self.fence(identity, state["repository_epoch"], require_quiet=False):
            pass
        return self.inspect()

    def resume_intake(self, request):
        key = request["plan_sha256"]
        if not re_full_sha(key):
            raise ValueError("恢复运行必须明确绑定已完成重置计划摘要")
        root = Path(os.environ["PILOT_BACKUP_DIRECTORY"]).resolve(strict=True)
        if digest(root / "plan.json") != key:
            raise ValueError("恢复计划摘要不匹配")
        plan = json.loads((root / "plan.json").read_text())
        audit = json.loads((root / ("apply-log-" + key + ".json")).read_text())
        # Independent protection recovery can be replayed after final verification.
        # Any later reset mutation still invalidates that receipt until reverified.
        verified = next((i for i in range(len(audit)-1, -1, -1) if audit[i].get("kind") == "verified"), None)
        if verified is None or any(event.get("kind") != "protection_recovered" for event in audit[verified+1:]):
            raise ValueError("重置尚未通过最终核验，禁止恢复运行")
        current = self.inspect()
        if any(current[k] != v for k, v in plan["identity"].items()) or current["refs"].get("refs/heads/main") != plan["root_commit"] or self.stable_protection(current["branch_protection"]) != self.stable_protection(plan["branch_protection"]):
            raise ValueError("恢复前身份、main 或保护规则已经变化")
        saved = Path(os.environ["PILOT_MAINTENANCE_STATE_DIR"]) / (key + ".json")
        body = json.loads(saved.read_text())
        result = self.ctl("resume", body)
        if result.get("intake_stopped") is not False:
            raise ValueError("恢复运行读回失败")
        return {"repository": REPO, "repository_id": current["repository_id"], "repository_epoch": result["repository_epoch"], "intake_stopped": False}

    @staticmethod
    def require_quiet(state):
        if state.get("intake_stopped") is not True or any(state.get(k) != 0 for k in ("active_agents", "stopping_agents", "active_workflow_leases", "inflight_outbox")):
            raise ValueError("控制服务尚未完整停收与排空")

    def git_env(self):
        # Git asks a short-lived helper for credentials; no token in argv, URLs,
        # repository config, stdout, backup or plan files.
        temp = tempfile.TemporaryDirectory(prefix="pilot-git-auth-")
        askpass = Path(temp.name) / "askpass.py"
        askpass.write_text("#!/usr/bin/env python3\nimport os,sys\nprint('x-access-token' if 'Username' in sys.argv[1] else os.environ['PILOT_GIT_TOKEN'])\n")
        askpass.chmod(0o700)
        env = os.environ.copy()
        env.update({"GIT_ASKPASS": str(askpass), "GIT_TERMINAL_PROMPT": "0", "PILOT_GIT_TOKEN": self.token, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull})
        return temp, env

    def backup(self, request):
        identity = self.identity()
        if any(identity[k] != request["expected_identity"][k] for k in ("repository", "repository_id", "installation_id")):
            raise ValueError("备份身份不匹配")
        root = Path(request["destination"]).resolve(strict=True)
        roles = {"git_bundle": "repository.bundle", "refs": "refs.json", "issues": "issues.json", "pull_requests": "pull-requests.json", "branch_protection": "branch-protection.json", "repository_identity": "repository-identity.json", "control_database": "control.dump", "outbox": "outbox.json", "control_artifacts": "control-artifacts.tar", "database_counts": "database-counts.json"}
        receipt_path = root / "backup-receipt.json"
        if receipt_path.exists():
            previous = json.loads(receipt_path.read_text())
            if previous["epoch"] != request["expected_epoch"] or any(previous["identity"][k] != identity[k] for k in ("repository", "repository_id", "installation_id")) or previous["refs"] != self.refs():
                raise ValueError("既有备份对应另一代次、身份或远端引用")
            if any(digest(root / roles[k]) != value for k, value in previous["hashes"].items()):
                raise ValueError("既有备份文件已损坏")
            return {**identity, "consistent_checkpoint": True, "files": [{"role": k, "path": v} for k, v in roles.items()]}
        if (root / "plan.json").exists():
            raise ValueError("已有审阅计划但没有完整备份回执，禁止覆盖")
        def backup_save(path, value):
            temporary = Path(path).with_suffix(".pending")
            with temporary.open("wb") as output:
                output.write(canonical(value));output.flush();os.fsync(output.fileno())
            os.replace(temporary,path)
            Path(path).chmod(0o600)
        with self.fence(request["expected_identity"], request["expected_epoch"]):
            self.require_quiet(self.ctl("status"))
            with contextlib.ExitStack() as stack:
                auth, env = self.git_env()
                stack.enter_context(auth)
                mirror = root / "repository.git"
                if mirror.exists():
                    if command(["git", "remote", "get-url", "origin"],cwd=mirror).decode().strip() != "https://github.com/" + REPO + ".git":
                        raise ValueError("既有镜像对应不同远端")
                    command(["git","fetch","--prune","origin"],env=env,cwd=mirror)
                else:
                    command(["git", "clone", "--mirror", "https://github.com/" + REPO + ".git", str(mirror)], env=env)
                replacement = os.environ.get("PILOT_ROOT_COMMIT")
                if replacement:
                    command(["git", "fetch", "origin", replacement], cwd=mirror, env=env)
                    command(["git", "update-ref", "refs/pilot-backup/replacement", replacement], cwd=mirror)
                bundle = root / "repository.bundle"
                command(["git", "bundle", "create", str(bundle), "--all"], cwd=mirror)
                command(["git", "bundle", "verify", str(bundle)], cwd=mirror)
            refs = self.refs()
            backup_save(root / "refs.json", refs)
            backup_save(root / "repository-identity.json", identity)
            backup_save(root / "issues.json", self.pages("/repos/" + REPO + "/issues?state=all"))
            backup_save(root / "pull-requests.json", self.pages("/repos/" + REPO + "/pulls?state=all"))
            backup_save(root / "branch-protection.json", self.protection())
            env = database_env(secret("PILOT_DATABASE_URL"))
            with exported_snapshot(env) as snapshot:
                command(["pg_dump", "--format=custom", "--no-owner", "--no-acl", "--snapshot", snapshot, "--file", str(root / "control.dump")], env=env)
                prefix = "BEGIN ISOLATION LEVEL REPEATABLE READ; SET TRANSACTION SNAPSHOT '" + snapshot + "'; "
                outbox = command(["psql", "--no-psqlrc", "-qAt", "-v", "ON_ERROR_STOP=1", "-c", prefix + "SELECT COALESCE(jsonb_agg(to_jsonb(o) ORDER BY id),'[]'::jsonb) FROM outbox o; COMMIT;"], env=env)
                (root / "outbox.json").write_bytes(outbox)
                counts = command(["psql", "--no-psqlrc", "-qAt", "-v", "ON_ERROR_STOP=1", "-c", prefix + "SELECT jsonb_build_object('workflows',(SELECT count(*) FROM workflows),'outbox',(SELECT count(*) FROM outbox),'agents',(SELECT count(*) FROM agent_runs)); COMMIT;"], env=env)
                (root / "database-counts.json").write_bytes(counts)
            source = Path(os.environ["PILOT_CONTROL_ARTIFACTS"]).resolve(strict=True)
            with tarfile.open(root / "control-artifacts.tar", "w") as archive:
                for path in sorted(source.rglob("*")):
                    if path.is_symlink():
                        raise ValueError("控制产物包含符号链接，需先明确备份边界")
                    archive.add(path, arcname=str(path.relative_to(source)), recursive=False)
            roles = {"git_bundle": "repository.bundle", "refs": "refs.json", "issues": "issues.json", "pull_requests": "pull-requests.json", "branch_protection": "branch-protection.json", "repository_identity": "repository-identity.json", "control_database": "control.dump", "outbox": "outbox.json", "control_artifacts": "control-artifacts.tar", "database_counts": "database-counts.json"}
            backup_save(root / "backup-receipt.json", {"identity": identity, "epoch": request["expected_epoch"], "refs": refs, "hashes": {key: digest(root / name) for key, name in roles.items()}})
            self.require_quiet(self.ctl("status"))
            if self.refs() != refs:
                raise ValueError("备份期间 Git 引用变化")
            self.last_backup = root
            return {**identity, "consistent_checkpoint": True, "files": [{"role": k, "path": v} for k, v in roles.items()]}

    def restore_verify(self, request):
        root = Path(request["backup_directory"]).resolve(strict=True)
        records = request["files"]
        paths = {}
        for item in records:
            path = (root / item["path"]).resolve(strict=True)
            if not path.is_relative_to(root) or digest(path) != item["sha256"]:
                raise ValueError("恢复源校验失败")
            paths[item["role"]] = path
        with tempfile.TemporaryDirectory(prefix="pilot-restore-") as directory:
            bare = Path(directory) / "restored.git"
            command(["git", "clone", "--mirror", str(paths["git_bundle"]), str(bare)])
            command(["git", "fsck", "--full"], cwd=bare)
            actual = dict(line.split(" ", 1)[::-1] for line in command(["git", "show-ref"], cwd=bare).decode().splitlines())
            for ref, sha in json.loads(paths["refs"].read_text()).items():
                if actual.get(ref) != sha:
                    raise ValueError("bundle 恢复后引用不匹配")
            with tarfile.open(paths["control_artifacts"]) as archive:
                for item in archive.getmembers():
                    target = (Path(directory) / "artifacts" / item.name).resolve()
                    if not target.is_relative_to(Path(directory) / "artifacts") or item.issym() or item.islnk() or not (item.isfile() or item.isdir()):
                        raise ValueError("产物备份包含不安全路径或特殊文件")
                archive.extractall(Path(directory) / "artifacts")
                for item in archive.getmembers():
                    if item.isfile():
                        restored_file = Path(directory) / "artifacts" / item.name
                        expected = hashlib.sha256(archive.extractfile(item).read()).hexdigest()
                        if digest(restored_file) != expected:
                            raise ValueError("产物隔离恢复摘要不匹配")
            admin_url = secret("PILOT_RESTORE_ADMIN_DATABASE_URL")
            if admin_url == secret("PILOT_DATABASE_URL"):
                raise ValueError("隔离恢复连接必须明确区别于生产控制数据库连接")
            parsed = urllib.parse.urlsplit(admin_url)
            if parsed.scheme not in ("postgres", "postgresql"):
                raise ValueError("恢复管理员连接必须为 PostgreSQL URI")
            name = "aiwf_restore_" + uuid.uuid4().hex
            env = database_env(admin_url)
            command(["psql", "--no-psqlrc", "-v", "ON_ERROR_STOP=1", "-c", 'CREATE DATABASE "' + name + '"'], env=env)
            try:
                restore_url = urllib.parse.urlunsplit(parsed._replace(path="/" + name))
                restore_env = database_env(restore_url)
                command(["pg_restore", "--exit-on-error", "--no-owner", "--no-acl", "--dbname", name, str(paths["control_database"])], env=restore_env)
                restored = command(["psql", "--no-psqlrc", "-At", "-c", "SELECT COALESCE(jsonb_agg(to_jsonb(o) ORDER BY id),'[]'::jsonb) FROM outbox o"], env=restore_env)
                if json.loads(restored) != json.loads(paths["outbox"].read_bytes()):
                    raise ValueError("隔离恢复的 Outbox 记录与备份不一致")
                counts = command(["psql", "--no-psqlrc", "-At", "-c", "SELECT jsonb_build_object('workflows',(SELECT count(*) FROM workflows),'outbox',(SELECT count(*) FROM outbox),'agents',(SELECT count(*) FROM agent_runs))"], env=restore_env)
                if json.loads(counts) != json.loads(paths["database_counts"].read_bytes()):
                    raise ValueError("隔离恢复记录数不一致")
            finally:
                command(["psql", "--no-psqlrc", "-v", "ON_ERROR_STOP=1", "-c", 'DROP DATABASE "' + name + '"'], env=env)
        result = {"isolated": True, "database_restored": True, "outbox_matches": True, "bundle_verified": True, "artifacts_verified": True, "files_digest": hashlib.sha256(canonical(records)).hexdigest()}
        existing = root / "restore-verification.json"
        if existing.exists():
            if json.loads(existing.read_text()) != result:
                raise ValueError("既有恢复验证回执与当前备份不同")
        else:
            save(existing, result)
        return result

    def inspect_replacement(self, request):
        commit = request["commit"]
        if commit != os.environ.get("PILOT_ROOT_COMMIT") or len(commit) != 40 or any(c not in "0123456789abcdef" for c in commit):
            raise ValueError("替换提交没有显式固定")
        data = self.gh("GET", "/repos/" + REPO + "/git/commits/" + commit)
        root = Path(os.environ["PILOT_BACKUP_DIRECTORY"]).resolve(strict=True)
        heads = command(["git", "bundle", "list-heads", str(root / "repository.bundle")]).decode()
        return {"commit": data["sha"], "parents": [x["sha"] for x in data["parents"]], "available_in_backup": any(line.startswith(commit + " ") for line in heads.splitlines())}

    @staticmethod
    def protection_input(original):
        if original is None:
            return None
        status = original.get("required_status_checks")
        if status:
            status = {k: status[k] for k in ("strict", "contexts", "checks") if k in status}
            if "checks" in status:
                status["checks"] = [{k: x[k] for k in ("context", "app_id") if k in x} for x in status["checks"]]
        reviews = original.get("required_pull_request_reviews")
        if reviews:
            reviews = {k: reviews[k] for k in ("dismiss_stale_reviews", "require_code_owner_reviews", "required_approving_review_count", "require_last_push_approval") if k in reviews}
            for key in ("dismissal_restrictions", "bypass_pull_request_allowances"):
                value = original["required_pull_request_reviews"].get(key)
                if value is not None:
                    reviews[key] = {"users": [x["login"] for x in value.get("users", [])], "teams": [x["slug"] for x in value.get("teams", [])], "apps": [x["slug"] for x in value.get("apps", [])]}
        restrictions = original.get("restrictions")
        if restrictions:
            restrictions = {"users": [x["login"] for x in restrictions.get("users", [])], "teams": [x["slug"] for x in restrictions.get("teams", [])], "apps": [x["slug"] for x in restrictions.get("apps", [])]}
        result = {"required_status_checks": status, "enforce_admins": bool(original.get("enforce_admins", {}).get("enabled")), "required_pull_request_reviews": reviews, "restrictions": restrictions}
        for key in ("required_linear_history", "allow_force_pushes", "allow_deletions", "block_creations", "required_conversation_resolution", "lock_branch", "allow_fork_syncing"):
            if key in original:
                result[key] = bool(original[key].get("enabled"))
        return result

    def recovery_plan(self, request):
        root = Path(os.environ["PILOT_BACKUP_DIRECTORY"]).resolve(strict=True)
        key = request["plan_sha256"]
        if not re_full_sha(key) or digest(root / "plan.json") != key:
            raise ValueError("恢复请求与明确审阅的计划不一致")
        plan = json.loads((root / "plan.json").read_text())
        if request.get("plan", plan) != plan or plan["repository"] != REPO:
            raise ValueError("请求替换了冻结计划")
        identity = self.identity()
        if any(identity[k] != value for k, value in plan["identity"].items()):
            raise ValueError("恢复前仓库或安装身份已变化")
        path = root / ("apply-log-" + key + ".json")
        events = json.loads(path.read_text()) if path.exists() else []
        def event(kind, detail):
            events.append({"at": dt.datetime.now(dt.timezone.utc).isoformat(), "kind": kind, "detail": detail})
            temporary = path.with_suffix(".tmp")
            with temporary.open("wb") as output:
                output.write(canonical(events));output.flush();os.fsync(output.fileno())
            os.replace(temporary, path)
            path.chmod(0o600)
        return root, plan, events, event

    @staticmethod
    def stable_protection(value):
        if isinstance(value,dict):
            return {key: Adapter.stable_protection(item) for key,item in value.items() if key not in ("updated_at","created_at","url","node_id","_links")}
        if isinstance(value,list):
            return [Adapter.stable_protection(item) for item in value]
        return value

    def restore_protection(self, plan, events, event):
        original = plan["branch_protection"]
        current = self.protection()
        old_branch = self.protection_input(original["branch"])
        branch = self.protection_input(current["branch"])
        temporary = dict(old_branch) if old_branch is not None else None
        if temporary is not None:
            temporary.update({"allow_force_pushes":True,"lock_branch":False,"enforce_admins":False})
        branch_intent = any(x["kind"] == "intent_main_force_exception" for x in events)
        if branch != old_branch and not (branch_intent and branch == temporary):
            raise ValueError("main 保护出现计划外变化；拒绝覆盖管理员的新规则")
        by_id = {x["id"]:x for x in current["rulesets"]}
        if set(by_id) != {x["id"] for x in original["rulesets"]}:
            raise ValueError("规则集身份集合出现计划外变化")
        for rule in original["rulesets"]:
            actual = by_id[rule["id"]]
            old = self.stable_protection(rule)
            observed = self.stable_protection(actual)
            original_mode = old.pop("enforcement")
            actual_mode = observed.pop("enforcement")
            allowed = any(x["kind"] == "intent_ruleset_disable" and x["detail"]["id"] == rule["id"] for x in events)
            if old != observed or (actual_mode != original_mode and not (allowed and actual_mode == "disabled")):
                raise ValueError("规则集内容出现计划外变化；禁止恢复覆盖")
        # Validate every rule before making the first restoration mutation.
        event("intent_restore_protection", {})
        errors = []
        if branch != old_branch:
            try:
                self.gh("PUT", "/repos/" + REPO + "/branches/main/protection", old_branch)
            except Exception:
                errors.append("main")
        for rule in original["rulesets"]:
            if by_id[rule["id"]]["enforcement"] == rule["enforcement"]:
                continue
            try:
                self.gh("PUT", "/repos/" + REPO + "/rulesets/" + str(rule["id"]), {"enforcement":rule["enforcement"]})
            except Exception:
                errors.append("ruleset:"+str(rule["id"]))
        readback = self.protection()
        if errors or self.stable_protection(readback) != self.stable_protection(original):
            event("protection_restore_unconfirmed", {"errors":errors})
            raise ValueError("原始保护规则尚未恢复确认；保持维护并运行 recover_protection")
        event("protection_recovered", {})

    def recover_protection(self, request):
        root,plan,events,event = self.recovery_plan(request)
        with self.fence(plan["identity"],plan["repository_epoch"],request["plan_sha256"]):
            self.restore_protection(plan,events,event)
        return {"protection_restored":True,"intake_stopped":True,"plan_sha256":request["plan_sha256"]}

    def apply_reset(self, request):
        if request.get("forbid_repository_recreation") is not True or request.get("require_force_with_lease") is not True:
            raise ValueError("重置安全约束缺失")
        root,plan,events,event = self.recovery_plan(request)
        resume = request.get("resume") is True
        if events and not resume:
            raise ValueError("已有重置日志；必须显式 resume，不能重启全新重置")
        # Revalidate actual immutable backup files even when called without wrapper.
        for item in plan["backup_files"]:
            path=(root/item["path"]).resolve(strict=True)
            if not path.is_relative_to(root) or digest(path)!=item["sha256"]:
                raise ValueError("恢复所需备份摘要不匹配")
        required={"git_bundle","refs","issues","pull_requests","branch_protection","repository_identity","control_database","outbox","control_artifacts"}
        if not required <= {item["role"] for item in plan["backup_files"]}:
            raise ValueError("适配器拒绝缺少完整备份的直接调用")
        verification=plan.get("restore_verification",{})
        if any(verification.get(key) is not True for key in ("isolated","database_restored","outbox_matches","bundle_verified","artifacts_verified")) or verification.get("files_digest")!=hashlib.sha256(canonical(plan["backup_files"])).hexdigest():
            raise ValueError("缺少绑定本次备份的隔离恢复成功回执")
        expected=plan["identity"]
        with self.fence(expected,plan["repository_epoch"],request["plan_sha256"]) as maintenance:
            self.require_quiet(self.ctl("status"))
            # Protection recovery is deliberately before refs/object validation:
            # a SIGKILL after exception creation must always have a safe repair path.
            self.restore_protection(plan,events,event)
            current=self.inspect()
            expected_refs=dict(plan["refs"])
            changed_refs={"refs/heads/main"} | {"refs/heads/"+b for b in plan["actions"]["delete_branches"]}
            if {k:v for k,v in current["refs"].items() if k not in changed_refs} != {k:v for k,v in expected_refs.items() if k not in changed_refs}:
                raise ValueError("存在计划外引用变化；保护已恢复，停止重置")
            main=current["refs"].get("refs/heads/main")
            if main not in (expected_refs["refs/heads/main"],plan["root_commit"]):
                raise ValueError("main 不是计划允许的旧值或新值")
            for branch in plan["actions"]["delete_branches"]:
                ref="refs/heads/"+branch
                if current["refs"].get(ref) not in (expected_refs[ref],None):
                    raise ValueError("测试枝出现计划外提交")
            for key in ("close_issues","close_pull_requests","delete_branches"):
                if not set(current["reset_actions"][key]) <= set(plan["actions"][key]):
                    raise ValueError("出现计划外待关闭对象或测试枝")
            if not resume and (current["refs"] != expected_refs or current["reset_actions"] != {k:plan["actions"][k] for k in ("close_issues","close_pull_requests","delete_branches")}):
                raise ValueError("首次执行的远端状态与审阅计划不一致")
            event("intent_retire", {"new_epoch":maintenance["new_epoch"]})
            retired=self.ctl("retire",maintenance)
            event("retired", {"repository_epoch":retired["repository_epoch"]})
            original=plan["branch_protection"]
            try:
                for rule in original["rulesets"]:
                    if rule["enforcement"] != "disabled":
                        event("intent_ruleset_disable",{"id":rule["id"]})
                        self.gh("PUT","/repos/"+REPO+"/rulesets/"+str(rule["id"]),{"enforcement":"disabled"})
                if original["branch"] is not None:
                    replacement=self.protection_input(original["branch"])
                    replacement.update({"allow_force_pushes":True,"lock_branch":False,"enforce_admins":False})
                    event("intent_main_force_exception",{})
                    self.gh("PUT","/repos/"+REPO+"/branches/main/protection",replacement)
                auth,env=self.git_env()
                with auth,tempfile.TemporaryDirectory(prefix="pilot-reset-git-") as directory:
                    command(["git","clone","--bare",str(root/"repository.bundle"),directory],env=env)
                    change=plan["actions"]["force_update"]
                    if change != {"ref":"refs/heads/main","old_sha":expected_refs["refs/heads/main"],"new_sha":plan["root_commit"]}:
                        raise ValueError("强推目标超出 main 精确范围")
                    if self.refs().get("refs/heads/main") != change["new_sha"]:
                        event("intent_main_update",change)
                        command(["git","push","--force-with-lease=refs/heads/main:"+change["old_sha"],"https://github.com/"+REPO+".git",change["new_sha"]+":refs/heads/main"],env=env,cwd=directory)
                    event("main_updated",change)
                    for key,endpoint in (("close_pull_requests","pulls"),("close_issues","issues")):
                        for number in plan["actions"][key]:
                            path="/repos/"+REPO+"/"+endpoint+"/"+str(number)
                            if self.gh("GET",path)["state"] != "closed":
                                event("intent_close",{"endpoint":endpoint,"number":number})
                                self.gh("PATCH",path,{"state":"closed"})
                            if self.gh("GET",path)["state"] != "closed":
                                raise ValueError("对象关闭后的读回不匹配")
                            event("closed",{"endpoint":endpoint,"number":number})
                    for branch in plan["actions"]["delete_branches"]:
                        if not branch.startswith(("codex/issue-","eval/","pilot-test/")):
                            raise ValueError("拒绝删除非测试枝")
                        ref="refs/heads/"+branch;old=expected_refs[ref]
                        current_sha=self.refs().get(ref)
                        if current_sha is not None:
                            if current_sha!=old:raise ValueError("删除前测试枝已变化")
                            event("intent_delete_branch",{"ref":ref,"old_sha":old})
                            command(["git","push","--force-with-lease="+ref+":"+old,"https://github.com/"+REPO+".git",":"+ref],env=env,cwd=directory)
                        event("branch_deleted",{"ref":ref,"old_sha":old})
            finally:
                self.restore_protection(plan,events,event)
            final=self.inspect()
            final_refs=dict(expected_refs);final_refs["refs/heads/main"]=plan["root_commit"]
            for branch in plan["actions"]["delete_branches"]:final_refs.pop("refs/heads/"+branch,None)
            if final["refs"]!=final_refs or any(final[k]!=v for k,v in expected.items()):
                raise ValueError("重置后引用或身份未通过精确验证")
            event("verified",{"repository_epoch":final["repository_epoch"]})
            return {"all_actions_verified":True,"identity":expected,"repository_epoch":final["repository_epoch"],"audit_file":str(root/("apply-log-"+request["plan_sha256"]+".json"))}

def main():
    request = json.loads(sys.stdin.buffer.read(32 << 20))
    if request.get("protocol") != "pilot-admin/v1" or request.get("repository") != REPO:
        raise ValueError("只允许精确 pilot-admin/v1 仓库请求")
    operation = request.get("operation")
    if operation not in ("inspect", "stop_intake", "resume_intake", "recover_protection", "backup", "restore_verify", "inspect_replacement", "apply_reset"):
        raise ValueError("不支持的管理操作")
    adapter = Adapter()
    result = adapter.inspect() if operation == "inspect" else getattr(adapter, operation)(request)
    print(json.dumps({"protocol": "pilot-admin/v1", "operation": operation, "ok": True, **result}, ensure_ascii=False))

if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        # Never echo exception data from third-party libraries, HTTP bodies or
        # environment values. Parent sees only a generic failed operation.
        print("管理操作失败，保持停收；请检查不含秘密的本地审计文件。", file=sys.stderr)
        raise SystemExit(1)
