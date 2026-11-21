---
type: guide
status: Accepted
updated: 2026-11-21
---

# 工程约定

更新日期：2026-11-21。

遵循 [主工作区工程约定](https://github.com/StuHelper/stuhelper/blob/develop/AGENTS.md)。

- 本模块拥有站内通知、互动偏好和消费回执。来源模块通过公开事件消费接口投递，目标模块按收件人的当前权限解析可见性。标题参数只接收目录规定的业务引用。规则见 [通知规格](docs/spec/notifications.md)。
- 先维护 [OpenAPI](contracts/notifications.openapi.json) 的操作与标题目录，再用 `make generate` 更新 SDK。新增关键业务场景时，在 [模块清单](module.yaml) 的 `scenario_tests` 登记实际测试函数。
- SQL 留在 `postgres/`。使用独立 PostgreSQL 验证运行身份权限、幂等、并发和迁移。测试夹具与诊断使用合成或脱敏数据。检查命令见 [README](README.md#本地检查)。
