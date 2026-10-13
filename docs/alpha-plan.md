# 从 MVP 到完整 v0.4 与 M4 有限 Alpha

> 2026-10-13 接续状态：用户在新会话中要求完成 PR #3 验收，已恢复验收准备；新测试仓库的适配器与恢复身份接线通过回归，用户改用个人测试仓库，App 安装待完成。实际结果见 [集中验收记录](alpha-acceptance.md#2026-10-13-pr-3-接续验收)。下方收尾决定描述此前提交 PR 时的范围，不限制本次已授权的接续验收。

> 本轮收尾决定：用户指定后续真实联调目标为 `ycychome/test-repo-for-ai-test`，随后要求结束测试并提交 PR。本计划中的真实试验与人工验收保留为未完成项；旧 pilot 重置工具仅保留原目标范围，不在本轮执行。当前证据见 [集中验收记录](alpha-acceptance.md)。

本计划以当前 `codex/dynamic-agents-alpha` 工作区源码、`docs/plan/Architecture_v0.4_Reviewed.md` 与 `docs/alpha-implementation.md` 为依据。目标是可恢复、可核验、支持动态双团队协作的 GitHub 开发工作流，最后由维护者合并。不是把更多固定角色接到串行脚本上，也不能把历史 MVP 测试结果作为本轮完成证据。

用户要求的实施顺序保持不变：**先完成全部实现，再集中测试和正式验收**。本计划中的测试是待执行合同；新增代码、测试文件、夹具和文档存在，不表示已经通过验收。实施阶段已结束，当前进入统一测试；实际结果以 `docs/alpha-acceptance.md` 为准，真实 GitHub 与模型评估单独记录。

## 完整产品边界与主链路

入口为已授权仓库的 Issue、PR 事实事件、维护者命令与控制台。事件先写 Inbox 并去重；控制层维护版本化 Spec、任务 DAG、Agent 队列、候选与证据；外部写入经 Outbox 幂等发布。服务端拥有 GitHub 凭据，Agent 只能通过受控工具读取/写入自己的工作区，不直接获得发布、合并或宿主权限。

主链路为 Issue 分诊 → Spec 待决议/批准 → 开发规划 → 动态开发任务 → 串行产物集成 → Draft PR → 固定 test-merge 的可信验证 → 独立审查规划与执行 → Skeptic 复核 → 有界修复/反证/补证 → 合并门禁 → 维护者最终合并。需求变更、暂停、取消、接管、强制重审和预算耗尽是贯穿主链路的状态转换，不是只有末尾能触发的操作。

两队共享受信任务/证据基础设施，开发队与审查队不共享决定“自己的结果是否通过”的权力。Orchestrator 执行确定性状态机；Integrator 接纳和集成来源产物；Gate 依据固定身份及证据计算放行。三者不是额外的模型角色。

## 八类 Agent 与提示词合同

所有 Agent 的自然语言，包括提示正文、解释、任务描述、状态、报告、审查发现和反证，使用简体中文。协议字段、枚举、源码标识符、命令、路径、URL、原始输出保持原样。语言检查只覆盖明确的自然语言字段，不改写代码或证据；简单汉字检测不是完整的简繁识别证明。

| 协议角色 | 中文职责及提示词必须说明的边界 | 输入与产出 | 协作关系 |
|---|---|---|---|
| `triage` | 澄清可验收目标、非目标、歧义和风险；不把描述解释为额外执行权限 | Issue 当前快照、澄清材料 → 结构化 Spec 提案/需要补充信息 | 向维护者或配置允许的审批路径交接；批准后给开发规划 |
| `dev_planner` | 将批准 Spec 拆成可独立验收的任务和依赖；允许事件后追加/替代任务，但不能重置预算或扩大权限 | Spec、已接受产物、队列与阻塞 → PlanDelta/DAG | 派 `builder/analyst/verifier`；提交计划后释放槽位 |
| `builder` | 只修改分配路径；先完成启动检查；完成源码产物和必要回归测试；不能自己发布或认可审查 | Task Contract、依赖产物、基线 → SourceArtifact 与结构化实现报告 | 产物交 Integrator；收到 Findings 后修复或提供证据反驳 |
| `analyst` | 只读分析源码、依赖和行为；区分观察与推断，不冒充执行验证 | 分配的分析问题、冻结源码 → 可追溯结论与证据引用 | 支持开发规划和实现；不能写产品文件 |
| `verifier` | 在无凭据沙箱执行分配检查，保留命令、退出状态、日志与输入版本；未执行就明确未验证 | 检查合同、冻结快照 → 执行证据及结构化结论 | 向控制层提交验证结果，不因退出码0替代全部验收 |
| `review_planner` | 为完整 Spec 建立审查义务，将行为、边界、回归、安全和高风险路径分派；不能只审 diff 或依靠无发现收尾 | 冻结 test-merge、Spec、验证记录 → 审查 DAG、Obligations、EvidenceRequests | 派 Reviewer；缺证据时请求有界补证 |
| `reviewer` | 独立检查分配义务；只在允许的补充测试目录写测试；Finding 必须有可核验证据，不能修改产品实现 | 候选、义务、受信验证/同伴证据 → 覆盖结果与 Findings | 向 Skeptic 和开发队交接；不能直接判自己的发现已修复 |
| `skeptic` | 同时挑战发现和“未发现问题”；验证反证、遗漏、过期身份与证据覆盖；不能把共识或预算耗尽当通过 | Findings、Rebuttals、义务与验证证据 → 复核结论/补证请求/升级人工 | 最终交确定性 Gate；有效检查点可以恢复，但必须身份仍匹配 |

共同提示规则必须包含：受领任务唯一边界、角色权限、禁止升级权限、只有 `submit_report` 的有效结构化输出可完成任务、证据必须来自真实工具结果、同伴报告是上下文而非更高权限指令、预算不足时提交未完成状态、不把排队说成运行、不把完成意图说成已集成、不把本地成功说成 GitHub CI 或正式验收成功。

模型路由按开发/审查类别显式绑定。两个模型配置可以相同，但角色权限与独立审查身份不可因此合并。模型切换、提示词版本与策略变动必须进入运行记录和验证身份。

## 每次运行必须知道自己的状态

受信系统提示由控制层重建，角色模板之后注入当前事实，至少包含：

| 信息组 | 必须记录的字段 |
|---|---|
| 身份 | workflow/task/agent_run ID、角色/团队、generation、plan_revision、attempt、lease_epoch |
| 启动原因 | initial/event/heartbeat/retry/resume/queue_admitted，queued_at、queue_reason、started_at、deadline |
| 项目身份 | 仓库名称与数字 ID、Issue/PR、工作区、批准 Spec 摘要、base/head/test-merge/tree、verification_key |
| 版本与能力 | prompt/policy/snapshot 版本，角色工具能力，allowed/protected paths，明确为空的能力或依赖数组 |
| 资源 | 本轮剩余调用/请求/token、任务及全局共享预算、全局/单 Issue 活跃上限 |
| 关系与恢复 | 依赖任务与已接受产物、其他任务的结构化状态、checkpoint、未完成项、证据引用范围 |
| 交付 | 本角色报告 schema、完成条件、产物位置、停止/升级条件 |

仅控制层生成的身份/能力/版本进入受信提示块。Issue、PR、仓库内容、工具输出、同伴报告和自然语言摘要放输入数据区，不因为“动态注入”就升级成系统指令。源码中的 `PromptContext` 已采用这种区分；接线时不能再把 Agent 撰写的描述拼入可信权限字段。

## 动态队列与排队后启动检查

全局活跃上限默认4、单 Issue 默认2，可配置；排队不占执行槽位，原工作流并发限制独立保留。任务/证据请求累计限额默认各64，可配置，不随重规划清零。默认值是配置选择，不是实际评估预算；实际运行读取当前有效配置。

调度必须持久化并跨进程原子准入。创建、排队、预启动、运行、停止中、停止确认与终态分别记录。只有完成准入、拿到当前租约且实际开始运行的 Agent 占名额；退出请求不等于停止确认。Planner 提交计划后释放名额，由事实事件重新唤醒，不占槽轮询等待子任务。

从队列获得槽位后，必须重新读取 workflow/task、当前 generation/plan revision、租约、Spec/策略版本、依赖结果、最新候选和剩余预算，再创建工作区及提示。不得直接使用入队时的陈旧 prompt。

启动检查分为确定性核验与 Agent 项目检查：控制层先拒绝失效租约/权限/依赖；Agent 读取当前 `workflow_state`、项目布局、适用说明和分配的项目证据，再提交 `submit_preflight`。服务器核验这些证据确属本次运行与当前快照，接受之前不能写入、执行命令或提交完成报告。首次启动、恢复、重试与排队后启动都重新检查。依赖被修订、base/head 或策略漂移，应重新规划/排队并使旧证据失效，不能机械续跑旧计划。

## 开发产物、集成与审查闭环

每个代码任务产出 SourceArtifact：来源任务/运行/租约、基线树、变更路径与内容摘要、任务合同与报告、验证证据。完成报告本身不等于代码可供下游使用；Integrator 在无凭据工作区串行验证来源身份和路径边界、集成补丁并记录 IntegrationRecord。冲突产生明确修复任务；不自动接受过期结果或越界补丁。下游只接收已接受、已集成且与依赖版本匹配的产物。

已新增 `IntegrationInput{Task, Artifact, Changes}` 与 `Engine.IntegrateSourceArtifacts(ctx,w,inputs)`。调用方先持有效工作流租约保存来源、证据、VALIDATED 任务及 `PendingIntegration` 的 PREPARING 记录；稳定 record ID 同时是专用资源 run label。集成器检查 Agent 已停止、已接受 attempt 的有效 lease epoch、证据摘要和实际变更，按 artifact ID 串行应用到独立父提交快照。每个来源树独立计算，总树按 Git 对象格式生成，并与沙箱 CollectChanges/Snapshot 双向核对。普通文件和执行位可用；符号链接、子模块、不支持模式和文件/目录结构替换会明确拒绝。

跨来源同路径或文件/目录冲突返回 `IntegrationConflict` 的 kind、paths、task_ids，交规划者修复。结果 VALIDATED 仅说明本地树一致；publisher 必须通过 Git Data API 精确对账 parent/tree 后再记录 CommitSHA。所有 Create 结果均有 StopRun 清理确认；失败返回 CLEANUP_PENDING。恢复时只有新持有工作流租约的控制器才能清理该专用 label，不能取消其他有效控制器；确认停止后将记录恢复 PREPARING，通过 `LoadIntegrationInputs` 从哈希校验的持久证据重建输入。失败或不完整记录不能发布。


审查需覆盖批准 Spec 的全部验收义务。EvidenceRequest 绑定请求者、义务、冻结候选、所需证据、预算和状态；不能无限递归或绕过并发预算。Finding 至少包含位置、问题、预期/实际、前置条件、严重度、证据类型与引用。模型猜测、源码推断、工具验证不可混为同一强度。

开发队可修复、提出可验证反证或请求澄清。审查队独立接受/拒绝反证，Skeptic 检查遗漏和反证质量。不能只因开发报告“已修复”关闭 Finding；需在新候选上复测。人工 waiver 必须保留精确 Finding、理由、批准者、适用候选/Spec/策略版本；不伪装为自动验证通过。修复轮数和补证预算有硬限，耗尽转人工，不自动 PASS。

## 高风险路径与外部 CI 身份

现有路径策略已将 `.git/**`、`.github/workflows/**` 加入保护范围；高风险分类是额外审查义务，不能替代禁止写入规则。新增 `ClassifyHighRiskPaths` 为工作流/本地 Action、迁移、认证/租户、控制策略与依赖文件提供默认分类。接线时必须覆盖修改、删除、重命名前后路径，并把仓库特有敏感路径补入受信配置；通用文件名分类不声称识别全部安全敏感代码。

外部 GitHub CI 不能仅凭检查名称、PR 评论、绿色图标、run.head_sha 或模型提供的 artifact JSON 放行。新增独立 API 读取工作流、run/attempt、文件 blob、当前 PR test-merge；`FetchTrustedCIRecord` 校验：

- 可信仓库数字 ID 与名称、工作流数字 ID/path/active 状态；固定来源提交和已审查 workflow blob。
- 当前 PR 仍 open，head/base 属于预期同仓库，head/base 与当前验证身份一致；当前 mergeable 和 merge_commit_sha 明确匹配。
- 指定 run ID/attempt 是该 run 最新重试；事件为受支持的 `pull_request`，关联 PR 唯一且完整；状态 completed 且结论 success。
- 当前 base/head/test-merge 中工作流 blob 均等于可信来源；同名工作流或 fork 自行生成的结果不能继承信任。
- test-merge 的两个父提交精确等于当前 base/head，tree 与已冻结快照一致。
- 可信独立执行器的 Ed25519 回执绑定 repo/PR/generation/verification_key/head/base/test-merge/tree/run/attempt/完成时间。签名方必须依据真实 checkout 和测试结果签发，候选代码不得接触私钥或签名接口。

CI 回执并不是为了给模型报告“加签”：签名控制器应与运行候选代码的作业隔离，并锁定工作流依赖和调用链；否则攻击代码仍可能骗取可信签名。当前实现只支持可信同仓库 PR CI，fork、`pull_request_target`、空关联 PR、缺字段、旧 attempt、未知状态一律非 PASS。未来支持 fork 需独立的可信调度/回执合同，不能把这一限制删掉就宣称支持。

`TrustedCIPolicy` 需要配置 repository ID/name、workflow ID/path、来源 commit/blob、公钥和最大记录时效；现有 `config.TrustedCI` 已接入仓库配置，`ci_gate.go` 从候选不可写的控制平面目录读取回执；可信独立执行器的真实签发与远端联调仍需验收。`TrustedCIRecord.Current` 用于本地证据复用前的身份/时效检查，最终门禁发布前还需远端重读；如果刷新失败不能保留旧绿。缺少公钥、回执或匹配运行，保持非 PASS，不降级为只看 success。

建议接线顺序：可信配置 → 当前 workflow 的 CICandidateIdentity → 控制平面获取签名回执 → FetchTrustedCIRecord → 证据落库/审计 → Gate 对当前身份重验 → Outbox 发门禁前再读远端。`VerificationKey` 仍由现有定义管理；新增外部证据必须绑定它，不能让候选回执反向决定当前 key。

## Spec 变更：现状与独立合同

当前源码已经做对的部分：`normalizedIssueSourceHash` 使用 `issue-v3`，只归一 CRLF，保留缩进、字符串与 Markdown 语义空白；`issueEdited` 更新 Issue 快照、清理旧 ProposalSpec、递增 generation、撤销旧门禁并暂停活跃执行，但保留已经批准的 Spec。`accept` 校验当前 SourceHash、完整验收条目和 SpecDigest 后才替换批准 Spec。不能为了新增提案流程重新定义全局 Issue hash，否则会破坏现有身份迁移和空白语义。

已新增 `domain.SpecChangeProposal`、`SpecChangeDecision` 和批准历史。`domain.ProposeSpecChange` 深复制提案，固化先前批准 digest、来源 hash 和内容身份；`domain.DecideSpecChange` 核验不可变内容、当前来源、先前批准版本和唯一决议。`Engine.ProposeWorkflowSpecChange`、`DecideWorkflowSpecChange` 提供持久化入口；publisher 的批准路径也调用同一 domain 合同。提案保持未批准原文，决议追加保存，接受后另生成批准 Spec 和历史记录。

接线必须维持事务/CAS 的 generation、旧批准版本与 Issue 来源检查，并重新检查维护者权限。接受新规格使旧任务、产物及审查授权失效；过期提案、重复决议或 Issue 再次编辑不能覆盖当前批准版本。当前已写实现和针对性测试，尚未执行集中验证，不能视为验收完成。全局 Issue hash 没有因提案合同改变。

自动需求审批配置仍可保留，但决议者应明确为受信策略身份并记录策略版本；高风险变更或用户配置要求人工时走人工决议。`forceaccept` 保留独立人工越过记录，不能写成模型/CI 已通过。接管、暂停和取消不因新提案或定时对账自动恢复。

## 从 MVP 到 M4 的交付与退出条件

| 阶段 | 应交付能力 | 退出证据 |
|---|---|---|
| M0 可行性与控制内核 | Fake Agent、Inbox/Outbox、generation/租约、权限与隔离、唯一门禁投影 | 重复事件、外部响应丢失、取消旧结果、基础 GitHub 阻塞与沙箱验证 |
| M1 最短真实闭环 | Issue→批准 Spec→单 Builder→Draft PR→独立 Reviewer→有界修复→人工合并 | 一条真实需求完整闭环，暂停和进程恢复有效；不宣称完整多 Agent |
| M2 动态双团队 | 八角色目录、持久队列、双层活跃上限、PlanDelta、preflight、DAG与SourceArtifact串行集成 | 依赖版本正确、并行无越权、冲突可修复、过期租约被拒绝、排队不耗执行名额 |
| M3 对抗与全程控制 | 全义务审查、Skeptic、补证、Finding/反证/waiver、不可变 Spec 变更、可信 CI 与高风险合同 | 缺项/超时不通过，误报可反驳，重复重规划不重置预算，状态与身份漂移撤绿 |
| M4 有限 Alpha | 生产型工单夹具、故障矩阵、脱敏审计、部署/备份/恢复资料、真实同预算评估 | 确定性验收全部通过、真实冒烟与人工合并、人工校核标签及三组成本/质量报告 |

v0.4 是完整行为合同，M0–M4 是实现与验收顺序；并非代码文件出现就跳过早期退出条件。当前分支已完成主流程接线，集中测试进行中，远端与真实模型验收仍需独立完成。自动合并、任意仓库开放、恶意多租户服务、无限自主运行不在有限 Alpha 放行范围。

## 正式验收与试点切换

集中执行既有 T01–T28，并扩展动态 Agent、签名 CI 和 Spec 提案的边界测试。至少覆盖：Webhook 重投与乱序；外部写成功但响应丢失；租约争抢/过期；暂停/取消在途工具和发布；进程重启；没有产物的成功退出；依赖与集成冲突；base/head/test-merge 漂移；同 SHA 重审与旧 Check 撤销；空报告/无证据/预算耗尽；反证与误报；Issue 提示注入；未授权命令；私钥/宿主 socket 隔离；队列重规划共享预算；候选删测试/改 CI；Issue/PR 关闭、重开和 Draft；旧 attempt/错误 repo/workflow/source/签名回执。

测试层次顺序：静态与单元 → 专用 PG 集成 → 无网络无凭据容器 → 生产型夹具与可信判定 → GitHub App/Checks/Outbox 联调 → 中文控制台和队列可见性 → 真实任务与人工合并。每层保留命令、版本、退出结果和局限，不能用离线单元测试替代远端验收。

保留原 calculator pilot 供旧 Fake 回归。新 `fixtures/issuedesk` 提供 Go/PG/中文Web工单、十个可独立重建缺陷与十对修复候选。`eval/manifest.json` 冻结题面、源码、判定与标签；隐藏测试、变体和标签只在可信控制侧，Agent 只挂载候选目录。A=单Builder，B=Builder+单Reviewer，C=完整双团队；各题各组三次共九十次，固定模型/工具/总预算。矩阵读取运行器有效预算配置；所有生成标签先待人工校核，真实评估前不能算已确认真值。缺失结果、基础设施失败和预算耗尽不能算成功，零价格是未知而非免费。

试点切换仍使用已授权的 `ycychome/ai-workflow-pilot` 仓库/App身份。先停稳旧流程、检查在途 Outbox、备份历史和数据库、核验保护规则与恢复路径，再按已授权范围重置代码历史、关闭旧对象、清理旧分支。旧代际不能在切换后复写新试点。备份、规则核验、真实调用、人工标注与最终合并均需要各自真实执行证据，不能由计划或 fixture 冒充。

## ARTEX 借鉴与来源边界

只参考 Hinln/ARTEX 备份固定提交 `f3e3f54b6c93916388a0a3dc6a439893a9abe37a`。仓库自述源于 Autumn-27/ARTEX v0.3.15；该备份 main 含后续修改，不能当原作者官方现状或声称此提交等于原 v0.3.15。以下是本轮一手源码可支持的五项借鉴：

1. [Planner 只派意图、事件唤醒及跨轮待办](https://github.com/Hinln/ARTEX/blob/f3e3f54b6c93916388a0a3dc6a439893a9abe37a/agent/planner.go#L295-L465)：转为双队规划，提交后释放槽位；不照搬“必须继续造意图”的无限探索规则。
2. [Worker 单意图和只读全局态势](https://github.com/Hinln/ARTEX/blob/f3e3f54b6c93916388a0a3dc6a439893a9abe37a/agent/worker.go#L316-L421)：保留任务不变量与全局理解的区别；本项目增加团队权限隔离和只接受结构化报告。
3. [可配置正文与代码拥有的提示尾部](https://github.com/Hinln/ARTEX/blob/f3e3f54b6c93916388a0a3dc6a439893a9abe37a/agent/prompt.go#L9-L64)：角色正文版本化，权限/语言/证据合同由框架组装；外部文本始终是不可信输入。
4. [任务准入队列与首次启动/恢复模式](https://github.com/Hinln/ARTEX/blob/f3e3f54b6c93916388a0a3dc6a439893a9abe37a/server/goals.go#L96-L250)：借鉴持久准入；本项目排队后重新检查项目和上下文，所有恢复同样受全局与单 Issue 硬预算控制。
5. [Frontier 原子领取](https://github.com/Hinln/ARTEX/blob/f3e3f54b6c93916388a0a3dc6a439893a9abe37a/db/exploration.go#L1135-L1180)：借鉴持久工作集与原子领取，扩展为 generation/lease epoch/依赖版本核验。

不移植渗透攻击业务提示、绕过策略、PermissionModeBypass、绕过池容量的人为续聊、默认认为任务必须持续探索等规则。ARTEX 默认提示多为中文，但未发现全角色统一“简体中文”合同；本项目的语言合同与运行状态字段是独立设计。

备份 [LICENSE](https://github.com/Hinln/ARTEX/blob/f3e3f54b6c93916388a0a3dc6a439893a9abe37a/LICENSE) 为 AGPL-3.0。当前仅借鉴抽象模式，提示词、服务和实现独立撰写；若将来复制或改编具体代码/提示文本，必须记录源仓库、完整 commit、文件与许可证并另行审查义务，不把备份维护者误标为原作者。

外部 CI 字段与事件语义依据 GitHub 的[工作流运行 REST API](https://docs.github.com/en/rest/actions/workflow-runs)及[pull_request 事件文档](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#pull_request)。这些文档说明运行/重试与合并引用的区别；本项目仍以受信执行回执证明实际测试内容，不从 API 名称推断已执行的 checkout。
