# StuHelper Notifications

站内通知模块，采用 Handler → Service → Repository。业务规格与当前实现见 [通知说明](docs/spec/notifications.md)，HTTP 定义见 [OpenAPI](contracts/notifications.openapi.json)。

## 本地检查

使用 Go 1.27.3、Node.js 24.22.1、pnpm 10.33.4。安装锁定依赖并启用 hooks：

```bash
make setup
pnpm run hooks
make generate
bash scripts/check-with-postgres.sh
```

数据库脚本创建独立、仅 Unix socket 可访问的临时 PostgreSQL，退出时清理。可通过 STUHELPER_TEST_PG_BIN 指定 PostgreSQL 程序目录。make check 要求 STUHELPER_NOTIFICATIONS_TEST_DSN 指向隔离测试服务器，测试会创建和删除专属数据库与角色。

## Backend 装配

1. 仓库为 StuHelper/stuhelper-notifications，默认分支 develop。候选在 feat/backend-decisions 分支和 PR #1 维护，集成时登记候选提交与依赖组合。
2. backend 引入模块，迁移身份调用 postgres.Migrate(ctx, db)。该迁移创建 notifications schema，已有通知不会被删除。
3. 单独创建运行身份，授予 schema USAGE、items/preferences 的 SELECT、INSERT、UPDATE、DELETE，revisions 的 SELECT、INSERT、UPDATE，以及 inbox 的 SELECT、INSERT。运行身份不得拥有 schema 或提权能力，也不得更新删除 inbox 或删除 revisions。
4. 使用运行连接构造 postgres.Repository，并调用 CheckRuntime。backend 管理连接池、请求超时、健康检查和连接关闭。迁移身份不进入业务进程。
5. 调用 notifications.New(repo, resolver, config, clock)。Resolver 根据来源模块和目标引用调用所属模块公开接口，按接收人当前权限返回状态。依赖错误返回 error，禁止默认判定可访问。
6. 将 httpapi.Handler 挂载至 /api/v1/notifications 及其子路径，Authenticate 回调复用平台认证，返回稳定用户标识。Cookie 认证由宿主同时执行 CSRF 防护。
7. 标题键、参数和目标类型必须符合 [标题目录](docs/spec/notifications.md#标题键目录)。向 backend relay 注入 notifications.Consumer。relay 保留生产者的 source、id、type、subject、revision，映射为 Event 后调用 Consume。Consume 成功后才能调用提供方 ConfirmOutbox；超时重试沿用原事件和映射。
8. 宿主按配置周期调用 Service.Cleanup(ctx)，每次一个有上限的批次。关闭服务时取消任务并等待退出。宿主配置任务超时，保留可再次执行的进度。
9. 负责人同步 stuhelper-next 规格的实现状态、模块登记和候选锁文件，并更新 Web 的 SDK 快照及实际通知页面。合并前验证真实候选组合。

## 配置

以下为 Go Config 字段，环境变量映射由 backend 负责。

| 配置 | 条件与用途 |
| --- | --- |
| Retention | 正时长，按创建时间保留通知 |
| AggregationWindow | 正时长且不超过 Retention，互动聚合窗口 |
| CleanupBatch | 1 至 10000，每次清理最大条数 |
| ResolveTimeout | 正时长，每次目标状态查询超时 |

backend 还须提供独立运行数据库连接、迁移连接、清理周期、清理超时和认证回调。模块没有内置凭据或隐含保留期限。

Resolver 顺序处理当前页目标，每页最多 100 次调用。宿主应设置整个请求的总超时，Resolver 必须响应 context 取消。profiles 缺失只影响未来外部渠道偏好，不影响当前能力。

当前 SDK 包含 Go HTTP 客户端和 TypeScript 类型，分别由 oapi-codegen 与 openapi-typescript 生成。Go Consumer 是内部模块接口，不开放网络投递入口。消费输入和 relay 处理约定见 [事件消费](docs/spec/notifications.md#事件消费)。
