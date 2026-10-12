# MVP 交付说明

当前实现覆盖 GitHub Issue 入口、需求批准、Dev DAG、串行提交、独立 Review DAG、Skeptic、证据账本、合并门禁，以及状态/配置 Web 控制台。最终合并由维护者执行。

## 使用入口

- 控制台：<http://127.0.0.1:8081>。在本机读取 `secrets/dashboard-token` 并登录；密钥不进入 Git。
- 实现 PR：<https://github.com/ycychome/ai-workflow/pull/2>。
- 公开试点：<https://github.com/ycychome/ai-workflow-pilot>，仅演示代码和任务。
- 本机运行：Colima `ai-workflow` 中的 Docker Compose control/PostgreSQL；独立容器执行任务，默认无网络、无宿主 socket 和密钥。
- 启停、备份与服务器部署说明见 [runbook](runbook.md)。当前 webhook 使用临时 Cloudflare 隧道；重启隧道后须更新 GitHub App webhook。持续运行应使用固定域名和常驻服务器。

设置页已配置 Responses 协议和 `deepseek-v4.1-flash`（Dev/Review）。API Token 仅保存于本机 `secrets`，设置接口不会回传。输入/输出价格未配置时显示未知成本；填写每百万 Token 单价后可观察估算值。调用数、执行时长、并发数、尝试和修复轮数均有上限。

## 工作流操作

1. 在试点仓库创建明确需求 Issue。新 Issue 进入 `INTAKE`，不会仅因公网用户发帖而消耗模型预算。启动时及每 60 秒通过 GitHub API 补发现漏送的开放 Issue，每仓库每轮读取最新页和一页轮转历史记录；补入不会自动接受需求或重放评论命令。
2. 维护者评论 `/ai accept`，或在已登录控制台接受，启动 Triage。
3. 查看提议 TaskSpec 的验收条件，再次 `/ai accept` 批准规格。开发任务按依赖和文件冲突调度，产生主要 PR。并行工作流数在「预算与执行」中可调（1–4，默认 2）；仅自动执行阶段的有效租约占用容量，需求补充、规格批准和人工合并等待不占名额。各 Issue 使用独立分支、PR、沙箱和证据；目标分支变化会使旧验证失效。
4. 可信验证和独立 Reviewer/Skeptic 对当前 base/head/spec/policy 身份记录证据。只有覆盖完整、验证成功且没有未解决阻塞发现时发布 `workflow/merge-gate` 成功。
5. 维护者在 GitHub 查看 PR、检查和证据后决定合并。试点 main 要求当前 CI、来自指定 App 的门禁、最新基线和一个人工批准。

可用命令包括 `/ai status`、`pause`、`resume`、`cancel`、`takeover`、`replan`、`review deep`、`reject`、`defer`。未授权或机器人评论不能执行维护者命令。配置变化会使旧验证失效；重新规划不会重置预算。

控制台采用参考 ARTEX 的浅色分组布局：全局侧栏保留工作流列表，选中工作流后进入独立详情页，通过概况、开发任务、审查与发现、证据页签切换。概况页顶部以 Dev Team 与 Review Team 两块独立区域显示架构和当前进度，用候选 PR 交付及 Finding／修复反馈连线连接两队；维护者和共同控制内核位于团队外。流程图按内容区域宽度切换双队并排或上下排列，限制最大尺寸；移动端保留可读字号并允许图内横向浏览。图内显示当前阶段、控制状态及澄清/修复/人工覆核分支，过期审查和旧门禁不显示为本轮通过。证据按 20 条分页并支持筛选，报告默认折叠；模型、预算与执行、GitHub 设置分开。Hash 路由支持刷新、返回和直达详情，状态轮询保留当前输入与筛选。页面显示运行阶段和控制状态、Dev/Review 任务、验收条件、发现、证据正文、事件审计、Token 和调用用量。GitHub 门禁与本地评审结论分别显示；门禁 SHA 不匹配当前 HEAD 时明确标记旧结果。

## 验证记录与范围

- 本机 Fake 闭环和真实 GitHub App Fake 闭环已到 `AWAITING_MAINTAINER/PASS`，完成实际沙箱命令、Git 数据提交、PR、测试合并快照和 Check Run。试点 Fake PR 为 [#2](https://github.com/ycychome/ai-workflow-pilot/pull/2)，随后取消以释放仓库执行名额，取消会撤销门禁。
- 真实 Responses 连通性已成功验证。首次真实任务 [Issue #3](https://github.com/ycychome/ai-workflow-pilot/issues/3) 产生 [PR #4](https://github.com/ycychome/ai-workflow-pilot/pull/4)，暴露非代码任务产物判定问题后已取消。修复增加 `code/analysis/verification` 类型、只读证据产物和计入原预算的瞬态连接重试。
- 修复后 [Issue #5](https://github.com/ycychome/ai-workflow-pilot/issues/5) → [PR #6](https://github.com/ycychome/ai-workflow-pilot/pull/6) 已完成真实 Responses Dev、两个独立 Reviewer 和 Skeptic。11 项义务全部有证据，进入 `AWAITING_MAINTAINER/PASS`。HEAD 为 `f0dfa59c0019b8430282ff7cc2b94796625e054e`，验证的测试合并提交为 `bc5078014bbfb101c96c074e36b4f80835c9ccb1`；App Check Run `114855222944` 在该 HEAD 上已确认 `completed/success`。源码 diff 只有 Add 的一行修复，GitHub CI 成功；尚未人工合并。
- Go 检查、编译、前端语法检查和关键实际闭环用于此次功能交付。按本次用户要求没有开展架构文档中的完整 T01–T28 故障矩阵、10任务模型基准或同预算审查效果对照；这些未标为通过。本交付不声称完成 Alpha 有效性评估或适合任意不可信生产代码。

本次调试累计使用 174 次模型请求，包含之前的失败与重审，不能作为单个 Add 修复的性能基准。用量未重置；本机调试单 Issue 上限为 200、每日 500。已修复只读目录范围、模型报告校验反馈、快照重复下载，以及 Skeptic 无法读取证据正文的问题。预算或传输错误后的恢复，只有当前身份完全匹配、可信验证通过、Reviewer 全部完成且 Skeptic 未完成时才复用该检查点；代码、基线、规格或策略变化仍整轮重审。所有过期 Worker/Outbox 继续由 generation 和租约隔离，Skeptic 未完成不会 PASS。

界面布局参考 [ARTEX 在线演示](https://artex-demo.vercel.app/) 的分组导航与独立页面组织，采用本项目原有实现，没有复制其 AGPL 源码。
