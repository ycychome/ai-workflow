# Alpha 集中验收记录

状态：**已验证提交的远端 CI 通过，最新修复完整 CI 复跑中；探针 001 失败保留，探针 002 已完成可信 CI 签发并进入独立审查；完整 Alpha 尚未放行**。旧 pilot 不在本轮操作范围内。以下历史结果来自 `codex/dynamic-agents-alpha` 实施分支，不复用历史 MVP 验收。

## 当前接续状态（2026-10-13）

- **目标身份：** 私有 `zymooll/ai-workflow-alpha-test`，repository ID `1416790756`；GitHub App ID `5285791` 的个人 installation `170932429`，范围为 `only selected repositories` 且仅选中该仓库。此前的组织仓库 `ycychome/test-repo-for-ai-test`（ID `1415305337`）是历史目标。旧 `ycychome/ai-workflow-pilot` 的维护与重置边界未改变。
- **PR #3 CI：** head `436730ad...` 对应 run `38395066796` 的两个 job 均成功。工作树上级的 `acceptance-evidence/ci-run-38395066796-summary.json` 显示 unit 357 passed／139 层隔离 skips，PostgreSQL 428／18，Docker 448／0；T01–T28 全部通过，`uncovered_skips` 为空。原始日志 SHA-256 为 `3ba02b31ad6f70babc484768f2e80c6893de947f10eb137eaa3956ada0ab651a`。GitHub artifact 仍因存储配额上传失败，因此不能将其描述为证据 artifact 已上传。
- **真实探针：** `connection-T01-001` 当前关联个人测试仓库 PR #2，generation 2，累计 Issue 调用 191/200；当日累计 270/500（包含此前 79 次），并未因新库或恢复清零。工作流 BLOCKED：最后一个工具调用时剩余 20 次额度，因此尝试耗尽。Verifier 已发现候选测试 DSN 配置错误及 gofmt 问题，集成尚未修复；`testedSHA` 与 `testedTree` 均为 null。该探针不属于正式冒烟，当前没有 signed receipt、Judge 或完整 gate 证据，不能记为 PASS。
- **运行环境与新探针：** 网络重试后 QUIC 公网隧道恢复，App Webhook 已备份更新；真实投递和同 GUID 去重已核验。运行器 PATH／临时目录说明修复通过真实 Docker 回归。新探针 `connection-T01-002` 对应 PR #5，固定合并提交验证和真实 CI 签发成功，独立审查尚未完成；旧 BLOCKED 探针保留。
- **正式放行仍待：** 十项 smoke、90 项 ABC 对照、20 项 blind review、人类 labels 校核及维护者 merge 均未完成。

远端 CI 通过只证明该 PR 的 CI 矩阵结果，不替代真实模型闭环、候选集成、签名回执、Judge 或人工验收。

## 已取得的本地证据

| 范围 | 当前结果 | 证据目录（工作区上级 `acceptance-evidence/`） |
|---|---|---|
| 最终完整 Go race | 577 个含子用例通过，0 失败，无数据竞争；2 个父进程 helper 跳过，其主用例通过；启用全部 PostgreSQL／Docker 用例 | `final-race-all-001/` |
| Go 单元层 | 325 个含子用例通过；数据库／Docker 用例按层隔离，未将跳过视作其验收通过 | `unit-004/` |
| Store PostgreSQL 与 race | 各 50 个含子用例通过；两个父层跳过项是由主用例启动的子进程 helper | `store-sandbox-20261013/` |
| Sandbox Docker race | 33 个含子用例通过，无跳过；含只读源码、独立补充测试目录、peer 隔离及停止日志保留 | `store-sandbox-20261013/` |
| 架构 T01–T28 | 28 个场景全部通过，无跳过；真实 PostgreSQL／Docker，GitHub 故障由本地 HTTP 服务注入 | `final-all-001/`、最终完整 race |
| CI 身份、等待与恢复 | 70 个单元子用例及 6 项 PostgreSQL／Docker 回归通过；覆盖崩溃幂等计时与真实集成树 | `trust-chain-unit-20261013/`、`trust-chain-pg-20261013/` |
| Python 评估、重置及验收汇总 | 27 项评估／适配器离线测试与 6 项验收汇总回归通过；未作为远端操作证据 | `final-static-001/`、`scripts/test_alpha_acceptance.py` |
| 生产型工单独立判定 | 20 个缺陷／修复候选均符合机械判定；判定器独立模块与进程，候选无法伪造输出；人工标签仍待校核 | `judge-external-migration-001/`、`python-evaluation/judge-adversarial-final.jsonl` |
| 完整 Fake 工作流 | 真实 PostgreSQL／Docker，从分诊至独立审查、Skeptic 和合并门禁；不调用真实模型或 GitHub | `trust-chain-regressions-003/` |
| 中文控制台 | 实际登录、创建演示、动态任务图、运行队列、排队后复查提示；并发上限保存重载后保留；浏览器未报告 JS 错误 | `ui-runtime/` |
| Alpha 运维恢复 | init／prewarm／up、备份、实际修改后的恢复与再次健康检查通过；数据库、配置、文件一致，schema 1–9 恢复 | `alpha-ops-20261013/` |

最终完整命令为 `AI_WORKFLOW_FORCE_INTEGRATION=1 AI_WORKFLOW_FORCE_DOCKER=1 go test -race -count=1 -timeout=20m ./...`，使用独立数据库配置。用时 561 秒；本机 Go 1.27.2，Linux runner 容器。此前 `final-all-001` 的数据库层曾因时钟偏差失败，已修复并在最终全量中通过；原失败记录保留。GitHub Actions 使用 Go 1.24，远端执行结果以 PR 检查为准。

证据保存首次失败和修复后结果，不覆盖原日志。JSON 汇总附日志哈希。隔离环境使用独立 Colima profile `aiwf-alpha-acceptance`、专用 PostgreSQL 和合成凭据，原服务保持运行。临时环境文件含测试凭据，不应提交到 Git。

## 集中测试发现并修复

- 无存储的配置校验初始化空指针；外部访问仍必须经过持久化仓库代际检查。
- 旧测试缺少新审查任务、集成回执、计划摘要和有效 owner，导致其在目标行为之前被正确拒绝；已补齐夹具并保留原断言。
- CI、集成器和隐藏判定进程崩溃后的计时缺口：执行开始／停止时间持久化，每次执行独立计费键，停止未确认不提前封存。
- 多 Colima 实例的随机宿主端口冲突：验收数据库显式绑定预检后的空闲端口。
- 控制台移除已不参与新并发调度的旧构建／审查数量输入；旧配置迁移继续兼容。

- Reviewer 命令可先改候选再还原，以及同代义务结论泄漏：候选与 `.git` 已强制只读，补充测试写入独立有界目录；独立 Reviewer 不再读取同伴结论。
- 同进程隐藏测试可被候选伪造协议：改为独立可信模块与容器，单独 PID 空间和日志通道；已验证 stdout／TestMain／init 伪造与跨进程终止攻击。
- CI 与 Judge 使用共同执行锁，存在未结算执行日志时拒绝交叉启动，恢复后补记运行时长。
- Planner 使用内部保留任务 ID 会卡住流程：两类规划报告及应用入口统一拒绝保留命名空间。
- Docker 层按测试名筛选漏掉安全回归：改为完整执行 sandbox／orchestration，汇总显式列出未运行和未覆盖跳过项。
- 宿主与数据库时钟差会把新证据判为过期：证据与 Agent 准入统一使用数据库时间，原失败回归连续 10 次通过。

## 尚待完成

- 当前真实联调目标和最新状态见上方“当前接续状态”。组织测试仓库仅保留为历史目标；个人仓库与 App installation 已核验。
- 真实 GitHub App／可信 CI 签发与最终门禁联调。
- 十项真实冒烟、九十次同预算开发对照、二十个固定候选审查、维护者标签校核和人工合并。

原部署已只读核验：每 Issue 200 次调用、每日 500 次、每 Issue 120 分钟、最多 2 次尝试及 2 轮修复；开发／审查模型均为 `deepseek-v4.1-flash`。价格为零表示未知，不能据此宣称免费。真实试验必须冻结有效配置，不使用本地合成测试的示例预算。

**历史记录（已由个人账号测试仓库替代）：** 初次接续时曾选用组织仓库 `ycychome/test-repo-for-ai-test`（ID `1415305337`），当时 App installation 查询返回 404。随后用户改为个人私有测试仓库；上方当前状态记录了最终固定目标与 installation 身份。

## 2026-10-13 PR #3 接续验收（阶段性历史日志）

以下条目按当时取得证据的先后保留；其中早期目标、额度和探针阶段已被上方“当前接续状态”更新，不作为最新总述。

用户在新会话要求接手完成 PR #3 验收，因此接续此前未运行的真实联调与评估。使用独立 `pr3-acceptance` 工作树；原工作树中的测试证据保留在 `dynamic-agents-alpha/acceptance-evidence/`，本轮证据保存在 `pr3-acceptance/acceptance-evidence/`。以下状态只覆盖实际取得的证据。

| 范围 | 结果 | 本轮证据 |
|---|---|---|
| 原全量 race 与源码一致性 | 校验原 JSONL 哈希及 577 pass／0 fail／2 helper skip；28 个架构场景均找到实际 pass。268 个快照文件中 264 个匹配接手时 HEAD `d422ff5`，4 个差异均为文档 | `source-evidence-audit.json` |
| 新测试仓库评估适配器 | 固定名称与数字 ID，显式绑定 App／installation；管理员安装冻结 CI，App 只读核验；恢复拒绝旧格式及混合身份回执，评估不登录重置维护控制面 | `target-regressions-001/`、`resume-regressions-001/` |
| Python 完整回归 | 个人目标接线完整复测 45 项通过，0 失败；覆盖评估、120 项离线准备、恢复身份、旧维护与重置边界、验收汇总 | `personal-target-regressions-001/summary.json` |
| 可执行文件 | 当前源码的 `control` 与 `ci-receipt` 均构建成功；未以构建成功替代运行验收 | 本地构建产物 |
| 评估准备 | 从实际配置加载器导出的非敏感有效配置生成 90 项矩阵和 10 个独立冒烟候选；隐藏 Judge 素材位于控制侧，人工标签仍 pending | `frozen-evaluation-001/` |
| GitHub 账号与目标 | 当次 `zymooll` 对目标仓库有 admin；repo `1415305337`，App `5285791`，现有 installation `170592609` 属于 `ycychome`，范围为 selected | `live-preflight.json` |
| App 安装访问权 | App 查询新仓库 installation 返回 404。仅为该仓库添加安装范围的 API 请求返回 403，要求 Organization Owner；请求未生效 | `app-repository-installation.json` |

原组织安装请求被拒后，用户要求在个人账号创建测试仓库。已创建私有 `zymooll/ai-workflow-alpha-test`（repo ID `1416790756`），仅包含 GitHub 初始化的 README，未重置原组织仓库。评估准备固定核验此仓库、所有者 `zymooll`／`51841832` 和 App `5285791`；安装 ID 将在个人安装完成后重新核验。写入凭据独立配置为 `PILOT_EVAL_WRITE_TOKEN(_FILE)`，未显式设置时读取 Git credential helper。当前尚未启动真实模型试验，未消费新的模型调用额度。

冻结矩阵配置摘要为 `6a3588991b1f35f2a972af1f45476725ce1a1c23e11ca83043cd65c703251767`，清单摘要为 `dd2af917d6bb1f4f1297fe0045f6a1577a6d0487c97b721000fec84798c27f3a`。该矩阵为接线前的准备记录；个人仓库、安装与可信 CI 配置会改变 model_profile，因此部署后必须重新从 `/api/evaluation/config` 导出并冻结新矩阵，其摘要一致才可执行；按日额度必须保留并持续累计，不能通过换库或重启清零。

安装范围补齐后，顺序为：重新核验 App 身份 → 安装固定可信 CI 与独立基线 → 隔离部署并核对有效配置／当前额度 → 真实 GitHub App／宿主签发与最终门禁联调 → 十项冒烟 → 九十次对照及二十个盲审 → 维护者标签校核与人工合并。App／CI、模型结果、人工标签和人工合并仍为未验收；适配器离线测试没有将这些条目标为通过。

远端接手时 HEAD `d422ff5` 的 [Actions run 38375368409](https://github.com/ycychome/ai-workflow/actions/runs/38375368409) 已结束：`control/checks` 通过，隔离验收失败。后者的 unit、PostgreSQL 和 T01–T28 层通过，Docker Judge 层失败；原日志只有失败名称而没有原始 Go 输出，不能据此确认根因。相同失败入口在独立 Colima／PG 定向复跑全部通过（隐藏判定 39.75 秒、伪造用例 92.85 秒、20 候选 291.19 秒）。本轮只增加总是上传 CI 验收 JSONL／摘要的步骤，不降低 Judge 断言或把未复现的远端失败记成通过；后续运行以远端完整证据为准。

个人 App 安装已经用户确认并完成本人认证，installation ID 为 `170932429`，已通过 UI 和实际 API 双重核验，仅选中 `zymooll/ai-workflow-alpha-test`。19 个基线／公开 CI 文件已在私有仓库精确读回；初始 source commit `94a8fc03fc71376557724dfdd239de2699794b4d`，workflow ID `383067637`，workflow blob `2f7d44396075f4a0a5ec6fb21fdac6d4e99aa292`。

已启动独立 native 控制器（loopback 28080／28081）和独立 Colima PostgreSQL，实际 `/readyz` 200、登录 204 后持有 session 并成功读有效配置。真实登录暴露了评估 CLI 对空 204 响应解码失败的问题，已修复并用真实 loopback HTTP＋Cookie 回归验证。原数据库只为只读额度核验短暂启动，读取后恢复原停止状态；原控制器未启动。验收库保留 2026-10-13 已用 79 次调用，不以新库清零每日额度。

真实 `connection-T01-001` 为独立接线探针，不计入十项正式冒烟或九十次对照；实际模型为 `deepseek-v4.1-flash`，已完成 triage／dev_planner 并进入 builder，尚无最终结论。有效配置摘要为 `e0e999c03544aac85929ecdd525f5882bb91cee78165aec2ebe4ecb12c42caed`；最终正式矩阵仍须在接线冻结后生成。证据位于 `live-runtime-001/`。

远端 [run 38380058253](https://github.com/ycychome/ai-workflow/actions/runs/38380058253) 再次在 Docker Judge 层失败；artifact 上传另因 GitHub 存储配额已满而失败。已改为在控制台保留有界的真实失败输出，并允许 artifact 上传独立失败；核心测试的失败退出与 incomplete 判定不变。下一轮须依据实际诊断确定原因，不能删除远端数据、绕过测试或把 artifact 配额问题当作测试通过。

真实接线探针曾在调用 62 次后因 `Git 树文件模式不支持：go.sum` 停于 BLOCKED。只读拉取真实 GitHub ZIP 证实其普通文件没有 Unix mode 属性；Go archive/zip 将这种 DOS 缺省权限解释为 0666，而旧 GitTreeSHA 只接受 0644／0755。远端基线 go.sum 摘要仍等于冻结清单，沙箱快照为 0644，不能把此问题归因于候选篡改锁文件。现已统一普通文件的 Git 权限语义，仅 owner execute 对应 100755，类型／setuid／setgid／sticky 继续拒绝；有真实 Git oracle、DOS ZIP 与权限差异回归。

集成失败的恢复另发现 generation 递增会使已接受来源过期；已增加严格受控的集成检查点恢复，检查当前身份、来源摘要、已停止 Agent 与无未决写入，随后仍走正常完整集成校验。真实 `/ai resume` 已验证保持 generation 1、62 次 Issue 调用和 141 次每日累计；随后来源精确发布到私有测试 [PR #2](https://github.com/zymooll/ai-workflow-alpha-test/pull/2)，提交 `6d9cbda9cf5e81ae2ac2594e5d6d8c41200e0ca5`，回归任务仍在执行。这是接线恢复证据，不是完整 Alpha 放行。

远端 [run 38389966626](https://github.com/ycychome/ai-workflow/actions/runs/38389966626) 的新诊断确认 Judge 用例约 60.9 秒后发生 context deadline／UNVERIFIED，属于测试基础设施期限不足。仅 Judge Docker 测试夹具扩展为有界 3 分钟，其他安全回归仍为 60 秒；Docker 层 Go 上限 60 分钟，CI job 上限 90 分钟，其他层仍 15 分钟。真实模型配置与预算不变，超时仍不能作为候选缺陷或安全通过。修复已通过 Go 单元／静态检查与隔离 PG 恢复边界检查，远端完整复跑仍须完成。


### 公网恢复与独立探针 002

用户要求重试网络后，QUIC 公网隧道注册成功；外部 `/api/state` 和 `/setup` 返回 404，未签名 `/webhooks/github` 返回 401。备份 App 原 Webhook 配置后更新至验签代理，真实个人仓库事件得到 GitHub 202 投递回执。相同 push delivery GUID 由 GitHub 重新投递仍为 202，Inbox 前后都只有一条 processed 记录；证据为 `live-runtime-001/webhook-redelivery-proof.json`。原网络失败日志保留，先前“App hook 未改”只描述恢复前状态。

运行环境修复：预装 go／gofmt 通过 `/usr/local/bin` 保持 login shell 可达；run 工具优先参数数组，shell 推荐 `sh -ceu`，说明每次 run 的 `/tmp` 独立、临时脚本须单次创建执行。真实 Docker 回归覆盖工具可用、缓存构建、失败传播、临时目录隔离与工作区持久；Go 单元／vet 及 50 项 Python 回归通过。提示版本升级为 `zh-CN/v2` 并纳入评估配置摘要，防止旧提示与新提示共用冻结配置。此修复后的完整远端 CI 须在新提交上再核验，不能沿用 `436730ad` 的绿色结果。

已在原验收数据库启动新的 `connection-T01-002`，Issue #3，初始独立缺陷提交 `625e658cc201a8840933df799a8a2b9a4551c0ce`；该探针同样不计正式冒烟。启动前每日累计仍为 270 次，保留探针 001 的 191 次与先前 79 次，不提高或重置额度。新有效配置摘要 `e8710dd3e8a2565253093197085366d442ab43b9c59f94397ea66e5c2d935383`，runner 固定到镜像内容 ID；正式 90 项矩阵已按实际 API 配置重新冻结，120 项独立 Git 快照准备进行中，正式模型评估尚未运行。

固定审查 PR 的 opened CI 可能早于评估 workflow 创建时间，因此管理员固定可信 CI 增加专用 `aiwf-ci-refresh` 标签触发，仅该标签可触发额外运行。宿主协调器对缺少符合时间边界 run 的精确 WAITING_CI 候选持久化一次性刷新预留，重读 PR 和候选后添加标签；仍保留完整签名、NotBefore、head/base／tree 和工作流来源校验。当前固定来源提交 `00d15a990e462cec464c89bdca15333554138d25`、blob `98365815d31fac129be799426ef3bd14c137b870`，该接线仍待实际签发与最终门禁结果证明。

真实探针 002 已取得固定 test-merge `71190131317bfddc8a72d4be1dd1e127b3813b29`、树 `3ec1e8ce79dd903a792d03c7b457690422223c45`。宿主 `ci-receipt` 对 Actions run `38406324523` attempt 1 独立测试、确认停止并签发成功，receipt digest `10ba35713ab2d5ce4a2d7c7c5d13afdd0bbfa604ee68aaa7cb4f73a83584d18b`；控制器实际进入 REVIEWING，未以 CLI 退出码代替最终审查或 Judge。

真实证据导出发现 JSON 摘要表示不一致：存储文件为缩进 JSON，而 Evidence.Hash 绑定紧凑 JSON；API 旧实现错误地比较原文件字节。现改为保留原顺序、数值和转义的 `json.Compact` 后严格校验并返回紧凑字节，不改变历史摘要。回归覆盖大整数、中文／转义、篡改、路径和 symlink 越界。此传输修复通过 Go 单元／vet，运行器将在当前 Agent 停稳后更新，再进行真实证据 GET 与摘要核验。


### v3 工具额度合同与失败留证

探针 002 已在冻结 v2 判定包中完成实际外部 Judge：PASS、退出 0、stopped_confirmed=true；完整流程仍是 failed（185 次模型 HTTP 请求，Skeptic 调用了未注册 get_goal），未计正式冒烟。API 成功导出可信 CI 证据并核对紧凑 JSON 摘要，随后实际 `record_result` 保存 255 个证据文件及原始失败；结果位于 `live-runtime-001/connection-probe-002-results/`。原版本素材复制并逐项校验于 `frozen-evaluation-002/judge-original-v2/`，版本升级未重写这一结果。

预算回归确认，普通工具批次个数与实际 HTTP 尝试是独立资源。现保持每 Issue 200／每日 500 个模型 HTTP 请求（含重试）、120 分钟、2 次尝试与 2 轮修复，只给每次 Agent run 的普通工具尝试使用独立有限上限 200；未知工具批次全部不执行，可在剩余 HTTP 预算内获得确定性纠正反馈。测试证明混合合法／未知批次没有副作用、持续未知在请求边界停止、批量合法工具不提前耗尽模型请求额度。模型可见字段明确区分 max_calls 与 max_requests。

提示版本 `zh-CN/v3`、执行字段 `runner.max_tool_calls_per_run` 和可信 CLI／协议已显式纳入新清单 `issuedesk-alpha-v3-tool-budget`。清单摘要 `45ed27b4f62f5b2f594777c1415cccb541fc8b563e40b2f85dea415c5419d86f`；有效配置摘要 `7d9ee4342ea0113c63f24c6d988c66c7c2aeada79bc6967c284f47dabc4c084b`，来自实际 API，预算与控制器返回逐项相等。版本审计证明服务基线、题面、变体、20 个标签及外部 driver／harness 的字节与 SHA-256 全部不变；新旧矩阵不能混用。正式 v3 的独立快照准备进行中，尚无正式模型结果。

原 002 准备中途因远端适配器失败安全停止，已保存 34 项独立快照与逐试验预留；不覆盖、重置或丢弃其未知结果。v3 使用相同来源内容和仓库身份恢复未执行的准备，每个引用仍由适配器读回核验；矩阵、清单与配置另存为 `frozen-evaluation-003/`。本轮 Go 单元／vet、50 项 Python 合同均通过，新提交完整远端 CI 仍须完成。


### 首项正式冒烟失败与预算规划修复

用户授权临时提高每日模型请求上限后，单日上限曾设为 25,000，单 Issue 200 次／120 分钟、2 次尝试与 2 轮修复保持不变，实际配置摘要为 `102537ce7504ad4a825d94c5bbaf4cf18559bfab3c0f2d64455c7041bd13d2af`。正式 `smoke-T01`（Issue #6／PR #7）在 200 次请求后达到 BLOCKED，结果 `budget_exhausted`；可信 CI 和独立隐藏 Judge PASS、进程已确认停止，审查及 Skeptic 未完成。306 个证据文件与失败回执已保存于 `formal-evaluation-004/`，未计 PASS；后续 9 项冒烟、90 对照与 20 盲审按准入协议未运行。

失败停稳后已恢复每日 500，并通过部署 API 核验；当时累计 655 次保持不变，恢复配置摘要 `7d9ee4342ea0113c63f24c6d988c66c7c2aeada79bc6967c284f47dabc4c084b`。记录和确认见 `live-runtime-001/temporary-budget-journal.json` 及 `restored-execution-config.json`。部分独立引用准备已安全停止，70 项准备及逐任务预留保留。

事实审计找到 5 次开发规划、独立构建验证／DB 验证／验收分析造成重复开销，但没有每次 Agent 的 HTTP 请求计数，不能把工具证据数量当请求数。v4 提示与控制器输入明确实际验证命令，要求同范围实现测试合并、完成事件计划充分时返回空变更、审查分组并预留 Skeptic 额度；全部义务、独立首轮、启动检查、CI 和 Judge 不省略。专用配置显式映射 ISSUEDESK_TEST_DATABASE_URL 到隔离服务 TEST_DATABASE_URL，实际运行数据库公开测试。

复验使用独立 `--experiment-id` 批次；trial_id、逻辑 run_key、回执、恢复与检查点显式区分，既有 trial／Issue／PR／失败／用量不能覆盖。评估版本 v4 只改可信协议摘要，基线、题面、变体、标签、CLI 与 driver／harness 摘要维持 v3 原值。旧数据继续独立保存。

提交 `bdd189a` 的远端 run `38413122976` 两 job 成功：unit 366 passed、PostgreSQL 432、Docker 449，无未覆盖 skip，T01–T28 全通过。artifact `11825380524` 这次实际上传并下载验证：32 文件，ZIP SHA-256 `6f2316ddd1305df51d8d3c58930a6ef9c549ee56a0f8445f0f3ed8d6ead764e9`；控制台原日志 SHA-256 `840fc63ee6b55f70e77720495c004b42a67aba5b2358ee1a4b58df02c3334496`。这是 v3 代码验证，v4 修改的完整远端 CI 另行核验。

### PR #3 合并及 v4 真实冒烟结果

用户于 2026-10-14 明确要求先合并当前版本、再继续测试。PR #3 的 HEAD `a17180361e38e73cf9d2abdc5d045f14a29d8f1b` 在完整 CI run `38426243586` 两 job 成功后合并为 `1234644ee0ffe03e19ef9290dd8c6465e3949e4d`；合并树与该 HEAD 相同。合并后的 push run `38433169188` 也两 job 成功：unit 368 passed、PostgreSQL 433、Docker 450；T01–T28 全通过，未覆盖 skip 为空。两次最新 run 的 artifact 均因存储配额未上传，原始控制台日志及汇总已本地保存并校验 SHA-256；不能宣称取得对应原始 JSONL artifact。

独立 `pr3-v4` 批次使用清单 `c7f3da2228e7b29deb64c9633bdb915b6e76e58f48df4a2a30b7506292061716` 与有效配置 `bf0a5171998677d3c46fba462729188e6eaa87dc98c9e47eb4cf43b5daa845c3`。首项 `smoke-T01`（测试 PR #9）完整开发／独立审查／Skeptic 与外部 Judge 通过，177 次模型 HTTP 请求，228 个文件摘要全部核验；累计执行 2102 秒，本地创建至最终更新 3193.658 秒。第二项 `smoke-T02`（测试 PR #11）在 103 次请求后 BLOCKED，外部 Judge PASS，但完整编排 failed；159 个证据文件摘要已核验。其余 8 项冒烟、90 对照与20盲审按准入规则未执行。74 项已准备引用及未知状态的预留保留，未重置或覆盖。

T02 实际失败来自后续 ReviewPlanner 引用不存在的 `F2-http-level-regression`、`F3-test-change-audit`；当前 ledger 只有 `F-89cb74e019`、`F-70c535789e`、`SK-01`。原事务校验正确拒绝未知身份，但报告校验只检查 DAG，错误发生在接受报告／保存计划后，未返回模型纠正。v5 给规划者当前验证身份下的有限 ID 摘要，在报告接受前预检请求字段和关联，再由原事务防线确认。Skeptic 在独立预览合并后校验，仍可引用同一报告中新建的有效发现；首轮 Reviewer 不获得同伴报告。修复不豁免任何发现，也不放宽请求预算或验证身份。

用户对精确测试候选授权后，又委托本验收批次逐次审阅同类测试变更：仅新增测试或修正本批次新增测试，保留原断言，并绑定具体提交和变化摘要；其他风险、人工标签和测试 PR 合并仍由用户决定。T02 失败停稳后已恢复每日 500，2026-10-14 的 264 次实际用量保持；累计 120 分钟限制的是活跃执行区间并集加控制器计费，排队和已确认停止的人工等待除外，端到端时长另行留证。当前审计的 `model_run` 记录含每次 run 的实际 requests，不以工具证据数量推算模型请求。

v4 原判定素材已按原 SHA-256 复制验证于 `frozen-evaluation-005/judge-original-v4/`。v5 提示与协议另行版本化；基线、题面、变体、20 个标签、外部 driver/harness 保持原字节。完整正式 Alpha 仍未放行，PR #3 的用户合并决议不将剩余试验或待人工核准标签改为通过。
