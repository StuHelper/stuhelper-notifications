---
type: spec
status: Accepted
updated: 2026-11-09
---

# 通知模块实现说明

本模块实现站内通知。来源模块通过公开 Go Consumer 接口投递，HTTP API 只允许当前登录用户读取和修改本人通知。profiles 为可选依赖，缺失时不影响站内通知。

通知包含类型、来源模块、目标引用、标题翻译键与字符串参数、创建时间、已读时间和聚合数量。调用方只提交目录规定的业务编号，不能放入正文或个人敏感信息。客户端负责八语言翻译。

列表按创建时间和 UUID 倒序，使用不透明游标，每页默认 30 条，上限 100 条。游标不是数据库快照，并发新增通知需刷新首页。未读数统计通知项，聚合项算一条。

列表通过 Resolver 查询目标当前权限。accessible 表示可访问，inaccessible 表示目标删除、下架或权限不足，unavailable 表示查询失败或依赖不可用。目标页仍须执行自己的授权。

单条已读保持第一次已读时间。全部已读更新事务执行时属于本人的通知。其他用户的通知返回 404。接口不接受用户 ID，身份由宿主认证回调提供。

互动通知默认开启，用户可关闭。业务处理、安全和账号通知始终接收。偏好修改与投递按接收人串行提交，因此偏好关闭后开始的投递不会写入互动通知。

互动聚合要求接收人、来源模块、目标、标题键和参数一致，并且原通知未读、处于配置窗口内。窗口从原通知创建时起计算，边界不聚合。聚合增加 count，保留原创建时间；已读后新事件产生新通知。

事件按 (source, id) 唯一。重复输入返回原结果，同一身份对应不同映射返回 idempotency_conflict。被偏好拒收的事件也保存结果，开启偏好后重投仍不入站。

通知、inbox 和已处理修订在同一数据库事务写入。清理按创建时间删除超过保留期限的通知，每次最多 CleanupBatch 条。并发清理跳过已锁定行。未到保留期限边界的通知继续保留。

inbox 和修订记录不随通知清理。inbox 保存来源、事件 ID、subject、revision、映射摘要及原结果，不保存标题参数。记录长期增长，回收需要明确上游最大重投期限，否则旧事件可能重新入站。

当前只实现站内渠道。邮件、QQ、客户端推送、免打扰及提醒提前量未接入，不对外声明可用。

## 标题键目录

OpenAPI 的 TitleKey 是封闭枚举。x-title-catalog 提供键与类型、目标和参数的机器可读映射，Notification 的条件 schema 校验每种组合。Consumer 在写入前校验同一目录，目录外键、类型或目标不符、缺少或额外参数均返回 invalid_input。

下表参数全部必填，只允许 1 至 64 位 ASCII 字母、数字及点、下划线、冒号、短横线，首位为字母或数字。参数是显示用业务编号，不能填姓名、学号、证件号、联系方式或正文。格式校验不能识别所有敏感编号，生产者须保证数据来源正确。

互动文案使用顶层 count 作为复数参数。Consumer 不传 count，首次为 1，聚合由仓库累加。客户端将 count 与 title_params 组合传给公共国际化接口，按语言选择复数形式。

| 键 | 类型 | target.kind | title_params 必填参数 |
| --- | --- | --- | --- |
| notifications.interaction.reply | interaction | review, comment, resource | content_id |
| notifications.interaction.like | interaction | review, comment, resource | content_id |
| notifications.content.approved | business | review, comment, resource | content_id |
| notifications.content.rejected | business | review, comment, resource | content_id |
| notifications.content.removed | business | review, comment, resource | content_id |
| notifications.resource_removal.approved | business | resource_removal_request | request_id |
| notifications.resource_removal.rejected | business | resource_removal_request | request_id |
| notifications.report.approved | business | report | report_id |
| notifications.report.rejected | business | report | report_id |
| notifications.student_verification.approved | business | verification_application | application_id |
| notifications.student_verification.rejected | business | verification_application | application_id |
| notifications.identity_verification.approved | account | verification_operation | operation_id |
| notifications.identity_verification.rejected | account | verification_operation | operation_id |
| notifications.verification_revocation.approved | account | verification_removal_request | request_id |
| notifications.verification_revocation.rejected | account | verification_removal_request | request_id |
| notifications.verification_deletion.approved | account | verification_removal_request | request_id |
| notifications.verification_deletion.rejected | account | verification_removal_request | request_id |
| notifications.teacher_verification.approved | business | teacher_application | application_id |
| notifications.teacher_verification.rejected | business | teacher_application | application_id |
| notifications.student_verification.revoked | account | verification_credential | credential_id |
| notifications.system.notice | business | review, comment, resource, resource_removal_request, report, verification_application, teacher_application, verification_credential, verification_removal_request, verification_operation | reference_id |
| notifications.system.security | security | review, comment, resource, resource_removal_request, report, verification_application, teacher_application, verification_credential, verification_removal_request, verification_operation | reference_id |
| notifications.system.account | account | review, comment, resource, resource_removal_request, report, verification_application, teacher_application, verification_credential, verification_removal_request, verification_operation | reference_id |

## 目标路由约定

target.id 是对应模块对象的原始 ID，title_params 是用于显示的业务编号。客户端按 target.kind 和 target.id 构造本地页面路由，禁止把参数当作 URL。仅 accessible 状态允许跳转，其他状态显示不可访问或暂不可用。

| target.kind | 对象与页面 |
| --- | --- |
| review | reviews 的评价详情 |
| comment | reviews 的评论对象；通过评论公开接口定位所属评价 |
| resource | resources 的资料详情 |
| resource_removal_request | resources 的下架申请或投诉回执，进入本人资料申请结果 |
| report | moderation 的举报回执，进入本人举报处理结果 |
| verification_application | verification 的学生认证 Application，进入本人申请详情 |
| teacher_application | verification 的教师认证 Application，进入本人教师申请详情 |
| verification_credential | verification 的学生 Credential，进入本人认证记录 |
| verification_removal_request | verification 的 RemovalRequest，进入本人撤销或删除申请结果 |
| verification_operation | verification 的 Operation，进入本人实名核验操作结果 |

系统通知的三个键均指向目录内已有业务对象，使用 reference_id 展示引用编号。目录不定义任意系统正文、外部链接或尚不存在的公告对象。来源模块与 Resolver 装配须能查询该对象，否则返回 unavailable。

HTTP 契约版本为 0.2.0，模块版本为 0.3.0。生产者使用目录键，客户端按目录提供八语言文案。

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
| Subject | 原 subject，最长 512 字节，不按接收人或通知目标重写 |
| Revision | 原 data.revision，正 int64 |
| Recipient | 稳定用户标识，最长 256 字节 |
| Type | 通知类别，必须与标题目录相符 |
| Target | 通知目标 kind 与 id |
| TitleKey、TitleParams | 标题目录规定的键和显示用编号 |

这是可信 Go 映射接口，不接收原始封套或任意网络请求。relay 校验 CloudEvents 1.0、JSON 内容类型、获准来源与事件类型、学校范围和接收人，按业务结果生成完整通知映射。不得把失败中间状态映射为业务成功通知。

每个源事件映射一位接收人的一项通知。对同一源事件更换接收人再次调用会返回冲突。若业务需要多接收人，须先定义完整的批量消费协议，不能由 relay 拼接事件 ID 或重写 subject 绕过去重。

消费按 (source, subject) 记录最大已处理修订。新 ID 的 revision 小于或等于该值时写入 Ignored 结果，不写通知、不推进修订。完整结果允许跨修订跳跃；本接口不接受需要逐条应用的增量事件。

重复 ID 优先返回已保存的 Result，之后收到更新修订也不会改变旧事件的结果。事件锁、修订行锁和接收人偏好锁在事务中依次获取，保证并发重复、乱序和偏好修改不会重复写入通知。

Result.ID 为通知 ID，Suppressed 表示互动偏好拒收，Ignored 表示修订过旧或相同。拒收与忽略均为成功消费且持久保存；拒收也推进修订。任何写入失败均回滚，包含通知聚合数量、修订和 inbox。

Consume 返回 nil error 后，relay 才可 ConfirmOutbox。确认丢失时重新读取并消费同一事件，返回原结果。取消或超时先按原事件重试，不根据错误猜测事务是否已提交。

relay 对提供方频道 LISTEN，通知到达后读取最多 100 条未发送事件。重连后立即补读，并每 60 秒兜底读取。失败从 5 秒指数退避，单次最长 1 小时，累计 24 小时后进入待处理队列。

迁移新增 inbox 和 revisions，保留已有通知数据。业务运行身份仅持有 inbox 的 SELECT、INSERT 和 revisions 的 SELECT、INSERT、UPDATE。启动时 CheckRuntime 验证权限，迁移身份不进入业务进程。

## 验收覆盖

| 场景 | 实现与测试 |
| --- | --- |
| NT-01 | 互动投递、聚合、偏好拒收、并发重投 |
| NT-02 | 单条与全部已读、重复操作、未读数 |
| NT-03 | 业务处理、安全及账号通知不受互动偏好影响 |
| NT-04 | 目标不可访问及依赖超时状态 |
| NT-05 | HTTP 未登录拒绝、他人通知隔离、严格偏好输入 |
| NT-08 | 乱序、旧修订、跨接收人重复冲突、原结果回放、事务回滚与锁等待取消 |
| NT-07 | 全部目录键投递、非法组合拒绝且不写入、回复和点赞聚合数量 |
| NT-06 | 投递失败回滚、保留期限边界、不同目标与接收人不聚合 |

实际测试函数在 module.yaml 的 scenario_tests 登记。隔离数据库测试还验证游标分页、清理上限、迁移重复执行与运行身份权限。
