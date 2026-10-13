#!/usr/bin/env python3
"""工单评估离线准备与结果汇总；不执行模型、GitHub 操作或测试。"""
from __future__ import annotations
import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import shutil
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
EVAL = ROOT / "eval"

def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()

def load(path: Path):
    return json.loads(path.read_text(encoding="utf-8"))

def output(path: Path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    # 独占创建，防止覆盖既有实验、证据或用户文件。
    with path.open("x", encoding="utf-8") as stream:
        json.dump(value, stream, ensure_ascii=False, indent=2, sort_keys=True)
        stream.write("\n")

def trusted_manifest():
    return load(EVAL / "manifest.json")

def verify_sources(manifest):
    for relative, expected in manifest["baseline_files"].items():
        path = ROOT / relative
        if not path.is_file() or digest(path.read_bytes()) != expected:
            raise ValueError(f"冻结基线校验失败：{relative}；应重新审定整个评估版本，不能静默更新")
    for task in manifest["tasks"]:
        if digest((EVAL / task["issue_path"]).read_bytes()) != task["issue_sha256"]:
            raise ValueError(f"冻结题面校验失败：{task['id']}")
    for relative, expected in manifest["trusted_files"].items():
        if digest((EVAL / relative).read_bytes()) != expected:
            raise ValueError(f"可信判定素材校验失败：{relative}")

def task_of(manifest, task_id):
    for task in manifest["tasks"]:
        if task["id"] == task_id:
            return task
    raise ValueError(f"未知任务：{task_id}")

def prepare(args):
    manifest = trusted_manifest()
    verify_sources(manifest)
    task = task_of(manifest, args.task)
    target = args.output.resolve()
    if target.exists():
        raise ValueError("候选目录已存在，拒绝覆盖；每轮使用新的独立目录")
    if target == ROOT or ROOT in target.parents:
        raise ValueError("候选目录必须位于源仓库之外，避免泄露评估素材")
    target.parent.mkdir(parents=True, exist_ok=True)
    stage = Path(tempfile.mkdtemp(prefix=".issuedesk-prepare-", dir=target.parent))
    try:
        for relative in manifest["baseline_files"]:
            destination = stage / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(ROOT / relative, destination)
        if args.candidate == "defect":
            for edit in load(EVAL / "variants.json")[args.task]["edits"]:
                path = stage / edit["path"]
                text = path.read_text(encoding="utf-8")
                if text.count(edit["before"]) != 1:
                    raise ValueError(f"确定性变体锚点不唯一：{edit['path']}")
                path.write_text(text.replace(edit["before"], edit["after"], 1), encoding="utf-8")
        shutil.copyfile(EVAL / task["issue_path"], stage / "ISSUE.md")
        # 候选只含服务、公开测试、模块锁和题面，不含标签、隐藏测试或生成补丁。
        (stage / "AGENTS.md").write_text("所有自然语言输出使用简体中文。按 ISSUE.md 完成任务并记录验证证据。不要推测或寻找隐藏评估素材。\n", encoding="utf-8")
        hashes = {str(p.relative_to(stage)): digest(p.read_bytes()) for p in sorted(stage.rglob("*")) if p.is_file()}
        os.rename(stage, target)
    except BaseException:
        shutil.rmtree(stage, ignore_errors=True)
        raise
    print(json.dumps({"task_id":args.task,"workspace":str(target),"snapshot_sha256":digest(json.dumps(hashes,sort_keys=True).encode()),"files":hashes}, ensure_ascii=False))

def trusted_bundle(args):
    manifest = trusted_manifest()
    verify_sources(manifest)
    target = args.output.resolve()
    if target.exists():
        raise ValueError("可信目录已存在，拒绝覆盖")
    target.mkdir(parents=True)
    shutil.copytree(EVAL / "trusted" / "harness", target / "harness")
    shutil.copytree(EVAL / "trusted" / "driver", target / "driver")
    output(target / "labels.json", manifest["review_pairs"])
    output(target / "verification.json", {"manifest_version":manifest["version"],"candidate_driver_destination":".aiwf-judge-driver/main.go","harness_module":"ai-workflow-trusted-judge","required_environment":["TEST_DATABASE_URL","AI_WORKFLOW_JUDGE_SOCKET"],"tasks":[{"id":t["id"],"test_pattern":t["test_pattern"]} for t in manifest["tasks"]],"isolation":"harness独立容器和只读工作区，不导入候选模块；候选输出只留日志，绝不解析为判定"})
    print("可信判定素材已导出；未执行任何验证。")

def number(value, name, positive=False):
    if isinstance(value,bool) or not isinstance(value,(int,float)) or not math.isfinite(value) or value < 0:
        raise ValueError(f"数值无效：{name}")
    if positive and value <= 0:
        raise ValueError(f"配置必须大于零：{name}")
    return value

def matrix(args):
    manifest = trusted_manifest()
    verify_sources(manifest)
    # 只接受运行器导出的非敏感有效配置；不读取产品 YAML 或环境凭证。
    raw = load(args.execution_config)
    budget_keys = ("issue_calls","daily_calls","issue_minutes","max_attempts","max_fix_rounds","input_price_per_million","output_price_per_million")
    runner_keys = ("attempt_minutes","agent_concurrency","workflow_agent_concurrency","max_tasks_per_workflow","max_evidence_requests","max_tool_calls_per_run")
    config = {"budget":{},"runner":{},"model_profile":raw.get("model_profile")}
    if not isinstance(config["model_profile"],str) or not config["model_profile"]:
        raise ValueError("必须提供固定且不含凭证的 model_profile 标识")
    for section, keys in (("budget",budget_keys),("runner",runner_keys)):
        for key in keys:
            value = raw.get(section,{}).get(key)
            number(value,f"{section}.{key}",positive="price" not in key and key != "max_fix_rounds")
            if "price" not in key and not isinstance(value,int):
                raise ValueError(f"整数配置无效：{section}.{key}")
            config[section][key] = value
    config_sha = digest(json.dumps(config,sort_keys=True,separators=(",",":")).encode())
    runs = [{"run_key":f"{task['id']}-{arm}-{repeat}","task_id":task["id"],"arm":arm,"repeat":repeat,"candidate":"defect","config_sha256":config_sha} for task in manifest["tasks"] for arm in manifest["arms"] for repeat in range(1,manifest["repetitions"]+1)]
    output(args.output,{"version":1,"manifest_version":manifest["version"],"config":config,"config_sha256":config_sha,"arms":manifest["arms"],"runs":runs,"execution_state":"not_started"})
    print(f"已冻结 {len(runs)} 个离线运行项；预算来自传入的有效执行配置，未启动模型。")

def summarize(args):
    manifest = trusted_manifest()
    plan = load(args.matrix)
    expected = {item["run_key"]:item for item in plan["runs"]}
    if len(expected)!=len(plan["runs"]):
        raise ValueError("实验矩阵含重复运行项")
    frozen_keys = {f"{task['id']}-{arm}-{repeat}" for task in manifest["tasks"] for arm in manifest["arms"] for repeat in range(1,manifest["repetitions"]+1)}
    if set(expected) != frozen_keys or plan.get("manifest_version") != manifest["version"] or plan.get("arms") != manifest["arms"]:
        raise ValueError("实验矩阵与冻结协议不一致，不能减少题目或重复次数")
    for key,item in expected.items():
        if key != f"{item['task_id']}-{item['arm']}-{item['repeat']}" or item.get("config_sha256") != plan["config_sha256"]:
            raise ValueError("实验矩阵运行身份或配置摘要无效")
    config = plan["config"]
    actual_sha = digest(json.dumps(config,sort_keys=True,separators=(",",":")).encode())
    if actual_sha != plan["config_sha256"]:
        raise ValueError("实验矩阵配置校验失败")
    records = []
    for line_no,line in enumerate(args.results.read_text(encoding="utf-8").splitlines(),1):
        if line.strip():
            item=json.loads(line)
            if not isinstance(item,dict): raise ValueError(f"结果第 {line_no} 行必须是对象")
            records.append(item)
    accepted = {}; review = []; evidence_base=args.results.resolve().parent
    for record in records:
        if record.get("kind") == "review":
            review.append(record); continue
        key=record.get("run_key")
        if key not in expected or key in accepted: raise ValueError(f"未知或重复的运行结果：{key}")
        if record.get("config_sha256")!=plan["config_sha256"]: raise ValueError(f"运行配置不一致：{key}")
        if record.get("outcome") not in ("success","failed","cancelled","budget_exhausted","infrastructure_error"):
            raise ValueError(f"运行终态无效：{key}")
        for name in ("model_calls","input_tokens","output_tokens","duration_seconds"):
            number(record.get(name),name)
            if name != "duration_seconds" and not isinstance(record[name],int):
                raise ValueError(f"调用量与 token 用量必须为整数：{key}")
        evidence=record.get("evidence",{})
        relative=Path(evidence.get("path",""))
        evidence_path=(evidence_base/relative).resolve()
        if relative.is_absolute() or evidence_base not in evidence_path.parents or not evidence_path.is_file():
            raise ValueError(f"证据必须为结果目录内的文件：{key}")
        if digest(evidence_path.read_bytes())!=evidence.get("sha256"):
            raise ValueError(f"证据摘要不匹配：{key}")
        if not isinstance(record.get("tests_passed"),bool):raise ValueError(f"缺少可信测试结论：{key}")
        accepted[key]=record
    arms={}
    for arm in plan["arms"]:
        items=[r for key,r in accepted.items() if expected[key]["arm"]==arm]
        over=[r for r in items if r["model_calls"]>config["budget"]["issue_calls"] or r["duration_seconds"]>config["budget"]["issue_minutes"]*60]
        success=[r for r in items if r["outcome"]=="success" and r["tests_passed"] and r not in over]
        input_tokens=sum(r["input_tokens"] for r in items);output_tokens=sum(r["output_tokens"] for r in items)
        ip=config["budget"]["input_price_per_million"];op=config["budget"]["output_price_per_million"]
        arms[arm]={"expected":sum(1 for r in expected.values() if r["arm"]==arm),"recorded":len(items),"verified_success":len(success),"budget_violations":len(over),"model_calls":sum(r["model_calls"] for r in items),"input_tokens":input_tokens,"output_tokens":output_tokens,"duration_seconds":sum(r["duration_seconds"] for r in items),"estimated_cost":(input_tokens*ip+output_tokens*op)/1_000_000 if ip>0 and op>0 else None,"outcomes":{state:sum(r["outcome"]==state for r in items) for state in ("success","failed","cancelled","budget_exhausted","infrastructure_error")}}
    pairs={c["candidate_id"]:{**c,"human_review":pair["human_review"]} for pair in manifest["review_pairs"] for c in pair["candidates"]}
    confusion={"true_positive":0,"false_positive":0,"true_negative":0,"false_negative":0};seen=set();pending=0
    for r in review:
        candidate=r.get("candidate_id")
        if candidate not in pairs or candidate in seen or not isinstance(r.get("reported_defect"),bool): raise ValueError("审查结果候选编号重复或无效")
        seen.add(candidate);truth=pairs[candidate]
        if truth["human_review"]!="approved":pending+=1;continue
        actual=truth["label"]=="defect";reported=r["reported_defect"]
        confusion[("true_" if actual==reported else "false_")+("positive" if reported else "negative")]+=1
    output(args.output,{"version":1,"config_sha256":plan["config_sha256"],"arms":arms,"missing_runs":sorted(set(expected)-set(accepted)),"complete":len(accepted)==len(expected),"review":{"recorded":len(review),"pending_human_review":pending,"confusion_matrix_approved_only":confusion},"limitation":"结果依据冻结配置和带摘要的可信执行证据汇总；未在汇总阶段重新执行测试。人工未核准的标签不计入准确率。"})
    print(f"已汇总 {len(accepted)} 次开发运行和 {len(review)} 条审查结果；缺失运行未记为成功。")

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    sub=parser.add_subparsers(dest="command",required=True)
    p=sub.add_parser("prepare",help="从冻结基线生成独立候选；不执行测试")
    p.add_argument("--task",required=True);p.add_argument("--candidate",choices=("baseline","defect","fix"),required=True);p.add_argument("--output",type=Path,required=True);p.set_defaults(run=prepare)
    p=sub.add_parser("trusted-bundle",help="导出给可信评估器的素材，不放入候选")
    p.add_argument("--output",type=Path,required=True);p.set_defaults(run=trusted_bundle)
    p=sub.add_parser("matrix",help="按实际有效配置冻结三组各三次的实验矩阵")
    p.add_argument("--execution-config",type=Path,required=True);p.add_argument("--output",type=Path,required=True);p.set_defaults(run=matrix)
    p=sub.add_parser("summarize",help="核验执行记录证据并汇总")
    p.add_argument("--matrix",type=Path,required=True);p.add_argument("--results",type=Path,required=True);p.add_argument("--output",type=Path,required=True);p.set_defaults(run=summarize)
    args=parser.parse_args()
    try: args.run(args)
    except (ValueError,OSError,KeyError,TypeError) as error:
        print(f"离线评估操作失败：{error}",file=sys.stderr);return 2
    return 0
if __name__=="__main__":
    raise SystemExit(main())
