# PR #2 审查修复

针对审查提交 `9546209381330aecc9c0c5176cbf0bbc476438c2` 的六项发现。

| 发现 | 修复 | 回归证据 |
| --- | --- | --- |
| P1 沙箱误删普通 VCS 配置 | 导出、列目录及搜索仅排除根目录/嵌套 `.git`；保留 `.gitignore`、`.gitattributes` | 真实 Docker 未修改工作区返回空变更；两文件修改、新增嵌套 `.gitignore` 正确收集；300 MiB 稀疏 `.git` 元数据未进入归档 |
| P1 旧基线发布成功门禁 | 校验当前 ReviewKey/证据及远端 PR 状态、仓库、base/head/ref；写前再次读取，写后复核并撤销漂移的成功检查 | 模拟 GitHub + 隔离 PostgreSQL：正确身份成功；基线、HEAD、分支、仓库、关闭、Draft、缺失身份及错误 ReviewKey 被拒绝；写后漂移成功检查转为 failure |
| P2 自身提交 Webhook 错误暂停 | 在途 publish 必须匹配 action、分支、父提交、消息及全部树变更；只延后精确匹配的回执，不能凭存在在途写入忽略外部 HEAD | 更新已有 PR（父提交不同于 base）的提前 Webhook 保持代次/ACTIVE；发布回执仍将任务标为 INTEGRATED；外部 HEAD 即使 publish 在途也暂停 |
| P2 普通评论丢执行结果 | 讨论属于元数据，不推进执行 Version；Worker 保存时原子保留最新讨论，其他 Version/generation/lease 校验保留 | 普通/机器人评论及评论 ID 去重后，原 Worker 成功保存任务与全部讨论；真正暂停后旧 Worker 仍被拒绝 |
| P2 标题变更遗漏 | 新需求来源摘要同时绑定归一化标题和正文；编辑 Webhook、周期同步、审批、强制接受、恢复使用同一身份规则 | 仅标题从 ascending 改为 descending 即递增代次并作废旧审查；旧规格保持而旧来源不能接受/恢复；周期同步同样检测 |
| P2 配置覆盖取消/接管 | 配置及策略失效只将 ACTIVE 转为 PAUSING，保留其他控制状态；未就绪配置不放行 WAITING_CONFIGURATION | model/runner/repository 变化及 Invalidate 的隔离 PostgreSQL 用例覆盖取消、取消中、接管、暂停等状态 |

当前使用保留语义空白的 `issue-v3` 来源摘要，仅统一 CRLF 行尾。一次性迁移会作废旧 v1/v2 来源的执行与门禁，保留已接受规格供审计，并要求重新分析和批准；不会把旧的有损摘要直接重绑定为有效批准。重复启动不重复迁移，人工暂停、取消和接管状态保持。普通评论作为后续上下文保存；需求行为变更使用 Issue 标题/正文编辑或既有重规划命令。

仅目标基线 SHA 移动时，会撤销旧门禁并自动进入重新验证；外部 HEAD/分支或权限身份变化保留暂停/接管规则。GitHub 的 PR 读取与 CheckRun 写入没有跨接口原子事务，因此同时采用写前/写后身份检查、发现漂移撤销成功及既有 strict 分支保护。

验证通过：

- `go test ./...`
- `go test -race -count=1 ./...`
- `go vet ./...`、gofmt、前端 JavaScript 语法及 diff 检查
- 真实 Docker 沙箱回归 `TestCollectChangesIncludesVCSConfigFilesDocker`
- 六项隔离 PostgreSQL 顶层集成用例，包含新增的 gate/Webhook HTTP 模拟、评论/标题以及控制状态用例；临时 schema 清理成功

数据库集成用例只在 `AI_WORKFLOW_FORCE_INTEGRATION=1` 的显式受控环境运行，使用独立临时 schema；没有启动 Engine.Run、调用真实模型或向真实 PR 发布门禁。Docker 回归需 `AI_WORKFLOW_FORCE_DOCKER=1` 和预建 runner 镜像。未执行完整 T01–T28 故障矩阵或生产合并验收。

## 后续复核修复与容量限制

以下修复覆盖后续审查中新增和未完整解决的问题：

- Git commit API 按真实顶层响应解析；Draft 强制接受在 ready 回执成功后幂等创建门禁；首次 PR 创建前的 base 移动可撤销旧任务并重新规划。
- 后台处理枚举全部工作流，200 条限制仅用于界面；Issue 摘要保留缩进和字面空格；配置、对账、Issue/PR 事件及恢复操作使用当前版本与世代，避免覆盖更新的批准或人工接管。
- 模型报告执行本地枚举校验；失败或超时工具记录不能证明成功；降低发现严重性、反驳或解决发现需要独立有效反证。读取已有证据只返回原 ID，不生成新的独立证明；截断预览仍返回合法 JSON。
- 单次 Outbox 发布最多 60 秒，另用 10 秒保存不确定结果；发布重试必须匹配精确消息、父提交及完整树差异，额外受保护文件不能被采纳。
- Reviewer、Skeptic 和补充测试使用机械验证对应的合并快照，核对 commit/tree/base/head；开发阶段差异工具仍使用开发 HEAD。GitHub 归档下载有 128 MiB 上限。
- 工作区会话始终使用创建它的 manager。每个工作区默认使用 512 MiB、32,768 inode 的受限 tmpfs 卷，由 keeper 保持跨命令数据；实际文件系统容量、类型和身份均检查，keeper 停止或重启后拒绝使用旧工作区。容量可由 `runner.workspace_bytes` 调整，变更会作废既有验证。
- 清理有 20 秒期限，失败保留句柄供重试。归档导出超过 256 MiB 明确报错；修复缓冲区的 `io.Copy` 快速路径绕过容量限制，防止截断归档被误判为文件删除。

最终本地回归包含全量 `go test -race -count=1 ./...`（开启隔离 PostgreSQL 和真实 Docker 测试）、`go vet`、gofmt、前端语法和 diff 检查。16 MiB 测试卷验证了多文件累计写满、inode 耗尽、跨命令持久性、释放空间后恢复、初始化失败清理、keeper 停止/重启、文件系统身份变化与命令超时；没有对宿主磁盘执行耗尽试验。容量变更和 Settings 保存另有真实 PostgreSQL 回归。部署约束和内存成本见 [运行手册](runbook.md#workspace-capacity-and-cleanup)。
