<!-- 📌 影响范围：定义插件系统重构后的目标架构、职责边界和迁移顺序；无外部变量。 -->
# 插件架构

> [!IMPORTANT]
> 本文描述当前已实现的插件架构。迁移阶段 1 至 10 已完成：所有内置业务插件使用 `PluginSpec` 和唯一执行链，旧 Manifest/Manager、数据库命令与权限矩阵、通用 AdminResource 均已删除，数据库重建为单一初始基线。

## 1. 目标与非目标

项目采用编译期内置插件，WebUI 是主要管理入口，QQ 命令仅作为应急入口。重构目标是减少新插件必须理解的平台概念，并建立一个唯一、可审查的执行链。

目标：

- 所有新插件全局默认关闭，所有群默认关闭。
- 命令允许身份在代码中显式声明，数据库不提供权限覆盖矩阵。
- 消息统一经过“全局开关 → 群开关 → 代码身份 → Handler”。
- WebUI 统一登录、导航、开关、反馈和公共组件。
- 少量标量设置使用小型通用配置表单；复杂业务使用插件专属页面和 API。
- 插件业务数据由插件自有表、Repository 和 Service 管理。

非目标：

- Go `.so` 动态加载或第三方插件市场。
- 万能 JSON Schema、万能 CRUD 或低代码页面引擎。
- 根据浏览器输入自动生成 SQL、组件、HTML 或脚本。
- 让数据库覆盖代码中声明的命令权限。

## 2. 单一事实来源

```text
代码：插件身份、命令、触发词、作用域、允许身份、配置 Schema
数据库：管理员可变状态、配置值、群开关、审计、插件业务数据
WebUI：主要管理入口
QQ：最小应急入口
```

管理体验可以统一，插件业务模型不强求统一。

## 3. 编译与注册模型

```text
compiled plugin package → PluginSpec → SpecCatalog → RuntimeController → Dispatcher
```

目标 `PluginSpec` 声明稳定 Key、展示信息、命令、可选观察器、可选配置、可选生命周期和可选管理页面键。命令声明稳定 Key、触发词、群消息作用域、`AllowedRoles` 和 Handler。`ObserverSpec` 声明稳定 Key、平台支持的群事件类型、执行阶段和 Observer Handler；零值阶段保持“命令未命中后观察”，安全监控可声明 `before_commands` 在命令匹配前观察。观察器没有命令身份语义，但两个阶段都始终经过全局和群门禁。纯后台插件通过生命周期启动任务，并使用受限的群门禁查询能力保护每次群副作用。

启动时拒绝重复插件 Key、命令 Key、观察器 Key、触发词、空身份集合、空观察事件集合和不受支持的事件类型。插件不得绕过 Dispatcher 向全局事件链私自注册命令或观察器。前后端插件页面均编译进程序，不加载远程代码。

## 4. 唯一执行链

```text
事件规范化
→ 命令匹配
→ 插件运行状态为 Ready
→ 当前群插件开关为 Enabled
→ 命令作用域匹配
→ 解析当前身份
→ AllowedRoles 包含当前身份
→ Handler
```

安全默认：

- 无全局状态记录等同关闭，无群状态记录等同关闭。
- 只有 `Ready` 接收新调用；`enabling`、`disabling`、`failed` 均拒绝。
- 身份未知或解析失败时拒绝。
- 私聊消息不进入普通插件执行链。
- 超级管理员不隐式绕过命令授权，命令需要时必须显式声明。
- 全局关闭时保留群开关数据，但群开关不生效。

目标架构中的普通插件只处理群消息，身份使用封闭集合：`super_admin`、`group_owner`、`group_admin`、`group_member`。私聊消息在群门禁前拒绝，不进入普通插件；QQ 应急管理入口属于平台管理服务，不复用普通插件命令授权链。若未来出现真实私聊插件需求，必须先设计独立的私聊投放门禁，不能静默绕过群开关。

观察型插件默认处理未匹配的群事件；需要覆盖命令消息的安全监控必须显式声明命令前阶段。两类观察器都必须经过全局 `Ready` 和当前群 `Enabled`，但不执行命令身份检查；命令前观察器失败时停止当前事件继续进入命令和默认观察器，避免安全检查故障时静默绕过。插件不得自行订阅绕过 Dispatcher。后台任务由全局生命周期启动和停止，停止时必须取消并排空；任何面向具体群的后台副作用都必须在执行前重新检查该群开关。

## 5. 状态、群开关与生命周期

持久化的 `desired_enabled` 表示管理员意图；进程内 `runtime_status` 表示 `disabled`、`enabling`、`ready`、`disabling` 或 `failed`。WebUI 必须同时展示期望状态、实际状态和最近错误。

平台表：

```text
plugin_states(plugin_key, desired_enabled, version, updated_at)
plugin_group_states(plugin_key, group_id, enabled, version, updated_at)
plugin_runtime_configs(plugin_key, config_json, version, updated_at)
admin_audit_logs(...)
```

开关写入必须鉴权、审计、使用乐观锁并保持幂等。启用完成前不接流量；禁用先停止新调用，等待在途 Handler，再释放资源。生命周期方法必须幂等、可取消并隔离 panic。

平台支持两类跨记录群门禁运维，并统一采用“预览 → 携带版本确认执行”：

- 群间复制采用合并语义，只复制源群显式存在的插件开关，保留目标群独有记录；源值、源版本和目标版本必须与预览一致，整批在一个事务内完成。
- 失效清理以 NapCat 当前群列表为权威，只删除 `plugin_group_states` 中已不在群列表的所选记录；执行时群重新出现或记录版本变化都会使整批回滚。
- 同值复制不递增版本，完全无变化时不写无意义审计；每个有效批次只写一条汇总审计。
- 批量关闭在数据库提交前先收紧进程内门禁，批量开启只在提交成功后发布，数据库或内存发布异常时保持 fail-closed。

对应管理 API 均要求 WebUI JWT 和服务层二次管理员授权：

```text
POST /api/plugin-runtimes/group-maintenance/copy/preview
POST /api/plugin-runtimes/group-maintenance/copy
POST /api/plugin-runtimes/group-maintenance/stale/preview
POST /api/plugin-runtimes/group-maintenance/stale/cleanup
```

## 6. 配置与业务数据

使用唯一分类规则：

```text
少量、有限、直接影响运行行为的标量设置
→ plugin_runtime_configs + 小型 ConfigSchema

会增长、分页、筛选、关联、审核或有独立生命周期的数据
→ 插件自有表 + Migration + Repository + Service
```

小型 Schema 只逐步支持 `string`、`multiline`、`integer/number`、`boolean`、`enum` 和 `secret`。业务记录、工作流、脚本表达式、SQL、任意嵌套对象和前端组件名不得进入 Schema。

插件业务表使用配对迁移。群业务表必须包含可信的 `group_id`，唯一约束通常包含 `group_id`，所有查询和写入都必须显式隔离群。时间使用 `TIMESTAMPTZ` 并以 UTC 读写。

## 7. WebUI

平台统一提供：

- 登录、管理员授权、插件导航。
- 全局开关、群开关、运行状态和错误。
- 群开关的跨群合并复制，以及已经退出群的失效门禁预览与清理。
- 每个插件独立路由的小型配置表单和全局 Toast。
- 公共表格、分页、筛选、表单弹窗、确认框、状态标签和群选择器。
- API 错误映射、时间展示和响应式布局。

复杂插件自行提供编译期 Vue 页面、TypeScript API client、后端 API、Repository 和业务表。前端使用固定注册表把插件 Key 映射到本地异步组件；后端只能返回稳定页面 Key，不能返回组件路径、URL、HTML 或脚本。

没有专属业务页面的插件仍可使用平台运行页管理状态和群开关，并通过稳定插件 Key 对应的独立配置路由管理简单配置。出现至少三个真实重复点后再抽取更高层公共组件，不预先建设万能资源协议。

## 8. 管理面与运行门禁

消息 Handler 始终要求全局 `Ready` 且群 `Enabled`。管理操作按真实副作用显式判断：

- 查看历史、编辑持久化配置和普通离线 CRUD 可以在插件关闭时进行。
- 调用 OneBot、模型、网络运行引擎或群内副作用时，服务端必须重新检查全局和群门禁。
- 启停操作本身不得被运行门禁阻断。

不为此建立另一套通用动作描述协议；专属 API 在服务端明确执行检查。

## 9. 专属业务模块规范

建议职责布局：

```text
plugins/{plugin}/
├── spec、command、handler
├── config、lifecycle
├── repository、service
├── admin HTTP handlers
└── tests

web/src/plugins/{plugin}/
├── Page.vue
├── api.ts
├── types.ts
└── components/
```

专属 API 必须执行管理员与群操作授权、严格输入校验、分页和载荷限制、事务、冲突检测、审计及错误映射。群 ID 来自已验证路径和上下文，不信任 Body。Repository 使用固定 SQL，不接受客户端表名、列名、排序片段或过滤表达式。

外部副作用不得伪装成普通字段更新，也不得与数据库事务构成无法回滚的假原子操作；需要可靠重试时使用 outbox/job。

## 10. 违禁词监控示例

- 直接处置权重阈值、模型开关、清洗后文本长度门槛等有限参数使用小型配置。
- 单一正权重违禁词、活跃成员白名单、违规记录和模型用量使用插件自有业务表。
- 消息先删除 CQ/媒体段和噪音字符；高分直接处置，未达本地阈值但清洗后足够长的消息（包括零分消息）交给大模型。
- 违规复核、误报、文本试判和加权词 CRUD 使用语义明确的专属 API；词条编辑使用乐观锁。
- `ForbiddenMonitorPage.vue` 使用公共表格、筛选、确认框和 Toast 组合专属页面。

```text
GET    /api/plugins/forbidden_message_monitor/violations
POST   /api/plugins/forbidden_message_monitor/violations/{id}/review
POST   /api/plugins/forbidden_message_monitor/text-trials
GET    /api/plugins/forbidden_message_monitor/terms
POST   /api/plugins/forbidden_message_monitor/terms
PUT    /api/plugins/forbidden_message_monitor/terms/{id}
DELETE /api/plugins/forbidden_message_monitor/terms/{id}
```

## 11. QQ 应急入口

仅保留插件列表、状态查询、全局启停和当前群启停。QQ 与 WebUI 必须复用同一应用服务、鉴权、状态版本和审计，不能维护第二套开关逻辑。复杂配置和业务数据只在 WebUI 管理。

## 12. 已完成的迁移阶段

1. [x] 冻结 `PluginSpec`、命令、身份、状态表和 API 命名。
2. [x] 实现 Catalog、Dispatcher、全局/群门禁和生命周期状态。
3. [x] 实现小型 ConfigSchema 与通用配置页。
4. [x] 迁移 Echo，验证最小插件路径。
5. [x] 迁移 Keyword Reply，验证专属业务表、API 和页面。
6. [x] 迁移 Forbidden Monitor，验证复杂工作流和外部副作用。
7. [x] QQ 管理命令复用新应用服务。
8. [x] 删除旧权限矩阵、Feature/命令同步和通用 AdminResource。
9. [x] 项目尚未上线，重建数据库基线，避免长期双读和兼容层。
10. [x] 更新架构、开发指南和测试，将本文确认为已实现架构。

每一阶段必须有明确的旧代码删除条件，不允许新旧体系长期并存。

## 13. 新插件架构验收

- Spec、命令和触发词是否稳定且唯一。
- 每个命令是否显式声明作用域和允许身份。
- 未配置状态时是否保证全局和群均关闭。
- 状态是否正确归类为标量配置或业务数据。
- 群业务数据是否按可信 `group_id` 隔离。
- 写操作是否具备授权、事务、审计和冲突检测。
- 是否明确插件关闭时仍允许的管理操作。
- WebUI 是否复用全局 Toast 和公共组件。
- 是否覆盖权限、越群、并发、生命周期和失败恢复测试。
