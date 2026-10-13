#!/usr/bin/env python3
"""仅对 ycychome/ai-workflow-pilot 准备和执行可审阅重置计划。默认不操作。"""
import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

TARGET = "ycychome/ai-workflow-pilot"
REQUIRED = {"git_bundle", "refs", "issues", "pull_requests", "branch_protection", "repository_identity", "control_database", "outbox", "control_artifacts"}

def canonical(x):
    return json.dumps(x, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()

def sha(path):
    h = hashlib.sha256()
    with Path(path).open("rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()

def save(path, value):
    path = Path(path)
    with path.open("x") as f:
        json.dump(value, f, ensure_ascii=False, indent=2)
        f.write("\n")
        f.flush()
        os.fsync(f.fileno())
    path.chmod(0o600)

def adapter_call(adapter, operation, payload):
    request = {"protocol": "pilot-admin/v1", "operation": operation, "repository": TARGET, **payload}
    result = subprocess.run([str(adapter)], input=canonical(request), stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False, timeout=1800)
    if result.returncode != 0:
        # Adapter stderr may contain credential material; do not echo it.
        raise ValueError("管理适配器失败，操作：" + operation)
    if len(result.stdout) > 32 << 20:
        raise ValueError("适配器响应过大")
    response = json.loads(result.stdout)
    if response.get("protocol") != request["protocol"] or response.get("operation") != operation or response.get("ok") is not True:
        raise ValueError("适配器未确认操作完成：" + operation)
    return response

def identity(value):
    if value.get("repository") != TARGET or not isinstance(value.get("repository_id"), int) or value["repository_id"] <= 0 or not isinstance(value.get("installation_id"), int) or value["installation_id"] <= 0:
        raise ValueError("仓库数字身份或 App 安装未核验")
    actor = value.get("actor", {})
    if not actor.get("login") or not isinstance(actor.get("id"), int) or actor["id"] <= 0 or actor.get("admin") is not True:
        raise ValueError("当前管理身份没有可验证的仓库管理员权限")
    return {k: value[k] for k in ("repository", "repository_id", "installation_id")}

def quiescent(value):
    if value.get("intake_stopped") is not True or value.get("active_agents") != 0 or value.get("stopping_agents") != 0 or value.get("inflight_outbox") != 0 or value.get("active_workflow_leases") != 0:
        raise ValueError("必须先停收，等待 ACTIVE、STOPPING、工作流租约和在途 Outbox 清空")
    if not value.get("repository_epoch") or not value.get("maintenance_lock_capable"):
        raise ValueError("缺少仓库 epoch 或贯穿重置过程的维护锁")

def check_files(root, records):
    roles = set()
    for item in records:
        path = (root / item["path"]).resolve()
        if not path.is_relative_to(root.resolve()) or not path.is_file() or path.is_symlink():
            raise ValueError("备份文件不在计划目录中")
        if sha(path) != item["sha256"] or path.stat().st_size != item["bytes"]:
            raise ValueError("备份校验失败：" + item["role"])
        roles.add(item["role"])
    if not REQUIRED <= roles:
        raise ValueError("备份缺少必需内容：" + ",".join(sorted(REQUIRED - roles)))

def stable_protection(value):
    if isinstance(value, dict):
        return {k: stable_protection(v) for k, v in value.items() if k not in ("updated_at", "created_at", "url", "node_id", "_links")}
    if isinstance(value, list):
        return [stable_protection(v) for v in value]
    return value

def prepare(args):
    if args.repository != TARGET:
        raise ValueError("目标必须明确为 " + TARGET)
    adapter = Path(args.adapter).resolve(strict=True)
    root = Path(args.output).resolve()
    root.mkdir(parents=True, exist_ok=args.resume)
    if (root / "plan.json").exists():
        raise ValueError("计划已存在；请使用原计划 apply/resume，不覆盖计划")
    root.chmod(0o700)
    current = adapter_call(adapter, "inspect", {})
    stable = identity(current)
    quiescent(current)
    backup = adapter_call(adapter, "backup", {"destination": str(root), "expected_identity": stable, "expected_epoch": current["repository_epoch"], "require_quiescent": True})
    if identity(backup) != stable:
        raise ValueError("备份期间仓库身份变化")
    records = []
    for item in backup["files"]:
        path = (root / item["path"]).resolve()
        if not path.is_relative_to(root) or not path.is_file():
            raise ValueError("备份文件无效")
        records.append({"role": item["role"], "path": str(path.relative_to(root)), "sha256": sha(path), "bytes": path.stat().st_size})
    check_files(root, records)
    if backup.get("consistent_checkpoint") is not True:
        raise ValueError("数据库和 Outbox 不是同一停机一致性快照")
    restored = adapter_call(adapter, "restore_verify", {"backup_directory": str(root), "files": records, "expected_identity": stable, "isolated_only": True})
    if restored.get("isolated") is not True or restored.get("database_restored") is not True or restored.get("outbox_matches") is not True or restored.get("bundle_verified") is not True or restored.get("artifacts_verified") is not True:
        raise ValueError("隔离恢复演练未全部通过")
    if restored.get("files_digest") != hashlib.sha256(canonical(records)).hexdigest():
        raise ValueError("恢复验证没有绑定本次备份摘要")
    # The adapter must inspect the intended replacement root commit without
    # modifying refs and prove that it has no parents and contains the pilot baseline.
    replacement = adapter_call(adapter, "inspect_replacement", {"commit": args.root_commit, "expected_identity": stable})
    if replacement.get("commit") != args.root_commit or replacement.get("parents") != [] or replacement.get("available_in_backup") is not True:
        raise ValueError("替换 root commit 未验证或尚未纳入备份")
    actions = current.get("reset_actions")
    if not isinstance(actions, dict) or not isinstance(actions.get("close_issues"), list) or not isinstance(actions.get("close_pull_requests"), list) or not isinstance(actions.get("delete_branches"), list):
        raise ValueError("适配器没有返回具体、可审阅的 Issue、PR、测试枝清单")
    if any(not isinstance(n, int) or n <= 0 for key in ("close_issues", "close_pull_requests") for n in actions[key]):
        raise ValueError("待关闭对象编号无效")
    if any(not b.startswith(("codex/issue-", "eval/", "pilot-test/")) for b in actions["delete_branches"]):
        raise ValueError("删除范围包含非测试分支")
    plan = {"version": 1, "repository": TARGET, "identity": stable, "actor_at_prepare": current["actor"], "repository_epoch": current["repository_epoch"], "created_at": dt.datetime.now(dt.timezone.utc).isoformat(), "adapter_sha256": sha(adapter), "backup_files": records, "restore_verification": restored, "refs": current["refs"], "branch_protection": current["branch_protection"], "root_commit": args.root_commit, "actions": {**actions, "force_update": {"ref": "refs/heads/main", "old_sha": current["refs"]["refs/heads/main"], "new_sha": args.root_commit}}, "postconditions": {"same_repository_id": True, "same_installation_id": True, "intake_stopped": True, "protection_restored": True}}
    save(root / "plan.json", plan)
    print("计划已准备，尚未重置：" + str(root / "plan.json"))
    print("计划 SHA256：" + sha(root / "plan.json"))

def apply(args):
    if not args.apply or args.repository != TARGET or args.confirm_repository != TARGET:
        raise ValueError("必须显式 --apply --repository 和 --confirm-repository " + TARGET)
    path = Path(args.plan).resolve(strict=True)
    if sha(path) != args.plan_sha256:
        raise ValueError("计划摘要与明确审阅的摘要不一致")
    plan = json.loads(path.read_text())
    if plan.get("repository") != TARGET or plan.get("version") != 1:
        raise ValueError("不是允许的 pilot 重置计划")
    adapter = Path(args.adapter).resolve(strict=True)
    if sha(adapter) != plan["adapter_sha256"]:
        raise ValueError("管理适配器已变化，必须重新准备计划")
    check_files(path.parent, plan["backup_files"])
    current = adapter_call(adapter, "inspect", {})
    if identity(current) != plan["identity"] or (not args.resume and current["repository_epoch"] != plan["repository_epoch"]):
        raise ValueError("仓库身份或 epoch 已变化")
    quiescent(current)
    if not args.resume and (current["refs"] != plan["refs"] or stable_protection(current["branch_protection"]) != stable_protection(plan["branch_protection"]) or current["reset_actions"] != {k: plan["actions"][k] for k in ("close_issues", "close_pull_requests", "delete_branches")}):
        raise ValueError("远端 refs、保护规则或对象清单已变化，需重新准备计划")
    # This operation is deliberately one adapter call: the adapter must hold the
    # server's maintenance fence and Git ref CAS throughout all remote changes.
    # It must leave intake stopped after partial failure and restore protection.
    receipt = adapter_call(adapter, "apply_reset", {"plan": plan, "plan_sha256": args.plan_sha256, "require_maintenance_fence": True, "require_force_with_lease": True, "forbid_repository_recreation": True, "forbid_installation_change": True, "resume": args.resume})
    save(path.parent / ("apply-receipt-" + dt.datetime.now().strftime("%Y%m%d%H%M%S") + ".json"), receipt)
    post = adapter_call(adapter, "inspect", {})
    if identity(post) != plan["identity"] or post["refs"].get("refs/heads/main") != plan["root_commit"] or stable_protection(post["branch_protection"]) != stable_protection(plan["branch_protection"]) or post.get("intake_stopped") is not True or receipt.get("all_actions_verified") is not True:
        raise ValueError("重置后条件未全部成立；保持停收，使用备份恢复并检查审计记录")
    print("重置和保护恢复已核验；保持停收，尚未开始任何评估。")

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="operation", required=True)
    p = sub.add_parser("prepare")
    for name in ("repository", "adapter", "output", "root-commit"):
        p.add_argument("--" + name, required=True)
    p.add_argument("--resume", action="store_true", help="恢复未写入plan的备份准备")
    p.set_defaults(func=prepare)
    p = sub.add_parser("apply")
    for name in ("repository", "confirm-repository", "adapter", "plan", "plan-sha256"):
        p.add_argument("--" + name, required=True)
    p.add_argument("--apply", action="store_true")
    p.add_argument("--resume", action="store_true", help="按同一计划和持久日志恢复部分完成的重置")
    p.set_defaults(func=apply)
    args = parser.parse_args()
    try:
        args.func(args)
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as exc:
        print("重置已停止：" + str(exc), file=sys.stderr)
        return 1
    return 0

if __name__ == "__main__":
    raise SystemExit(main())
