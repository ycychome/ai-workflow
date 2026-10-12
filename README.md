# AI Workflow · M0 Spike

基于 [架构设计 v0.4](docs/plan/Architecture_v0.4_Reviewed.md) 的最小实现。当前使用固定 patch 的假 Builder 和固定验证命令的 Reviewer；最终合并由 Maintainer 执行。

已实现的通路：GitHub Webhook → 验签与持久化 Inbox → 授权 Issue → 独立 clone + Docker Runner → 产物校验与干净集成 → Outbox → Draft PR → 独立验证 → `workflow/merge-gate`。

**本地 PostgreSQL/Docker 和 HTTP 故障模拟验收已执行；真实 GitHub App、必需检查来源及分支规则实测仍待专用测试仓库配置。M0 的外部退出条件尚未全部关闭。** 详见 [验收记录](docs/m0/acceptance.md)。

## 本地验证

依赖：Go 1.25、Git、可访问本机 Unix socket 的 Docker，以及非 root 的控制进程用户。生产运行还需要 PostgreSQL 17 和 GitHub App。Go 依赖固定在 `go.mod`/`go.sum`；未复制参考项目代码。

```sh
go test -race ./...
go vet ./...
make build
```

没有设置集成环境变量时，PostgreSQL 和 Docker 用例明确显示为 SKIP；上述普通测试不代表全部 M0 验收通过。运行真实持久化和容器测试：

```sh
docker pull alpine@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
docker pull python@sha256:2d9aefe2fef018a7eb2c13064c89c71929800fd2e5dccdbf52ea5da5bb8d929a
docker run -d --name m0-postgres \
  -e POSTGRES_USER=m0 -e POSTGRES_PASSWORD=m0 -e POSTGRES_DB=m0 \
  -p 127.0.0.1:55432:5432 \
  postgres@sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24
docker exec m0-postgres pg_isready -U m0
M0_TEST_DATABASE_URL='postgres://m0:m0@127.0.0.1:55432/m0?sslmode=disable' \
M0_TEST_DOCKER=docker make test-integration
```

测试为每个 PostgreSQL 用例创建独立 schema，结束时删除；勿将 `M0_TEST_DATABASE_URL` 指向生产库。控制进程启动时执行幂等 schema 创建。CI 自动运行数据库、容器、端到端、race 和构建检查。

## GitHub 测试仓库接入

1. 专用测试仓库已选为 `ycychome/test-repo-for-ai-test`，测试项目为 `fixtures/repository/` 中的零依赖 Python 问候服务。将夹具目录内容（包括 `.github`）提交到 `main`。使用 `.env.example` 的 `greeting-service` 验证配置，读取 `greeting.txt`、`README.md`、`app.py` 和 `test_app.py`；Builder 仅允许修改 `greeting.txt`。测试任务定义在 `fixtures/m0-task.json`。
2. 安装专用 GitHub App，授予 Metadata 读取以及 Contents、Issues、Pull requests、Checks 的写权限；无需 Actions 写权限或管理权限。订阅 Issues、Issue comment、Pull request、Push 事件。
3. 配置 HTTPS Webhook 到 `/webhooks/github`，设置至少 32 字节的随机 webhook secret。复制 `.env.example` 为 `.env` 并填入配置；私钥放在工作区之外，只供控制进程读取。App 安装令牌由控制进程通过 JWT 自动换取。
4. 将环境变量载入进程后运行 `bin/control`。程序不自动读取 `.env`，也不需要模型密钥。`/healthz` 检查数据库连通性。
5. Maintainer 为测试 Issue 添加 `ai:m0` 标签。控制进程会创建 Draft PR，提交固定 patch 并运行独立验证。Issue 正文不提供 shell、网络权限或动态工具。
6. 按 [GitHub 实测步骤](docs/m0/github-live-validation.md) 配置必需检查和预期 App 来源，验证阻塞与同 SHA 重审。Maintainer 最终手动将 PR 标记为 ready 并合并；机器人不会合并、部署或覆盖人工提交。

`greeting-service` 在 Python 容器里运行 7 个业务回归测试，并执行控制二进制内置的独立 HTTP 验收：健康检查、默认/姓名/中文问候、非法参数、404 和 405。五项必需覆盖义务全部完成后才可 PASS；候选自带测试成功不能替代实际 HTTP 行为。`VERIFICATION_PROFILE=greeting-file` 保留最小文件夹具验证；更换 Profile、命令或镜像会使同 SHA 的旧门禁身份失效。

SSH 公钥只提供 Git push/pull 身份，不提供 REST/Checks API 权限。云环境网络和仓库授权的当前阻塞详见 [仓库接入诊断](docs/m0/repository-access.md)。

命令只接受具有 GitHub `maintain` 或 `admin` 权限的人类新建独立评论：`/ai status`、`/ai pause`、`/ai resume`、`/ai cancel`、`/ai takeover`。评论编辑、机器人命令、引用和自然语言中的指令不会执行。同一评论经不同 delivery 重投也只执行一次。

## 运行边界

M0 选定的实际执行器是 `Docker Process Runner`，接口包含 Capabilities、Start、Events、Status、Cancel、CollectArtifacts；不支持原会话 Resume，恢复会创建新的 Attempt。M1 再接模型执行器，不能把本次固定命令测试当成模型适配器测试。

每仓库只运行一个 Supervisor，由 PostgreSQL advisory lock 保护；数据库租约和 CAS 仍防止旧消费者推进状态。容器使用独立 clone、非 root 用户、无网络、只读根文件系统、资源/进程限制、默认 seccomp、去除 capabilities 和 no-new-privileges。唯一可写宿主挂载是该 Attempt 的源码副本；无 Docker socket、SSH Agent 或 GitHub 凭据。控制进程是可信进程，持有 App 凭据及宿主 Docker 权限。

Worker 的 `.git/config` 不会进入产物采集或发布；采集直接读取受限文件字节、拒绝符号链接、删除、额外路径及模式变化。集成在另一份干净仓库中进行。发布使用 Git Data API，保存预期 commit SHA，不执行项目脚本，不进行 force push。

数据和效果至少一次处理；不存在跨 PostgreSQL/GitHub 的 exactly-once 保证。Outbox 将不确定结果保存为 `unknown`，重试先对账 bot 身份、对象关系、action 标识和预期 SHA。暂停首先撤销 generation，容器停止确认和在途写入对账均完成后才成为 PAUSED。已被 GitHub 接受的操作不会被暂停自动回退。

私有运行证据保存于 `WORKSPACE_ROOT/evidence`，数据库仅存索引和摘要哈希。部署时持久化并备份该目录及数据库，保留目录的 0700 权限，按组织要求设置保留期限；M0 不提供自动清理或多副本共享存储。进程恢复先移除本仓库孤儿容器，再重领未完成执行和 Inbox/Outbox，并周期查询当前 GitHub 快照。

尚不开放任意 Fork、外部恶意代码或真实需求自动开发。`greeting-service` 的源码只在无凭据容器执行，受信任验证脚本固定在控制二进制中；容器仍不等于恶意多租户安全边界。完整 Dev/Review DAG、多 Worker、Skeptic、需求变更审批及费用计量属于后续里程碑。
