# Alpha 工单夹具与离线评估接入

新增 `fixtures/issuedesk` 是同主仓库 Go module 的独立 HTTP + PostgreSQL 服务，使用已固定的 pgx v5.7.6，没有 npm 构建依赖。原 `fixtures/pilot` 保留。此阶段只实现代码、测试和确定性素材，不运行模型、不创建 GitHub 任务、不触发 CI。

公开入口：`issuedesk.Migrate(context.Context,*pgxpool.Pool)`、`issuedesk.Seed(context.Context,*pgxpool.Pool)`、`issuedesk.NewApp(*pgxpool.Pool) http.Handler`。CLI 路径为 `fixtures/issuedesk/cmd/issuedesk`，环境变量 `ISSUEDESK_DATABASE_URL` 支持 TCP 或 Unix socket。合成数据由 `-seed` 显式启用；HTTP 默认监听回环地址。API 与角色矩阵见 `fixtures/issuedesk/README.md`。

目录：

- `fixtures/issuedesk/`：模型、事务 Store、HTTP、嵌入式中文 HTML/JS/CSS、迁移、公开测试及 Docker/Compose。
- `eval/tasks/`：十个冻结 Issue 与可见验收条件，不含补丁或隐藏判定说明。
- `eval/variants.json`：十个精确替换变体，只供可信候选生成器使用。
- `eval/trusted/harness/`：独立 Go module 的可信 HTTP／数据库判定，不导入候选 Go 包。
- `eval/trusted/driver/main.go.txt`：候选服务驱动，只暴露应用与固定迁移接口，不含隐藏断言。
- `eval/trusted/issuedesk_hidden_test.go.txt`：v1 历史素材，已退出可信清单和导出路径，不再用于正式判定。
- `eval/manifest.json`：源文件摘要、题面摘要、判定摘要、三组各三次协议与十对待人工校核标签。
- `eval/cli.py`：只实现 prepare、trusted-bundle、matrix、summarize 四个离线命令；不会执行测试或模型。

运行器接入顺序：导出非敏感的有效执行配置 → matrix 冻结九十项 → prepare 为各轮生成独立 defect 候选 → 只挂载候选给实际执行器/Fake → 固定产物快照 → 为候选服务与 trusted-bundle 创建两个独立只读工作区，通过私有 Unix socket 连接 → 记录命令/退出码/摘要 → 写结果 JSONL → summarize。候选无需主仓库其他业务代码，复制根 go.mod/go.sum 后可独立编译服务。运行器若需 Git，可在独立候选内初始化仓库；离线 CLI 不做 Git 操作。

Fake 接入时可以在隔离候选中应用已有 fixture 修复动作或返回结构化产物，必须仍经过同一可信验证。不得直接把 `fix` 标签作为通过证据。真实模型运行需要另行授权并由调度器落实共享预算；本工具只读运行器导出的预算，不硬编码 example 数字。

集中验收清单：

1. 主仓库公开测试与 fixture HTTP 冒烟；专用 PostgreSQL 下 baseline 的十项可信测试全过。
2. 十个 defect 均能编译，分别被目标可信断言拒绝；十个 fix 通过；编译或环境失败不计作有效缺陷。
3. 重建同一候选两次内容摘要相同；候选不包含 `eval/`、隐藏测试、labels 或 variants。
4. 对 matrix/汇总做缺失记录、重复记录、配置漂移、错误证据摘要、零价格、超预算与待人工标签回归。
5. Docker/Compose 在本机独立端口验证；六个角色、双租户以及 Web 创建/修改/评论流程验证。

验证命令和记录格式见 `eval/protocol.md`。在实际集中验收执行前，不得称这些检查已通过。

## 可信隐藏判定执行闭环

`Engine.JudgeEvaluation` 读取管理员提供的清单路径，按试验冻结的 manifest 原始字节 SHA 校验版本、可信文件和题面。候选的 go.mod/go.sum 必须匹配冻结依赖，不能借判定流程更换模块身份或工具链。隐藏测试仅复制到独立无凭据、无网络沙箱；PostgreSQL 使用该次执行专属 Unix socket。Builder、Reviewer 及其普通工作区均不获得这份测试源码。

判定目标是已持久的固定 test-merge 和 Git tree；注入前、初始化后各自计算真实树。实际执行 `go test -json`，要求精确目标测试出现 run/pass、整个包 pass、退出码为 0 且未超时。TestMain 直接退出、测试被跳过、缺少目标测试或失败日志截断都不能成为 PASS。结果先经 StopRun 确认，再复核当前工作流、冻结配置及 GitHub PR 的 head/base/merge/tree/parents。

成功执行后的 PASS/FAIL 保存真实命令、日志和身份为 `TRUSTED_EVALUATION_JUDGMENT` 证据，结果通过审计和原子文件持久化。API 和评估执行器只从该 Judgment 填写 tests_passed；公开测试绿色本身不是真值。预算耗尽、取消、基础设施错误、候选漂移或清理未确认属于未判定，不能宣称通过，也不能当作经隐藏测试确认的缺陷标签。

同一候选已完成结果可从哈希证据验证后复用，避免请求重试重复执行。独立进程锁、专用资源标签和 journal 支持崩溃恢复；实际执行时间使用唯一 run ID 幂等累加到现有 Issue 总预算，排队不计费，旧执行至停止确认之间的时间不会因重启而清零。

以上为已编写的实现与测试；本轮没有运行隐藏判定、测试、编译或实际 CLI。生成标签仍需人工校核，不能把机器 PASS/FAIL 或尚未执行的测试当作人工真值。


v2 判定修复了候选 TestMain/init 伪造 Go 测试输出的同进程边界问题。candidate stdout/stderr 只作为附属日志，判定只来自独立 harness 进程的真实退出码与目标测试事件；两方 PID、源码卷与命令输出隔离，共享的仅是私有 HTTP socket 和专用 PostgreSQL socket。
