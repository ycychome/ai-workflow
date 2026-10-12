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

1. 在试点仓库创建明确需求 Issue。新 Issue 进入 `INTAKE`，不会仅因公网用户发帖而消耗模型预算。
2. 维护者评论 `/ai accept`，或在已登录控制台接受，启动 Triage。
3. 查看提议 TaskSpec 的验收条件，再次 `/ai accept` 批准规格。开发任务按依赖和文件冲突调度，产生主要 PR。
4. 可信验证和独立 Reviewer/Skeptic 对当前 base/head/spec/policy 身份记录证据。只有覆盖完整、验证成功且没有未解决阻塞发现时发布 `workflow/merge-gate` 成功。
5. 维护者在 GitHub 查看 PR、检查和证据后决定合并。试点 main 要求当前 CI、来自指定 App 的门禁、最新基线和一个人工批准。

可用命令包括 `/ai status`、`pause`、`resume`、`cancel`、`takeover`、`replan`、`review deep`、`reject`、`defer`。未授权或机器人评论不能执行维护者命令。配置变化会使旧验证失效；重新规划不会重置预算。

控制台显示运行阶段和控制状态、Dev/Review 任务、验收条件、发现、证据正文、事件审计、Token 和调用用量。GitHub 门禁与本地评审结论分别显示；门禁 SHA 不匹配当前 HEAD 时明确标记旧结果。

## 验证记录与范围

- 本机 Fake 闭环和真实 GitHub App Fake 闭环已到 `AWAITING_MAINTAINER/PASS`，完成实际沙箱命令、Git 数据提交、PR、测试合并快照和 Check Run。试点 Fake PR 为 [#2](https://github.com/ycychome/ai-workflow-pilot/pull/2)，随后取消以释放仓库执行名额，取消会撤销门禁。
- 真实 Responses 连通性已成功验证。首次真实任务 [Issue #3](https://github.com/ycychome/ai-workflow-pilot/issues/3) 产生 [PR #4](https://github.com/ycychome/ai-workflow-pilot/pull/4)，暴露非代码任务产物判定问题后已取消。修复增加 `code/analysis/verification` 类型、只读证据产物和计入原预算的瞬态连接重试。
- 修复后使用同样需求 [Issue #5](https://github.com/ycychome/ai-workflow-pilot/issues/5) 重新运行真实 Dev/Review，最终结果在交付时补充。
- Go 检查、编译、前端语法检查和关键实际闭环用于此次功能交付。按本次用户要求没有开展架构文档中的完整 T01–T28 故障矩阵、10任务模型基准或同预算审查效果对照；这些未标为通过。本交付不声称完成 Alpha 有效性评估或适合任意不可信生产代码。
