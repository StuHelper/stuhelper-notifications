---
type: spec
status: Accepted
updated: 2026-11-08
---

# 通知模块实现说明

本模块实现站内通知。来源模块通过公开 Go Sender 接口投递，HTTP API 只允许当前登录用户读取和修改本人通知。profiles 为可选依赖，缺失时不影响站内通知。

通知包含类型、来源模块、目标引用、标题翻译键与字符串参数、创建时间、已读时间和聚合数量。调用方只提交目录规定的业务编号，不能放入正文或个人敏感信息。客户端负责八语言翻译。

列表按创建时间和 UUID 倒序，使用不透明游标，每页默认 30 条，上限 100 条。游标不是数据库快照，并发新增通知需刷新首页。未读数统计通知项，聚合项算一条。

列表通过 Resolver 查询目标当前权限。accessible 表示可访问，inaccessible 表示目标删除、下架或权限不足，unavailable 表示查询失败或依赖不可用。目标页仍须执行自己的授权。

单条已读保持第一次已读时间。全部已读更新事务执行时属于本人的通知。其他用户的通知返回 404。接口不接受用户 ID，身份由宿主认证回调提供。

互动通知默认开启，用户可关闭。业务处理、安全和账号通知始终接收。偏好修改与投递按接收人串行提交，因此偏好关闭后开始的投递不会写入互动通知。

互动聚合要求接收人、来源模块、目标、标题键和参数一致，并且原通知未读、处于配置窗口内。窗口从原通知创建时起计算，边界不聚合。聚合增加 count，保留原创建时间；已读后新事件产生新通知。

投递幂等键按接收人和来源模块隔离。重复输入返回原结果，键对应不同内容返回 idempotency_conflict。被偏好拒收的事件也保存结果，开启偏好后重投仍不入站。

通知和去重记录在同一数据库事务写入。清理按创建时间删除超过保留期限的通知，每次最多 CleanupBatch 条。并发清理跳过已锁定行。未到保留期限边界的通知继续保留。

去重记录不随通知清理，避免旧事件重投后重新出现。记录保存接收人、来源、事件键、摘要和通知 ID，不保存标题参数。去重记录长期增长，后续如增加回收必须先定义上游最大重投期限。

当前只实现站内渠道。邮件、QQ、客户端推送、免打扰及提醒提前量未接入，不对外声明可用。

## 标题键目录

OpenAPI 的 TitleKey 是封闭枚举。x-title-catalog 提供键与类型、目标和参数的机器可读映射，Notification 的条件 schema 校验每种组合。Sender 在写入前校验同一目录，目录外键、类型或目标不符、缺少或额外参数均返回 invalid_input。

下表参数全部必填，只允许 1 至 64 位 ASCII 字母、数字及点、下划线、冒号、短横线，首位为字母或数字。参数是显示用业务编号，不能填姓名、学号、证件号、联系方式或正文。格式校验不能识别所有敏感编号，生产者须保证数据来源正确。

互动文案使用顶层 count 作为复数参数。Sender 不传 count，首次为 1，聚合由仓库累加。客户端将 count 与 title_params 组合传给公共国际化接口，按语言选择复数形式。

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

当前契约版本为 0.2.0。投递方需改用目录键，消费者需更新 SDK 和八语言文案。数据库不自动转换旧候选的自由标题键；已有候选数据须由负责人盘点并按业务含义处理，前端对旧未知键显示通用不可用提示。

## 验收覆盖

| 场景 | 实现与测试 |
| --- | --- |
| NT-01 | 互动投递、聚合、偏好拒收、并发重投 |
| NT-02 | 单条与全部已读、重复操作、未读数 |
| NT-03 | 业务处理、安全及账号通知不受互动偏好影响 |
| NT-04 | 目标不可访问及依赖超时状态 |
| NT-05 | HTTP 未登录拒绝、他人通知隔离、严格偏好输入 |
| NT-07 | 全部目录键投递、非法组合拒绝且不写入、回复和点赞聚合数量 |
| NT-06 | 投递失败回滚、保留期限边界、不同目标与接收人不聚合 |

实际测试函数在 module.yaml 的 scenario_tests 登记。隔离数据库测试还验证游标分页、清理上限、迁移重复执行与运行身份权限。
