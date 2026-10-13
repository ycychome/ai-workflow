# Alpha 集中验收记录

状态：**PR #3 已接手续验，个人 App 已安装，真实联调与 CI 诊断进行中，完整 Alpha 尚未正式放行**。实现阶段结束后统一运行测试。以下历史结果来自 `codex/dynamic-agents-alpha` 实施分支，不复用历史 MVP 验收。

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

- 真实联调目标为用户指定的 `ycychome/test-repo-for-ai-test`；用户随后改为个人账号测试仓库，已创建 `zymooll/ai-workflow-alpha-test`（`1416790756`），评估入口固定到该目标，个人 App 安装已完成并核验为仅限该仓库；真实联调进行中。旧 pilot 未重置。
- 真实 GitHub App／可信 CI 签发与最终门禁联调。
- 十项真实冒烟、九十次同预算开发对照、二十个固定候选审查、维护者标签校核和人工合并。

原部署已只读核验：每 Issue 200 次调用、每日 500 次、每 Issue 120 分钟、最多 2 次尝试及 2 轮修复；开发／审查模型均为 `deepseek-v4.1-flash`。价格为零表示未知，不能据此宣称免费。真实试验必须冻结有效配置，不使用本地合成测试的示例预算。

本轮收尾时经 Git credential helper 只读核验，`zymooll` 对实现仓库与新测试仓库具有管理权限；新测试仓库数字 ID 为 `1415305337`。现有 App 对新仓库的安装查询返回 404，尚未验证可用安装。用户随后要求结束测试并提交 PR，因此本轮不启动新远端试验，也不执行旧仓库重置。

## 2026-10-13 PR #3 接续验收

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
