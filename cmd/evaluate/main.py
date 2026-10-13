#!/usr/bin/env python3
"""受信评估执行器。plan 离线；run 必须显式 --execute，不会合并或填写人工标签。"""
import argparse
import datetime as dt
import hashlib
import http.cookiejar
import json
import os
from pathlib import Path
import random
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

PROFILES = {"A": "builder_ci", "B": "single_review", "C": "full"}
TERMINAL = {"EVALUATION_COMPLETE", "APPROVED", "MERGED", "FAILED", "CANCELLED", "ARCHIVED", "REJECTED"}

def digest_bytes(data):
    return hashlib.sha256(data).hexdigest()

def canonical(value):
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()

def read(path):
    return json.loads(Path(path).read_text())

def atomic(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(path.name + ".tmp")
    with tmp.open("w") as f:
        json.dump(value, f, ensure_ascii=False, indent=2)
        f.write("\n")
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)

def checked_manifest(path):
    path = Path(path).resolve()
    raw = path.read_bytes()
    manifest = json.loads(raw)
    root = path.parent.parent
    for item, expected in manifest["baseline_files"].items():
        if digest_bytes((root / item).read_bytes()) != expected:
            raise ValueError("冻结基线文件已变化：" + item)
    for item, expected in manifest["trusted_files"].items():
        if digest_bytes((path.parent / item).read_bytes()) != expected:
            raise ValueError("受信评估文件已变化：" + item)
    for task in manifest["tasks"]:
        if digest_bytes((path.parent / task["issue_path"]).read_bytes()) != task["issue_sha256"]:
            raise ValueError("冻结题面已变化：" + task["id"])
    return manifest, digest_bytes(raw)

def make_plan(args):
    manifest, manifest_digest = checked_manifest(args.manifest)
    matrix = read(args.matrix)
    snapshots = read(args.snapshots)["trials"]
    by_id = {x["trial_id"]: x for x in snapshots}
    if len(by_id) != len(snapshots):
        raise ValueError("重复试验 ID")
    if matrix["manifest_version"] != manifest["version"] or len(matrix["runs"]) != 90:
        raise ValueError("矩阵不是冻结的 90 次开发试验")
    expected_runs = {f"{task['id']}-{arm}-{repeat}" for task in manifest["tasks"] for arm in PROFILES for repeat in range(1, 4)}
    if {run["run_key"] for run in matrix["runs"]} != expected_runs:
        raise ValueError("矩阵缺少冻结的三组各三次任务 ID")
    trials = []
    for task in manifest["tasks"]:
        trials.append({"trial_id": "smoke-" + task["id"], "kind": "smoke", "task_id": task["id"], "profile": "full", "candidate_id": task["id"] + "-defect", "repeat": 1})
    development = []
    for run in matrix["runs"]:
        task = run.get("task_id", run.get("task"))
        development.append({"trial_id": run["run_key"], "kind": "development", "task_id": task, "profile": PROFILES[run["arm"]], "candidate_id": task + "-defect", "repeat": run.get("repeat", run.get("repetition")), "arm": run["arm"]})
    random.Random(args.seed).shuffle(development)
    trials += development
    reviews = []
    for pair in manifest["review_pairs"]:
        for candidate in pair["candidates"]:
            reviews.append({"trial_id": "review-" + hashlib.sha256((str(args.seed) + ":" + candidate["candidate_id"]).encode()).hexdigest()[:20], "kind": "review", "task_id": pair["pair_id"], "profile": "full", "candidate_id": candidate["candidate_id"], "repeat": 1})
    random.Random(args.seed + 1).shuffle(reviews)
    trials += reviews
    if len(trials) != 120 or len({trial["trial_id"] for trial in trials}) != 120:
        raise ValueError("应恰有 10 smoke、90 development、20 review")
    for trial in trials:
        snap = by_id.get(trial["trial_id"])
        if snap is None or any(snap.get(k) != v for k, v in trial.items() if k != "arm"):
            raise ValueError("试验缺少匹配的受信基线：" + trial["trial_id"])
        if snap["config_digest"] != matrix["config_sha256"] or snap["manifest_digest"] != manifest_digest or snap["seed"] != args.seed:
            raise ValueError("试验配置、清单或随机种子不一致")
        trial["request"] = {"trial_id": trial["trial_id"], "config_digest": snap["config_digest"], "manifest_digest": manifest_digest}
    if Path(args.output).exists():
        raise ValueError("计划已存在；拒绝覆盖不可变试验计划")
    atomic(args.output, {"version": 1, "protocol": "alpha-evaluation/v1", "manifest_digest": manifest_digest, "config_sha256": matrix["config_sha256"], "config": matrix["config"], "seed": args.seed, "trials": trials, "human_review": "pending", "human_merge": "pending"})
    print("已生成 120 个冻结试验计划；尚未执行。")

class Control:
    def __init__(self, base, token_file):
        parsed = urllib.parse.urlsplit(base)
        if parsed.username or parsed.password or parsed.query or parsed.fragment or parsed.path not in ("", "/"):
            raise ValueError("控制 API 地址不能包含凭据、路径或查询参数")
        if parsed.scheme != "https" and not (parsed.scheme == "http" and parsed.hostname in ("127.0.0.1", "localhost", "::1")):
            raise ValueError("远端控制 API 必须使用 HTTPS")
        self.base = base.rstrip("/")
        self.judge_timeout = 1800
        jar = http.cookiejar.CookieJar()
        # Do not follow a redirect carrying dashboard authentication to another host.
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, *args, **kwargs):
                return None
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar), NoRedirect())
        self.request("POST", "/api/login", {"token": Path(token_file).read_text().strip()})

    def request(self, method, path, body=None, raw=False):
        data = canonical(body) if body is not None else None
        headers = {"Content-Type": "application/json", "Origin": self.base}
        req = urllib.request.Request(self.base + path, data=data, headers=headers, method=method)
        with self.opener.open(req, timeout=self.judge_timeout if path.endswith("/judge") else 90) as response:
            result = response.read(32 << 20)
            if len(result) == 32 << 20:
                raise ValueError("API 响应超过大小限制")
            if raw:
                return result
            if not result.strip():
                return None
            return json.loads(result)

def record_result(control, trial, snapshot, out):
    workflow = snapshot["workflow"]
    trial_id = trial["trial_id"]
    folder = out / "evidence" / trial_id
    folder.mkdir(parents=True, exist_ok=True)
    atomic(folder / "snapshot.json", snapshot)
    receipts = [{"path": str((folder / "snapshot.json").relative_to(out)), "sha256": digest_bytes((folder / "snapshot.json").read_bytes())}]
    for index, evidence in enumerate(workflow.get("evidence") or []):
        query = urllib.parse.urlencode({"evidence_id": evidence["id"]})
        data = control.request("GET", "/api/evaluation/runs/" + urllib.parse.quote(trial_id, safe="") + "?" + query, raw=True)
        if digest_bytes(data) != evidence["hash"]:
            raise ValueError("实际证据内容摘要不匹配：" + evidence["id"])
        path = folder / ("proof-%04d.bin" % index)
        path.write_bytes(data)
        receipts.append({"path": str(path.relative_to(out)), "sha256": digest_bytes(data)})
    verdict = workflow["review"].get("verdict", "")
    judgment = snapshot["trusted_judgment"]
    verified = judgment.get("status") != "UNVERIFIED"
    if verified and (judgment.get("trial_id") != trial_id or judgment.get("config_digest") != trial["request"]["config_digest"] or judgment.get("manifest_digest") != trial["request"]["manifest_digest"] or judgment.get("stopped_confirmed") is not True):
        raise ValueError("可信隐藏判定身份或进程停止回执无效")
    tests_passed = verified and judgment.get("passed") is True
    passed = verdict in ("PASS", "PROFILE_PASS") and tests_passed
    error = workflow.get("last_error", "")
    budget = snapshot["config"]["budget"]
    exhausted = workflow["model_calls"] >= budget["issue_calls"] or workflow["runtime_seconds"] >= budget["issue_minutes"] * 60
    outcome = "success" if passed else "budget_exhausted" if exhausted else "cancelled" if workflow["control"] == "CANCELLED" else "infrastructure_error" if not verified else "failed"
    result = {"kind": trial["kind"], "run_key": trial_id, "candidate_id": trial["candidate_id"], "config_sha256": trial["request"]["config_digest"], "manifest_sha256": trial["request"]["manifest_digest"], "profile": trial["profile"], "outcome": outcome, "tests_passed": tests_passed, "trusted_judgment": judgment, "model_calls": workflow["model_calls"], "input_tokens": workflow["input_tokens"], "output_tokens": workflow["output_tokens"], "duration_seconds": workflow["runtime_seconds"], "cost": snapshot["cost"], "models": snapshot["models"], "effective_config": snapshot["config"], "workflow_id": workflow["id"], "issue_url": snapshot["issue_url"], "pull_request_url": snapshot["pull_request_url"], "verification_key": workflow["review"].get("key"), "evidence": receipts[0], "evidence_files": receipts, "human_review": "pending", "human_merge": "pending"}
    if trial["kind"] == "review":
        review_completed = any(t.get("status") == "ACCEPTED" and t.get("role") == "reviewer" for t in workflow["review"].get("tasks", []))
        result["reported_defect"] = any(f.get("state") not in ("RESOLVED", "REFUTED", "DISMISSED", "WAIVED") for f in workflow["review"].get("findings", [])) if review_completed else None
        result["accuracy_status"] = "pending_human_labels" if review_completed else "not_evaluated"
    atomic(out / "records" / (trial_id + ".json"), result)
    return result

def export_records(plan, out):
    for kind in ("smoke", "development", "review"):
        records = []
        for trial in plan["trials"]:
            path = out / "records" / (trial["trial_id"] + ".json")
            if trial["kind"] != kind or not path.exists():
                continue
            record = read(path)
            # Do not turn CI failure or a missing reviewer into a claimed negative.
            if kind == "review" and not isinstance(record.get("reported_defect"), bool):
                continue
            records.append(record)
        (out / (kind + ".jsonl")).write_text("".join(json.dumps(x, ensure_ascii=False, sort_keys=True) + "\n" for x in records))

def run_plan(args):
    if not args.execute:
        raise ValueError("真实执行必须显式指定 --execute")
    out = Path(args.output).resolve()
    out.mkdir(parents=True, exist_ok=True)
    # A process lock prevents duplicate writers. Durable API reservations provide
    # remote idempotence if the host crashes before saving its local checkpoint.
    import fcntl
    lock = (out / "executor.lock").open("a")
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        plan = read(args.plan)
        import re
        if any(not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,100}", t["trial_id"]) for t in plan["trials"]):
            raise ValueError("计划试验 ID 无效")
        for trial in plan["trials"]:
            path = out / "records" / (trial["trial_id"] + ".json")
            if trial["kind"] == "smoke" and path.exists() and read(path)["outcome"] != "success":
                raise ValueError("已有 smoke 失败；不能通过重新运行跳过准入检查")
        plan_hash = digest_bytes(Path(args.plan).read_bytes())
        checkpoint_path = out / "checkpoint.json"
        checkpoint = read(checkpoint_path) if checkpoint_path.exists() else {"plan_sha256": plan_hash, "completed": [], "pending": None}
        if checkpoint["plan_sha256"] != plan_hash:
            raise ValueError("恢复计划与首次执行的计划不一致")
        for completed in checkpoint["completed"]:
            if not (out / "records" / (completed + ".json")).is_file():
                raise ValueError("检查点缺少已完成试验回执，不能跳过：" + completed)
        for trial in plan["trials"]:
            record_path = out / "records" / (trial["trial_id"] + ".json")
            if not record_path.exists():
                continue
            record = read(record_path)
            if record["config_sha256"] != plan["config_sha256"] or record["run_key"] != trial["trial_id"]:
                raise ValueError("既有回执绑定另一试验或配置")
            for evidence in record["evidence_files"]:
                proof = (out / evidence["path"]).resolve()
                if not proof.is_relative_to(out) or digest_bytes(proof.read_bytes()) != evidence["sha256"]:
                    raise ValueError("恢复时既有证据摘要不匹配")
        control = Control(args.server, args.token_file)
        actual = control.request("GET", "/api/evaluation/config")
        if actual["protocol"] != plan["protocol"] or actual["config_sha256"] != plan["config_sha256"]:
            raise ValueError("控制器实际协议或配置与冻结计划不一致")
        control.judge_timeout = max(300, actual["config"]["budget"]["issue_minutes"] * 60 + 120)
        atomic(out / "execution-config.json", actual)
        for trial in plan["trials"]:
            trial_id = trial["trial_id"]
            if trial_id in checkpoint["completed"]:
                continue
            record_path = out / "records" / (trial_id + ".json")
            # A persisted final receipt survives a crash before checkpoint update.
            if record_path.exists():
                checkpoint["completed"].append(trial_id)
                atomic(checkpoint_path, checkpoint)
                continue
            checkpoint["pending"] = trial_id
            atomic(checkpoint_path, checkpoint)
            control.request("POST", "/api/evaluation/runs", trial["request"])
            terminal_wait_started = None
            while True:
                snapshot = control.request("GET", "/api/evaluation/runs/" + urllib.parse.quote(trial_id, safe=""))
                if snapshot["config_sha256"] != plan["config_sha256"]:
                    raise ValueError("运行中的实际配置已变化，停止评估并保留检查点")
                atomic(out / "latest.json", snapshot)
                workflow = snapshot["workflow"]
                if workflow["phase"] == "WAITING_DAILY_BUDGET" and snapshot["daily_calls"] < snapshot["daily_limit"]:
                    control.request("POST", "/api/evaluation/runs/" + urllib.parse.quote(trial_id, safe="") + "/resume", {})
                    continue
                if workflow["phase"] in TERMINAL or workflow["control"] in ("FAILED", "CANCELLED", "ARCHIVED") or (workflow["phase"] == "BLOCKED" and "daily model request budget exhausted" not in workflow.get("last_error", "")):
                    if snapshot.get("judge_ready") is not True:
                        terminal_wait_started = terminal_wait_started or time.monotonic()
                        if time.monotonic() - terminal_wait_started < control.judge_timeout:
                            time.sleep(args.poll_seconds)
                            continue
                    # The terminal state may be visible just before its supervisor
                    # releases the workflow lease. Do not confuse that brief race
                    # with an independent test result.
                    for attempt in range(4):
                        try:
                            judgment = control.request("POST", "/api/evaluation/runs/" + urllib.parse.quote(trial_id, safe="") + "/judge", {})
                            break
                        except (urllib.error.HTTPError, urllib.error.URLError, TimeoutError) as exc:
                            if attempt < 3:
                                time.sleep(5)
                                continue
                            judgment = {"status": "UNVERIFIED", "passed": False, "stopped_confirmed": False, "judge_error": "可信判定未完成", "http_status": getattr(exc, "code", None)}
                    snapshot = control.request("GET", "/api/evaluation/runs/" + urllib.parse.quote(trial_id, safe=""))
                    snapshot["trusted_judgment"] = judgment
                    result = record_result(control, trial, snapshot, out)
                    export_records(plan, out)
                    checkpoint["completed"].append(trial_id)
                    checkpoint["pending"] = None
                    atomic(checkpoint_path, checkpoint)
                    if trial["kind"] == "smoke" and result["outcome"] != "success":
                        raise ValueError("smoke 未通过，暂停正式试验；检查点与实际失败证据已保存")
                    if snapshot["daily_calls"] >= snapshot["daily_limit"]:
                        checkpoint["waiting"] = {"reason": "daily_budget", "day": snapshot["daily_day"]}
                        atomic(checkpoint_path, checkpoint)
                        print("当日额度已用完；本次结果已保存，次日继续剩余试验。")
                        return
                    break
                if workflow["phase"] == "WAITING_DAILY_BUDGET" or snapshot["daily_calls"] >= snapshot["daily_limit"]:
                    checkpoint["waiting"] = {"reason": "daily_budget", "day": snapshot["daily_day"], "trial_id": trial_id}
                    atomic(checkpoint_path, checkpoint)
                    print("每日额度已用完；已保存检查点，次日重跑同一命令恢复，不重置 Issue 总预算。")
                    return
                if workflow["phase"] in ("WAITING_RISK", "SPEC_READY", "NEEDS_INFO", "INTAKE", "WAITING_CONFIGURATION", "DEFERRED", "REJECT_PROPOSED", "AWAITING_MAINTAINER") or workflow["control"] != "ACTIVE":
                    checkpoint["waiting"] = {"reason": "human_action", "phase": workflow["phase"], "control": workflow["control"], "trial_id": trial_id}
                    atomic(checkpoint_path, checkpoint)
                    print("等待明确的人工决议；执行器不会自动批准风险、豁免、Spec 或合并。")
                    return
                time.sleep(args.poll_seconds)
        export_records(plan, out)
        print("所有自动试验记录已写入；准确率、人类接受和人工合并结论仍 pending。")
    finally:
        lock.close()

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    p = sub.add_parser("plan")
    for name in ("manifest", "matrix", "snapshots", "output"):
        p.add_argument("--" + name, required=True)
    p.add_argument("--seed", type=int, required=True)
    p.set_defaults(func=make_plan)
    p = sub.add_parser("run")
    for name in ("plan", "server", "token-file", "output"):
        p.add_argument("--" + name, required=True)
    p.add_argument("--execute", action="store_true")
    p.add_argument("--poll-seconds", type=int, default=15)
    p.set_defaults(func=run_plan)
    args = parser.parse_args()
    if getattr(args, "poll_seconds", 1) < 1:
        parser.error("轮询间隔必须为正")
    try:
        args.func(args)
    except (OSError, ValueError, KeyError, urllib.error.HTTPError) as exc:
        print("评估已停止：" + str(exc), file=sys.stderr)
        return 1
    return 0

if __name__ == "__main__":
    raise SystemExit(main())
