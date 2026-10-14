# 冻结工单评估协议

评估版本为 `issuedesk-alpha-v5-report-feedback`。同一个正确服务基线分别施加十种确定性变体，形成十道独立开发任务；每道任务重新建目录和独立 PostgreSQL 服务，不依赖其他题目的修改或数据。对应十对缺陷/修复候选可用于 Reviewer 独立判断，修复候选为同一冻结正确基线。所有标签初始 `human_review=pending`，正式报告必须由人工核准，不能把生成标签声称为已确认真值。

## 实验分组

- A：单 Builder，加相同常规 CI 与可信验收。
- B：单 Builder 与单 Reviewer，加相同常规 CI 与可信验收。
- C：完整双团队编排与对抗验证，加相同常规 CI 与可信验收。

每组每题重复三次，共九十次开发运行。模型配置、代码快照、题面、可用工具与每题总预算保持一致；不能给 C 每个 Agent 各一份完整预算。随机种子、模型供应方/版本、开始时间和实际调用量均由运行器留证。任务顺序应在运行器中按记录的随机种子打乱；本 CLI 输出的是稳定逻辑矩阵，不冒充已经随机执行。

`matrix --execution-config` 接收运行器导出的**有效非敏感配置 JSON**：`model_profile`，`budget` 中的 `issue_calls/daily_calls/issue_minutes/max_attempts/max_fix_rounds/input_price_per_million/output_price_per_million`，`runner` 中的 `attempt_minutes/agent_concurrency/workflow_agent_concurrency/max_tasks_per_workflow/max_evidence_requests/max_tool_calls_per_run`。这些字段都必须显式提供，不从 example 配置猜预算。`max_fix_rounds=0` 是有效配置，表示不允许自动修复轮，仍允许完成同预算评估。价格为零表示未知，成本输出 null。有效预算硬限仍由运行器执行；离线汇总只检测已记录的调用量、累计执行时间超限，不冒充调用前拦截。

`issue_calls` 与 `daily_calls` 限制实际模型 HTTP 请求尝试，包含传输重试，跨所有 Agent 共享；`max_tool_calls_per_run=200` 单独限制每次 Agent run 的普通工具调用尝试，包含被拒绝的未知工具批次。`max_calls` 与 `max_requests` 分别向模型呈现这两个轴。未知工具不会执行；修正反馈继续消耗原有 HTTP 请求额度，耗尽时停止。

v3 更新工具与 HTTP 的独立计数合同；v4 继续保留全部义务与独立审查，只调整预算规划、明确既定验证命令，并为独立复验增加批次身份。v5 将补证请求的字段与发现引用校验提前到报告接受前，并向审查规划者提供当前有效 ID 摘要；未知或过期引用在原请求预算内获得纠错反馈，事务内身份检查仍保留。v2 的失败探针和 v3／v4 的原始判定包、失败回执保留；服务基线、题面、确定性变体、候选标签、隐藏 driver／harness 与真值断言保持原 SHA-256，不以协议更新覆盖失败记录。

## 准备与隔离

`python3 eval/cli.py prepare --task T01 --candidate defect --output /独立目录/候选` 从 manifest 校验过的服务源文件、根模块锁和题面构建候选；候选目录必须尚不存在且位于源仓库外。`baseline` 与 `fix` 生成正确基线。CLI 不调用 Git、不写 GitHub、不运行 Go、不调用模型、不读取产品凭证。

候选仅包含 `go.mod/go.sum`、`fixtures/issuedesk/`、`ISSUE.md` 和中文 `AGENTS.md`。Agent 的文件系统/容器边界必须只挂载该候选；**不能只靠 cwd 隔离隐藏素材**。候选内容相同不等于 Git commit 相同，运行器可在候选内初始化新的独立 Git 仓库并记录 commit。生成结果标准输出含文件摘要与工作区摘要，保存在可信控制记录，不需要给 Agent。

`python3 eval/cli.py trusted-bundle --output /可信目录/判定包` 导出隐藏测试及候选标签。该目录只向评估器开放，不能挂载给 Builder/Reviewer。人工核准前标签仍标待校核。

Agent 结束后，评估器先固定候选 commit、父提交和真实 Git 树。候选工作区仅追加受信服务驱动并封为只读；另建只含可信模块锁与 harness 的只读工作区。两方是独立容器和 PID 空间，通过专用 Unix HTTP socket 与独立 PostgreSQL socket 交互，环境使用 `TEST_DATABASE_URL` 和 `AI_WORKFLOW_JUDGE_SOCKET`。候选不能读取或修改 harness，候选输出永远不参与判定事件解析。禁止连接真实业务数据库。

公开测试仍可执行 `go test ./fixtures/issuedesk/...`。正式隐藏判定通过控制器 Judge API 执行独立 harness 的目标 `TestHiddenTxx`，不再向候选 Go 包注入测试。正确基线／修复候选应通过；缺陷候选必须被对应真实断言拒绝。可信 harness 编译失败、目标缺失、跳过或超时返回未验证，不能冒充候选失败。候选服务编译或启动失败则由真实外部目标测试记录服务不可用，但不能用于证明某个预设缺陷的业务断言有效。

## 结果与汇总

`matrix` 的输出冻结九十个 `run_key` 及预算配置摘要。`summarize --matrix 矩阵.json --results 结果.jsonl --output 汇总.json` 只读运行器的已结束记录，不自动补齐、不默认判通过。

每条开发结果包含 `run_key/config_sha256/outcome/tests_passed/model_calls/input_tokens/output_tokens/duration_seconds/evidence`。`outcome` 为 `success/failed/cancelled/budget_exhausted/infrastructure_error`；`evidence` 包含结果 JSONL 所在目录下的相对文件 `path` 与 `sha256`。证据应保存候选 commit、实际命令、退出码、原始测试结果及执行配置，不含凭证。汇总验证证据存在且摘要一致，但不会重跑测试；真实性边界是可信运行器与只读证据存储。成功需 `success`、可信测试通过并未超记录的单题调用/时间预算。基础设施失败单列，不能被记为模型成功。

审查记录 `kind=review`，含 `candidate_id` 与布尔 `reported_defect`。二十个候选各提交一次独立判断；同一 Reviewer 不应先看到成对的另一候选、变体脚本或标签。当前汇总只对人工 `approved` 标签统计混淆矩阵；待校核样本单列，不能算准确率。正式人工核准需另行版本化 manifest 与审批证据，不能静默改写冻结素材。

十题覆盖有限业务边界，九十次结果不能外推为生产安全保证。性能、真实身份提供商、跨地域容灾和真实公网部署不在本夹具验收范围内。


## 外部可信判定（v2）

冻结清单 v2 使用两个独立容器进程。候选只获得固定服务驱动，通过私有 Unix HTTP socket 提供工单服务与迁移调用；可信 harness 不导入任何候选 Go 包，只通过 HTTP 和专用数据库核验结果。候选 stdout/stderr 只记录到 candidate_result，不参与 Go 测试事件解析。harness 源码、输出及进程空间均不向候选挂载。双方源码只读，停止确认及预算计账沿用同一可信执行标签。v1 同进程隐藏测试的结果不能作为 v2 抗候选伪造验收。


## 独立评估批次

准备器和 plan 支持 `--experiment-id`，将 1–24 位 ASCII 批次名加入每个 trial_id；省略时保留旧 ID。开发结果同时记录不可变 trial_id 和矩阵逻辑 run_key，不能将不同批次的同逻辑运行混为一条结果。准备、恢复、证据与检查点显式绑定批次；改变实现／执行配置须新建批次，保留旧失败，不修改旧 trial、Issue、PR、判定或用量。

预算规划将同一修改范围内的实现与测试合并，Planner 明确读取控制器实际验证配置；全量 mandatory obligations、独立 Reviewer 首轮隔离、Skeptic、可信 CI 和外部 Judge 继续执行。运行器配置显式映射专用数据库环境变量，实际执行数据库公开测试，不能用数据库测试 skip 宣称已覆盖。

累计执行时间使用已启动 Agent 活跃区间的并集，加控制器独立执行计费；排队和已确认停止后的人工等待不计入 Issue 执行时间预算。端到端 elapsed 从本地 workflow.created_at 到最终更新另行留证，不能把 runtime_seconds 宣称为端到端硬限或 GitHub Issue 原始创建时长。
