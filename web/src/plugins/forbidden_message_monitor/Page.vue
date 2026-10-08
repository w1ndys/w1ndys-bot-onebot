<!-- 📌 影响范围：管理违禁复核、文本试判和加权违禁词；操作结果统一通过应用级 Toast 展示。 -->
<script setup lang="ts">
import { NAlert, NButton, NCard, NDataTable, NDescriptions, NDescriptionsItem, NEmpty, NInput, NInputNumber, NModal, NPagination, NPopconfirm, NSpace, NTabPane, NTabs, NTag, type DataTableColumns } from 'naive-ui'
import { h, onMounted, reactive, ref } from 'vue'
import { ApiError } from '../../api'
import { useAppFeedback } from '../../feedback'
import * as api from './api'

const feedback = useAppFeedback()
const pageSize = 20
const busy = ref(false)
const errorMessage = ref('')
const violations = ref<api.RecordItem<api.ViolationData>[]>([]), violationPage = ref(1), violationTotal = ref(0)
const terms = ref<api.Term[]>([]), termPage = ref(1), termTotal = ref(0)
const trialText = ref(''), trial = ref<api.RecordItem<api.TrialData> | null>(null)
const termModal = ref(false), editingTerm = ref<api.Term | null>(null), termDraft = reactive<{ text: string; weight: number | null }>({ text: '', weight: 1 })
let loadSequence = 0

const localTime = (value: string) => {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString(undefined, { hour12: false })
}
async function loadAll(): Promise<boolean> {
  const sequence = ++loadSequence
  busy.value = true
  errorMessage.value = ''
  try {
    const [v, t] = await Promise.all([
      api.listViolations(violationPage.value, pageSize),
      api.listTerms(termPage.value, pageSize),
    ])
    if (sequence !== loadSequence) return false
    violations.value = v.items; violationTotal.value = v.total
    terms.value = t.items; termTotal.value = t.total
    return true
  } catch (error) {
    if (sequence !== loadSequence) return false
    errorMessage.value = error instanceof Error ? error.message : '加载违禁监控数据失败'
    return false
  } finally { if (sequence === loadSequence) busy.value = false }
}
async function mutate(action: () => Promise<unknown>, success: string): Promise<boolean> {
  if (busy.value) return false
  busy.value = true
  try {
    await action()
    const refreshed = await loadAll()
    if (refreshed) feedback.success(success)
    else feedback.warning(`${success}，但数据刷新失败，请点击重试`)
    return true
  } catch (error) {
    const suffix = error instanceof ApiError && error.status === 409 ? '，数据可能已变化，请刷新后重试' : ''
    feedback.error(error, '操作失败', suffix)
    await loadAll()
    return false
  } finally { busy.value = false }
}
async function runTrial(): Promise<void> {
  if (busy.value) return
  const text = trialText.value.trim()
  if (!text) { feedback.warning('请输入需要试判的文本'); return }
  busy.value = true; trial.value = null
  try { trial.value = await api.runTextTrial(text); feedback.success('文本试判完成') }
  catch (error) { feedback.error(error, '文本试判失败') }
  finally { busy.value = false }
}
function openTerm(term: api.Term | null): void {
  editingTerm.value = term; termDraft.text = term?.text ?? ''; termDraft.weight = term?.weight ?? 1; termModal.value = true
}
async function saveTerm(): Promise<void> {
  const text = termDraft.text.trim(), weight = termDraft.weight
  if (!text) { feedback.warning('请输入违禁词'); return }
  if (weight === null || weight < 1 || weight > 1000) { feedback.warning('权重必须为 1 到 1000'); return }
  const input = { text, weight }
  const target = editingTerm.value
  if (await mutate(() => target ? api.updateTerm(target, input) : api.createTerm(input), target ? '词条已更新' : '词条已新增')) termModal.value = false
}
const violationColumns: DataTableColumns<api.RecordItem<api.ViolationData>> = [
  { title: '群 / 用户', key: 'scope', render: (r) => `${r.data.group_id} / ${r.data.user_id}` },
  { title: '消息', key: 'data.msg_content', ellipsis: { tooltip: true }, render: (r) => r.data.msg_content },
  { title: '依据', key: 'reason', render: (r) => `${r.data.reason || '-'}${r.data.violations?.length ? `（${r.data.violations.join('、')}）` : ''}` },
  { title: '时间', key: 'time', render: (r) => localTime(r.data.message_time || r.data.created_at) },
  { title: '操作', key: 'actions', render: (r) => h(NSpace, null, { default: () => [
    h(NPopconfirm, { onPositiveClick: () => mutate(() => api.reviewViolation(r, '确认'), '违规已确认') }, { trigger: () => h(NButton, { size: 'small', type: 'error', disabled: busy.value }, { default: () => '确认违规' }), default: () => '确定记录该消息为违规吗？' }),
    h(NPopconfirm, { onPositiveClick: () => mutate(() => api.reviewViolation(r, '误报'), '已标记为误报') }, { trigger: () => h(NButton, { size: 'small', disabled: busy.value }, { default: () => '标记误报' }), default: () => '误报可能解除该记录对应的自动禁言，确定继续吗？' }),
  ] }) },
]
const termColumns: DataTableColumns<api.Term> = [
  { title: '违禁词', key: 'text' }, { title: '权重', key: 'weight' }, { title: '更新时间', key: 'updated_at', render: (r) => localTime(r.updated_at) },
  { title: '操作', key: 'actions', render: (r) => h(NSpace, null, { default: () => [h(NButton, { size: 'small', disabled: busy.value, onClick: () => openTerm(r) }, { default: () => '编辑' }), h(NPopconfirm, { onPositiveClick: () => mutate(() => api.deleteTerm(r), '词条已删除') }, { trigger: () => h(NButton, { size: 'small', disabled: busy.value }, { default: () => '删除' }), default: () => '删除后检测引擎会立即重建，确定继续吗？' })] }) },
]
onMounted(loadAll)
</script>

<template>
  <section class="monitor-page" :aria-busy="busy">
    <NAlert v-if="errorMessage" type="error" title="数据加载失败"><NSpace justify="space-between"><span>{{ errorMessage }}</span><NButton size="small" @click="loadAll">重试</NButton></NSpace></NAlert>
    <NTabs type="line" animated>
      <NTabPane name="review" tab="违规复核"><NCard title="待复核记录"><NDataTable :columns="violationColumns" :data="violations" :loading="busy" :scroll-x="900" /><NEmpty v-if="!busy && !violations.length" description="当前没有待复核记录" /><NPagination v-if="violationTotal" v-model:page="violationPage" :page-size="pageSize" :item-count="violationTotal" @update:page="loadAll" /></NCard></NTabPane>
      <NTabPane name="trial" tab="文本试判"><NCard title="文本试判"><p class="hint">试判使用与实时监控一致的清洗、加权和大模型流程，可能消耗模型额度，但不会触发群内处罚。</p><NInput v-model:value="trialText" type="textarea" maxlength="4000" show-count :disabled="busy" :autosize="{ minRows: 4, maxRows: 10 }" /><NSpace justify="end"><NButton type="primary" :loading="busy" :disabled="busy" @click="runTrial">开始试判</NButton></NSpace><NDescriptions v-if="trial" bordered :column="1"><NDescriptionsItem label="判定"><NTag>{{ trial.data.decision }}</NTag></NDescriptionsItem><NDescriptionsItem label="检测阶段">{{ trial.data.stage }}</NDescriptionsItem><NDescriptionsItem label="清洗文本">{{ trial.data.cleaned_text || '无有效文本' }}</NDescriptionsItem><NDescriptionsItem label="清洗后长度">{{ trial.data.cleaned_length }}</NDescriptionsItem><NDescriptionsItem label="本地总权重">{{ trial.data.local_score }}</NDescriptionsItem><NDescriptionsItem label="命中违禁词">{{ trial.data.matched_terms?.join('、') || '无' }}</NDescriptionsItem><NDescriptionsItem label="理由">{{ trial.data.reason }}</NDescriptionsItem><NDescriptionsItem label="大模型结果">{{ trial.data.llm_used ? `${trial.data.llm_risk_level || '未知'} / ${trial.data.llm_total_score ?? 0}` : '未调用' }}</NDescriptionsItem><NDescriptionsItem label="建议动作">{{ trial.data.suggested_action }}</NDescriptionsItem></NDescriptions></NCard></NTabPane>
      <NTabPane name="terms" tab="违禁词"><NCard title="加权违禁词"><template #header-extra><NButton :disabled="busy" @click="openTerm(null)">新增违禁词</NButton></template><NDataTable :columns="termColumns" :data="terms" :loading="busy" :scroll-x="620" /><NEmpty v-if="!busy && !terms.length" description="暂无违禁词" /><NPagination v-if="termTotal" v-model:page="termPage" :page-size="pageSize" :item-count="termTotal" @update:page="loadAll" /></NCard></NTabPane>
    </NTabs>
    <NModal v-model:show="termModal" preset="card" style="max-width:520px" :title="editingTerm ? '编辑违禁词' : '新增违禁词'"><NSpace vertical><NInput v-model:value="termDraft.text" :disabled="busy" maxlength="100" show-count placeholder="违禁词文本" /><NInputNumber v-model:value="termDraft.weight" :disabled="busy" :min="1" :max="1000" placeholder="正权重（1-1000）" /><NButton type="primary" :loading="busy" :disabled="busy" @click="saveTerm">保存</NButton></NSpace></NModal>
  </section>
</template>

<style scoped>
.monitor-page { width: 100%; min-width: 0; }
.monitor-page :deep(.n-card), .monitor-page :deep(.n-alert) { margin-bottom: var(--space-4); }
.monitor-page :deep(.n-pagination), .monitor-page :deep(.n-descriptions), .monitor-page :deep(.n-space) { margin-top: var(--space-4); }
.hint { color: var(--color-text-secondary); line-height: var(--line-height-body); }
@media (max-width: 640px) { .monitor-page :deep(.n-card__content) { padding: var(--space-3); } }
</style>
