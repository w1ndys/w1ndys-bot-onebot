<!-- 📌 影响范围：读写目标架构插件的全局与逐群开关；提供群门禁复制与失效记录清理。 -->
<script setup lang="ts">
import { NAlert, NButton, NCard, NDataTable, NEmpty, NInput, NPopconfirm, NSpace, NSwitch, NTag, type DataTableColumns } from 'naive-ui'
import { computed, h, onMounted, reactive, ref } from 'vue'
import { RouterLink } from 'vue-router'
import {
  cleanupStalePluginRuntimeGroupStates,
  copyPluginRuntimeGroupStates,
  listPluginRuntimes,
  previewPluginRuntimeGroupCopy,
  previewStalePluginRuntimeGroupStates,
  setPluginRuntimeEnabled,
  setPluginRuntimeGroupEnabled,
  type PluginRuntimeCommand,
  type PluginRuntimeGroup,
  type PluginRuntimeState,
  type RuntimeGroupCopyItem,
  type RuntimeGroupCopyPreview,
  type RuntimeGroupStateRecord,
} from '../api'
import { useAppFeedback } from '../feedback'

const feedback = useAppFeedback()
const states = ref<PluginRuntimeState[]>([])
const loading = ref(false)
const saving = ref(false)
const errorMessage = ref('')
const groupInputs = reactive<Record<string, string>>({})
const copySourceInput = ref('')
const copyTargetInput = ref('')
const copyPreview = ref<RuntimeGroupCopyPreview | null>(null)
const copyError = ref('')
const copyBusy = ref(false)
const stalePreview = ref<RuntimeGroupStateRecord[] | null>(null)
const staleError = ref('')
const staleBusy = ref(false)
const selectedStaleKeys = ref<Array<string | number>>([])
let loadSequence = 0
let mutationSequence = 0

const statusLabels: Record<string, string> = {
  disabled: '已停用',
  enabling: '启用中',
  ready: '运行中',
  disabling: '停用中',
  failed: '故障',
}

const roleLabels: Record<string, string> = {
  super_admin: '超级管理员',
  group_owner: '群主',
  group_admin: '群管理员',
  group_member: '群成员',
}

// loadStates 重新读取全部目标插件的权威运行状态。
// @param 无。
// @returns Promise<boolean>，当前请求成功完成时为 true。
// ⚠️副作用说明：发起网络请求并替换页面状态。
async function loadStates(): Promise<boolean> {
  const sequence = ++loadSequence
  loading.value = true
  errorMessage.value = ''
  try {
    const next = await listPluginRuntimes()
    // [决策理由] 连续刷新时旧请求不得覆盖更新的快照。
    if (sequence !== loadSequence) return false
    states.value = next
  } catch (error) {
    // [决策理由] 过期请求的错误不应污染当前页面。
    if (sequence !== loadSequence) return false
    errorMessage.value = error instanceof Error ? error.message : '加载插件运行状态失败'
    return false
  } finally {
    // [决策理由] 只有最新请求可以结束 loading 状态。
    if (sequence === loadSequence) loading.value = false
  }

  // >>> 数据演变示例
  // 1. 有效会话 -> [echo{ready}] -> 渲染卡片。
  // 2. 网络失败 -> errorMessage -> false。
  return true
}

// replaceState 用写操作返回的权威状态替换列表中的同一插件。
// @param next：后端返回的插件权威状态。
// @returns 无。
// ⚠️副作用说明：替换响应式列表中的对应元素。
function replaceState(next: PluginRuntimeState): void {
  states.value = states.value.map((item) => (item.plugin_key === next.plugin_key ? next : item))

  // >>> 数据演变示例
  // 1. echo 从 v1 切到 v2 -> 列表中 echo 被替换。
  // 2. 其他插件 -> 保持原对象。
}

// changeGlobal 按乐观锁切换插件全局启用意图。
// @param state：当前插件状态；enabled：目标意图。
// @returns Promise，写入结束后完成。
// ⚠️副作用说明：写入后端状态与审计，并触发插件启停生命周期。
async function changeGlobal(state: PluginRuntimeState, enabled: boolean): Promise<void> {
  const sequence = ++mutationSequence
  saving.value = true
  try {
    const next = await setPluginRuntimeEnabled(state.plugin_key, enabled, state.version)
    // [决策理由] 只有最新写请求可以刷新列表并提示。
    if (sequence !== mutationSequence) return
    replaceState(next)
    // [决策理由] 意图已保存但生命周期失败时不能误报“已启用”，必须提示实际状态。
    if (next.status === 'failed') {
      feedback.warning(`${next.display_name} 意图已保存，但运行时进入故障状态`)
      return
    }
    feedback.success(enabled ? `${next.display_name} 已启用` : `${next.display_name} 已停用`)
  } catch (error) {
    // [决策理由] 过期写请求的错误不得覆盖新操作的反馈。
    if (sequence !== mutationSequence) return
    feedback.error(error, '切换插件开关失败', '，已重新读取最新状态')
    await loadStates()
  } finally {
    // [决策理由] 只有最新写请求可以解除单飞锁。
    if (sequence === mutationSequence) saving.value = false
  }

  // >>> 数据演变示例
  // 1. echo+true+v1 -> PATCH -> v2 且 status=ready。
  // 2. 陈旧版本 -> 409 -> 提示并重载。
}

// changeGroup 按乐观锁切换插件在单个群的开关。
// @param state：当前插件状态；groupID：群号；enabled：目标值；expectedVersion：0 表示尚无记录。
// @returns Promise，写入结束后完成。
// ⚠️副作用说明：写入后端群状态与审计，并刷新运行门禁。
async function changeGroup(state: PluginRuntimeState, groupID: number, enabled: boolean, expectedVersion: number): Promise<void> {
  const sequence = ++mutationSequence
  saving.value = true
  try {
    const next = await setPluginRuntimeGroupEnabled(state.plugin_key, groupID, enabled, expectedVersion)
    // [决策理由] 只有最新写请求可以刷新列表并提示。
    if (sequence !== mutationSequence) return
    replaceState(next)
    feedback.success(enabled ? `群 ${groupID} 已开启` : `群 ${groupID} 已关闭`)
  } catch (error) {
    // [决策理由] 过期写请求的错误不得覆盖新操作的反馈。
    if (sequence !== mutationSequence) return
    feedback.error(error, '切换群开关失败', '，已重新读取最新状态')
    await loadStates()
  } finally {
    // [决策理由] 只有最新写请求可以解除单飞锁。
    if (sequence === mutationSequence) saving.value = false
  }

  // >>> 数据演变示例
  // 1. echo+100+true+v0 -> PUT -> 新增群记录 v1。
  // 2. 群记录已存在时提交 v0 -> 409 -> 提示并重载。
}

// addGroup 为手工输入的群号新增开关记录。
// @param state：当前插件状态；enabled：新增记录的目标值。
// @returns Promise，写入结束后完成。
// ⚠️副作用说明：写入后端群状态并清空输入框。
async function addGroup(state: PluginRuntimeState, enabled: boolean): Promise<void> {
  const raw = groupInputs[state.plugin_key] ?? ''
  // [决策理由] 前端先拒绝非正整数，后端仍执行权威校验。
  if (!/^[1-9]\d{0,18}$/.test(raw)) {
    feedback.warning('请输入正确的 QQ 群号')
    return
  }
  const groupID = Number(raw)
  // [决策理由] 已存在的群号应使用列表中的开关切换，避免用 0 版本覆盖既有记录导致冲突。
  if (state.groups.some((item) => item.group_id === groupID)) {
    feedback.warning(`群 ${groupID} 已在列表中，请直接切换其开关`)
    return
  }
  await changeGroup(state, groupID, enabled, 0)
  groupInputs[state.plugin_key] = ''

  // >>> 数据演变示例
  // 1. 输入 100 -> 新增群记录 -> 清空输入框。
  // 2. 输入 abc -> 前端拒绝 -> 不发请求。
}

// statusLabel 将运行状态映射为中文文案。
// @param status：后端返回的运行状态。
// @returns 中文状态文案；未知状态原样返回。
// ⚠️副作用说明：无。
function statusLabel(status: string): string {
  const result = statusLabels[status] ?? status

  // >>> 数据演变示例
  // 1. "ready" -> "运行中"。
  // 2. 未知值 -> 原样返回。
  return result
}

// statusTagType 根据运行状态选择标签配色。
// @param status：后端返回的运行状态。
// @returns Naive UI 标签类型。
// ⚠️副作用说明：无。
function statusTagType(status: string): 'success' | 'error' | 'warning' | 'default' {
  // [决策理由] 故障必须与普通停用在视觉上区分，避免管理员误判为已正常关闭。
  if (status === 'failed') return 'error'
  if (status === 'ready') return 'success'
  if (status === 'enabling' || status === 'disabling') return 'warning'

  // >>> 数据演变示例
  // 1. "failed" -> error。
  // 2. "disabled" -> default。
  return 'default'
}

// diverged 判断管理员意图与实际运行状态是否分歧。
// @param state：当前插件状态。
// @returns 意图启用但未运行，或意图停用但仍在运行时为 true。
// ⚠️副作用说明：无。
function diverged(state: PluginRuntimeState): boolean {
  const running = state.status === 'ready'
  const result = state.desired_enabled !== running

  // >>> 数据演变示例
  // 1. desired=true,status=failed -> true。
  // 2. desired=true,status=ready -> false。
  return result
}

// groupInput 读取指定插件的群号输入值。
// @param pluginKey：稳定插件 Key。
// @returns 当前输入字符串；未输入时为空字符串。
// ⚠️副作用说明：无。
function groupInput(pluginKey: string): string {
  const result = groupInputs[pluginKey] ?? ''

  // >>> 数据演变示例
  // 1. 已输入 100 -> "100"。
  // 2. 未输入 -> ""。
  return result
}

// updateGroupInput 写入指定插件的群号输入值。
// @param pluginKey：稳定插件 Key；value：输入框最新值。
// @returns 无。
// ⚠️副作用说明：修改响应式输入表。
function updateGroupInput(pluginKey: string, value: string): void {
  groupInputs[pluginKey] = value

  // >>> 数据演变示例
  // 1. echo+"100" -> 记录输入。
  // 2. echo+"" -> 清空输入。
}

// buildGroupColumns 构建指定插件的群开关表格列。
// @param state：当前插件状态。
// @returns 绑定该插件写操作的列定义。
// ⚠️副作用说明：列中的开关会在切换时发起写请求。
function buildGroupColumns(state: PluginRuntimeState): DataTableColumns<PluginRuntimeGroup> {
  const result: DataTableColumns<PluginRuntimeGroup> = [
    { title: '群号', key: 'group_id' },
    {
      title: '群开关',
      key: 'enabled',
      render: (item) =>
        h(NSwitch, {
          value: item.enabled,
          disabled: saving.value,
          'onUpdate:value': (value: boolean) => changeGroup(state, item.group_id, value, item.version),
        }),
    },
    {
      title: '最终状态',
      key: 'effective',
      render: (item) => {
        // [决策理由] 全局未运行时群开关不生效，最终状态必须显示为未生效而不是启用。
        const effective = state.status === 'ready' && item.enabled
        return h(NTag, { type: effective ? 'success' : 'default' }, { default: () => (effective ? '生效中' : '未生效') })
      },
    },
  ]

  // >>> 数据演变示例
  // 1. status=ready+群开启 -> "生效中"。
  // 2. status=disabled+群开启 -> "未生效"。
  return result
}

function parseGroupID(raw: string): number | null {
  if (!/^[1-9]\d{0,18}$/.test(raw)) return null
  const value = Number(raw)
  return Number.isSafeInteger(value) ? value : null
}

function updateCopyInput(target: 'source' | 'target', value: string): void {
  if (target === 'source') copySourceInput.value = value
  else copyTargetInput.value = value
  copyPreview.value = null
  copyError.value = ''
}

async function previewCopy(): Promise<void> {
  const sourceGroupID = parseGroupID(copySourceInput.value)
  const targetGroupID = parseGroupID(copyTargetInput.value)
  if (sourceGroupID === null || targetGroupID === null) {
    feedback.warning('请输入正确的源群号和目标群号')
    return
  }
  if (sourceGroupID === targetGroupID) {
    feedback.warning('源群和目标群不能相同')
    return
  }
  copyBusy.value = true
  copyError.value = ''
  copyPreview.value = null
  try {
    const preview = await previewPluginRuntimeGroupCopy(sourceGroupID, targetGroupID)
    copyPreview.value = preview
    feedback.success(`复制预览完成，共 ${preview.states.length} 个显式插件开关`)
  } catch (error) {
    copyError.value = error instanceof Error ? error.message : '生成群开关复制预览失败'
    feedback.error(error, '生成群开关复制预览失败')
  } finally {
    copyBusy.value = false
  }
}

async function executeCopy(): Promise<void> {
  const preview = copyPreview.value
  if (preview === null || preview.states.length === 0 || copyBusy.value) return
  copyBusy.value = true
  copyError.value = ''
  try {
    const result = await copyPluginRuntimeGroupStates(preview)
    copyPreview.value = null
    feedback.success(`已将 ${result.states.length} 个插件开关复制到群 ${result.target_group_id}`)
    await loadStates()
  } catch (error) {
    copyPreview.value = null
    copyError.value = error instanceof Error ? error.message : '复制群开关失败，请重新预览'
    feedback.error(error, '复制群开关失败', '，请重新预览')
  } finally {
    copyBusy.value = false
  }
}

async function previewStale(): Promise<void> {
  if (staleBusy.value) return
  staleBusy.value = true
  staleError.value = ''
  stalePreview.value = null
  selectedStaleKeys.value = []
  try {
    const preview = await previewStalePluginRuntimeGroupStates()
    stalePreview.value = preview
    feedback.success(`失效记录预览完成，共 ${preview.length} 条`)
  } catch (error) {
    staleError.value = error instanceof Error ? error.message : '生成失效群开关预览失败'
    feedback.error(error, '生成失效群开关预览失败')
  } finally {
    staleBusy.value = false
  }
}

function staleStateKey(item: RuntimeGroupStateRecord): string {
  return `${item.plugin_key}:${item.group_id}`
}

const selectedStaleStates = computed(() => {
  const selected = new Set(selectedStaleKeys.value.map(String))
  return (stalePreview.value ?? []).filter((item) => selected.has(staleStateKey(item)))
})

async function executeStaleCleanup(): Promise<void> {
  const selected = selectedStaleStates.value
  if (selected.length === 0 || staleBusy.value) return
  staleBusy.value = true
  staleError.value = ''
  try {
    const deleted = await cleanupStalePluginRuntimeGroupStates(selected)
    stalePreview.value = null
    selectedStaleKeys.value = []
    feedback.success(`已清理 ${deleted.length} 条失效群开关记录`)
    await loadStates()
  } catch (error) {
    stalePreview.value = null
    selectedStaleKeys.value = []
    staleError.value = error instanceof Error ? error.message : '清理失效群开关失败，请重新预览'
    feedback.error(error, '清理失效群开关失败', '，请重新预览')
  } finally {
    staleBusy.value = false
  }
}

function pluginLabel(pluginKey: string): string {
  const state = states.value.find((item) => item.plugin_key === pluginKey)
  return state === undefined ? pluginKey : `${state.display_name}（${pluginKey}）`
}

function targetStateLabel(item: RuntimeGroupCopyItem): string {
  if (item.target_enabled === undefined || item.target_enabled === null) return '无显式记录'
  return item.target_enabled ? '开启' : '关闭'
}

function copyChangeLabel(item: RuntimeGroupCopyItem): string {
  if (item.target_enabled === undefined || item.target_enabled === null) return '新增'
  return item.target_enabled === item.enabled ? '保持不变' : '修改'
}

const copyColumns: DataTableColumns<RuntimeGroupCopyItem> = [
  { title: '插件', key: 'plugin_key', render: (item) => pluginLabel(item.plugin_key) },
  { title: '源群', key: 'enabled', render: (item) => h(NTag, { type: item.enabled ? 'success' : 'default' }, { default: () => (item.enabled ? '开启' : '关闭') }) },
  { title: '目标群当前值', key: 'target_enabled', render: targetStateLabel },
  { title: '执行结果', key: 'change', render: copyChangeLabel },
]

const staleColumns: DataTableColumns<RuntimeGroupStateRecord> = [
  { type: 'selection' },
  { title: '插件', key: 'plugin_key', render: (item) => pluginLabel(item.plugin_key) },
  { title: '群号', key: 'group_id' },
  { title: '开关值', key: 'enabled', render: (item) => (item.enabled ? '开启' : '关闭') },
  { title: '版本', key: 'version' },
]

const commandColumns: DataTableColumns<PluginRuntimeCommand> = [
  { title: '命令', key: 'display_name' },
  { title: '触发词', key: 'triggers', render: (item) => item.triggers.join('、') },
  { title: '允许身份', key: 'allowed_roles', render: (item) => item.allowed_roles.map((role) => roleLabels[role] ?? role).join('、') },
]

onMounted(loadStates)
</script>

<template>
  <NSpace vertical size="large">
    <NAlert v-if="errorMessage" type="error">{{ errorMessage }}</NAlert>
    <NSpace align="center" justify="space-between">
      <span>触发词与允许身份由代码持有，此处只读展示；数据库仅保存开关意图。</span>
      <NButton :disabled="loading || saving" @click="loadStates">刷新</NButton>
    </NSpace>
    <NCard title="群开关维护">
      <NAlert type="warning" title="操作边界">
        复制采用合并模式，只覆盖源群显式存在的插件开关；清理只删除已经退出群的门禁记录，不删除关键词、违规记录、训练样本等插件业务数据。
      </NAlert>
      <div class="maintenance-grid">
        <NSpace vertical>
          <h3>按群复制插件开关</h3>
          <div class="maintenance-form">
            <NInput class="group-id-input" :value="copySourceInput" :disabled="copyBusy" placeholder="源 QQ 群号" @update:value="(value: string) => updateCopyInput('source', value)" />
            <NInput class="group-id-input" :value="copyTargetInput" :disabled="copyBusy" placeholder="目标 QQ 群号" @update:value="(value: string) => updateCopyInput('target', value)" />
            <NButton :loading="copyBusy" :disabled="copyBusy" @click="previewCopy">预览复制</NButton>
          </div>
          <NAlert v-if="copyError" type="error" title="复制预览失败">{{ copyError }}</NAlert>
          <template v-if="copyPreview">
            <span>源群：{{ copyPreview.source.group_name || '未命名群' }}（{{ copyPreview.source.group_id }}） → 目标群：{{ copyPreview.target.group_name || '未命名群' }}（{{ copyPreview.target.group_id }}）</span>
            <NDataTable :columns="copyColumns" :data="copyPreview.states" :bordered="false" :scroll-x="680" size="small" />
            <NEmpty v-if="copyPreview.states.length === 0" description="源群没有显式插件开关，无需复制" />
            <NPopconfirm v-else positive-text="确认复制" negative-text="取消" @positive-click="executeCopy">
              <template #trigger><NButton type="primary" :disabled="copyBusy">确认复制到目标群</NButton></template>
              将按当前预览版本原子复制 {{ copyPreview.states.length }} 个开关，确认继续？
            </NPopconfirm>
          </template>
        </NSpace>
        <NSpace vertical>
          <h3>清理已退出群的开关记录</h3>
          <span>预览会主动读取 NapCat 当前群列表；候选项默认不选中，执行前会再次核验。</span>
          <NButton :loading="staleBusy" :disabled="staleBusy" @click="previewStale">预览失效记录</NButton>
          <NAlert v-if="staleError" type="error" title="失效记录预览失败">{{ staleError }}</NAlert>
          <template v-if="stalePreview !== null">
            <NDataTable
              v-if="stalePreview.length > 0"
              v-model:checked-row-keys="selectedStaleKeys"
              :columns="staleColumns"
              :data="stalePreview"
              :row-key="staleStateKey"
              :bordered="false"
              :scroll-x="650"
              size="small"
            />
            <NEmpty v-else description="没有失效的群开关记录" />
            <NPopconfirm
              v-if="stalePreview.length > 0"
              positive-text="确认清理"
              negative-text="取消"
              :disabled="selectedStaleStates.length === 0 || staleBusy"
              @positive-click="executeStaleCleanup"
            >
              <template #trigger><NButton type="error" :disabled="selectedStaleStates.length === 0 || staleBusy">清理所选 {{ selectedStaleStates.length }} 条</NButton></template>
              只删除所选插件群门禁，插件业务数据会保留。确认继续？
            </NPopconfirm>
          </template>
        </NSpace>
      </div>
    </NCard>
    <NEmpty v-if="!loading && states.length === 0" description="当前二进制没有目标架构插件" />
    <NCard v-for="state in states" :key="state.plugin_key" :title="state.display_name" :loading="loading">
      <template #header-extra>
        <NSpace align="center">
          <NTag :type="state.desired_enabled ? 'info' : 'default'">意图：{{ state.desired_enabled ? '启用' : '停用' }}</NTag>
          <NTag :type="statusTagType(state.status)">实际：{{ statusLabel(state.status) }}</NTag>
        </NSpace>
      </template>
      <NSpace vertical size="large">
        <span>{{ state.description }}</span>
        <NAlert v-if="state.last_error" type="error" title="最近一次运行错误">{{ state.last_error }}</NAlert>
        <NAlert v-else-if="diverged(state)" type="warning">管理员意图与实际运行状态不一致，插件当前不接收新调用。</NAlert>
        <NSpace align="center">
          <NSwitch :value="state.desired_enabled" :disabled="saving" @update:value="(value: boolean) => changeGlobal(state, value)" />
          <span>全局开关（关闭时保留群开关数据，但所有群均不生效）</span>
        </NSpace>
        <NDataTable :columns="commandColumns" :data="state.commands" :bordered="false" size="small" />
        <RouterLink v-if="state.has_config" :to="{ name: 'plugin-runtime-config', params: { pluginKey: state.plugin_key } }">
          <NButton>打开插件配置</NButton>
        </RouterLink>
        <RouterLink v-if="state.admin_page_key" :to="{ name: 'plugin-page', params: { pageKey: state.admin_page_key } }">
          <NButton>打开专属管理页面</NButton>
        </RouterLink>
        <NSpace>
          <NInput :value="groupInput(state.plugin_key)" :disabled="saving" placeholder="手工输入 QQ 群号" @update:value="(value: string) => updateGroupInput(state.plugin_key, value)" />
          <NButton :disabled="saving" @click="addGroup(state, true)">新增并开启</NButton>
          <NButton :disabled="saving" @click="addGroup(state, false)">新增并关闭</NButton>
        </NSpace>
        <NDataTable :columns="buildGroupColumns(state)" :data="state.groups" :bordered="false" size="small" />
      </NSpace>
    </NCard>
  </NSpace>
</template>

<style scoped>
.maintenance-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(min(100%, 420px), 1fr));
  gap: 24px;
  margin-top: 20px;
}

.maintenance-grid h3 {
  margin: 0;
}

.maintenance-form {
  display: flex;
  flex-wrap: wrap;
  gap: 12px;
}

.group-id-input {
  flex: 1 1 180px;
}
</style>
