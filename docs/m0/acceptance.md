# M0 实现与验收记录

验证日期：2026-10-12。依据：`Architecture_v0.4_Reviewed.md` §11 的 M0 与 PR-1/PR-2/PR-3。

## 交付内容

| 批次 | 实现 |
| --- | --- |
| PR-1 协议与模拟器 | Workflow/Attempt、generation/lease_epoch/state_version、VerificationKey、覆盖义务与 Gate、严格 Check 映射 |
| PR-2 GitHub 纵向接入 | 原始字节 HMAC、仓库/安装白名单、持久化 Inbox、业务幂等、权限 API、固定 patch Draft PR、评论和 Check 的 Outbox 发布及未知结果对账 |
| PR-3 隔离与 Runner | Docker Process Runner、独立 clone、非 root 沙箱、整容器取消、产物范围与模式校验、干净 Git 集成、可信 Git Data 发布、孤儿清理、证据归档 |

这些是实施批次对应项，当前工作区没有为每个批次分别创建外部 PR。

## 已实际执行的验证

本地使用 Go 1.25.3、真实 PostgreSQL 17 容器、Docker 28.4.0 及固定 digest 的 Alpine 镜像。GitHub 使用 HTTP 仿真服务器，模拟服务端先写入再关闭连接的响应丢失，而非直接返回预设成功结果。

最新本地结果：28 个主测试、67 个测试项（含子测试和父测试记录）通过；零失败、零测试跳过，race 检查通过。问候服务另有 7 个 HTTP 业务回归测试通过。`go vet ./...` 和控制进程构建通过。

本次从 `m0-spike-source.zip` 恢复后，在新的非 root 云环境中重新执行以上验收，使用 Go 1.25.3、Docker 28.4.0、真实 PostgreSQL 17 及固定 digest 的 Alpine/Python 镜像，得到相同的 28/67 通过结果。Go JSON 日志 SHA-256 为 `e58dbd65d5b80b3ceeb5b532a98b08ee749dc5304225ee462e060547a3292c7e`，本地证据位于工作区之外的 `/workspace/.m0-evidence/go-test.jsonl`。三个没有测试文件的包由 Go 标为 package-level skip；没有具体测试用例跳过。

新增的 `greeting-service` 验证配置已在真实 Python 容器中运行：固定 patch 之后通过 HTTP 验证默认、姓名、中文问候及错误响应；故意让服务忽略配置、同时把候选测试变成空测试集时，独立验收仍失败并发布阻塞结论。HTTP/兼容性义务纳入覆盖账本并关联持久化运行证据。源码和测试文件对 Builder 保持不可修改；同 SHA 更换 Profile 必须重新验证。

```sh
M0_TEST_DATABASE_URL='postgres://m0:m0@127.0.0.1:55432/m0?sslmode=disable' \
M0_TEST_DOCKER=/usr/local/bin/docker go test -race -count=1 ./...
go vet ./...
make build
```

| 架构验收项 | 本地覆盖 | 范围 |
| --- | --- | --- |
| T01 | 同 delivery 重投十次；不同 delivery 对同 Issue；处理失败/租约恢复 | PostgreSQL + 签名 Webhook |
| T02 | 旧 synchronize payload 不回滚当前快照 | HTTP 仿真；控制层查询当前 GitHub 对象 |
| T03/T04 | PR/评论创建成功但响应丢失；恢复后没有重复对象 | HTTP 仿真 + PostgreSQL + Docker |
| T05/T08 | 16 个消费者争抢一个 Attempt；过期、重派、旧发布租约拒绝 | PostgreSQL 并发 + race |
| T06/T07 | PAUSING 撤销旧 writer；未知远端写入阻止 PAUSED；暂停后认领旧 PR | 状态事务 + 仿真；容器整进程取消另测 |
| T09 | 空/无效 fixed patch 不释放代码依赖 | 实际文件采集与 Git 集成 |
| T12 | `.git/config`、refs、objects 不共享；采集不执行 Worker Git 配置 | 两个实际 clone |
| T13 | PR 响应丢失后人工 push；发布停止并保留人工 SHA | HTTP 仿真，无 force push 通路 |
| T14/T15 | 所有 VerificationKey 字段变化失效；head/base 更新；同 SHA 重审复用 Check | 领域测试 + HTTP 仿真 |
| T16/T17 | 超时、未验证义务、空覆盖、重复覆盖、无证据不 PASS | Gate + 容器超时 |
| T18/T19 | 仅 PASS 映射 success；无 neutral/skipped/stale；拒绝假来源同名 Check | Adapter 仿真；原生规则行为待实测 |
| T22/T23 | 伪造 marker、未授权人、机器人命令、评论编辑、引用指令 | HTTP/命令测试；权限采用 maintain/admin |
| T24 | 凭据/宿主 socket 不可见；无网络、非 root、根目录不可写 | 实际容器执行 |
| T25 | 处理租约恢复、丢失初始 Webhook 的 Issue 发现、遗漏 base push 的周期对账 | PostgreSQL + HTTP 仿真 |
| T26 | 恢复/重新执行不重置总 Attempt 预算；每仓库准入上限 | PostgreSQL |
| T28 | Issue 关闭、PR 转 Draft 停止执行；PR 未合并关闭不等于完成；只有真实 merged 快照进入 MERGED | HTTP 仿真；重新打开需显式重新准入 |

上述仅覆盖 M0 相关语义，不表示设计中的全部 28 项验收完成。多任务代码依赖/冲突（T10/T11）、修复轮次与反证（T20/T21）、候选篡改复杂 CI（T27）、完整重规划与真实 Agent 成本边界属于后续阶段。

## M0 外部退出条件

用户已指定项目仓库 `https://github.com/ycychome/ai-workflow` 和测试仓库 `https://github.com/ycychome/test-repo-for-ai-test`。项目仓库可访问，M0 源码已恢复。Tiny Greeting Service 项目及固定 patch 任务已准备，并完成独立 HTTP 验收接入。当前 HTTPS GitHub API 已连通，但测试仓库 HTTPS Git 返回 403，两个已连接 GitHub 账号读取测试仓库均返回 404；SSH TCP 代理仍拒绝连接。提供的 OpenSSH 私钥已在仓库外解析验证，但不具备替代 GitHub App 凭据的能力。目前尚未向测试仓库写入项目、Issue 或 PR，原生门禁验收状态仍为待验证。详情见 `repository-access.md`。

| 退出条件 | 状态 |
| --- | --- |
| 固定 patch 假 Agent 端到端 | 本地已验证 |
| 重复/丢失响应不会产生不可控重复 | HTTP 故障注入已验证；真实 GitHub 仍待联调 |
| 取消拒绝旧结果 | 状态/租约/容器已验证 |
| 选定并执行一个真实 Runner | Docker Process Runner 已实际执行；M1 模型 Runner 未接入 |
| 一个 GitHub 专用测试仓库 | 已指定；夹具与接入步骤已提供，远端访问及 App 配置仍待完成 |
| GitHub 必需检查正确阻塞 | API 映射已验证；真实分支保护、指定 App 来源及同 SHA 行为待验证 |

因此当前可交付 M0 实现与本地 Spike 结果，**不能将 M0 的真实 GitHub 退出验收标为完成，也不能开放真实 Issue 自动开发**。

## 已知限制

- 原生 GitHub 合并界面与 Spec/Policy 更新不具备跨系统瞬时原子性；管理员 bypass 的边界仍需实测。
- 固定 patch 只允许修改 `greeting.txt`，不是任意仓库的通用 Agent；Issue 文本不会改变已接受的固定验收条件。文件 Profile 读取两个文件，服务 Profile 读取四个文件并独立检查实际 HTTP 行为。
- 沙箱使用 Linux 容器，不代表已验证 VM 级隔离；服务源码及回归测试只在无凭据容器执行，出站网络全部关闭，HTTP 验收通过容器内回环地址进行。
- 一仓库一个 Supervisor；有界 Inbox 重试后进入 dead_letter，Outbox 未知写入持续对账并阻塞停止确认，需人工处理长期远端故障。
- M0 的 global budget 是总 Attempt 次数，不是模型费用硬上限；暂不支持 replan/deep-review/规则豁免指令。
