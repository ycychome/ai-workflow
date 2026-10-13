# Pilot 重置与受信评估

本文说明可执行入口及仓库内的具体 GitHub/PostgreSQL 管理适配器。当前代码交付不表示已执行重置、试验、恢复演练或管理员权限检查。仓库只能是 `ycychome/ai-workflow-pilot`；不能删除重建仓库或重新安装 GitHub App。

## 重置前置条件

先停止该仓库的入口，等待 ACTIVE 和 STOPPING Agent、工作流租约、在途 Outbox 全部清空。控制器、GitHub 管理适配器必须使用同一仓库维护栅栏；单独停止某个进程不能替代它。待发送但尚未发送的 Outbox 必须备份，并在重置时通过 repository epoch 失效，不能重新启动后发到新一代仓库。

`scripts/pilot-reset.py prepare` 读取远端真实身份与 refs，调用受审阅的管理员适配器创建 Git bundle、Issue/PR 元数据、分支保护、控制数据库、Outbox 和证据/工作区备份，再进行隔离恢复演练。只有全部回执完整才写出 `plan.json`。备份可能含私有资料；目录权限为 0700，计划为 0600，不上传版本库。

```sh
python3 scripts/pilot-reset.py prepare \
  --repository ycychome/ai-workflow-pilot \
  --adapter "$PWD/scripts/pilot-github-admin.py" \
  --output /secure/backups/pilot-reset-YYYYMMDD \
  --root-commit <已准备且纳入bundle的40位root提交>
```

逐项审阅计划中的数字仓库 ID、安装 ID、当前管理员、旧 refs、具体 Issue/PR 编号、测试分支删除清单、旧 main SHA、新 root SHA、保护策略和恢复回执。准备不改 refs，不关 Issue/PR。

```sh
python3 scripts/pilot-reset.py apply --apply \
  --repository ycychome/ai-workflow-pilot \
  --confirm-repository ycychome/ai-workflow-pilot \
  --adapter "$PWD/scripts/pilot-github-admin.py" \
  --plan /secure/backups/pilot-reset-YYYYMMDD/plan.json \
  --plan-sha256 <准备阶段打印且人工审阅的摘要>
```

apply 重新核验所有文件摘要、仓库身份、epoch、refs、保护策略和对象清单。适配器必须在维护锁内再检查这些条件，并以旧 main SHA 为比较条件修改引用，不能无条件强推。任一步失败保持停收，保留审计和恢复信息；保护规则无论成功失败都必须恢复。不会自动启动评估。

## 管理适配器协议

适配器是明确指定的可执行文件，使用标准输入接收一个 JSON，标准输出返回一个 JSON；不得在 stdout 输出凭据或非 JSON 日志。脚本冻结该可执行文件的 SHA256。适配器必须进一步冻结自身依赖或采用固定镜像。不能使用只返回成功的模拟适配器执行真实 apply。

请求公共字段：`protocol: pilot-admin/v1`、`operation`、`repository`。响应必须原样返回 protocol、operation，并返回 `ok: true`；否则失败关闭。身份字段为 `repository`、正整数 `repository_id`、`installation_id` 和 `actor: {login,id,admin:true}`。身份必须来自当次 GitHub API 的仓库与当前认证身份，禁止仅回显配置。

操作约定：

- `inspect`：返回真实身份、`intake_stopped`、`active_agents`、`stopping_agents`、`active_workflow_leases`、`inflight_outbox`、`repository_epoch`、`maintenance_lock_capable`、`refs`（完整 ref 到 SHA 映射）、`branch_protection`（规则、rulesets、绕过权限及是否允许强推的探测回执）、`reset_actions`（`close_issues`、`close_pull_requests` 编号数组与 `delete_branches` 数组）。不存在保护规则也须明确记录 API 成功返回的空规则，而非权限不足时当作空。
- `backup`：请求有 destination、expected_identity、expected_epoch、require_quiescent。持维护锁产生一致性快照并返回 `consistent_checkpoint:true`、身份和 `files:[{role,path}]`。必需 role 为 git_bundle、refs、issues、pull_requests、branch_protection、repository_identity、control_database、outbox、control_artifacts。path 必须为备份目录下的实际相对文件。bundle 必须保留所有旧 refs 及替换 root；数据库和 Outbox 不能由时间不同的独立导出冒充一致备份。包含关闭对象的元数据，以便重新开启。
- `restore_verify`：只在隔离数据库/临时 bare 仓库恢复，绝不能覆盖当前控制 DB 或远端 refs。返回 `isolated`、`database_restored`、`outbox_matches`、`bundle_verified`、`artifacts_verified` 全部 true，以及 `files_digest`（请求 files 数组的 UTF-8 JSON，键排序、无空白、ensure_ascii=false 的 SHA256）和可审计恢复日志路径/摘要。核验记录数、Outbox 状态、引用可解析性和对象完整性。
- `inspect_replacement`：返回真实 commit、parents 空数组、available_in_backup=true。提交必须为准备好的基线 root，不得仅使用未知 SHA。
- `apply_reset`：接收完整 plan、plan_sha256 和保护标志。取得跨入口/worker/outbox的仓库维护锁；重新核验身份、旧 epoch、所有状态和备份；推进 repository epoch 并退休旧控制工作流、使旧 Outbox 失效；在 GitHub 管理权限允许的临时保护例外内，以 force-with-lease 等价 CAS 将 main 指向指定 root；关闭清单内旧 Issue/PR；删除清单内测试分支；恢复原保护；重新逐项读取验证。操作日志持久化，返回 all_actions_verified=true 和逐项回执。禁止重建仓库、变更 App 安装、扩大清单或恢复收件。部分失败必须保持停收并检查持久日志；当 refs 或 epoch 已变化，外层脚本拒绝原计划盲目重放，需先通过备份和日志完成明确恢复或准备新的恢复计划。

仓库提供 `scripts/pilot-github-admin.py`，直接使用 GitHub REST API、Git、PostgreSQL 工具和控制维护 API。它实际核验管理员 `/user`、仓库数字 ID、App JWT 对应安装；使用 PostgreSQL exported snapshot 同步导出数据库和 Outbox，再创建独立随机数据库执行 pg_restore、对比记录和 Outbox、销毁该临时数据库。产物在临时目录恢复并核验。Git main 与测试枝采用 force-with-lease CAS。遇到组织/企业 rulesets 会明确拒绝，避免扩大管理范围。以上是代码能力，并非已运行的权限或恢复证明。

## 冻结评估配置和引用

控制台鉴权后的 `GET /api/evaluation/config` 返回真实安全配置、config_sha256、models 和 runtime profiles。把其中 `config` 对象独立保存为 execution-config.json，再交给冻结的 `eval/cli.py matrix --execution-config`。profiles 对应 A=builder_ci、B=single_review、C=full；不能用 Issue 标签选择。配置中的价格只是用户配置的估算价格，不是账单金额。

准备脚本使用仓库内 `pilot-eval-github.py` 真实 Git Data API 适配器，显式 --execute 后才产生远端变更：

```sh
python3 scripts/pilot-eval-prepare.py --execute \
  --repository ycychome/ai-workflow-pilot --repository-id <ID> --installation-id <ID> \
  --manifest eval/manifest.json --matrix /secure/eval/matrix.json \
  --adapter "$PWD/scripts/pilot-eval-github.py" --seed 20261013 \
  --workspace-root /secure/eval/candidates --output /secure/eval/snapshots.json
```

准备脚本每次使用冻结生成器独立重建候选，校验实际源码文件摘要，不把标签、变体补丁或隐藏测试放进候选工作区。适配器协议为 `pilot-eval-prepare/v1`，operation=prepare_independent_trial；请求包含数字身份、trial、workspace、source_files、source_sha256、baseline_source_sha256 和 idempotency_key。适配器必须核验实际远端身份，独立创建 `eval/<trial>/base`（不能继承前一试验的修复），审查试验另建候选分支和真实 PR，然后从远端读取 commit 内容核对 source_files。响应返回 ok/protocol/repository/repository_id/installation_id/source_sha256/independently_rebuilt=true/base_source_sha256/base_sha/base_branch/candidate_sha/candidate_branch/pull_request。同一 idempotency_key 永远返回相同 refs/PR，不覆盖不同内容的既有分支。该具体适配器逐个下载远端 blob 再核验完整树，持久保存 commit/ref/PR 创建回执；PR 结果未知时只搜索原 PR，不重复创建。审查 trial ID 使用不含 defect/fix 的摘要，避免在题面标记、分支和提交信息中泄露标签。

开发和 smoke 的 base=candidate=缺陷快照。review 的 base 是冻结无缺陷基线，head 是指定 defect/fix 候选；API只审查固定候选，不经过 Builder。对于 fix 与 baseline 内容相同的情况，适配器需要可创建真实 PR 的独立提交拓扑（允许树相同，不能伪造 diff 或引入业务变更）。适配器须保留源码摘要证明与实际 GitHub PR 链接。

以只读文件将冻结 `eval/manifest.json` 及相对题面挂载到 `AI_WORKFLOW_EVAL_MANIFEST`，将准备回执 snapshots.json 挂载到 `AI_WORKFLOW_EVAL_SNAPSHOTS`。普通 Issue 和模型无法写这些文件。API再次核验题面摘要、快照归属、App 安装、数字仓库 ID、真实 base SHA 和 review PR 的 head/base/repo。Spec 从冻结题面的可见验收条目建立，并精确绑定含保留评估标记的 issue-v3 source hash。

## 执行、暂停与恢复

```sh
python3 cmd/evaluate/main.py plan --manifest eval/manifest.json \
  --matrix /secure/eval/matrix.json --snapshots /secure/eval/snapshots.json \
  --seed 20261013 --output /secure/eval/plan.json
python3 cmd/evaluate/main.py run --execute --plan /secure/eval/plan.json \
  --server https://<控制面> --token-file /secure/dashboard-token \
  --output /secure/eval/results
```

plan 只操作本地文件。run 使用控制面会话认证，拒绝不安全远端 HTTP 和重定向。顺序为 10 smoke、90 个随机排序的开发试验、20 个随机排序的固定候选审查；smoke 失败停止后续评估。每个 trial 的 POST 只传 trial_id/config_digest/manifest_digest，服务端从只读快照决定所有身份和配置。Issue 创建前持久预留；未知创建结果只搜索原标记，不再创建第二个 Issue。若长期找不到 Issue，需维护者核对实际 GitHub 请求结果，不能自行删除预留记录冒充全新试验。

达到每日额度保存 checkpoint 并退出；次日运行同一命令，原试验通过专用 daily resume 继续。Issue 累计 calls、tokens、runtime、attempts 和修复轮次不重置。总预算耗尽记失败。等待风险授权、规格人工门或其他人工决议时保存检查点并退出；执行器绝不会自动发 authorize-risk、waive、forceaccept 或 merge。

每次试验记录真实模型、有效配置、calls/tokens/runtime、估算 cost、冻结身份、结果、实际证据与 GitHub 链接。终态通过 `/judge` 调用独立可信隐藏判定，tests_passed 只来自该判定及其停止回执；公共测试通过不代替隐藏判定。基础设施、预算或清理失败记 UNVERIFIED，不假造正确性判断。所有证据下载后核验摘要。records/*.json 为试验回执，development.jsonl/review.jsonl 可用于冻结汇总器，smoke.jsonl 单独保存。人工标签、人类接受与人工合并一律 pending，未签核的缺陷/修复标签不计入准确率。适配器与真实模型尚未部署、hidden trusted CI 未配置或证据缺失时不得声称 120 次评估已通过。

## 具体适配器部署参数

运行主机需要 Python 3.10+、git、openssl、psql、pg_dump、pg_restore；不要求安装 gh。脚本检查缺少工具时失败关闭，不自动安装。准备环境变量只配置路径/数字身份，真实值不进入计划或日志：

- `PILOT_GITHUB_ADMIN_TOKEN_FILE`（或显式 `PILOT_GITHUB_ADMIN_TOKEN`）：能读取当前用户且对精确仓库拥有 admin 的凭据。
- `PILOT_GITHUB_APP_ID`、`PILOT_GITHUB_APP_KEY_FILE`、`PILOT_INSTALLATION_ID`、`PILOT_REPOSITORY_ID`：用于 App JWT 和安装/仓库当次读回校验。
- `PILOT_CONTROL_URL`、`PILOT_CONTROL_TOKEN_FILE`：本地 HTTP 或远端 HTTPS 控制面；自动登录，拒绝重定向。
- `PILOT_DATABASE_URL_FILE`、`PILOT_RESTORE_ADMIN_DATABASE_URL_FILE`：明确不同的生产备份连接与隔离恢复管理员连接。临时库名固定为随机 `aiwf_restore_*`，绝不 drop 生产库。密码只通过子进程环境传递。
- `PILOT_CONTROL_ARTIFACTS`、`PILOT_BACKUP_DIRECTORY`：控制产物目录与本次备份目录；后者须与 prepare 的 output 相同。
- `PILOT_ROOT_COMMIT`：与 prepare 的 root-commit 相同，必须已存在于远端、无父提交；备份会额外抓取并打包。
- `PILOT_MAINTENANCE_STATE_DIR`：0700 的维护令牌目录，不能上传或删除；令牌先写磁盘再 acquire，支持未知获取结果重放。

首次停收是独立显式操作，使用真实 adapter stdin：

```sh
scripts/pilot-github-admin.py <<'JSON'
{"protocol":"pilot-admin/v1","operation":"stop_intake","repository":"ycychome/ai-workflow-pilot"}
JSON
```

此操作只停止入口和新派发；已有执行允许清理退出。等待 status 中 ACTIVE/STOPPING/工作流租约/在途 Outbox 均为零，然后 prepare。unknown Outbox 必须先核对实际远端结果，不能直接改成 done。

retire API在同一仓库退休完成后只持久更新该仓库配置 epoch，不触发其他仓库全局失效。apply 完成仍保持停收。只有审阅最终回执、真实 main/保护/身份再次验证一致后，才可显式恢复：

```sh
scripts/pilot-github-admin.py <<'JSON'
{"protocol":"pilot-admin/v1","operation":"resume_intake","repository":"ycychome/ai-workflow-pilot","plan_sha256":"<已核验的plan摘要>"}
JSON
```

resume 会读取本地已完成审计、持久令牌，核验远端当前身份和保护，再要求控制面 DB/config epoch 完全一致。多控制器部署仍须让所有进程读取已更新配置后再执行正式试验。

评估 GitHub 适配器另外需要：`PILOT_EVAL_STATE_DIRECTORY`（独立引用/PR预留回执）；`PILOT_EVAL_BASELINE_DIRECTORY`（冻结无缺陷基线源码）；`PILOT_EVAL_TRUSTED_FILES`（只读 JSON，CI目标路径映射 `{path,sha256}`，路径相对该 JSON 目录）。只允许 `.github/workflows/` 和 `.github/actions/` 下的受信 CI 配置，不接受任意隐藏测试代码。CI必须指向已授权的受信验证环境；未配置时明确失败。所有工作区除冻结文件、ISSUE.md、AGENTS.md和受信CI配置外不得含其他文件。

## 中断后的明确恢复

所有破坏性操作和临时保护例外都会先写入同计划的持久意图日志。遇到 SIGKILL 或网络未知结果，保留备份、plan、维护令牌目录和日志；不要删除预留记录。恢复保护可独立执行，无需 main 仍为旧 SHA：

```sh
scripts/pilot-github-admin.py <<'JSON'
{"protocol":"pilot-admin/v1","operation":"recover_protection","repository":"ycychome/ai-workflow-pilot","plan_sha256":"<原plan摘要>"}
JSON
```

此入口先核验真实仓库/安装，再只允许原规则或日志中明确记录的临时例外；未知规则增删或修改会拒绝覆盖。恢复全部原规则后逐项读回，保持停收。确认后用原 apply 命令增加 `--resume`，adapter 重获同一个维护身份，允许 main 为计划旧/新值、测试枝为原 SHA/已删除、清单对象已关闭，按读回幂等继续。任何计划外引用、对象或保护变动都停止，不把它们纳入重置范围。

备份准备尚未写出 plan 时，可在原 prepare 命令增加 `--resume`；完整备份回执按摘要读回复用，未完成的自有备份文件重新生成，绝不覆盖已经存在的 plan。已批准计划不因脚本重新启动变化。
