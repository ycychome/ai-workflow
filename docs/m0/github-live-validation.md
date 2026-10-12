# M0 · GitHub 原生门禁实测

这份记录是待执行步骤，不是通过报告。需要专用测试仓库、GitHub App 安装，以及有权管理目标分支规则的 Maintainer。不要向聊天粘贴私钥或安装令牌。

## 前置配置

按根目录 README 接入夹具仓库和控制进程。分支规则要求：

- `workflow/merge-gate` 是 required status check，来源指定为该 GitHub App。
- 启用目标分支必须更新（strict/up-to-date）和至少一个人工审批。
- 检查管理员/角色 bypass 配置和该仓库计划是否支持这些规则。
- 普通 CI 从受信任配置运行；不要以有凭据的 `pull_request_target` 执行候选代码。

记录仓库 ID、App ID、installation ID、目标分支、GitHub 计划、规则截图/导出、规则 ID、运行 SHA 和时间；配置值与测试结果应单独保留。

## 实测顺序

| 场景 | 操作 | 应观察到 |
| --- | --- | --- |
| 首次完整闭环 | 建立 Issue，添加 `ai:m0` | 只创建一个固定 patch Draft PR；验证前 Check 为 in_progress；之后仅完整验证才 success |
| 人工合并条件 | Maintainer 将 PR ready，未审批时查看 mergeability | 系统 success 不能代替人工审批；机器人不自动合并 |
| 取消/暂停 | 在执行中发独立 `/ai pause` 评论 | generation 增加；旧结果不能写 PASS；容器停止与未知写入对账前为 PAUSING |
| 重投 | 在 App Webhook 管理界面重复 redeliver 同一次 delivery | 一个 Workflow，一个效果；处理失败仍能继续 |
| 远端成功但响应丢失 | 通过测试 HTTP 代理对 PR/评论/Check POST 在收到远端成功响应后断开下游连接 | Outbox unknown；先查询 bot 所有权与预期 SHA，再认领；无重复 PR/评论/Check |
| 新 SHA | PASS 后人工 push 修改 greeting 内容 | 当前 SHA 不沿用旧绿灯；重新验证内容错误时 failure |
| 同 SHA 重审 | PASS 后 `/ai pause`；确认失败门禁；`/ai resume` | 同一 SHA 的同名门禁被撤销，再由新 generation 重新验证；旧动作不能改绿；检查是否仍存在历史绿色歧义 |
| 仅 base 更新 | PASS 后改变目标分支 | strict 规则阻止旧基线合并；内部 VerificationKey 失效，重新验证后才可 success |
| 不完整/工具失败 | 使用测试环境让 Docker 启动失败或命令超时 | failure 或仍在执行；不得 neutral/skipped |
| 假同名来源 | 另一个测试 App 发布同名 success | 指定来源规则拒绝该 Check 作为真实门禁 |
| GitHub 特殊通过结论 | 在另建的临时诊断检查中观察 neutral/skipped 规则语义 | 记录 GitHub 原生行为；真实 `workflow/merge-gate` 从不使用这些结论 |
| 未合并关闭 | 关闭 Draft PR但不合并 | 不记录 MERGED；停止旧发布；重新打开不盲目重放旧任务 |
| 人工推送与未知响应 | 在发布响应丢失后手动更新 bot 分支 | 预期 commit 不匹配时停止；不 force overwrite |
| 人工合并 | 所有当前条件满足，Maintainer 合并 | 仅 GitHub merged=true 快照将业务阶段记为 MERGED |

对“响应丢失”测试应使用明确的专用环境代理，并保持证书校验；不要在生产流程注入故障。本地自动用例已覆盖此逻辑，远端实测须单独记录。

## 完成记录模板

每个场景记录：Issue/PR URL、事件 delivery ID、workflow ID、generation/lease_epoch、base/head SHA、VerificationKey、Check run ID 与 App 来源、规则配置、预期、实际、证据路径，以及通过/失败/无法验证。

只有上述关键场景实际通过并审查绕过边界后，才将 `acceptance.md` 的真实 GitHub 退出条件改为已完成。若规则不受当前计划支持或同 SHA 投影存在歧义，维持阻塞并修正适配器/配置，不能用接口存在代替验收。
