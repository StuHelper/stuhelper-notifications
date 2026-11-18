---
type: spec
status: Accepted
updated: 2026-11-19
---

# 通知模块实现说明

本模块实现站内通知。来源模块通过公开 Go Consumer 接口投递，HTTP API 仅允许登录用户访问本人通知。notifications.inbox 能力依赖可选模块 identity，profiles 缺失不影响站内通知；reviews、resources、verification、moderation 缺失时仅对应目标返回 unavailable。

六个用户 HTTP 操作在请求人的账号已注销时统一返回 403 和错误码 `forbidden`。OpenAPI 和生成的客户端均声明该响应。

通知包含类型、来源模块、目标引用、标题翻译键与字符串参数、创建时间、最近动态时间（updated_at）、已读时间和聚合数量。调用方只提交目录规定的业务编号，不能放入正文或个人敏感信息。客户端负责八语言翻译。

列表按最近动态时间（updated_at）和 UUID 倒序，使用不透明游标，每页默认 30 条，上限 100 条。列表支持按分箱（interaction、review、verification、system）、通知类别（type）与仅未读（unread_only）过滤，游标绑定当前筛选条件。未读统计同时返回总未读数、按分箱计数与按类别计数，聚合项算一条。

聚合更新最近动态时间并保留首次创建时间。旧游标可按原创建时间继续读取。

首页和续页使用独立查询。并发新增记录可在刷新首页后读取。

列表通过 Resolver 查询目标当前权限。accessible 表示可访问，inaccessible 表示目标删除、下架或权限不足，仅子查询超时或下游依赖失败且父请求上下文仍有效时降级为 unavailable。若父级请求 context 已取消或超时，立即中断剩余目标解析并返回错误（HTTP 503），不伪装为 200 OK。目标页仍须执行自己的授权。

单条已读与全部已读仅更新 read_at IS NULL 的未读行，保持首次已读时间且不重写已读元组。其他用户的通知返回 404。只读查询通知偏好不开启写事务或写入默认行。接口不接受用户 ID，身份由宿主认证回调提供。

通知 ID 与游标中的 ID 均使用标准带连字符的 UUID。格式无效时返回 400 invalid_input。

路径必须匹配已声明的路由。非单段 {id}/read 子路径返回 404，未知或重复查询参数返回 400，宿主权限拒绝返回 403。204 响应省略 Content-Type。

批量已读可按分箱与通知类型分别限定范围。升级生成的 Go SDK 后，全量已读调用传入 `nil` 查询参数。现有客户端继续使用无查询参数的 HTTP 请求。

互动通知默认开启，细分为评论回复（reply_enabled）和点赞（like_enabled）两个独立开关，并保留总状态（interaction_enabled）兼容旧客户端读写。业务处理、安全和账号通知始终接收。偏好修改与投递按接收人串行提交，关闭对应开关后开始的投递不会写入该类互动通知。

互动聚合要求接收人、来源模块、目标、标题键和参数一致，并且原通知未读、处于配置窗口内。消费在获取收件人锁后采样当前时间，窗口下界按原通知创建时起计算，容忍并发交错与多实例时钟微偏。聚合增加 count，保留原创建时间；已读后新事件产生新通知。

标记已读保留最近动态时间。清理仍按创建时间判断保留期。

事件按 (source, id) 唯一。重复输入返回原结果，同一身份对应不同映射返回 idempotency_conflict。被偏好拒收的事件也保存结果，开启偏好后重投仍不入站。

通知、inbox 和已处理修订在同一数据库事务写入。清理按创建时间删除超过保留期限的通知，每次最多 CleanupBatch 条。并发清理跳过已锁定行。未到保留期限边界的通知继续保留。

inbox 去重键和修订记录不随通知条目清理。inbox 保存来源、事件 ID、subject、revision、映射摘要、消费时间及投递详情，不保存标题参数。独立维护连接按 90 天保留期限调用 CleanupInboxDetails 有界清空过期 result 详情并保留去重键，过期重放返回 Ignored 且不重新入站。

业务账号注销调用 CloseAccount(ctx, owner, closureID)，在同一事务中写入 closed_owners 栅栏并删除该账号的 items 与 preferences。相同 closureID 重复调用幂等成功，不同 closureID 返回 idempotency_conflict。

已注销账号的列表、未读数、已读和偏好接口均返回 403 forbidden。上游延迟事件投递遇到已注销收件人时记录 account_closed 抑制原因，不写入 items 或重建 preferences 行。

MaintenanceStats 查询过期未删通知数、最早过期时间及 inbox 与 revisions 估算行数，不含个人信息。Config.Observer 可选记录消费分布、目标解析降级与清理批次指标。

当前只实现站内渠道。邮件、QQ、客户端推送、免打扰及提醒提前量未接入，不对外声明可用。

## 标题键目录

OpenAPI 的 TitleKey 是封闭枚举。x-title-catalog 提供键与类型、目标和参数的机器可读映射，scripts/generate.mjs 据此自动生成各 TitlePayload 子模式并校验一致性。Consumer 在写入前校验同一目录，目录外键、类型或目标不符、缺少或额外参数均返回 invalid_input。

下表必填参数只允许 1 至 64 位 ASCII 字母、数字及点、下划线、冒号、短横线，首位为字母或数字。参数是显示用业务编号，不能填姓名、学号、证件号、联系方式或正文。驳回、下架、撤销与补件键可附带封闭枚举参数 reason_code（见 OpenAPI ReasonCode），禁止传入自由文本或审核员备注。

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

target.id 是对应模块对象的原始 ID，title_params 是用于显示的业务编号。客户端按 target.kind 和 target.id 构造本地页面路由，禁止把参数当作 URL。仅 accessible 状态允许跳转，其他状态显示不可访问或暂不可用。

| target.kind | 对象与页面 |
| --- | --- |
| review | reviews 的评价详情 |
| comment | reviews 的评论对象；启用名称解析且可访问时附带所属根评价 context_id |
| resource | resources 的资料详情 |
| resource_removal_request | resources 的下架申请或投诉回执，进入本人资料申请结果 |
| report | moderation 的举报回执，进入本人举报处理结果 |
| moderation_appeal | moderation 的处置申诉复核记录，进入本人申诉结果 |
| verification_application | verification 的学生认证 Application，进入本人申请详情 |
| teacher_application | verification 的教师认证 Application，进入本人教师申请详情 |
| verification_credential | verification 的学生 Credential，进入本人认证记录 |
| verification_removal_request | verification 的 RemovalRequest，进入本人撤销或删除申请结果 |
| verification_operation | verification 的 Operation，进入本人实名核验操作结果 |
| group_membership | groups 的本人群准入与成员资格记录，进入我的群页面 |
| storage_contribution | storage-contribution 的本人存储贡献记录，进入存储贡献面板 |
| billing_order | billing 的本人充值或退款订单记录，进入账户明细页面 |

系统通知的三个键均指向目录内已有业务对象，使用 reference_id 展示引用编号。目录不定义任意系统正文、外部链接或尚不存在的公告对象。来源模块与 Resolver 装配须能查询该对象，否则返回 unavailable。

HTTP 契约版本为 0.3.1，模块版本为 0.5.1。生产者使用目录键，客户端按目录提供八语言文案。

## 事件消费

notifications 是事件消费者，不提供 OutboxChannel、PendingOutbox 或 ConfirmOutbox。backend 读取提供方 outbox 后调用公开消费接口：

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
| Subject | 原 subject，最长 512 字节；互动事件须使用独立互动实体标识（如评论 ID 或目标加点赞者关系键），禁止多条评论或点赞共用根内容 subject |
| Revision | 原 data.revision，正 int64 |
| Recipients | 1–20 个稳定用户标识，每项最长 256 字节；提交列表为空或超过 20 项时拒绝 |
| Type | 通知类别，必须与标题目录相符 |
| Target | 通知目标 kind 与 id |
| TitleKey、TitleParams | 标题目录规定的键和显示用编号 |

这是可信 Go 映射接口，不接收原始封套或任意网络请求。用户标识及 Event 各字符串字段必须为合法 UTF-8，且禁止包含控制字符或首尾空白，违规输入在入库前返回 invalid_input。relay 校验 CloudEvents 1.0、JSON 内容类型、获准来源与事件类型、学校范围和接收人，按业务结果生成完整通知映射。不得把失败中间状态映射为业务成功通知。

每个源事件一次提交全部收件人，共享通知类别、目标和标题参数。模块先检查提交列表长度，再去重并按用户标识排序。全部收件人的通知、聚合结果、偏好拒收结果、修订和 inbox 在同一事务提交。

消费按 (source, subject) 记录最大已处理修订，互动事件同时绑定对应标题键，跨互动类型复用同一 subject 或点赞直接使用目标 ID 作为 subject 返回 invalid_input。新 ID 的 revision 小于或等于已处理修订时返回 Ignored，不写通知、不推进修订。

点赞事件按 (owner, source, subject) 去重。同一用户在点赞、取消后再次点赞递增修订时，返回 duplicate_interaction 抑制原因，不重复累加聚合数量，也不在原通知已读后重复生成新通知。

升级时，迁移按旧 inbox 回执关联收件人和通知条目，补录已投递的点赞并绑定互动类型。同键重放先校验原摘要，再返回已有结果。已清空详情的旧回执仍可抑制重复投递。

首次建立点赞去重表时，若旧回执详情已过期、有效接收人的通知条目缺失或同一 subject 曾使用不同互动类型，迁移会回滚。此时需依据来源记录核对最小去重事实，避免重复通知。已注销账号不补录个人记录。

需要重建时，维护方先核对来源投递记录，在同一迁移事务内准备完整的 `like_subjects(owner, source, subject)` 和 `revisions.interaction_title_key`。表定义及联合主键见本仓 schema.sql。运行身份对去重表只需 SELECT、INSERT。

随后再执行 Migrate 安装注销清理触发器。已过期的 inbox.result 保持为空。独立 PostgreSQL 用例验证了缺少证据时回滚，以及从来源事实重建后的重放和再次点赞。

重复 ID 优先返回已保存的 Result。收件人顺序改变或重复条目增减不改变请求摘要，实际收件人集合改变则返回冲突。事件锁、修订行锁和按用户标识排序的偏好锁依次获取，避免重叠收件人集合反向加锁。

Result.Recipients 按用户标识排序，每项包含 Recipient、ID 和 SuppressionReason。成功入站时 ID 非空且原因为空；偏好拒收原因是 interaction_disabled；账号已注销原因是 account_closed；旧修订原因是 stale_revision 并设置 Result.Ignored。

偏好拒收、已注销抑制和旧修订忽略均为成功消费。偏好拒收与已注销抑制仍推进修订，同批其他收件人照常入站。任何收件人写入或 inbox 写入失败时，整批通知、聚合数量、偏好行和修订都回滚。

Consume 返回 nil error 后，relay 才可 ConfirmOutbox。确认丢失时重新读取并消费同一事件，返回原结果。取消或超时先按原事件重试，不根据错误猜测事务是否已提交。

relay 对提供方频道 LISTEN，通知到达后读取最多 100 条未发送事件。重连后立即补读，并每 60 秒兜底读取。失败从 5 秒指数退避，单次最长 1 小时，累计 24 小时后进入待处理队列。

inbox 的 result JSON 保存整批消费结果，由独立维护身份按 90 天期限清理。运行身份可读写 items、preferences，可查询、插入和更新 revisions，对 inbox、closed_owners、like_subjects 仅有 SELECT、INSERT。CheckRuntime 核验所需权限和禁止的修改权限。

空的单收件人候选 inbox 可原位迁移，保留运行权限和已有通知。有记录的候选 inbox 会拒绝自动升级并保留数据，因为部分记录缺少收件人。负责人须依据原始事件恢复收件人与规范化摘要后再升级，不能直接清空去重历史。

结果类型如下：

```go
type RecipientResult struct {
    Recipient         string
    ID                string
    SuppressionReason string
}
type Result struct {
    Recipients []RecipientResult
    Ignored    bool
}
```

## 验收覆盖

| 场景 | 实现与测试 |
| --- | --- |
| NT-01 | 互动投递、聚合、偏好拒收、并发重投 |
| NT-02 | 单条与全部已读、重复操作、未读数 |
| NT-03 | 业务处理、安全及账号通知不受互动偏好影响 |
| NT-04 | 目标不可访问及依赖超时状态 |
| NT-05 | HTTP 未登录拒绝、他人通知隔离、严格偏好输入 |
| NT-09 | 多收件人去重、部分拒收、整批回滚、并发重投、重叠收件人锁顺序、迁移保护 |
| NT-08 | 乱序、旧修订、跨接收人重复冲突、原结果回放、事务回滚与锁等待取消 |
| NT-07 | 全部目录键投递、非法组合拒绝且不写入、回复和点赞聚合数量 |
| NT-06 | 投递失败回滚、保留期限边界、不同目标与接收人不聚合、inbox 投递详情到期回收 |
| PC-07 | 业务账号注销原子清理个人通知与偏好、关闭栅栏拦截延迟投递与读取 |

实际测试函数在 module.yaml 的 scenario_tests 登记。隔离数据库测试还验证游标分页、清理上限、迁移重复执行与运行身份权限。

## 读取时补充内容名称

原 TitleKey 和编号参数保持不变。请求 include_content_names=true 时，review、comment、resource 目标在 title_params 中增加 content_name 与 content_name_state。名称来自所属模块的当前公开接口，不接受事件生产者提交，也不写入通知表或聚合指纹。

content_name_state 为 available 时，content_name 为当前评价标题或资料标题。回复采用根评价标题，空标题采用所属课程或教师目录名称。名称最多 256 个 Unicode 字符，不含控制字符。

对象删除、下架或失去阅读权限时，状态为 inaccessible，名称为空。依赖失败或名称无效时为 unavailable，名称为空。前端分别使用本地化的“内容不可用”和“名称暂不可用”，避免重用缓存名称。

宿主实现 BatchResolver，一次处理本页去重后的目标，并按所属模块使用批量接口。旧 Resolver 继续兼容；未提供名称解析的旧宿主返回名称暂不可用。通知分页、计数、消费幂等与原编号字段保持原行为。

读取参数由 x-title-catalog 的 read_parameters 声明，生产者参数仍由 parameters 与 optional_parameters 封闭校验。旧客户端继续使用编号参数，新客户端按名称状态选择可读标题或占位文案。

comment 目标在 target_status 为 accessible 时一并解析所属根评价 ID 作为 context_id，不可访问或标识异常时不返回，供新客户端构造一步直达路由。

旧 HTTP 请求和 Go List 保留原封闭参数结构。新客户端通过 include_content_names=true 或 Go ListWithContentNames 启用名称。参数只接受单个 true 或 false，未指定时为 false。
