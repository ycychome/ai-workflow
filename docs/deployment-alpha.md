# 有限 Alpha 隔离部署与集中验收

本文描述可执行入口。实现阶段已结束，当前统一验收的执行记录见 `alpha-acceptance.md`。旧根目录 `compose.yaml`、`scripts/up.sh`、`secrets/` 和正在运行的服务不属于这些新脚本的默认目标。

## Linux 与 Colima 共用部署

Linux 主机预装 Docker Engine、Compose v2、Python 3；执行本机 Go 检查还需 Go 1.24。macOS 使用独立 Colima profile（建议至少 6 CPU、8 GiB 内存、40 GiB 磁盘）。现有 `bootstrap-macos.sh` 会安装软件、修改 Docker context，并在 DNS 失败时改 VM DNS，因此不能把它当无副作用探测脚本。已有环境直接显式指定 `DOCKER_CONTEXT=colima-<独立profile>`；不要切换其他服务的上下文。

以下命令只操作指定 Alpha 项目；`ALPHA_STATE_DIR` 必须是新建目录，父目录需存在。脚本拒绝覆盖已有配置。

```sh
export ALPHA_PROJECT=aiwf-alpha
export ALPHA_STATE_DIR="$HOME/aiwf-alpha-state"
# macOS 按本机实际独立 profile 设置；Linux 使用目标 Engine context。
# export DOCKER_CONTEXT=colima-ai-workflow-alpha
scripts/alpha-init.sh
# 在新 state/secrets/config.yaml 中配置试点仓库和 App/模型设置。
scripts/alpha-prewarm.sh
scripts/alpha-up.sh
```

`deploy/compose.alpha.yaml` 是独立 Compose 项目，默认仅监听 `127.0.0.1:18080` 与 `127.0.0.1:18081`。数据库与控制面数据分别保存在项目专属 `postgres-data`、`control-data` 卷。配置及密钥只在 `$ALPHA_STATE_DIR/secrets`，不会读取旧 checkout 的 `secrets`。控制器必须有 Docker socket；worker、keeper、PostgreSQL sidecar 均不挂该 socket。不要让两个控制面项目连接同一个控制面数据库，也不要复用 state 目录。

`ALPHA_CONTROL_IMAGE` / `ALPHA_RUNNER_IMAGE` 可设置为独立版本标签。改变 runner 标签时同步修改新配置的 `runner.image`。配置更改后重启本项目控制器，不能改动正在运行的另一项目。

## 离线 Attempt 依赖

`alpha-prewarm.sh` 预拉取 `postgres:16-bookworm`（验证 sidecar）和 `postgres:17`（控制面），构建控制器及 runner；runner 构建时 `go mod download` 将当前锁文件依赖烘焙到 `/go/pkg/mod`，并安装 PostgreSQL 客户端。正式 worker 为 `network=none`、`GOPROXY=off`、`GOSUMDB=off`、`GOTOOLCHAIN=local`。新项目若依赖不同模块，先用受信 Dockerfile 与锁文件准备对应 runner 镜像，不能临时放开执行网络。

仓库配置 `verification_services: [postgres]` 后，每个 Attempt 获得独立 sidecar：Unix socket 专属卷以只读方式挂进该 worker；数据为 sidecar 独有的 512 MiB tmpfs，worker 不挂 PGdata。sidecar 无网络、768 MiB 内存、64 PID、1 CPU、最多 2×5 MiB 日志；tmpfs 字节/inode 上限通过 statfs 验证。临时用户、密码与数据库仅属于该 Attempt。`DATABASE_URL`、`TEST_DATABASE_URL`、`AGENT_QUEUE_TEST_DATABASE_URL` 指向同一私有 socket，不继承控制面 DSN。

`CreateWithServices` 等待最终 postgres 主进程就绪，不能把初始化临时数据库就绪视作完成。服务重启、keeper 丢失或无法确认退出均报错。取消销毁 workspace、sidecar、socket 和数据；租约过期本身不能释放槽位。恢复仅按持久 AgentRun.ID 的精确标签执行 `RecoverWorkspace` / `StopRun`，旧无目标 `Recover` 已禁用，不能批量清理所有 managed 资源。

## Schema 升级、备份与回滚

当前新增版本 4：Agent 队列和公平轮转表；版本 5：`prompt_version`、`start_reason`，通过 `ADD COLUMN IF NOT EXISTS ... DEFAULT ''` 兼容历史行；版本 6：仓库运行 epoch 和退役工作流记录；版本 7：跨控制器共享的 Agent 并发额度；版本 8：持久化 Agent 活跃区间和控制器额外执行时间的独立计量列；版本 9：按仓库持久化维护令牌与停收标记。启动控制器执行幂等迁移，不改历史 workflow JSON。历史空提示词版本不应被解释为当前模板版本。不要让旧二进制与新二进制同时写同一库。

升级前先暂停/取消工作流，等待 ACTIVE 与 STOPPING Agent 全部确认退出。备份脚本会停止控制器，并拒绝在 Agent 容量仍占用时继续；失败后不会偷偷重启服务。记录当前镜像并保留本机旧镜像，再构建新版本。

```sh
scripts/alpha-backup.sh "$ALPHA_STATE_DIR/backups/before-schema-9"
scripts/alpha-prewarm.sh
scripts/alpha-up.sh
```

备份包括自定义格式 PG dump、控制面 `/data`、配置/密钥、schema 列表、镜像 ID 和 SHA-256 清单。备份目录权限受 umask 077 保护，包含真实配置密钥，应按凭据文件保管。备份不是自动测试；首次部署仍需在隔离项目实际演练恢复。版本 3 无 `agent_runs` 时计数检查兼容返回零。

回滚使用完整备份，不能只删除 migration 行或只换旧二进制。恢复仅针对当前明确选择的 Alpha 项目；脚本校验文件哈希、拒绝越界 tar 路径和链接、要求数据库凭据与备份一致，并保持控制器停止。恢复前还应再备份当前状态。旧镜像 ID 在备份文件中，必须留在本机或可信镜像仓库。

```sh
scripts/alpha-restore.sh --replace-alpha "$ALPHA_STATE_DIR/backups/before-schema-9"
export ALPHA_CONTROL_IMAGE="$(cat "$ALPHA_STATE_DIR/backups/before-schema-9/control-image-id.txt")"
# 同步恢复的 config.yaml 已记录原 runner 标签；必要时将保存的 runner ID 重新标为该标签。
scripts/alpha-up.sh
```

脚本不会启动另一数据库、删除其他项目卷、运行远端命令或修改现有 `secrets/`。若 Docker 不可达、资源未退出或恢复步骤失败，保留失败状态并调查；不要手动将 Agent 标成 FINISHED 来绕过容量上限。

## 集中验收入口

所有入口只在“全部实现完成后统一测试”阶段执行。日志目录必须是新目录，旧结果不覆盖。脚本产出 Go `-json` 日志、哈希与 `summary.json`；缺失用例、跳过或失败不会形成通过记录。

```sh
scripts/alpha-acceptance.py list
scripts/alpha-acceptance.py unit --output /tmp/alpha-unit-001
# 启动独立临时 PostgreSQL、生成临时配置，结束后删除；不会使用控制面数据库。
scripts/alpha-postgres-check.sh /tmp/alpha-postgres-001
# Docker 层完整运行 sandbox+orchestration；需隔离 CONFIG_FILE/AGENT_QUEUE_TEST_DATABASE_URL，
# 并预备默认 ai-workflow-runner:local 镜像和 postgres:16-bookworm。
scripts/alpha-acceptance.py docker --output /tmp/alpha-docker-001
# 一次创建专用数据库并运行全部层（含 Docker 与 T01–T28）：
scripts/alpha-postgres-check.sh /tmp/alpha-all-001 all
# 完整架构矩阵：按需提供隔离 CONFIG_FILE / AGENT_QUEUE_TEST_DATABASE_URL 与 FORCE 开关。
AI_WORKFLOW_FORCE_INTEGRATION=1 AI_WORKFLOW_FORCE_DOCKER=1 \
  scripts/alpha-acceptance.py matrix --output /tmp/alpha-matrix-001
```

Docker Go 测试默认用 `ai-workflow-runner:local`；它与部署配置的 alpha 标签不同。验收应使用独立 Docker context 将同一构建镜像标为测试默认标签，不能覆盖另一正在运行项目的 runner 标签。数据库集成用例使用独立临时 schema，但仍应连接脚本创建的专用验收数据库。

架构 T01–T28 与评估夹具 `eval/tasks/T01...` 是不同编号体系。矩阵要求 `TestAcceptanceT01` 至 `TestAcceptanceT28` 对应架构 12.1 的真实场景，可带非数字后缀；当前没有该命名用例的条目会明确判为缺失。不能用一个相近单元测试、JSON清单或手工写入 passed 文件代替正式场景。部分层通过不等于真实 GitHub App、分支保护、UI、故障注入矩阵或同预算模型评估通过。真实 GitHub/界面验收应另保存仓库、App 身份、候选 SHA、屏幕/事件证据，评估按 `eval/protocol.md` 执行。

仓库重置使用独立 `repositories[].epoch`，空值按 `legacy` 处理。先停止控制器、确认 Agent 退出及外部写入结果、完成备份，再由管理入口调用 `AcquireRepositoryMaintenance`，待所有计数归零后以同一令牌、旧/新 epoch 和计划摘要调用 `RetireRepositoryMaintenance` 并同步配置新 epoch；它保留历史工作流，取消旧代次，拒绝旧 webhook 和旧工作流重启。ACTIVE / STOPPING Agent 或 running / unknown Outbox 仍存在时会拒绝重置。不要用删表、清空工作流或只改 YAML 模拟重置；本文脚本不会自动执行退役。

并发额度以数据库 `agent_limits` 为权威。新控制器只在未初始化时写入配置值，不能用旧本地配置覆盖其他控制器已调整的额度；修改须走持久化配置入口。降额后既有 ACTIVE / STOPPING 仍保留名额，等待真实退出后再发新名额。

执行预算从持久化 Agent 会话起止区间计算，重叠区间取并集，排队时间不计入；STOPPING 在确认退出前仍计费。schema 8 将历史已计入的 runtime 保留为控制器时间，避免升级清零预算。

维护状态持久化在 `repository_maintenance`。获取维护令牌后，目标仓库的新 Intake、工作流、Agent、普通 Outbox 和标签 Outbox 均停止派发；其他仓库继续工作。已在途的停止与回执仍可提交，状态接口报告活跃/停止中 Agent、有效工作流租约及 running/unknown Outbox。退役后旧工作流在事务写入点也被拒绝。`ReleaseRepositoryMaintenance(..., true)` 保留停收标记；旧的无令牌退役接口已禁用。

### 只读候选与外部判定进程

非 Builder 会话在可信初始化后调用 `SealReadOnly`：候选卷以只读方式挂入命令容器和文件工具容器，不能通过“修改测试、执行、再还原”伪造原树的验证证据。`/tmp` 与 Go 构建缓存仍可写。审查补充测试位于独立 `review_tests/` tmpfs，额外容量为 `min(workspace_bytes, 64 MiB)`，另有 inode 上限和 keeper；它不计作候选源卷的一部分。候选本身若占用 `review_tests` 路径，审查封存明确失败，不能静默遮蔽候选文件。

外部可信 Judge 使用 `CreateJudgePeer` 创建独立只读源码卷和独立 PID 命名空间：候选服务与可信 harness 均无网络，只共享本次私有 PostgreSQL socket 及额外 1 MiB 的 Unix HTTP socket 卷。候选无法看到 harness 源码或其 stdout；harness 的 HTTP socket 挂载只读。候选 stdout 只作不可信日志，不能作为 harness 测试结果。所有 peer、传输 keeper、服务和卷使用同一运行 ID 标签，`StopRun` 必须确认全部消失后才能释放运行容量。

验收摘要将父进程专用辅助测试的预期跳过列为 `parent_helper_skips`，其余跳过列为 `unexecuted_skips`。完整 `all` 汇总要求这些非辅助测试在其他层实际通过，否则 `uncovered_skips` 非空且结果为 incomplete。仅包编译成功而没有命名测试执行记为 not_run；Docker 层不再按名字筛选，缺少集成前置条件会拒绝通过。

### 后续真实仓库联调

真实 GitHub 联调的指定目标为 `zymooll/ai-workflow-alpha-test`，数字 ID 固定为 `1416790756`（用户要求个人账号下新建的私有测试仓库）。`pilot-eval-prepare.py` / `pilot-eval-github.py` 已改为独立的评估目标接线，要求显式提供 `--repository`、`--repository-id`、`--app-id` 与 `--installation-id`；名称或数字 ID 不匹配时，在访问 GitHub 之前拒绝请求。保存的准备回执也绑定四项身份，旧格式和混合身份不能直接恢复。

适配器先核验当次人类操作员是所有者 `zymooll`／`51841832` 且具有目标仓库 admin 权限，再用 App JWT 验证该仓库的 installation／App 身份，并申请只限该仓库的 `actions:read`、`contents:read`、`pull_requests:read` 安装令牌。只读远端核验使用此令牌；安装经哈希固定的 CI 文件、创建冻结 Git 对象／引用及审查 PR 使用已核验的操作员凭据。操作员凭据通过 `PILOT_EVAL_WRITE_TOKEN(_FILE)` 独立设置，未设置则读取 github.com Git credential helper；需具备写入 workflow 文件的权限，App 不需要新增 `workflows:write`。评估不会构造重置维护 Adapter，也不需要登录维护控制面。`pilot-reset.py` 与管理重置适配器的旧仓库边界保持不变。

原组织 installation `170592609` 的添加请求被 GitHub 以 403 拒绝，用户随后改用个人账号新建的测试仓库。安装现有 Public App `5285791` 时选择 `zymooll`、`Only select repositories` 并仅勾选 `ai-workflow-alpha-test`；重新取得个人 installation ID，不能沿用组织安装 ID。评估仍需真实安装核验，不能用管理员写入替代 App 联调。当前证据及剩余正式验收见 [alpha-acceptance.md](alpha-acceptance.md)。
