# 工单服务生产型评估夹具

这是独立的 Go HTTP + PostgreSQL 工单服务，全部账号及业务数据均为合成数据。它具有真实数据库约束、事务和 HTTP 接口，用于验证开发工作流；不替换旧的 `fixtures/pilot` calculator 回归夹具。这里的“生产型”指业务复杂度，不代表已经通过正式验收或适合直接暴露互联网。

## 启动

从仓库根目录执行 `docker compose -f fixtures/issuedesk/compose.yaml up --build`，浏览器打开 `http://127.0.0.1:8080`。数据卷保留数据库内容，重复启动会执行校验式幂等迁移。

本地已有专用 PostgreSQL 时，设置 `ISSUEDESK_DATABASE_URL` 后运行 `go run ./fixtures/issuedesk/cmd/issuedesk -seed`。Unix socket 示例格式为 `host=/你的/socket/目录 port=5432 user=你的测试用户 dbname=你的专用数据库 sslmode=disable`。不要指向生产数据库。`-migrate-only` 只执行迁移，`-seed` 显式创建六个演示账号及每租户一条合成工单和评论，默认不创建账号。

| 合成令牌 | 租户 | 角色 | 用户编号 |
|---|---|---|---|
| `demo-alpha-requester` | 甲 | 客户 | 101 |
| `demo-alpha-agent` | 甲 | 客服 | 102 |
| `demo-alpha-admin` | 甲 | 管理员 | 103 |
| `demo-beta-requester` | 乙 | 客户 | 201 |
| `demo-beta-agent` | 乙 | 客服 | 202 |
| `demo-beta-admin` | 乙 | 管理员 | 203 |

这些公开令牌仅供本机合成演示。数据库只保存令牌摘要。Web 页面将令牌保存在内存中；刷新后重新输入，不使用 Cookie 或浏览器持久存储。演示环境没有真实账号注册、认证提供商或审计合规集成。

## HTTP 契约

所有 `/api/` 请求使用 `Authorization: Bearer <token>`；JSON 请求使用 `Content-Type: application/json`。未知 JSON 字段、额外 JSON 对象或超过 64 KiB 的请求会被拒绝。时间为 UTC/RFC3339；整数 ID 保持为 JSON 数字。错误格式固定为 `{"error":{"code":"...","message":"简体中文说明"}}`，不暴露数据库错误或连接串。

| 方法与路径 | 行为 |
|---|---|
| `GET /healthz` | 存活检查 |
| `GET /readyz` | 数据库可达检查 |
| `GET /api/me` | 当前用户、租户与角色 |
| `POST /api/tickets` | 创建工单，正文 `title/body/initial_comment`；必须携带 `Idempotency-Key`；201、Location、ETag |
| `GET /api/tickets` | 返回 `items/next_cursor`；支持 `limit`（1–100，默认20）、`cursor/status/q/assignee_id` |
| `GET /api/tickets/{id}` | 读取工单与 ETag |
| `PATCH /api/tickets/{id}` | 更新 `title/body/status/assignee_id/unassign/comment`；需要 `If-Match: "版本号"` |
| `DELETE /api/tickets/{id}` | 管理员软删除；需要 If-Match；成功204 |
| `GET /api/tickets/{id}/comments` | 按评论 ID 升序返回 `items` |
| `POST /api/tickets/{id}/comments` | 新增评论，正文 `body`；需要 If-Match；返回版本递增后的工单 |

客户仅能看见、修改自己创建的本租户工单；客服与管理员可处理本租户所有工单。客户不能分派或删除。客服与管理员只能向本租户客服或管理员分派。不存在、已删除、跨租户或其他客户的工单对读取接口统一返回404。

客服/管理员状态流转为 `open → in_progress → resolved → closed`；`resolved → in_progress` 允许重新处理；`open/in_progress → cancelled` 允许取消。客户只能将自己的 `open` 工单改为 `cancelled`。相同状态可用于普通编辑；关闭/取消不能重新开启。非法流转409；乐观锁冲突409；缺少 If-Match 为428。

创建工单、初始评论与幂等记录在一个事务中提交。幂等键作用域为租户+用户；相同键同请求重试返回原201结果和 `Idempotency-Replayed: true`，不同请求409。工单修改及附带评论原子提交。所有修改、评论、删除都推进版本，两个相同版本的并发更新最多成功一个。

列表按创建时间和 ID 双字段降序 keyset 分页；游标绑定当前用户、租户及过滤条件。同时间记录不会遗漏或重复；新建记录不会插入已经向后翻页的窗口。搜索是不区分大小写的字面子串，`%`、`_`、反斜杠没有通配符含义。过滤结果属于动态数据，翻页期间修改过滤字段可能改变成员集合，不声明跨请求数据库快照隔离。

迁移在单个事务和数据库锁内顺序执行；已应用脚本校验和必须一致。002 创建列表/评论索引，不销毁业务数据。数据与表结构变更不支持自动向下迁移；演示重建应使用独立新数据库。

## 集中验证

公开测试：`go test ./fixtures/issuedesk/...`。可信评估测试不在候选工作区；由评估器在隔离验证副本中注入，并设置 `ISSUEDESK_TEST_DATABASE_URL`。可信测试会创建、删除随机测试 schema，只允许在专用测试数据库运行。数据库用户需有建 schema 权限。完整协议见主仓库 `eval/protocol.md`。
