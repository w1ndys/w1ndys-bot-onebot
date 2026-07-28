// 📌 影响范围：读取浏览器当前域名与会话 Token；调用后端 /api HTTP 接口。
import { setSessionToken, sessionToken } from './session'

export interface ApiEnvelope<T> {
  code: string
  message: string
  data: T
}

export class ApiError extends Error {
  readonly status: number
  readonly code: string

  // constructor 创建保留 HTTP 状态与业务码的 API 错误。
  // @param status：HTTP 状态；code：后端业务码；message：用户可读消息。
  // @returns ApiError 实例。
  // ⚠️副作用说明：创建错误对象，不修改外部状态。
  constructor(status: number, code: string, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code

    // >>> 数据演变示例
    // 1. 409+plugin_config_conflict+版本冲突 -> 可按 code 识别的 Error。
    // 2. 404+plugin_config_not_supported+暂无配置 -> 可按 status 识别的 Error。
  }
}

export type PluginConfigFieldType = 'string' | 'multiline' | 'integer' | 'boolean' | 'enum' | 'secret' | 'string_list_json' | 'weighted_terms_json' | 'combination_rules_json'

export interface PluginConfigField {
  key: string
  display_name: string
  description?: string
  type: PluginConfigFieldType
  required: boolean
  default?: unknown
  options?: string[]
}

export interface PluginConfigSchema {
  fields: PluginConfigField[]
}

export interface LoginResult {
  token: string
  expires_in: number
}

export interface SettingState {
  key: string
  value: unknown
  description: string
  overridden: boolean
}

export interface AuditState {
  id: number
  actor_id: string
  actor_role: string
  channel: string
  action: string
  target_type: string
  target_id: string
  before: unknown
  after: unknown
  success: boolean
  error_message: string
  request_id: string
  created_at: string
}

export type AuditSummary = Omit<AuditState, 'before' | 'after'>

export interface AuditPage {
  items: AuditSummary[]
  page: number
  page_size: number
  total: number
}

export interface AuditQuery {
  page: number
  page_size: number
  actor_id?: string
  action?: string
  target_type?: string
  target_id?: string
  start_time?: string
  end_time?: string
}

// apiRequest 执行统一鉴权请求并解析 code/message/data 响应。
// @param path：以 /api 开头的接口路径；options：Fetch 请求参数。
// @returns 成功响应中的 data，失败时抛出包含后端 message 的 Error。
// ⚠️副作用说明：发起网络请求；401 时清理浏览器会话 Token。
export async function apiRequest<T>(path: string, options: RequestInit = {}): Promise<T> {
  const headers = new Headers(options.headers)
  headers.set('Accept', 'application/json')
  // [决策理由] 仅在存在请求体时声明 JSON，避免 GET 产生无意义预检差异。
  if (options.body) {
    headers.set('Content-Type', 'application/json')
  }
  // [决策理由] 登录接口没有 Token，其他请求存在会话时统一附加 Bearer 凭证。
  if (sessionToken.value !== '') {
    headers.set('Authorization', `Bearer ${sessionToken.value}`)
  }
  const response = await fetch(path, { ...options, headers })
  const envelope = (await response.json()) as ApiEnvelope<T>
  // [决策理由] 401 表示本地 Token 已失效，应立即清理以便路由返回登录页。
  if (response.status === 401) {
    setSessionToken('')
  }
  // [决策理由] HTTP 状态和业务码必须同时成功，防止代理异常被误作业务数据。
  if (!response.ok || envelope.code !== 'ok') {
    throw new ApiError(response.status, envelope.code, envelope.message || '请求失败')
  }

  // >>> 数据演变示例
  // 1. 200+code=ok+data插件列表 -> 返回插件数组。
  // 2. 401+unauthorized -> 清Token -> 抛出后端错误。
  return envelope.data
}

// login 使用唯一管理员 QQ 和共享环境密码登录。
// @param qq：SUPER_ADMIN_QQ；password：WEBUI_PASSWORD。
// @returns 登录 Token 与有效秒数。
// ⚠️副作用说明：发起登录网络请求；成功时保存 Token。
export async function login(qq: string, password: string): Promise<LoginResult> {
  const result = await apiRequest<LoginResult>('/api/auth/login', {
    method: 'POST',
    body: JSON.stringify({ qq, password }),
  })
  setSessionToken(result.token)

  // >>> 数据演变示例
  // 1. 正确QQ+密码 -> API Token -> localStorage -> 返回结果。
  // 2. 错误密码 -> apiRequest抛错 -> Token不变。
  return result
}

// listSettings 获取全部受控系统设置及当前有效值。
// @param 无。
// @returns 合并默认值与数据库覆盖后的设置列表。
// ⚠️副作用说明：发起鉴权网络请求。
export function listSettings(): Promise<SettingState[]> {
  const result = apiRequest<SettingState[]>('/api/settings')

  // >>> 数据演变示例
  // 1. prefix已覆盖 -> 返回value="!"且overridden=true。
  // 2. DB无覆盖 -> 返回后端定义默认值且overridden=false。
  return result
}

// setSetting 保存一个受控系统设置的JSON值。
// @param key：稳定设置键；value：符合该键定义的值。
// @returns 保存并热刷新后的设置状态。
// ⚠️副作用说明：写入设置与审计记录，并刷新运行时设置快照。
export function setSetting(key: string, value: unknown): Promise<SettingState> {
  const result = apiRequest<SettingState>(`/api/settings/${encodeURIComponent(key)}`, {
    method: 'PUT',
    body: JSON.stringify({ value }),
  })

  // >>> 数据演变示例
  // 1. command_prefix+"!" -> PUT -> 返回覆盖状态。
  // 2. default_page_size+500 -> 400 -> 抛出校验错误。
  return result
}

// resetSetting 删除数据库覆盖并恢复后端定义默认值。
// @param key：稳定设置键。
// @returns 删除成功后的空数据。
// ⚠️副作用说明：删除设置覆盖、写入审计并刷新运行时设置快照。
export function resetSetting(key: string): Promise<null> {
  const result = apiRequest<null>(`/api/settings/${encodeURIComponent(key)}`, { method: 'DELETE' })

  // >>> 数据演变示例
  // 1. prefix覆盖存在 -> DELETE -> null并恢复"/"。
  // 2. 无覆盖 -> 404 -> 抛出无覆盖错误。
  return result
}

// listAuditLogs 分页读取只读审计日志。
// @param query：分页、操作者、动作、资源与UTC时间范围筛选。
// @returns 审计记录、当前分页和总数。
// ⚠️副作用说明：发起鉴权网络请求。
export function listAuditLogs(query: AuditQuery): Promise<AuditPage> {
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(query)) {
    // [决策理由] 空筛选不应传给后端，否则会把“不限”误写成精确空值条件。
    if (value !== undefined && value !== '') {
      params.set(key, String(value))
    }
  }
  const result = apiRequest<AuditPage>(`/api/audit-logs?${params.toString()}`)

  // >>> 数据演变示例
  // 1. page=2+action=plugin.enable -> URL参数编码 -> 返回第2页审计。
  // 2. 空可选筛选+page=1 -> 仅发送分页参数 -> 返回全部类型首屏。
  return result
}

// getAuditLog 获取单条审计的完整前后快照。
// @param id：正整数审计记录ID。
// @returns 后端权威审计详情。
// ⚠️副作用说明：发起鉴权网络请求。
export function getAuditLog(id: number): Promise<AuditState> {
  const result = apiRequest<AuditState>(`/api/audit-logs/${id}`)

  // >>> 数据演变示例
  // 1. id=8 -> GET详情 -> 返回完整before/after。
  // 2. id=404 -> 后端404 -> 抛出“审计日志不存在”。
  return result
}

export interface PluginRuntimeCommand {
  key: string
  display_name: string
  description: string
  triggers: string[]
  scope: string
  allowed_roles: string[]
}

export interface PluginRuntimeGroup {
  group_id: number
  enabled: boolean
  version: number
  updated_at: string
}

export interface PluginRuntimeState {
  plugin_key: string
  display_name: string
  description: string
  admin_page_key: string
  desired_enabled: boolean
  version: number
  updated_at: string
  status: string
  in_flight: number
  last_error: string
  has_config: boolean
  commands: PluginRuntimeCommand[]
  groups: PluginRuntimeGroup[]
}

export interface RuntimeGroupInfo {
  group_id: number
  group_name: string
  group_remark: string
  group_all_shut: number
  member_count: number
  max_member_count: number
}

export interface RuntimeGroupCopyItem {
  plugin_key: string
  enabled: boolean
  source_version: number
  target_version: number
  target_enabled?: boolean | null
}

export interface RuntimeGroupCopyPreview {
  source: RuntimeGroupInfo
  target: RuntimeGroupInfo
  states: RuntimeGroupCopyItem[]
}

export interface RuntimeGroupStateRecord {
  plugin_key: string
  group_id: number
  enabled: boolean
  version: number
  updated_at: string
}

export interface RuntimeGroupCopyResult {
  source_group_id: number
  target_group_id: number
  states: RuntimeGroupStateRecord[]
}

// listPluginRuntimes 读取目标架构插件的意图、实际运行状态与群开关。
// @param 无。
// @returns 按编译期目录顺序排列的插件运行状态。
// ⚠️副作用说明：发起鉴权网络请求。
export function listPluginRuntimes(): Promise<PluginRuntimeState[]> {
  const result = apiRequest<PluginRuntimeState[]>('/api/plugin-runtimes')

  // >>> 数据演变示例
  // 1. 有效Token -> GET -> [echo{status:ready}]。
  // 2. 过期Token -> 清理会话并抛错。
  return result
}

// setPluginRuntimeEnabled 按乐观锁修改插件全局启用意图。
// @param pluginKey：稳定插件 Key；enabled：目标意图；expectedVersion：当前版本。
// @returns 写入并驱动生命周期后的权威状态。
// ⚠️副作用说明：修改后端状态、审计记录并触发插件启停。
export function setPluginRuntimeEnabled(pluginKey: string, enabled: boolean, expectedVersion: number): Promise<PluginRuntimeState> {
  const result = apiRequest<PluginRuntimeState>(`/api/plugin-runtimes/${encodeURIComponent(pluginKey)}`, {
    method: 'PATCH',
    body: JSON.stringify({ enabled, expected_version: expectedVersion }),
  })

  // >>> 数据演变示例
  // 1. echo+true+v1 -> PATCH -> v2且status=ready。
  // 2. 陈旧版本 -> 409 -> 抛出冲突错误。
  return result
}

// setPluginRuntimeGroupEnabled 按乐观锁修改插件在单个群的开关。
// @param pluginKey：稳定插件 Key；groupID：群号；enabled：目标值；expectedVersion：0 表示尚无记录。
// @returns 写入后的插件权威状态。
// ⚠️副作用说明：修改后端群状态、审计记录并刷新运行门禁。
export function setPluginRuntimeGroupEnabled(pluginKey: string, groupID: number, enabled: boolean, expectedVersion: number): Promise<PluginRuntimeState> {
  const result = apiRequest<PluginRuntimeState>(`/api/plugin-runtimes/${encodeURIComponent(pluginKey)}/groups/${groupID}`, {
    method: 'PUT',
    body: JSON.stringify({ enabled, expected_version: expectedVersion }),
  })

  // >>> 数据演变示例
  // 1. echo+100+true+v0 -> PUT -> 新增群记录v1。
  // 2. 全局关闭时写入 -> 200，但命令仍被全局门禁拒绝。
  return result
}

// previewPluginRuntimeGroupCopy 使用 NapCat 当前群列表预览源群到目标群的显式开关复制。
export function previewPluginRuntimeGroupCopy(sourceGroupID: number, targetGroupID: number): Promise<RuntimeGroupCopyPreview> {
  return apiRequest<RuntimeGroupCopyPreview>('/api/plugin-runtimes/group-maintenance/copy/preview', {
    method: 'POST',
    body: JSON.stringify({ source_group_id: sourceGroupID, target_group_id: targetGroupID }),
  })
}

// copyPluginRuntimeGroupStates 按预览快照中的版本原子复制源群显式开关。
export function copyPluginRuntimeGroupStates(preview: RuntimeGroupCopyPreview): Promise<RuntimeGroupCopyResult> {
  return apiRequest<RuntimeGroupCopyResult>('/api/plugin-runtimes/group-maintenance/copy', {
    method: 'POST',
    body: JSON.stringify({
      source_group_id: preview.source.group_id,
      target_group_id: preview.target.group_id,
      states: preview.states.map((item) => ({
        plugin_key: item.plugin_key,
        enabled: item.enabled,
        source_version: item.source_version,
        target_version: item.target_version,
      })),
    }),
  })
}

// previewStalePluginRuntimeGroupStates 使用 NapCat 当前群列表预览失效的显式门禁记录。
export function previewStalePluginRuntimeGroupStates(): Promise<RuntimeGroupStateRecord[]> {
  return apiRequest<RuntimeGroupStateRecord[]>('/api/plugin-runtimes/group-maintenance/stale/preview', { method: 'POST' })
}

// cleanupStalePluginRuntimeGroupStates 按预览版本原子删除所选失效门禁记录。
export function cleanupStalePluginRuntimeGroupStates(states: RuntimeGroupStateRecord[]): Promise<RuntimeGroupStateRecord[]> {
  return apiRequest<RuntimeGroupStateRecord[]>('/api/plugin-runtimes/group-maintenance/stale/cleanup', {
    method: 'POST',
    body: JSON.stringify({
      states: states.map((item) => ({ plugin_key: item.plugin_key, group_id: item.group_id, expected_version: item.version })),
    }),
  })
}

export interface PluginRuntimeConfigState {
  plugin_key: string
  schema: PluginConfigSchema
  config: Record<string, unknown>
  version: number
  updated_at: string
}

// getPluginRuntimeConfig 读取目标插件的配置 Schema 与脱敏后的当前值。
// @param pluginKey：稳定插件 Key。
// @returns Schema、脱敏配置与乐观锁版本。
// ⚠️副作用说明：发起鉴权网络请求。
export function getPluginRuntimeConfig(pluginKey: string): Promise<PluginRuntimeConfigState> {
  const result = apiRequest<PluginRuntimeConfigState>(`/api/plugin-runtimes/${encodeURIComponent(pluginKey)}/config`)

  // >>> 数据演变示例
  // 1. echo -> Schema+脱敏值+v2。
  // 2. 未声明配置的插件 -> 404 -> 抛错。
  return result
}

// putPluginRuntimeConfig 按乐观锁保存目标插件配置并热应用。
// @param pluginKey：稳定插件 Key；config：完整非敏感配置及用户填写的 secret；expectedVersion：当前版本。
// @returns 保存并热应用后的权威配置快照。
// ⚠️副作用说明：修改后端配置、审计记录并调用插件热应用钩子。
export function putPluginRuntimeConfig(pluginKey: string, config: Record<string, unknown>, expectedVersion: number): Promise<PluginRuntimeConfigState> {
  const result = apiRequest<PluginRuntimeConfigState>(`/api/plugin-runtimes/${encodeURIComponent(pluginKey)}/config`, {
    method: 'PUT',
    body: JSON.stringify({ config, expected_version: expectedVersion }),
  })

  // >>> 数据演变示例
  // 1. echo+{response_prefix:"[bot] "}+v1 -> 保存并热应用 -> v2。
  // 2. 陈旧版本 -> 409 -> 抛出冲突错误。
  return result
}
