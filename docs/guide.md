<!-- 📌 影响范围：说明机器人当前架构、数据库基线和开发验收；无外部变量。 -->
# W1ndys Bot 开发指南

## 当前架构

本项目是 Go 编写的 NapCat OneBot 11 群机器人，使用 PostgreSQL 持久化运行状态、配置、审计和插件业务数据，Vue 3 WebUI 是主要管理入口。

```text
NapCat 群事件 → 反向 WebSocket → EventDispatcher
              → 命令前观察器门禁 → BeforeCommands Observer
              → 命令匹配 → 全局 Ready → 群 Enabled → 代码身份 → Handler
              → 未命中命令的群事件 → 默认观察器门禁 → AfterCommands Observer
```

插件以 `PluginSpec` 编译进程序，由 `cmd/bot` 显式装入 `SpecCatalog`。代码持有稳定插件 Key、命令、触发词、作用域和允许身份；数据库不提供可变命令或权限覆盖矩阵。观察器默认只接收未命中命令的事件，安全监控可显式声明 `before_commands` 阶段覆盖命令消息；两个阶段都经过全局和群门禁。普通插件只处理群事件，私聊不进入插件执行链。QQ 应急入口直属平台管理服务，提供插件列表、状态以及全局/当前群启停，并与 WebUI 复用 `RuntimeService`。

所有插件和所有群默认关闭。未知身份、缺失状态、私聊、未声明身份及非 `ready` 状态均 fail-closed。运行时状态与管理员的持久化意图分离；启停使用乐观锁和事务审计，禁用先停止准入并排空在途调用。

Action Client 使用唯一 `echo` 关联请求与响应。WebSocket 读取循环优先处理响应，普通事件交给受限并发 worker，避免插件等待 Action 响应时阻塞收包。

## 目录

```text
cmd/bot/                 服务入口与依赖装配
internal/ws/             反向 WebSocket、事件模型和 Action Client
internal/onebot/         类型化 OneBot API
internal/plugin/         PluginSpec、目录、分发、生命周期和运行状态服务
internal/admin/          系统设置、管理员授权和审计
internal/webapi/         平台与插件专属管理 API
internal/migration/      数据库迁移执行器与 SQL
plugins/                 编译期内置插件
web/                     Vue 3 + TypeScript 管理界面
```

## 状态与管理

平台公共页 `/plugin-runtimes` 管理全局/群开关、实际状态和错误，并提供平台级群门禁维护：把一个当前群的显式插件开关合并复制到另一个当前群，以及预览并清理机器人已经退出群的门禁记录。页面初始加载不读取 NapCat 群列表，只有管理员主动预览时才查询；写入时再次核验群目录和预览版本，任一项变化会使整批操作冲突回滚。

复制不会删除目标群独有记录，同值状态不会递增版本或产生无意义写入。失效记录预览默认不选中，清理只删除 `plugin_group_states`，不会删除违禁词、违规记录等插件业务数据。批量写入使用单事务、乐观锁和单条汇总审计；内存门禁在数据库写入前先执行必要的关闭，数据库成功后才开放目标为开启的门禁。

每个插件的有限标量配置使用独立路由 `/plugin-runtimes/:pluginKey/config`。复杂业务使用插件自有表、Repository、Service、语义化专属 API 和编译期 Vue 页面。安全的离线配置和历史读取可在插件关闭时使用；OneBot、模型、网络引擎和群副作用必须重新检查运行门禁。

持久化时间统一使用 `TIMESTAMPTZ` 和 UTC。WebAPI 返回含时区的时间，WebUI 按浏览器时区展示。操作反馈统一使用应用级全局 Toast。

## 数据库基线

项目尚未上线，初始迁移为 `000001_initial_schema`。基线包含：

- `system_settings`、`admin_audit_logs`
- `plugin_states`、`plugin_group_states`、`plugin_runtime_configs`
- `keyword_reply_rules`
- Forbidden Monitor 的发言、白名单、违规、模型用量，以及旧版反馈、训练、候选、分类词条和组合表

`000002_simplify_forbidden_monitor` 将运行词库收缩为“违禁词 + 正权重”，保留旧表作为可回滚数据但不再由运行时、管理 API 或 WebUI 使用。消息在检测前会去除 CQ/媒体段、空白、换行、标点和符号；本地累计权重达到阈值时直接处置，否则清洗后达到长度门槛的消息（包括零分消息）进入大模型审核。活跃成员白名单、模型并发/超时/每日额度、违规审计、人工复核和自动处罚保持不变。

基线不包含旧 `plugin_config`、Manifest/Feature/命令同步表、权限矩阵、旧群覆盖表或数据库管理员表。项目正式部署后不得修改已部署迁移，结构变化必须新增配对迁移。

## 开发与验证

从仓库根目录使用 Task：

```bash
task setup
task run
task web-dev
task lint
task test
task web-build
task web-e2e
task migrate-up
task migrate-down
```

新增或修改插件前必须阅读 `.agents/skills/plugin-development/SKILL.md`。完整架构与验收边界见 `docs/plugin-architecture-v2.md`，实现指南见 `docs/plugin-development.md`。

提交前至少运行 `task lint`、`task test`、涉及 WebUI 时运行 `task web-build` 和 `task web-e2e`，并运行相关 race test 与 `git diff --check`。

## 安全配置

密钥仅写入未跟踪的 `.env`。部署必须设置 `DB_PASSWORD`、`NAPCAT_TOKEN`、至少 32 字节的 `JWT_SECRET`、至少 12 字符的 `WEBUI_PASSWORD` 和启用管理入口所需的 `SUPER_ADMIN_QQ`。生产环境不应长期使用 `LOG_LEVEL=debug`，原始事件可能包含聊天内容、QQ 标识和 URL。
