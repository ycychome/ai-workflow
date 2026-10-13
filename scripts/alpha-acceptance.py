#!/usr/bin/env python3
"""Run explicit acceptance layers and retain JSON evidence; never infer missing passes."""
import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
SCENARIOS = [
    'Webhook 重投十次', '事件乱序与当前快照对账', 'PR 成功但响应丢失', '评论成功后崩溃去重',
    '跨进程 Intent 争抢', 'Builder 运行时暂停', 'GitHub 写入在途暂停', '旧 Worker 返回',
    '成功退出但无产物', '依赖消费已集成 SHA', '同路径变更冲突', 'Git 元数据跨 Attempt 隔离',
    '人工推送不覆盖', '候选换 HEAD 重新门禁', 'base/spec/policy 变化失效', 'Review 超时不能 PASS',
    '义务覆盖完整性', '门禁来源与 neutral/skipped', 'GitHub 禁止非法 stale', '修复轮次上限',
    '有证据反驳 Finding', '提示注入不扩大权限', '未授权命令拒绝', '密钥及宿主 socket 隔离',
    '重启后有界对账', '预算与并发不重置', 'CI 与验收不可被候选替换', '关闭重开 Draft 生命周期',
]

# These entry points intentionally skip in the parent process; their owning
# tests launch the same binary with an explicit child environment.
PARENT_PROCESS_HELPERS = {'TestAgentQueueProcessWorker', 'TestAlphaAcceptanceQueueProcess'}

def summarize_events(name, events, returncode, matrix=False):
    passed = sorted({e['Test'] for e in events if e.get('Test') and e.get('Action') == 'pass'})
    skipped = sorted({e['Test'] for e in events if e.get('Test') and e.get('Action') == 'skip'})
    failed = sorted({e['Test'] for e in events if e.get('Test') and e.get('Action') == 'fail'})
    helper_skips = sorted(set(skipped) & PARENT_PROCESS_HELPERS)
    unexecuted = sorted(set(skipped) - PARENT_PROCESS_HELPERS)
    if returncode != 0 or failed:
        status = 'failed'
    elif not passed:
        status = 'blocked_or_skipped' if skipped else 'not_run'
    elif (name == 'docker' or matrix) and unexecuted:
        status = 'blocked_or_skipped'
    elif matrix and not any(t.startswith('TestAcceptance' + name) for t in passed):
        status = 'not_run'
    else:
        status = 'passed'
    return {'layer': name, 'status': status, 'exit_code': returncode,
            'passed': passed, 'skipped': skipped, 'failed': failed,
            'parent_helper_skips': helper_skips, 'unexecuted_skips': unexecuted}

def run_go(args, name, packages, pattern=None, matrix=False):
    env = os.environ.copy()
    if name == 'unit':
        for key in ('AI_WORKFLOW_FORCE_INTEGRATION', 'AI_WORKFLOW_FORCE_DOCKER', 'AGENT_QUEUE_TEST_DATABASE_URL', 'TEST_DATABASE_URL', 'DATABASE_URL', 'CONFIG_FILE'):
            env.pop(key, None)
    elif name == 'postgres':
        env.pop('AI_WORKFLOW_FORCE_DOCKER', None)
        for key in ('CONFIG_FILE', 'AGENT_QUEUE_TEST_DATABASE_URL'):
            if not env.get(key):
                raise ValueError(f'{key} must point at the isolated acceptance database/config')
        env['AI_WORKFLOW_FORCE_INTEGRATION'] = '1'
    elif name == 'docker':
        for key in ('CONFIG_FILE', 'AGENT_QUEUE_TEST_DATABASE_URL'):
            if not env.get(key):
                raise ValueError(f'{key} is required for the complete Docker/orchestration layer')
        env['AI_WORKFLOW_FORCE_INTEGRATION'] = '1'
        env['AI_WORKFLOW_FORCE_DOCKER'] = '1'
    command = ['go', 'test', '-json', '-count=1', '-timeout=15m'] + packages
    if pattern:
        command += ['-run', pattern]
    log = args.output / (name + '.jsonl')
    with log.open('x') as output:
        completed = subprocess.run(command, cwd=ROOT, env=env, stdout=output, stderr=subprocess.STDOUT)
    events = []
    for line in log.read_text(errors='replace').splitlines():
        try:
            events.append(json.loads(line))
        except json.JSONDecodeError:
            pass
    result = summarize_events(name, events, completed.returncode, matrix)
    result.update({'command': command, 'evidence': log.name,
                   'sha256': hashlib.sha256(log.read_bytes()).hexdigest()})
    return result

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('layer', choices=['unit', 'postgres', 'docker', 'matrix', 'all', 'list'])
    parser.add_argument('--output', type=Path)
    args = parser.parse_args()
    if args.layer == 'list':
        for n, scenario in enumerate(SCENARIOS, 1):
            print(f'T{n:02d}\tTestAcceptanceT{n:02d}*\t{scenario}')
        return 0
    if args.output is None:
        parser.error('--output NEW_DIRECTORY is required')
    args.output.mkdir(parents=True, exist_ok=False)
    args.output.chmod(0o700)
    results = []
    try:
        layers = ['unit', 'postgres', 'docker', 'matrix'] if args.layer == 'all' else [args.layer]
        for layer in layers:
            if layer == 'unit':
                results.append(run_go(args, layer, ['./...']))
            elif layer == 'postgres':
                results.append(run_go(args, layer, ['./internal/store', './internal/orchestration', './internal/server']))
            elif layer == 'docker':
                results.append(run_go(args, layer, ['./internal/sandbox', './internal/orchestration']))
            else:
                for n in range(1, 29):
                    identifier = f'T{n:02d}'
                    result = run_go(args, identifier, ['./...'], '^TestAcceptance' + identifier + '($|[^0-9])', matrix=True)
                    result['scenario'] = SCENARIOS[n - 1]
                    results.append(result)
    except (ValueError, OSError) as exc:
        results.append({'status': 'blocked', 'reason': str(exc)})
    # Unit/PG layers deliberately omit other prerequisites. In the complete
    # run every non-helper skip must be covered by an actual pass elsewhere.
    all_passed = {test for result in results for test in result.get('passed', [])}
    uncovered_skips = sorted({test for result in results for test in result.get('unexecuted_skips', [])} - all_passed)
    complete = bool(results) and all(r['status'] == 'passed' for r in results)
    if args.layer == 'all' and uncovered_skips:
        complete = False
    report = {'created_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
              'status': 'passed' if complete else 'incomplete',
              'uncovered_skips': uncovered_skips,
              'scope': args.layer, 'results': results,
              'note': 'Layer results are not live GitHub or UI acceptance. Matrix requires all 28 named tests; missing or skipped cases remain incomplete.'}
    (args.output / 'summary.json').write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n')
    print(f"{report['status']}: {args.output / 'summary.json'}")
    return 0 if report['status'] == 'passed' else 1

if __name__ == '__main__':
    sys.exit(main())
