#!/usr/bin/env python3
"""为 120 个试验创建独立冻结引用；必须显式 --execute，使用经审阅管理适配器。"""
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("evaluation_executor", ROOT / "cmd/evaluate/main.py")
evaluation = importlib.util.module_from_spec(spec)
spec.loader.exec_module(evaluation)
target_spec = importlib.util.spec_from_file_location("evaluation_target", ROOT / "scripts/pilot-eval-target.py")
target_policy = importlib.util.module_from_spec(target_spec)
target_spec.loader.exec_module(target_policy)

def go_json(value):
    return json.dumps(value, ensure_ascii=False, separators=(",", ":")).replace("&", "\\u0026").replace("<", "\\u003c").replace(">", "\\u003e").replace("\u2028", "\\u2028").replace("\u2029", "\\u2029").encode()

def call(adapter, payload):
    result = subprocess.run([str(adapter)], input=evaluation.canonical(payload), stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=1800, check=False)
    if result.returncode:
        raise ValueError("评估准备适配器失败；保留已保存回执，未暴露适配器日志")
    response = json.loads(result.stdout)
    if response.get("protocol") != "pilot-eval-prepare/v1" or response.get("ok") is not True:
        raise ValueError("适配器没有验证独立快照")
    return response

def validate_saved_identity(saved, expected):
    if not isinstance(saved, dict) or not isinstance(saved.get("trials"), list):
        raise ValueError("已保存准备回执格式无效")
    if any(saved.get(key) != value for key, value in expected.items()):
        raise ValueError("已保存准备回执绑定了不同的冻结计划或 GitHub 身份")
    identity_keys = ("repository", "repository_id", "installation_id", "app_id")
    for trial in saved["trials"]:
        if not isinstance(trial, dict) or any(trial.get(key) != expected[key] for key in identity_keys):
            raise ValueError("已保存试验回执缺少匹配的 GitHub 身份，拒绝跨身份恢复")

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("manifest", "matrix", "adapter", "output", "workspace-root", "repository"):
        parser.add_argument("--" + name, required=True)
    for name in ("repository-id", "installation-id", "app-id", "seed"):
        parser.add_argument("--" + name, required=True, type=int)
    parser.add_argument("--execute", action="store_true")
    args = parser.parse_args()
    if not args.execute:
        raise ValueError("必须显式指定 --execute")
    target_policy.validate_identity(args.repository, args.repository_id, args.installation_id, args.app_id)
    manifest, manifest_digest = evaluation.checked_manifest(args.manifest)
    matrix = evaluation.read(args.matrix)
    if matrix["manifest_version"] != manifest["version"]:
        raise ValueError("矩阵清单版本不匹配")
    task_map = {t["id"]: t for t in manifest["tasks"]}
    trials = [{"trial_id": "smoke-" + t["id"], "task_id": t["id"], "profile": "full", "kind": "smoke", "candidate_id": t["id"] + "-defect", "repeat": 1} for t in manifest["tasks"]]
    trials += [{"trial_id": t["run_key"], "task_id": t["task_id"], "profile": evaluation.PROFILES[t["arm"]], "kind": "development", "candidate_id": t["task_id"] + "-defect", "repeat": t["repeat"]} for t in matrix["runs"]]
    trials += [{"trial_id": "review-" + hashlib.sha256((str(args.seed) + ":" + c["candidate_id"]).encode()).hexdigest()[:20], "task_id": pair["pair_id"], "profile": "full", "kind": "review", "candidate_id": c["candidate_id"], "repeat": 1} for pair in manifest["review_pairs"] for c in pair["candidates"]]
    if len(trials) != 120 or len({x["trial_id"] for x in trials}) != 120:
        raise ValueError("试验集合必须恰有 120 个独立 ID")
    out = Path(args.output).resolve()
    work = Path(args.workspace_root).resolve()
    if work == ROOT or ROOT in work.parents:
        raise ValueError("候选工作区必须在产品仓库之外")
    work.mkdir(parents=True, exist_ok=True)
    saved_identity = {"repository": args.repository, "repository_id": args.repository_id, "installation_id": args.installation_id, "app_id": args.app_id}
    saved_pins = {**saved_identity, "manifest_digest": manifest_digest, "config_digest": matrix["config_sha256"], "seed": args.seed}
    saved = evaluation.read(out) if out.exists() else {"trials": [], **saved_pins}
    validate_saved_identity(saved, saved_pins)
    completed = {t["trial_id"] for t in saved["trials"]}
    adapter = Path(args.adapter).resolve(strict=True)
    for trial in trials:
        if trial["trial_id"] in completed:
            continue
        task = task_map[trial["task_id"]]
        candidate = "fix" if trial["candidate_id"].endswith("-fix") else "defect"
        target = work / trial["trial_id"]
        if not target.exists():
            subprocess.run([sys.executable, str(ROOT / "eval/cli.py"), "prepare", "--task", task["id"], "--candidate", candidate, "--output", str(target)], stdout=subprocess.PIPE, check=True)
        expected = task[candidate + "_source_sha256"]
        hashes = {p: evaluation.digest_bytes((target / p).read_bytes()) for p in manifest["baseline_files"]}
        # Frozen source hashes use the Python sorted mapping representation.
        actual = hashlib.sha256(json.dumps(hashes, sort_keys=True).encode()).hexdigest()
        if actual != expected:
            raise ValueError("独立候选内容与冻结清单不一致：" + trial["trial_id"])
        request = {"protocol": "pilot-eval-prepare/v1", "operation": "prepare_independent_trial", "repository": args.repository, "repository_id": args.repository_id, "installation_id": args.installation_id, "app_id": args.app_id, "trial": trial, "workspace": str(target), "source_sha256": expected, "source_files": hashes, "baseline_source_sha256": task["baseline_source_sha256"], "forbid_inheriting_prior_candidate": True, "require_existing_or_create_candidate_pr": trial["kind"] == "review", "idempotency_key": trial["trial_id"]}
        receipt = call(adapter, request)
        if receipt.get("repository_id") != args.repository_id or receipt.get("installation_id") != args.installation_id or receipt.get("app_id") != args.app_id or receipt.get("repository") != args.repository or receipt.get("source_sha256") != expected or receipt.get("independently_rebuilt") is not True:
            raise ValueError("适配器回执没有绑定实际仓库、安装与源码摘要")
        if len(receipt.get("base_sha", "")) != 40 or len(receipt.get("candidate_sha", "")) != 40 or not receipt.get("base_branch"):
            raise ValueError("适配器没有返回冻结 Git 引用")
        entry = {**trial, "seed": args.seed, "manifest_version": manifest["version"], "manifest_digest": manifest_digest, "config_digest": matrix["config_sha256"], "repository": args.repository, "repository_id": args.repository_id, "installation_id": args.installation_id, "app_id": args.app_id, "source_sha256": expected, "base_source_sha256": receipt["base_source_sha256"], **{k: receipt.get(k, "" if k != "pull_request" else 0) for k in ("base_sha", "candidate_sha", "base_branch", "candidate_branch", "pull_request")}}
        if trial["kind"] != "review" and entry["base_sha"] != entry["candidate_sha"]:
            raise ValueError("开发试验必须从自身独立缺陷快照开始")
        if trial["kind"] == "review":
            if not entry["pull_request"] or entry["base_source_sha256"] != task["baseline_source_sha256"]:
                raise ValueError("审查试验缺少真实候选 PR 或匹配基线")
            text = (Path(args.manifest).parent / task["issue_path"]).read_text()
            title = task["id"] + " 评估任务"
            body = "<!-- ai-workflow-evaluation-trial:" + trial["trial_id"] + " -->\n\n" + text
            criteria = [{"id": "AC%d" % (i + 1), "description": line[2:]} for i, line in enumerate(x for x in text.splitlines() if x.startswith("- "))]
            if not criteria:
                raise ValueError("冻结题面没有可见验收条目")
            source_hash = "issue-v3:" + hashlib.sha256(go_json([title.replace("\r\n", "\n"), body.replace("\r\n", "\n")])).hexdigest()
            entry["spec"] = {"revision": 1, "title": title, "description": text, "criteria": criteria, "non_goals": ["不得修改冻结候选代码；仅独立验证和审查"], "risk": "按冻结题面验证", "source_hash": source_hash, "approved": False, "approved_by": "", "digest": ""}
        saved["trials"].append(entry)
        evaluation.atomic(out, saved)
        evaluation.atomic(work / (trial["trial_id"] + ".receipt.json"), receipt)
    print("冻结引用准备完成；须把快照清单只读安装到控制器，尚未运行任何模型或测试。")

if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as exc:
        print("准备已停止：" + str(exc), file=sys.stderr)
        raise SystemExit(1)
