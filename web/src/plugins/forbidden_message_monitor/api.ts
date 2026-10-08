// 📌 影响范围：调用违禁消息监控专属管理 API；写操作会修改违规复核或热更新加权词库。
import { apiRequest } from '../../api'

export interface RecordItem<T> { id: number; version: number; data: T }
export interface Page<T> { items: T[]; page: number; page_size: number; total: number }
export interface ViolationData { msg_content: string; group_id: string; user_id: string; status: string; detection_source: string; risk_score?: number; reason: string; violations: string[]; message_time: string; created_at: string }
export interface TrialData { decision: string; stage: string; cleaned_text: string; cleaned_length: number; local_score: number; matched_terms: string[]; reason: string; llm_used: boolean; llm_risk_level?: string; llm_total_score?: number; suggested_action: string }
export interface Term { id: number; text: string; weight: number; version: number; updated_at: string }
const base = '/api/plugins/forbidden_message_monitor'
const pageQuery = (page: number, size: number) => `page=${page}&page_size=${size}`

export const listViolations = (page: number, size: number) => apiRequest<Page<RecordItem<ViolationData>>>(`${base}/violations?${pageQuery(page, size)}`)
export const reviewViolation = (item: RecordItem<ViolationData>, status: '确认' | '误报') => apiRequest<RecordItem<ViolationData>>(`${base}/violations/${item.id}/review`, { method: 'POST', body: JSON.stringify({ status, expected_version: item.version }) })
export const runTextTrial = (text: string) => apiRequest<RecordItem<TrialData>>(`${base}/text-trials`, { method: 'POST', body: JSON.stringify({ text }) })
export const listTerms = (page: number, size: number) => apiRequest<Page<Term>>(`${base}/terms?${pageQuery(page, size)}`)
export const createTerm = (input: Pick<Term, 'text' | 'weight'>) => apiRequest<Term>(`${base}/terms`, { method: 'POST', body: JSON.stringify(input) })
export const updateTerm = (term: Term, input: Pick<Term, 'text' | 'weight'>) => apiRequest<Term>(`${base}/terms/${term.id}`, { method: 'PUT', body: JSON.stringify({ ...input, expected_version: term.version }) })
export const deleteTerm = (term: Term) => apiRequest<{ deleted: boolean }>(`${base}/terms/${term.id}`, { method: 'DELETE', body: JSON.stringify({ expected_version: term.version }) })
