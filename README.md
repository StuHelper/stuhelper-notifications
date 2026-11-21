---
type: index
status: Accepted
updated: 2026-11-22
---

# StuHelper Notifications

更新日期：2026-11-22。

站内通知模块拥有通知条目、互动偏好和投递去重数据。来源模块通过公开 Go Consumer 投递，用户通过 HTTP API 访问本人通知。见 [通知规格](docs/spec/notifications.md)、[OpenAPI](contracts/notifications.openapi.json) 和 [模块清单](module.yaml)。

## 本地检查

安装 `.go-version`、`.node-version` 和 `package.json` 指定的工具链及 PostgreSQL，以普通用户执行：

```bash
make setup
bash scripts/check-with-postgres.sh
```

脚本在独立 Unix socket 上启动临时 PostgreSQL，执行 `make check` 并在退出时清理。通过 `STUHELPER_TEST_PG_BIN` 可指定 PostgreSQL 程序目录。直接运行 `make check` 时，`STUHELPER_NOTIFICATIONS_TEST_DSN` 必须指向隔离测试服务器。

检查使用 `GOWORK=off`，覆盖生成一致性、仓库规则、文档、SQL 边界、场景登记、Go race 测试、构建和工作流配置。修改 OpenAPI 后执行 `make generate` 同步 SDK。需要暂存区检查时执行 `pnpm run hooks`。

## Backend 装配

1. 用迁移身份调用 `postgres.Migrate(ctx, db)`，在事务内更新 `notifications` schema。先核对历史已读数据及回执关联。见 [通知迁移](docs/spec/notifications.md#通知保留与迁移)和[回执迁移](docs/spec/notifications.md#回执保留与迁移)。
2. 以独立运行身份构造 `postgres.Repository` 并调用 `CheckRuntime`。调用方通过 `notifications.UnavailableReason(err)` 读取脱敏原因。宿主管理连接池、请求超时、健康检查和关闭过程。
3. 调用 `notifications.New(repo, resolver, config, clock)`。宿主按收件人权限解析目标，使用 `BatchResolver` 一次解析本页去重后的目标和标题。只实现 `Resolver` 时最多顺序发起 100 次调用。解析器应响应上下文取消，宿主设置请求总超时。
4. 按契约挂载 `httpapi.Handler`。宿主的 `Authenticate` 回调提供稳定用户标识，Cookie 认证同时执行 CSRF 防护。见 [HTTP 操作](docs/spec/notifications.md#http-操作)。
5. 在 relay 中注入 `notifications.Consumer`。按标题目录映射事件，保留来源身份和修订，将同一事件的全部收件人一起提交。消费成功后确认来源 outbox。见 [事件消费](docs/spec/notifications.md#事件消费)。
6. 宿主周期调用 `Service.Cleanup(ctx)`，每次清理一个有上限的批次，停止服务时取消任务并等待退出。业务账号注销调用 `Service.CloseAccount(ctx, owner, closureID)`，原子删除个人通知、偏好并写入关闭记录。

### 数据库授权

| 身份 | 权限 |
| --- | --- |
| 运行 | schema `USAGE`，`items`、`preferences` 的 `SELECT`、`INSERT`、`UPDATE`、`DELETE` |
| 运行 | `revisions` 的 `SELECT`、`INSERT`、`UPDATE` |
| 运行 | `inbox`、`closed_owners`、`like_subjects` 的 `SELECT`、`INSERT` |
| 维护 | 清理回执详情使用独立连接，对 `inbox` 具备选择清理批次及更新 `result` 所需权限 |
| 迁移 | 管理本模块 schema 与迁移对象 |

`CheckRuntime` 拒绝可提权、可在本模块 schema 建对象、拥有 schema 或表的运行身份，并检查去重记录和关闭记录的修改权限。迁移连接由部署工具管理。

### 配置

Backend 使用 `read_retention_seconds` 和 `unread_retention_seconds` 配置期限，默认分别为 15552000 秒和 31536000 秒。下表为模块的 Go `Config` 字段。

| 字段 | 条件与用途 |
| --- | --- |
| `ReadRetention` | 大于零且最多 180 日，首次标为已读后起算，Backend 默认 180 日 |
| `UnreadRetention` | 大于零且最多 365 日，从创建起算，Backend 默认 365 日 |
| `AggregationWindow` | 正时长且不超过 `UnreadRetention`，限制互动聚合窗口 |
| `CleanupBatch` | 1 至 10000，每批清理条数上限 |
| `ResolveTimeout` | 正时长，限制每次目标解析调用 |
| `Diagnostics` | 可选回调，接收脱敏操作名与固定原因码 |
| `Observer` | 可选观察接口，接收消费、目标解析和清理的汇总指标 |

## 实施与验证

### 当前生产组合

StuHelper v0.1.31 已启用站内通知。Backend v0.1.46 固定本模块的 Go 发布标签 v0.1.2。该组合提供通知列表、未读统计、已读与互动总开关，目标由装配了对应适配器的模块按当前权限解析。见 [StuHelper v0.1.31 组件清单](https://github.com/StuHelper/stuhelper/releases/download/v0.1.31/COMPONENTS.json)。

### 源码与持续集成

develop 提供分箱筛选、按类别统计、回复与点赞独立偏好、最近动态排序及单条删除。Go 发布标签和 OpenAPI 版本分别记录，消费方按锁定源码生成 SDK。两套契约的版本号均为 0.3.1。通知条目按已读与未读分别到期，PostgreSQL 场景覆盖 v0.1.2 旧表迁移、期限边界、主动删除、并发清理和失败重试。

develop 源码 `b9be6e7` 已通过隔离 PostgreSQL 的 Notifications checks。测试覆盖用例见 [场景登记](docs/spec/notifications.md#验收场景)。见 [CI 运行](https://github.com/StuHelper/stuhelper-notifications/actions/runs/44604785405)。

### 待验证

develop 的新增路由、偏好字段、通知标题和回执保留需要在升级后端与客户端后做实际组合验证。首次部署点赞去重迁移时，待核对既有回执和来源记录能否完整重建去重事实。账号注销与回执到期清理的现行生产运行结果待验证。

邮件、QQ、客户端推送、免打扰及提醒提前量仍待实现和验证。
