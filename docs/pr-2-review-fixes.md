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

旧版仅含正文的来源摘要由一次性迁移绑定到此前保存的 Issue 标题，保留既有 TaskSpec Digest 和 ReviewKey；迁移推进执行 Version，防止滚动升级中的旧 Worker 覆盖来源记录。重复启动不重复迁移。普通评论作为后续上下文保存；需求行为变更使用 Issue 标题/正文编辑或既有重规划命令。

仅目标基线 SHA 移动时，会撤销旧门禁并自动进入重新验证；外部 HEAD/分支或权限身份变化保留暂停/接管规则。GitHub 的 PR 读取与 CheckRun 写入没有跨接口原子事务，因此同时采用写前/写后身份检查、发现漂移撤销成功及既有 strict 分支保护。

验证通过：

- `go test ./...`
- `go test -race -count=1 ./...`
- `go vet ./...`、gofmt、前端 JavaScript 语法及 diff 检查
- 真实 Docker 沙箱回归 `TestCollectChangesIncludesVCSConfigFilesDocker`
- 六项隔离 PostgreSQL 顶层集成用例，包含新增的 gate/Webhook HTTP 模拟、评论/标题以及控制状态用例；临时 schema 清理成功

数据库集成用例只在 `AI_WORKFLOW_FORCE_INTEGRATION=1` 的显式受控环境运行，使用独立临时 schema；没有启动 Engine.Run、调用真实模型或向真实 PR 发布门禁。Docker 回归需 `AI_WORKFLOW_FORCE_DOCKER=1` 和预建 runner 镜像。未执行完整 T01–T28 故障矩阵或生产合并验收。
