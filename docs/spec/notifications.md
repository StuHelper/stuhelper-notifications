---
type: spec
status: Accepted
updated: 2026-11-22
---

# 站内通知

## 数据归属与依赖

notifications 拥有站内通知、互动偏好、投递回执、修订和账号关闭记录。来源模块通过 Go `Consumer` 投递。目标的内容、当前可见性和读取权限由目标所属模块管理。

| 清单声明 | 模块或能力 | 装配影响 |
| --- | --- | --- |
| 必需依赖 | platform | 提供宿主运行平台 |
| 可选依赖 | identity | `notifications.inbox` 的能力依赖，支撑已登录用户的站内访问 |
| 可选依赖 | reviews、resources、verification、moderation | 支撑相应目标状态与名称查询，是否可用由宿主解析器决定 |
| 可选依赖 | profiles | 当前站内逻辑无需个人资料 |
| 对外能力 | `notifications.inbox`、`notifications.consume` | 分别用于用户 HTTP 操作和可信 Go 事件消费 |

清单记录宿主启用模块时的要求。标题目录定义允许投递的目标。各类型目标能否跳转取决于宿主是否装配相应的解析器，以及收件人能否访问当前对象。

## HTTP 操作

接口仅处理登录用户的本人通知，用户标识来自宿主认证。账号已注销时返回 403 `forbidden`。

| 方法 | 路径 | 行为 |
| --- | --- | --- |
| GET | `/api/v1/notifications` | 按条件分页列出未到期通知 |
| GET | `/api/v1/notifications/unread-count` | 返回未到期的未读总数及分箱、通知类型统计 |
| POST | `/api/v1/notifications/{id}/read` | 标记一条未到期的通知已读 |
| POST | `/api/v1/notifications/read-all` | 按可选分箱、通知类型标记未到期的通知已读 |
| DELETE | `/api/v1/notifications/{id}` | 删除本人一条通知 |
| GET、PUT | `/api/v1/notifications/preferences` | 读取和更新互动通知偏好 |

通知包括类型、来源模块、目标引用、标题翻译键和参数、创建时间、最近动态时间（`updated_at`）、已读时间和聚合数量。客户端按八种语言展示标题。

列表按最近动态时间和 UUID 倒序，默认每页 30 条，上限 100 条。支持按分箱、通知类型和 `unread_only` 过滤。游标绑定筛选条件。并发新投递和前移的聚合项可在刷新首页后读取。

分箱包括互动（`interaction`）、内容审核（`review`）、认证（`verification`）和系统（`system`）。通知类型包括互动、业务、安全和账号。未读统计中的聚合项按一条计数。

已读操作仅更新未读且未到期的行，保留已有的首次已读时间及最近动态时间。单条标记遇到已到期条目返回 404，批量标记跳过这些条目。

本人可直接删除数据库中仍存在的通知，成功时返回 204。重复删除、目标不存在或目标属于其他用户时返回 404。删除保留消费回执、修订和点赞去重记录，原事件重放不会恢复条目。

通知 ID 与游标中的 ID 均使用标准带连字符的 UUID。ID 格式无效、查询参数未知或重复时返回 400。未声明的路径返回 404，204 响应省略 Content-Type。

列表、未读统计和批量标记已读使用完整标题目录，并按各操作的筛选条件处理通知。

## 互动偏好与聚合

评论回复（`reply_enabled`）与点赞（`like_enabled`）默认开启。GET 和 PUT 均返回两项当前值。PUT 可提交一项或两项，只更新提交的字段。

业务处理、安全和账号通知始终接收。偏好修改与投递按接收人串行提交，关闭开关后开始的投递按新偏好处理。读取偏好时直接返回当前值或默认值。

升级时，数据库按原总开关和单项选择保留实际投递偏好。总开关关闭的账号迁移为两项均关闭。迁移在同一事务删除原列，后端与客户端按 OpenAPI 0.5.0 同步切换。

互动聚合要求接收人、来源模块、目标、标题键和参数相同，且原通知未读、处于配置窗口内。持有收件人锁后采样时间，窗口以首次创建时间为准。聚合保留首次创建时间，增加计数并更新最近动态时间。已读后的新互动产生新条目。

## 标题键目录

OpenAPI 的 TitleKey 是封闭枚举。x-title-catalog 提供键与类型、目标和参数的机器可读映射，scripts/generate.mjs 据此自动生成各 TitlePayload 子模式并校验一致性。Consumer 在写入前校验同一目录，目录外键、类型或目标不符、缺少或额外参数均返回 invalid_input。

必填投递参数引用业务对象，长度为 1 至 64 位。首位为 ASCII 字母或数字，后续可包含点、下划线、冒号和短横线。姓名、学号、证件号、联系方式和正文不得用作参数。

允许附带 `reason_code` 的标题及可用原因由 OpenAPI 的标题目录和 `ReasonCode` 声明。审核备注只保留在来源模块。

互动文案使用顶层 count 作为复数参数。Consumer 不传 count，首次为 1，聚合由仓库累加。客户端将 count 与 title_params 组合传给公共国际化接口，按语言选择复数形式。

| 键 | 类型 | target.kind | title_params 必填参数 |
| --- | --- | --- | --- |
| notifications.interaction.reply | interaction | review, comment, resource | content_id |
| notifications.interaction.like | interaction | review, comment, resource | content_id |
| notifications.content.approved | business | review, comment, resource | content_id |
| notifications.content.rejected | business | review, comment, resource | content_id |
| notifications.content.removed | business | review, comment, resource | content_id |
| notifications.content.draft_expiring | business | review | content_id |
| notifications.resource_removal.approved | business | resource_removal_request | request_id |
| notifications.resource_removal.rejected | business | resource_removal_request | request_id |
| notifications.report.approved | business | report | report_id |
| notifications.report.rejected | business | report | report_id |
| notifications.appeal.approved | business | moderation_appeal | appeal_id |
| notifications.appeal.rejected | business | moderation_appeal | appeal_id |
| notifications.student_verification.approved | business | verification_application | application_id |
| notifications.student_verification.rejected | business | verification_application | application_id |
| notifications.student_verification.supplement_required | business | verification_application | application_id |
| notifications.identity_verification.approved | account | verification_operation | operation_id |
| notifications.identity_verification.rejected | account | verification_operation | operation_id |
| notifications.verification_revocation.approved | account | verification_removal_request | request_id |
| notifications.verification_revocation.rejected | account | verification_removal_request | request_id |
| notifications.verification_deletion.approved | account | verification_removal_request | request_id |
| notifications.verification_deletion.rejected | account | verification_removal_request | request_id |
| notifications.teacher_verification.approved | business | teacher_application | application_id |
| notifications.teacher_verification.rejected | business | teacher_application | application_id |
| notifications.teacher_verification.supplement_required | business | teacher_application | application_id |
| notifications.teacher_homepage_request.approved | business | teacher_homepage_request | request_id |
| notifications.teacher_homepage_request.rejected | business | teacher_homepage_request | request_id |
| notifications.student_verification.revoked | account | verification_credential | credential_id |
| notifications.group_membership.grace_expiring | business | group_membership | group_id |
| notifications.storage_contribution.exit_completed | business | storage_contribution | contribution_id |
| notifications.billing.topup_review_required | business | billing_order | order_id |
| notifications.billing.refund_completed | business | billing_order | order_id |
| notifications.billing.refund_rejected | business | billing_order | order_id |
| notifications.system.notice | business | review, comment, resource, resource_removal_request, report, moderation_appeal, verification_application, teacher_application, verification_credential, verification_removal_request, verification_operation, group_membership, storage_contribution, billing_order | reference_id |
| notifications.system.security | security | review, comment, resource, resource_removal_request, report, moderation_appeal, verification_application, teacher_application, verification_credential, verification_removal_request, verification_operation, group_membership, storage_contribution, billing_order | reference_id |
| notifications.system.account | account | review, comment, resource, resource_removal_request, report, moderation_appeal, verification_application, teacher_application, verification_credential, verification_removal_request, verification_operation, group_membership, storage_contribution, billing_order | reference_id |

## 目标路由约定

客户端按 `target.kind` 和 `target.id` 构造目标页路由。仅 `accessible` 状态允许跳转，其他状态显示不可访问或暂不可用。标题参数通过国际化接口用于展示，禁止将参数当作 URL。

| target.kind | 对象与页面 |
| --- | --- |
| review | reviews 的评价详情 |
| comment | reviews 的评论对象，可访问且取得根评价 ID 时附带 `context_id` |
| resource | resources 的资料详情 |
| resource_removal_request | resources 的下架申请或投诉回执，进入本人资料申请结果 |
| report | moderation 的举报回执，进入本人举报处理结果 |
| moderation_appeal | moderation 的处置申诉复核记录，进入本人申诉结果 |
| verification_application | verification 的学生认证 Application，进入本人申请详情 |
| teacher_application | verification 的教师认证 Application，进入本人教师申请详情 |
| teacher_homepage_request | catalog 的教师主页更正申请，进入本人申请详情 |
| verification_credential | verification 的学生 Credential，进入本人认证记录 |
| verification_removal_request | verification 的 RemovalRequest，进入本人撤销或删除申请结果 |
| verification_operation | verification 的 Operation，进入本人实名核验操作结果 |
| group_membership | groups 的本人群准入与成员资格记录，进入我的群页面 |
| storage_contribution | storage-contribution 的本人存储贡献记录，进入存储贡献面板 |
| billing_order | billing 的本人充值或退款订单记录，进入账户明细页面 |

系统通知的三个键均指向目录内的业务对象，使用 `reference_id` 显示引用编号。目标页打开时独立执行授权。

目标删除、下架、权限不足或未知时，解析器返回 `inaccessible`。依赖未装配、子查询超时或结果缺失时对应目标标记 `unavailable`。父请求取消或超时会中断剩余解析，并返回 HTTP 503。

## 事件消费

backend 从来源模块的 outbox 读取事件，并调用 notifications 的公开消费接口。

```go
type Consumer interface {
    Consume(context.Context, Event) (Result, error)
}

func (s *Service) Consume(ctx context.Context, event Event) (Result, error)
```

| Event 字段 | 映射规则 |
| --- | --- |
| Source | 原 CloudEvents source，模块标识，最长 80 字节 |
| ID | 原事件 id，最长 256 字节，重试保持不变 |
| EventType | 原事件 type，如 moderation.result.v1，最长 128 字节 |
| Subject | 原 subject，最长 512 字节；互动事件使用独立实体标识，例如评论 ID 或目标与点赞者的关系键 |
| Revision | 原 data.revision，正 int64 |
| Recipients | 1 至 20 个稳定用户标识，每项最长 256 字节 |
| Type | 通知类别，必须与标题目录相符 |
| Target | 通知目标 kind 与 id |
| TitleKey、TitleParams | 标题目录规定的键和显示用编号 |

relay 负责校验 CloudEvents 1.0、JSON 内容类型、获准来源、事件类型、学校范围和收件人。Consumer 接收已转换的可信 Go 事件，校验用户标识及字符串字段为合法 UTF-8，且无控制字符和首尾空白。非法输入返回 `invalid_input`。

每个源事件一次提交全部收件人，共享通知类别、目标和标题参数。模块先检查提交列表长度，再去重并按用户标识排序。全部收件人的通知、聚合结果、偏好拒收结果、修订和 inbox 在同一事务提交。

消费按 `(source, subject)` 记录最大修订。互动实体的 subject 绑定一种互动标题键，具有冲突标题键的交付返回 `invalid_input`。点赞新交付须使用独立关系键。新 ID 的修订若不大于已处理修订，记录 `Ignored` 回执，保留既有通知和修订。

点赞事件按 (owner, source, subject) 去重。同一用户在点赞、取消后再次点赞递增修订时，返回 duplicate_interaction 抑制原因，不重复累加聚合数量，也不在原通知已读后重复生成新通知。

以 `(source, id)` 去重。回执详情仍在时，重复投递返回原结果。收件人重排或重复条目增减不会改变摘要，映射改变时返回 `idempotency_conflict`。事务依次锁定事件、修订行及按用户标识排序的收件人。

`Result.Recipients` 按用户标识排序，每项包含 `Recipient`、`ID` 和 `SuppressionReason`。成功入站时返回通知 ID，抑制时返回以下原因。

| 原因 | 条件 |
| --- | --- |
| `interaction_disabled` | 接收人关闭了该类互动通知 |
| `account_closed` | 接收人已注销 |
| `stale_revision` | 事件修订已过时，消费结果同时置 `Ignored` |
| `duplicate_interaction` | 此收件人已收到同一点赞关系的通知 |

抑制和旧修订忽略均为成功消费。有效新修订即使被偏好、账号注销或点赞去重抑制，仍推进修订并保存本次结果。收件人写入或回执写入失败时，整批通知、聚合、偏好行和修订都回滚。

`Consume` 成功后，relay 才能在来源模块执行 `ConfirmOutbox`。确认丢失、取消或超时时使用同一事件重试，由消费回执判断此前的结果。

## 通知保留与迁移

站内通知首次标为已读后保留 180 日。未读期间自创建起最多保留 365 日。以 UTC 时间戳为起点，每日按 24 小时计算，到达截止时间即到期。最近动态时间不影响保留期。主动删除无需等待截止时间。

清理按到期时间顺序删除，每批最多 `CleanupBatch` 条，跳过被并发事务锁定的行。失败的批次回滚，后续批次重新选择当前到期的行。监控记录汇总数量和脱敏错误原因。回执键和修订记录独立保留。

已发行版本用 `read_at` 存储首次已读时间，空值表示未读。升级建立两类到期索引，保留已有已读时间与未读状态。旧数据中只有具备独立已读证据的条目需要修复缺失时间。

对于另有证据确认已读、但缺失 `read_at` 的导入或恢复数据，使用最早可证实条目已经读过的时间，例如首次已读事件、带可信时间的已读快照或首次核对时间。该时间须不早于条目创建时间。

维护方先在隔离导入环境核对行和证据时间，对已确认的行填入 `read_at`，保留已有值。未找到有效证据时保留原状态，供维护方核对。

## 回执保留与迁移

回执保存来源、事件 ID、subject、revision、映射摘要、消费时间和逐人投递结果。独立维护身份按已确认的 90 天保留期计算截止时间，再调用 `CleanupInboxDetails` 分批清空 `result`。回执键和摘要继续抑制重复投递。详情过期后，重放返回 `Ignored`。

业务账号注销在同一事务删除本人的通知、偏好及点赞去重数据，并写入 `closed_owners` 阻止重新投递和读取。相同 `closureID` 重试幂等成功，不同 `closureID` 返回 `idempotency_conflict`。

`MaintenanceStats` 返回过期条目数量、最早过期条目的创建时间及回执、修订表的估算行数。见 [数据库授权](https://github.com/StuHelper/stuhelper-notifications/blob/develop/README.md#数据库授权)。

空的旧单收件人 inbox 可以原位升级。含历史记录时迁移会拒绝并保留数据，需依据来源事件重建收件人集合与规范化摘要后再升级。

升级时，迁移按旧 inbox 回执关联收件人和通知条目，补录已投递的点赞并绑定互动类型。同键重放先校验原摘要，再返回已有结果。已清空详情的旧回执仍可抑制重复投递。

首次建立点赞去重表时，若旧回执详情已过期、有效接收人的通知条目缺失或同一 subject 曾使用不同互动类型，迁移会回滚。此时需依据来源记录核对最小去重事实，避免重复通知。已注销账号不补录个人记录。

需要重建时，维护方先核对来源投递记录，在同一迁移事务内准备完整的 `like_subjects(owner, source, subject)` 和 `revisions.interaction_title_key`。表定义及联合主键见本仓 schema.sql。运行身份对去重表只需 SELECT、INSERT。

随后执行 `Migrate` 安装注销清理触发器。已过期的 `inbox.result` 保持为空。

## 读取时补充内容名称

读取通知时，允许名称解析的 review、comment、resource 目标在 `title_params` 中返回 `content_name` 与 `content_name_state`。所属模块的公开接口按当前权限提供名称。通知条目和聚合指纹只保存投递参数。

content_name_state 为 available 时，content_name 为当前评价标题或资料标题。回复采用根评价标题，空标题采用所属课程或教师目录名称。名称最多 256 个 Unicode 字符，不含控制字符。

对象删除、下架或失去阅读权限时，状态为 inaccessible，名称为空。依赖失败或名称无效时为 unavailable，名称为空。前端分别使用本地化的“内容不可用”和“名称暂不可用”，避免重用缓存名称。

宿主使用 `BatchResolver` 一次处理本页去重后的目标，并按所属模块调用批量接口。仅实现 `Resolver` 的宿主仍返回目标状态，可访问目标的名称状态为 `unavailable`。

OpenAPI 的 `x-title-catalog` 用 `read_parameters` 声明读取时补充的字段。生产者按 `parameters` 与 `optional_parameters` 提交投递参数。

## 教师主页申请通知

Catalog 在审批通过或拒绝时持久化结果事件。站内通知仅发送给原申请账号，标题参数仅保存业务编号 `request_id`。打开详情时，Catalog 检查账号、当前认证和目录教师归属。

该类通知属于业务类型和审核分箱，纳入列表、未读统计及批量标记已读。

## 验收场景

| 场景 | 预期结果 |
| --- | --- |
| NT-01 | 投递互动通知，按偏好与聚合窗口处理，并发更新保留首次创建时间 |
| NT-02 | 按条件分页、统计未读及标记已读，单条删除只影响本人通知 |
| NT-03 | 关闭互动开关后仍接收业务处理、安全与账号通知 |
| NT-04 | 目标不可访问与依赖失败各有稳定状态，父请求取消则停止解析 |
| NT-05 | HTTP 拒绝未登录、越权或无效请求，保留原偏好或通知 |
| NT-06 | 投递失败回滚，通知在已读和未读边界到期。清理受批次限制，并发跳过锁行，失败可重试。主动删除立即生效，回执详情另行清理 |
| NT-07 | 投递匹配封闭标题目录，读取可按权限补充当前标题 |
| NT-08 | 同一事件重放返回原结果，忽略旧修订，重复点赞不会重复通知 |
| NT-09 | 同一事件的收件人去重，逐人应用偏好，整批原子写入 |
| NT-11 | 教师主页决定只通知申请人，纳入分页、未读统计和批量标记已读 |
| PC-07 | 账号注销清理个人通知与偏好，并阻止延迟投递 |

实际测试函数在 [module.yaml](https://github.com/StuHelper/stuhelper-notifications/blob/develop/module.yaml) 的 `scenario_tests` 登记。生产与独立 CI 情况见 [实施与验证](https://github.com/StuHelper/stuhelper-notifications/blob/develop/README.md#实施与验证)。
