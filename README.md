# AI Workflow

**把 GitHub Issue 交给 AI 团队，得到经过测试和独立审查的 Pull Request，再由你决定是否合并。**

AI Workflow 是一个可自行部署的开发助手。你在 GitHub 上描述需求，它负责需求分析、任务拆分、代码修改、验证和审查；你可以在中文 Web 控制台查看进度、证据和调用消耗，并随时暂停或接管。

项目目前处于有限 Alpha 阶段，适合先在测试仓库体验。完整真实场景验收仍在进行，进展见[验收记录](docs/alpha-acceptance.md)。

## 可以做什么

| 功能 | 对你的帮助 |
| --- | --- |
| 从 Issue 开始开发 | 分析需求、提出规格；信息不足时在 Issue 中请求补充 |
| 动态分工 | 按任务安排规划、开发、分析和验证角色，支持并行执行 |
| 自动准备 PR | 集成开发结果、执行配置的验证命令并创建 Draft PR |
| 独立审查 | Reviewer 检查候选代码，Skeptic 复核问题、反证及覆盖遗漏，必要时进入修复或补证 |
| 中文控制台 | 查看工作流、任务报告、审查发现、事件审计和模型用量 |
| 可控执行 | 设置调用预算、并发数和时限；暂停、取消、人工接管，并对高风险改动单独授权 |
| 隔离与恢复 | 在无网络、无宿主凭据的 Docker 沙箱中执行代码，持久化进度及 GitHub 操作记录 |
| 本地演示 | Fake 模式无需模型或 GitHub 凭据，即可体验固定流程 |

典型流程：

```text
提交 Issue → 分析需求 → 拆分任务并开发 → 验证 → Draft PR
         → 独立审查与复核 → 按需修复 → 等待维护者合并
```

默认情况下，新 Issue 会自动开始分析，AI 批准规格后继续开发。你也可以开启分析前、开发前的人工确认。**最终合并始终由维护者在 GitHub 上完成。**

## 快速开始

下面使用独立的 Alpha 部署，默认控制台端口为 **18081**，Webhook 端口为 **18080**。配置和数据与旧版 `scripts/up.sh` 部署分开。

### 1. 准备环境

需要 Git、Python 3、Docker Engine 和 Docker Compose v2。macOS 可使用 Colima，建议为独立虚拟机分配至少 6 CPU、8 GiB 内存、40 GiB 磁盘。首次构建需要联网拉取镜像和依赖；使用 Docker 部署无需在宿主安装 Go。

```sh
git clone https://github.com/ycychome/ai-workflow.git
cd ai-workflow

# macOS 使用 Colima 时，先创建并选择本次使用的独立环境：
# colima start --profile ai-workflow-alpha --cpu 6 --memory 8 --disk 40
# export DOCKER_CONTEXT=colima-ai-workflow-alpha

export ALPHA_PROJECT=aiwf-alpha
export ALPHA_STATE_DIR="$HOME/aiwf-alpha-state"
scripts/alpha-init.sh
```

`ALPHA_STATE_DIR` 必须是尚不存在的绝对路径，父目录需已存在。初始化会生成数据库凭据、登录令牌和 `secrets/config.yaml`，不会覆盖已有目录。后续在新终端执行部署命令时，需重新设置这两个变量及所用的 `DOCKER_CONTEXT`。

### 2. 构建并启动

```sh
scripts/alpha-prewarm.sh
scripts/alpha-up.sh
```

打开 [http://127.0.0.1:18081](http://127.0.0.1:18081)，使用本机文件 `$ALPHA_STATE_DIR/secrets/dashboard-token` 的内容登录。

先体验流程：进入 **设置 → 预算与执行**，将运行模式改为 **模拟** 并保存，再回到仪表盘点击 **创建本地演示**。演示无需配置模型或 GitHub；它运行固定的计算器修复流程，最后停在待维护者合并阶段。

### 3. 接入真实项目

完成下方的模型、仓库和 GitHub App 配置后，将运行模式切换为 **Responses API · 真实模型**。在已绑定的仓库中创建一个范围明确、验收标准清晰的 Issue，即可开始。

## 配置方法

常用模型参数、预算和执行选项可以在控制台修改并保存。仓库范围、验证命令及密钥路径在 `$ALPHA_STATE_DIR/secrets/config.yaml` 中配置；完整字段可参考[配置示例](configs/config.example.yaml)。

下方 YAML 片段用于修改现有配置中的对应部分，请保留文件中的其他字段，避免重复添加同名配置段。

直接编辑 YAML 前，应先暂停工作流并等待执行中的任务退出，修改后重启当前项目的控制器：

```sh
docker compose -p "$ALPHA_PROJECT" -f deploy/compose.alpha.yaml restart control
```

### 模型服务

在 **设置 → 模型** 中填写：

| 设置 | 填写内容 |
| --- | --- |
| 服务基础地址 | 供应商的 API 基础地址，例如 `https://api.example.com/v1` |
| 开发模型 | 用于需求分析与开发的模型名称 |
| 审查模型 | 用于审查的模型名称，可与开发模型相同 |
| 访问 Token | 供应商提供的 API 密钥；留空保存表示保留已有密钥 |
| 最大输出 Token | 单次响应的输出上限，默认 `8192` |
| 启用流式输出 | 默认开启 |

服务需要兼容 **Responses API**，仅支持 Chat Completions 的接口不能直接使用。保存完整配置后，点击 **运行诊断** 检查连接；真实模式下的模型探测会发送一次模型请求。

也可以编辑 YAML，密钥单独保存在文件中：

```yaml
model:
  base_url: https://api.example.com/v1
  token_file: /secrets/model-token
  dev: your-dev-model
  review: your-review-model
  stream: true
  max_output_tokens: 8192
```

这里的 `/secrets/model-token` 是**容器内路径**，对应宿主的 `$ALPHA_STATE_DIR/secrets/model-token`。控制台会把提交的 Token 写入该文件，页面不会回读已保存的密钥。

### 仓库与验证命令

将示例中的试点仓库替换为自己的仓库，不要保留示例账号：

```yaml
repositories:
  - name: your-account/your-repo
    id: 0 # 完成 App 安装后，在控制台绑定得到实际的数字 Repository ID
    base_branch: main
    maintainers: [your-github-username]
    allowed_paths: ["**"]
    protected_paths: [".github/**"]
    policy_version: v1
    verification:
      - [go, test, ./...]
      - [go, vet, ./...]
```

- `maintainers` 是可以发出工作流控制指令的 GitHub 用户名。
- `allowed_paths` 限定允许修改的路径；`protected_paths` 标记受保护路径。系统始终保护 Git 元数据与 GitHub Actions 工作流。
- `verification` 中每一项都是命令参数数组，全部成功才算本地验证通过。需要 shell 语法时，显式使用 `[sh, -ceu, "你的命令"]`。
- 需要临时 PostgreSQL 时，添加 `verification_services: [postgres]`。每次执行得到独立数据库，可通过 `TEST_DATABASE_URL` 访问；项目若使用自定义测试变量，需在验证命令中映射。

默认 runner 预装 Go 与 PostgreSQL 客户端。执行沙箱无法联网下载依赖；接入依赖不同的项目时，需先准备包含工具链和依赖的 runner 镜像，并更新 `runner.image`。镜像预热方式见[隔离部署说明](docs/deployment-alpha.md#离线-attempt-依赖)。

### GitHub App 与 Webhook

真实运行需要一个 GitHub App，并将其安装到配置中的仓库。仅选择需要交给 AI 的仓库即可。

1. 在 GitHub 创建 App，配置下表中的权限和事件，填写一个强随机的 **Webhook secret**，并通过 **Generate a private key** 生成私钥。
2. 将私钥保存为 `$ALPHA_STATE_DIR/secrets/github-app.pem`，将与 GitHub 设置完全相同的 Secret 保存为同目录的 `webhook-secret`，文件权限设为 `600`。
3. 在 App 的 **Install App** 页面安装到目标个人账号或组织，选择 **Only select repositories** 并勾选目标仓库。
4. 编辑 YAML 中的 `github` 配置并重启控制器。
5. 在 **设置 → GitHub 绑定** 中点击 **发现安装**，选择安装并点击 **绑定所选安装**，确认仓库的数字 ID 已匹配。
6. 在 GitHub App 的 Webhook 投递记录中核对一次真实事件的投递结果。

| 项目 | 配置 |
| --- | --- |
| Repository permissions：Read & write | Contents、Issues、Pull requests、Checks |
| Repository permissions：Read-only | Actions、Metadata |
| 订阅事件 | Issues、Issue comment、Pull request、Pull request review、Push、Check run、Check suite、Workflow run |
| Webhook URL | `https://你的公网域名/webhooks/github` |

```yaml
github:
  app_id: 123456 # 你自己的 App ID
  private_key_file: /secrets/github-app.pem
  webhook_secret_file: /secrets/webhook-secret
  installation_id: 0 # 可由控制台发现并绑定
  api_url: https://api.github.com
  public_url: https://your-webhook.example.com
```

公网入口需将 `POST /webhooks/github` 转发至 `127.0.0.1:18080/webhooks/github`，控制台保持仅本机访问。`public_url` 填基础地址；GitHub 的 Webhook URL 需包含完整路径。临时隧道更换地址后，也要更新 GitHub App 的 Webhook 配置。

现有 App 注册流程的回调固定使用 **8081**，`scripts/tunnel.sh` 的代理固定转发至 **8080**，不能直接用于上述默认 Alpha 端口。使用默认 Alpha 部署时按上面的手动配置接入；旧版注册和隧道流程见[运行手册](docs/runbook.md#github-app-setup-and-webhook-tunnel)。

如需把固定 GitHub Actions 来源和独立签名回执纳入门禁，另行配置 `repositories[].trusted_ci` 并部署受信执行器，见[可信 CI 配置与签发](docs/ci-evidence.md)。启用后，Actions 显示绿色仍需等待匹配当前候选的回执，流程才能离开 `WAITING_CI`。

### 预算与执行资源

以下均可在 **设置 → 预算与执行** 中调整：

| 参数 | 默认值 | 含义 |
| --- | --- | --- |
| `runner.mode` | `responses` | 真实模型；`fake` 为固定演示 |
| `runner.workflow_concurrency` | `2` | 同时自动执行的工作流数，范围 1–4 |
| `runner.agent_concurrency` | `4` | 全局同时运行的 Agent 数 |
| `runner.workflow_agent_concurrency` | `2` | 单工作流同时运行的 Agent 数，不超过全局值 |
| `runner.max_tasks_per_workflow` | `64` | 单工作流累计任务上限 |
| `runner.max_evidence_requests` | `64` | 单工作流累计补证请求上限 |
| `runner.attempt_minutes` | `30` | 单次尝试的时长上限，单位分钟 |
| `budget.issue_calls` | `80` | 单 Issue 累计模型请求上限 |
| `budget.daily_calls` | `500` | 每日累计模型请求上限 |
| `budget.issue_minutes` | `120` | 单 Issue 执行时间上限，单位分钟 |
| `budget.max_attempts` | `2` | 最大尝试次数 |
| `budget.max_fix_rounds` | `2` | 最大修复轮数 |

这些是配置默认值，实际模型消耗取决于任务与模型。每日额度耗尽时流程保留检查点等待额度恢复；单 Issue 累计额度耗尽则停止该流程。排队和人工等待不计入 Agent 执行时间，并行 Agent 的活跃时段按并集计时。

若希望先人工确认，再消耗模型或进入开发，可开启：

```yaml
runner:
  require_accept_for_analysis: true
  require_accept_for_development: true
```

两个选项默认均为 `false`。Token 单价可配置为每百万 Token 的价格；价格 `0` 表示未知。

工作区容量另在 YAML 中设置 `runner.workspace_bytes`，默认 `536870912`（512 MiB），最大 8 GiB。工作区使用 tmpfs，会占用 Docker 主机内存及可能的 swap；多任务并行时需预留对应资源。

## 日常使用

1. **创建 Issue**：写明目标、相关路径、约束和可验证的完成标准，建议先从小范围修复开始。
2. **跟进需求**：信息不足时，维护者直接在 Issue 回复补充；开启人工确认时，通过控制台或 `/ai accept` 批准继续。
3. **查看执行**：在控制台查看当前任务、调用消耗、测试证据及审查发现；Issue 和 PR 的 `ai-workflow:` 标签也会同步阶段。
4. **处理例外**：遇到风险授权或失败时，先查看具体提交及原因，再决定继续、暂停或接管。
5. **检查并合并**：确认最终 diff、测试与审查证据后，在 GitHub 上手动合并 PR。

维护者可以在对应 Issue／PR 评论中使用：

| 指令 | 用途 |
| --- | --- |
| `/ai analyze` | 请求需求分析或重新分析 |
| `/ai accept` | 接受待处理的分析请求或规格，按当前阶段继续 |
| `/ai pause` | 暂停工作流 |
| `/ai resume` | 恢复允许继续的工作流 |
| `/ai cancel` | 取消工作流 |
| `/ai takeover` | 转为人工接管 |

## 排查与维护

先使用控制台的 **运行诊断**。需要查看服务日志时，使用与启动相同的环境变量：

```sh
docker compose -p "$ALPHA_PROJECT" -f deploy/compose.alpha.yaml logs --tail=100 control
```

| 现象 | 优先检查 |
| --- | --- |
| 模型未就绪 | Responses 基础地址、Token、开发／审查模型是否完整；是否仍为模拟模式 |
| Issue 没有开始 | App 安装范围、仓库数字 ID、Webhook 投递和分析前人工确认开关 |
| `WAITING_CI` | 受信 CI 执行器及当前候选的签名回执是否就绪 |
| 测试缺少模块或命令 | runner 是否预装了项目依赖及所需工具 |
| 工作区创建或写入失败 | Docker 可用性、tmpfs 支持、工作区容量及 Docker 主机剩余内存 |

备份命令会停止控制器，并确认执行资源已退出；备份包括数据库、控制面数据及配置密钥，完成后可重新启动：

```sh
scripts/alpha-backup.sh "$ALPHA_STATE_DIR/backups/manual-001"
scripts/alpha-up.sh
```

停止当前项目并保留数据：

```sh
docker compose -p "$ALPHA_PROJECT" -f deploy/compose.alpha.yaml stop
```

恢复、升级和镜像管理见[部署与恢复说明](docs/deployment-alpha.md)。登录令牌、模型密钥和 GitHub 私钥均保存在本机配置目录，不应提交到仓库。

## 更多文档

- [隔离部署与运维](docs/deployment-alpha.md)：启动、依赖预热、升级、备份和恢复。
- [可信 CI](docs/ci-evidence.md)：固定 CI 来源、独立重验及签名回执。
- [试点与评估](docs/alpha-fixture.md)：IssueDesk 示例项目和正式评估方法。
- [验收记录](docs/alpha-acceptance.md)：已执行检查、实际结果及未完成项。
- [Alpha 设计计划](docs/alpha-plan.md)、[架构说明](docs/plan/Architecture_v0.4_Reviewed.md)、[领域合同](contracts/README.md)：实现原理与设计约束。
- [旧版运行手册](docs/runbook.md)：根目录 Compose 部署及原有 8080／8081 端口流程。
