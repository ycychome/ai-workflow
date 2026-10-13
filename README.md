# GitHub 原生多 Agent 工作流

以 GitHub Issue 驱动需求分诊、开发、独立审查与人工合并。Go 控制器负责持久状态、调度、无凭据沙箱、可信验证和 GitHub 写入；PostgreSQL 保存 Inbox、Outbox、任务、租约、证据及审计记录。

本分支已完成 v0.4 有限 Alpha 的主要实现与本地集中验收，正在提交 PR。**真实模型、远端联调和人工验收仍未完成**，当前证据及剩余工作见[验收记录](docs/alpha-acceptance.md)。历史 MVP 的真实冒烟记录只说明当时版本的能力，不作为此分支的验收结果。

## 动态团队与提示词

开发规划者根据已批准规格动态安排 Builder、Analyst、Verifier；审查规划者分配独立 Reviewer，Skeptic 复核发现、反证和覆盖遗漏。Planner 提交计划后释放名额，完成事件到来后重新检查是否需要补充任务。

- `runner.agent_concurrency` 限制全局同时运行的 Agent，默认 4。
- `runner.workflow_agent_concurrency` 限制单 Issue 同时运行的 Agent，默认 2。
- 排队不占活跃名额，不消耗模型调用或执行时长；停止中的运行在确认清理完成前仍占名额。
- 队列持久化，并在跨进程事务中准入。获得名额后重新读取项目状态、依赖、候选和预算，再创建工作区。
- Agent 必须先读取当前流程与项目证据，提交启动检查，才可写文件、运行命令或提交报告。
- 所有 Agent 的提示正文与自然语言报告使用简体中文。协议字段、源码、命令、路径和原始日志保持原样。

八类角色、相互职责、提示词上下文和 ARTEX 参考来源见[完整计划](docs/alpha-plan.md)。累计任务与补证请求默认各 64，可配置；重规划、重启与按日额度恢复不重置累计预算。原工作流并发配置独立保留，范围为 1–4。

## 开发与审查链路

Issue → 需求分诊与规格决议 → 动态开发 DAG → 来源产物 → 无凭据串行集成 → Draft PR → 固定 test-merge 验证 → 独立审查 → Skeptic → 有界修复、反证或补证 → 合并门禁 → 维护者合并。

集成器校验任务、运行租约、允许路径、来源证据、父提交及实际树。发布器通过 Outbox 发布精确父提交和树，不接收 Agent 提供的任意 GitHub 操作。验证、审查、风险授权、豁免和 CI 回执均绑定当前候选身份；候选或 CI 身份失效会撤销旧门禁。`forceaccept` 保留明确人工越过记录，不记作自动验证通过。控制器不自动合并。

自动需求审批和两个可选人工关卡仍受配置控制。当日调用额度耗尽进入 `WAITING_DAILY_BUDGET` 并保留检查点；单 Issue 总额度耗尽是实际失败。`WAITING_CI` 等待独立可信回执，不反复执行测试。可信 CI 的来源固定、签名执行及恢复规则见[CI 证据说明](docs/ci-evidence.md)。

## 本地运行

已有 MVP 的运行入口为 `scripts/up.sh`，默认 webhook 端口 8080、控制台端口 8081。控制台登录令牌保存在本地 `secrets/dashboard-token`，不由服务打印；设置页面保存 Responses 地址、开发／审查模型与令牌，浏览器不回读已保存令牌。GitHub App 配置、诊断及隧道使用见[运行手册](docs/runbook.md)。

Alpha 开发与验收应使用[隔离部署说明](docs/deployment-alpha.md)：独立 Compose 项目、数据库、数据目录及 18080／18081 端口。Linux 可使用 Docker Engine；macOS 可使用 Colima。初始化、镜像预热、启动、备份和恢复都有独立脚本，先阅读说明再操作既有服务。

工作区默认容量为 `536870912` 字节（512 MiB），覆盖源码、Git 元数据及工作区构建缓存。Docker 使用受限 tmpfs 卷，不支持配额时拒绝创建，不回退至无界磁盘。可选 PostgreSQL 服务使用每次运行独占的无网络 Unix socket、合成凭据和独立数据，不挂载控制数据库。

## 试点与评估

`fixtures/pilot` 保留原始计算器 Fake 回归，其初始 `Add` 测试故意失败。`fixtures/issuedesk` 提供 Go／PostgreSQL／中文 Web 工单服务，包含权限、租户、状态流转、幂等与并发等验收场景。

[评估协议](docs/alpha-fixture.md)定义十项真实冒烟、三配置 × 十题 × 三次的九十项同预算对照，以及二十个缺陷／修复候选。真实隐藏测试在独立可信副本中执行，Agent 工作区不包含判定素材；公开测试通过与流程判定不能代替隐藏测试真值。人工标签校核及最终人工合并分别记录。

后续真实联调使用用户指定的 `ycychome/test-repo-for-ai-test`。本轮按要求结束测试并提交 PR，尚未在该仓库运行试验；评估适配器的新仓库身份接线仍待完成。旧 `ycychome/ai-workflow-pilot` 的[重置工具](docs/pilot-reset.md)保留原目标边界，不把旧仓库的历史重置授权迁移到新仓库。

## 文档

- [完整 Alpha 计划](docs/alpha-plan.md)与[实施／验收状态](docs/alpha-implementation.md)
- [架构 v0.4](docs/plan/Architecture_v0.4_Reviewed.md)与[原始领域合同](contracts/README.md)
- [隔离部署](docs/deployment-alpha.md)、[可信 CI](docs/ci-evidence.md)、[试点评估](docs/alpha-fixture.md)
- [历史 MVP 交付范围](docs/mvp-delivery.md)与[运行手册](docs/runbook.md)
