# 第六轮任务 A 交付记录

通知模块位于独立本地仓库，工作分支 feat/backend-decisions。未配置远程仓库，因此没有 origin/develop 可刷新，也没有远程 CI 结果。负责人需创建公开仓库 StuHelper/stuhelper-notifications，并设置默认分支 develop。

## 本地验证

隔离 PostgreSQL 18.6 实例中完成 make check：生成一致性、仓库规范、SQL 分层、场景登记、文档、gofmt、go vet、go test -race、go build 和 Actions 校验通过。数据库实例使用独立临时目录和 Unix socket，检查结束后清理。

数据库测试覆盖未登录、他人通知隔离、游标分页、单条与全部已读幂等、12 个并发请求去重、聚合窗口、互动偏好、不可关闭类型、事务回滚、取消、清理上限和期限边界。

迁移连续执行两次通过。运行身份不能访问其他 schema、创建业务表或更新删除去重记录。迁移身份被运行身份检查拒绝。目标权限撤销与查询失败分别返回 inaccessible 和 unavailable。

工作流使用 ubuntu-26.04。actionlint 1.7.12 尚未内置该标签，沿用其他模块的标签声明配置；没有关闭工作流检查。

## HTTP 路径

| 方法 | 新增路径 |
| --- | --- |
| GET | /api/v1/notifications |
| GET | /api/v1/notifications/unread-count |
| POST | /api/v1/notifications/{id}/read |
| POST | /api/v1/notifications/read-all |
| GET、PUT | /api/v1/notifications/preferences |

契约为 contracts/notifications.openapi.json。Go 与 TypeScript SDK 已生成。内部投递使用公开 Go Sender 接口，通知投递不提供客户端 HTTP 入口。

## 负责人收尾

backend 装配步骤、配置项和数据库权限见 [README](README.md)。需要注入认证、目标权限查询和来源模块投递接口，并安排迁移、连接池及清理任务。

受写入范围限制，stuhelper-next 原规格未修改。当前实现说明保存在 [模块规格](docs/spec/notifications.md)，负责人需将实现状态同步至中央规格，更新模块登记和候选组合，再验证 Web 消费者。

尚未进行 backend 联调、Web 真正页面接入、远程 CI、发布、部署或真实账号验收。外部通知渠道未实现。去重记录永久保留，回收策略需要先确定来源模块最大重投期限。
