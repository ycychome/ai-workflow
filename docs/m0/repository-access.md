# 测试仓库接入诊断

项目仓库：`https://github.com/ycychome/ai-workflow`。测试仓库：`https://github.com/ycychome/test-repo-for-ai-test`。日期：2026-10-12。本记录更新为本次恢复环境的实际观测；交接包中的受限 HTTPS 结果不是当前结果。

## 当前观测

| 检查 | 结果 |
| --- | --- |
| GitHub 连接读取项目仓库，账号 zymooll | 成功；仓库 ID 1415164547，默认分支 main；API 返回 push/maintain/admin 权限 |
| 项目仓库 HTTPS Git | 成功；恢复前 main 为 9bd7b671fb348f2007156772a5683bf7cd67fcda |
| GitHub 连接读取测试仓库 | zymooll 与 KawakazeNotFound 均返回 API 404；无法获得仓库内容/ID/默认分支 |
| 测试仓库 HTTPS Git | GitHub 403，Write access to repository not granted |
| cloud_environment.environment_status | observations_current=true，observed_phase=running，network mode=unrestricted，state=unknown；不表示策略已被确认 enforced |
| 当前实例 HTTPS 请求 https://api.github.com/meta | HTTP 200；此前 HTTPS 连接阻塞已解除 |
| 当前 `/etc/codex/network-policy.json` | TCP domains=[]，ip_ranges=[] |
| 配置的 TCP 代理 `proxy:8088` | 建立代理连接时 ConnectionRefusedError，尚未发送 SSH 认证 |
| 用户提供的 OpenSSH 私钥 | 本地解析成功，公钥指纹与交接记录一致；尚未到达 GitHub，不能证明远端 SSH 授权有效 |
| 测试仓库项目、Issue、PR、Checks 写入 | 尚未进行 |

当前可以通过 HTTPS 恢复和交付项目代码。测试仓库的 HTTPS/连接器授权仍未成功；404 不能单独区分仓库不存在与当前身份不可见。配置的 TCP 代理本身拒绝连接，尚未发送 SSH 认证；不能把它解释为公钥无效。本次未修改系统网络策略，也未绕过代理。

## 已准备

- Tiny Greeting Service：`fixtures/repository/`，7 个业务测试通过。
- M0 任务：`fixtures/m0-task.json`，仅修改 `greeting.txt`。
- `greeting-service` 受信任验收：业务回归加独立 HTTP 校验，已接入覆盖账本和 Merge Gate，在本地 Docker/PostgreSQL 链路验证通过。
- SSH 公钥文件位于代码仓库之外：`/workspace/.m0-ssh/test-repo-ed25519.pub`；指纹 `SHA256:s/t8TWaWiXxVtzdtBPy8ejYqO+XCyJLjtnsF87JU4VA`。私钥不在仓库中，不写入 Worker 工作区。
- 交接包的 M0 源码已恢复到项目仓库的独立分支，原有 `docs/plan/` 内容保持原样；本次重新执行真实 PostgreSQL/Docker/race 验收，28 个主测试、67 个测试记录通过，无失败或测试跳过，另有 7 个 Python 业务测试通过。

## 继续远端实测所需配置

1. 确认测试仓库地址及所选账号对它的访问权限，并向连接器/环境的 GitHub 授权开放该仓库。当前 HTTPS 已连通，无需再次解决 HTTPS 通用出站问题。
2. 若继续使用 SSH，需恢复配置的 TCP 代理并允许 `github.com` 的 SSH 出站，同时确认公钥已加入有仓库写权限的账号或设置为可写 deploy key。仅恢复 SSH 不能解决控制进程的 API 授权。
3. 为 GitHub 原生 Issue/PR/Checks 及指定 App 来源配置专用 GitHub App。控制进程还需要 `.env.example` 中的 App ID、installation ID、测试仓库 ID、bot login、webhook secret 和环境内 RSA App 私钥路径。用户提供的 Ed25519 OpenSSH 私钥不是 GitHub App 的 RSA 私钥。不要在聊天中粘贴密钥。

恢复连通后先读取现有仓库和开发约定，再加入项目基线；已有文件不直接覆盖，最终 PR 合并由 Maintainer 完成。只有真实 GitHub 验收通过后才更新 M0 的外部退出状态。
