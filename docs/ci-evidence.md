# 可信 CI 回执签发与验收

`cmd/ci-receipt` 是独立受信执行器。它读取控制器配置、数据库中当前工作流及指定 GitHub Actions run；没有任意 JSON 签名模式，没有从候选读取命令、SHA、输出路径或私钥的入口。实现已进入集中测试；本地结果参见 `alpha-acceptance.md`，真实 GitHub 签发仍需独立验收。

## 输入与信任边界

由维护者在控制器宿主准备匹配 `trusted_ci.receipt_public_key` 的 Ed25519 私钥文件：标准 Base64 的 64 字节私钥编码、普通文件、仅属主权限（0600）。不得把它放入 Git 仓库、不可信 CI checkout、候选工作区或共享挂载。CLI 只读取文件，拒绝符号链接及其他用户可访问权限，错误和结果不输出私钥路径、私钥内容或签名。现有 GitHub App 配置凭据同样仅用于宿主 API 读取，不传入沙箱。

正式调用格式如下；真实 GitHub 签发尚未执行：

```text
ci-receipt --config <控制器配置> --workflow <工作流编号> --run <运行编号> --private-key <宿主私钥文件>
```

候选身份由数据库推导：repo ID、PR、generation、VerificationKey、head、base、test-merge、tree，NotBefore 与现有门禁一致使用 Workflow.CreatedAt。最新 run attempt 由 GitHub API 读取。工作流注册 ID/path、固定来源 commit/blob、当前 PR、事件、仓库、合并父提交、最新 attempt 全部由 `FetchTrustedCIRecord` 再核验。未知、排队、失败、过期 attempt、fork、来源变动都不能签发。

## 执行与持久化

1. 在宿主获取按工作流隔离、与外部隐藏判定共用的 PostgreSQL advisory lock。两种受信执行及恢复不能并行消费同一 Issue 的剩余时间；另一种执行若留下尚未停止或记账的 journal，必须先恢复，不能直接启动新命令。只有取得该锁的执行器能回收对应旧 journal；CI 还要求当前无有效工作流租约且处于 WAITING_CI、AWAITING_MAINTAINER 或 EVALUATION_COMPLETE 检查点，并在持锁后重新读取候选。
2. 先原子保存 `data/ci-executors/<workflow摘要>.json` PREPARING，再创建独立沙箱。每次执行使用独立 run label，并在 journal 绑定候选、受信命令配置和计时身份。
3. 获取固定 test-merge 源码，按 Git 对象格式核验完整 tree。沙箱初始化后再次 Snapshot 对账并封为只读；命令期间不能修改候选，缓存与临时数据库使用独立可写存储。不运行候选提供的初始化步骤。
4. 仅执行控制器 repository.verification 配置命令，以整个命令序列的期限约束执行。网络为 none、无宿主凭据；必要 PostgreSQL 服务仅用私有 Unix socket，候选拿到的仅是本次临时数据库能力。
5. 每个命令都必须真实退出 0 且未超时。结果写受信执行证据；失败退出非零，不签发。候选测试本身仍可能不充分，因此这份回执只证明受信命令确实运行，不能代替隐藏测试和独立审查。
6. StopRun 确认所有专用标签资源停止后才在宿主内存签名。回执 CompletedAt 是独立重验完成时间，必须不早于 GitHub run.UpdatedAt，并不晚于当前时间。
7. 签名后立即执行完整 GitHub 身份校验；事务内再次检查当前工作流代际、控制状态、固定提交、批准 Spec 和 VerificationKey。将回执原子写入 `data/ci-receipts/<workflow>/<key>.json`，执行证据与审计一起提交。提交失败删除本次回执，门禁保持不可用。

文件替换使用同目录 0600 临时文件、fsync、rename 和目录 fsync。文件系统与数据库不是一个分布式事务：崩溃点仍可能留下未完成审计的回执，但其已签名且已实际执行，门禁每次仍独立校验当前身份；恢复重跑补齐审计，不把文件存在视为验收完成。

## 中断与恢复

普通错误、取消或信号都执行专用 label 的 StopRun；清理失败 journal 记为 CLEANUP_PENDING，返回非零。SIGKILL 或主机重启后由下一次同工作流 CLI 在持有签发器互斥锁后清理旧 journal，再重建独立沙箱。它不复用 AgentRun，也不结束其他控制器的 Agent 资源。即使工作流已取消或远端 CI 已变化，也可调用 `ci-receipt --config <配置> --workflow <编号> --recover-only` 或 `RecoverTrustedCIExecutor`，在同一 advisory lock 下只回收专用资源，不签名、不执行候选。控制器若增加统一恢复入口，必须遵循同一 advisory lock；不能仅依据 journal 时间判断可以杀进程。

## 集中验收清单

单元：实际失败/超时停止后续命令、缺命令拒绝、密钥权限/符号链接/公钥匹配、原子回执、完成时间前后约束。集成：精确 test-merge 的真实 PG/Docker 验证、运行中修改 head/base/Spec/profile、同 run 重试、停止失败、签发与数据库失败、进程强杀后的 journal 恢复。远端：固定可信 workflow 来源、受信宿主签发、最终门禁再次重读，均需留存真实证据；已执行范围与待办参见 `alpha-acceptance.md`。

## 非终态等待与预算

本地可信命令全部成功后，控制器先持久化 WAITING_CI、固定 TestedSHA/TestedTreeSHA、Review.Key 和成功命令证据，再读取签名回执。没有回执或远端仍在运行时保持等待，不进入 BLOCKED，也不再次申请模型或重跑测试。独立 CLI 因而能读取已落库的固定候选。

恢复 ticker 在工作流租约释放、所有已启动 Agent 均停止确认后处理 WAITING_CI；事务内以 version、generation、候选和 review key 防止多个控制器重复复苏。它重新核验冻结评估配置、当前命令 profile、哈希证据和成功退出，再核验真实 CI 回执；full/single 接续 REVIEWING，builder_ci 结束其对照评估。候选或配置漂移回 VERIFYING，错误回执保持非 PASS。冻结 review 评估中的机械失败可以继续独立审查，但 VerificationPassed 保持 false，不能授权生产合并。

CI 签发器在启动前和锁准入后检查剩余 Issue 时间；实际快照、创建、命令和停止确认按唯一执行编号幂等累计，锁等待不计入。执行上下文受剩余预算限制，达到时限或时间记账失败都不能签发。每次执行将开始／停止时间持久化到控制层 journal；进程崩溃后先停止确认再补记，重复恢复不重复扣减。停止未确认时不提前封存计时回执。

统一验收阶段已执行 CI 身份及等待的针对性单元用例、独立 PostgreSQL 运行时幂等回归和真实 Docker 集成树／恢复记账测试。证据位于工作区上级 acceptance-evidence/trust-chain-unit-20261013 与 trust-chain-pg-20261013；这些本地检查不代表真实 GitHub 签发或九十次评估已完成。

生产 `ExecuteTrustedCIReceipt` 已在精确 GitHub API mock 配合真实 PostgreSQL／Docker 下验证成功签发、实际命令失败拒签和来源变化拒签，并检查宿主签名、回执落盘、审计及门禁消费。证据在 `acceptance-evidence/ci-executor-engine-docker-001`。外部 Judge 使用独立受信测试进程和私有 Unix HTTP／PG 通道，候选 stdout 只作日志；20 个缺陷／修复候选与防伪造对抗结果见 `acceptance-evidence/judge-external-migration-001`。这些结果仍不包含真实 GitHub 联调。
