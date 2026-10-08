// 📌 影响范围：读取违禁监控运行时规则并可调用已配置的大模型；不访问QQ、不写数据库。
package forbiddenmessagemonitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/w1ndys/w1ndys-bot/internal/management"
)

const maxTextTestRunes = 4000

type textTestResourceHandler struct{ owner *implementation }

type textTestPayload struct {
	Text string `json:"text"`
}

type textTestResult struct {
	Decision        string   `json:"decision"`
	Stage           string   `json:"stage"`
	CleanedText     string   `json:"cleaned_text"`
	CleanedLength   int      `json:"cleaned_length"`
	LocalScore      float64  `json:"local_score"`
	MatchedTerms    []string `json:"matched_terms"`
	Reason          string   `json:"reason"`
	LLMUsed         bool     `json:"llm_used"`
	LLMRiskLevel    string   `json:"llm_risk_level,omitempty"`
	LLMTotalScore   int      `json:"llm_total_score,omitempty"`
	SuggestedAction string   `json:"suggested_action"`
}

// List 返回无持久化试判记录的空分页。
// @param ctx/actor/query：通用资源查询参数。
// @returns 与请求分页一致的空结果。
// ⚠️副作用说明：无。
func (h *textTestResourceHandler) List(_ context.Context, _ management.Actor, query management.ResourceQuery) (management.ResourcePage, error) {
	result := management.ResourcePage{Items: []management.ResourceRecord{}, Page: query.Page, PageSize: query.PageSize, Total: 0}
	// >>> 数据演变示例
	// 1. page1,size20 -> 空items,total0。
	// 2. page2,size10 -> 空items,page2。
	return result, nil
}

// Create 对输入文本执行无副作用试判。
// @param ctx：请求上下文；actor：已授权管理员；raw：仅含text的JSON。
// @returns 包含本地规则与可选LLM结论的临时资源记录。
// ⚠️副作用说明：需要模型时会占用并发与当日请求额度并发送测试文本；不执行群管理或写违规审计。
func (h *textTestResourceHandler) Create(ctx context.Context, _ management.Actor, raw json.RawMessage) (management.ResourceRecord, error) {
	payload, err := decodeTextTestPayload(raw)
	// [决策理由] 无效或过长文本不得进入规则与外部模型。
	if err != nil {
		return management.ResourceRecord{}, err
	}
	snapshot := h.owner.snapshot.Load()
	// [决策理由] 配置尚未发布时不存在权威试判规则。
	if snapshot == nil || snapshot.engine == nil {
		return management.ResourceRecord{}, fmt.Errorf("违禁消息检测配置尚未初始化")
	}
	cleaned := NormalizeDetectionText(payload.Text)
	cleanedLength := len([]rune(cleaned))
	base := textTestResult{CleanedText: cleaned, CleanedLength: cleanedLength, MatchedTerms: []string{}}
	// [决策理由] 清洗后只有媒体或噪音的输入没有可检测内容，也不得发送给外部模型。
	if cleaned == "" {
		base.Decision = "放行"
		base.Stage = "preprocessing"
		base.Reason = "清洗后没有有效文本"
		base.SuggestedAction = "pass"
		return textTestRecord(base)
	}
	score := snapshot.engine.Score(cleaned)
	base.LocalScore = score.Score
	base.MatchedTerms = score.MatchedTerms
	// [决策理由] 本地累计权重达到阈值时无需调用模型。
	if score.Band == RiskBandHigh {
		base.Decision = "违规"
		base.Stage = "weighted_score"
		base.Reason = "违禁词累计权重达到直接处置阈值"
		base.SuggestedAction = "block"
		return textTestRecord(base)
	}
	// [决策理由] 长度门槛只计算清洗文本；短消息无论分数是否为零都不调用模型。
	if messageBelowLLMMinimum(cleaned, snapshot.minLLMMessageLength) {
		base.Decision = "放行"
		base.Stage = "llm_length_filter"
		base.Reason = fmt.Sprintf("清洗后文本短于大模型最短检测长度%d", snapshot.minLLMMessageLength)
		base.SuggestedAction = "pass"
		return textTestRecord(base)
	}
	// [决策理由] 达到长度门槛后，即使本地零分也必须进入模型；不可用时明确返回人工复核。
	if snapshot.evaluator == nil {
		base.Decision = "人工复核"
		base.Stage = "llm"
		base.Reason = "大模型未启用"
		base.SuggestedAction = "manual_review"
		return textTestRecord(base)
	}
	// [决策理由] 文本试判与真实流量共享并发上限，防止管理页面绕过资源保护。
	if !h.owner.tryAcquireLLM(snapshot.llmMaxConcurrency) {
		base.Decision = "人工复核"
		base.Stage = "llm"
		base.Reason = "大模型并发已满"
		base.SuggestedAction = "manual_review"
		return textTestRecord(base)
	}
	defer h.owner.releaseLLM()
	now := time.Now().UTC()
	// [决策理由] 测试实例可注入稳定时钟，生产实例使用同一插件时钟计算UTC日额度。
	if h.owner.now != nil {
		now = h.owner.now().UTC()
	}
	reserved, err := h.owner.repository.TryReserveLLMRequest(ctx, now, snapshot.llmDailyRequestLimit)
	// [决策理由] 额度状态不可确认或已耗尽时不得绕过预算调用外部模型。
	if err != nil || !reserved {
		base.Decision = "人工复核"
		base.Stage = "llm"
		base.Reason = "大模型每日额度不可用"
		base.SuggestedAction = "manual_review"
		return textTestRecord(base)
	}
	llmContext, cancel := context.WithTimeout(ctx, snapshot.llmTimeout)
	defer cancel()
	llmResult, err := snapshot.evaluator.Evaluate(llmContext, LLMEvaluationRequest{Message: cleaned})
	// [决策理由] 模型失败不能伪造安全结论，应把测试结果标为人工复核并保留本地证据。
	if err != nil {
		base.Decision = "人工复核"
		base.Stage = "llm"
		base.Reason = "大模型研判失败"
		base.LLMUsed = true
		base.SuggestedAction = "manual_review"
		return textTestRecord(base)
	}
	base.Decision = textTestDecision(llmResult.SuggestedAction)
	base.Stage = "llm"
	base.Reason = llmResult.Reason
	base.LLMUsed = true
	base.LLMRiskLevel = llmResult.RiskLevel
	base.LLMTotalScore = llmResult.TotalScore
	base.SuggestedAction = llmResult.SuggestedAction
	// >>> 数据演变示例
	// 1. 高权重短文本 -> weighted_score -> 违规/block且不调用QQ。
	// 2. 零分长文本+LLM pass -> llm -> 放行/pass。
	return textTestRecord(base)
}

// Update 拒绝修改临时试判结果。
// @param ctx/actor/id/version/raw：通用资源更新参数。
// @returns ErrInvalidResourceData。
// ⚠️副作用说明：无。
func (h *textTestResourceHandler) Update(context.Context, management.Actor, int64, int64, json.RawMessage) (management.ResourceRecord, error) {
	// >>> 数据演变示例
	// 1. PATCH试判结果 -> 拒绝。
	// 2. 空载荷PATCH -> 拒绝。
	return management.ResourceRecord{}, management.ErrInvalidResourceData
}

// Delete 拒绝删除不存在的试判记录。
// @param ctx/actor/id/version：通用资源删除参数。
// @returns ErrInvalidResourceData。
// ⚠️副作用说明：无。
func (h *textTestResourceHandler) Delete(context.Context, management.Actor, int64, int64) error {
	// >>> 数据演变示例
	// 1. DELETE id1 -> 拒绝。
	// 2. DELETE id0 -> 拒绝。
	return management.ErrInvalidResourceData
}

// decodeTextTestPayload 严格解析并限制测试文本。
// @param raw：仅允许text字段的JSON对象。
// @returns 去首尾空白的合法文本或领域输入错误。
// ⚠️副作用说明：无。
func decodeTextTestPayload(raw json.RawMessage) (textTestPayload, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var payload textTestPayload
	// [决策理由] 类型错误和未知字段不得进入检测器。
	if err := decoder.Decode(&payload); err != nil {
		return textTestPayload{}, fmt.Errorf("%w: %v", management.ErrInvalidResourceData, err)
	}
	var trailing any
	err := decoder.Decode(&trailing)
	// [决策理由] 只有EOF表示请求仅包含一个JSON值。
	if !errors.Is(err, io.EOF) {
		return textTestPayload{}, fmt.Errorf("%w: 必须仅提交一个JSON对象", management.ErrInvalidResourceData)
	}
	payload.Text = strings.TrimSpace(payload.Text)
	// [决策理由] 空文本没有检测意义，长度上限防止模型费用与内存被滥用。
	if payload.Text == "" || len([]rune(payload.Text)) > maxTextTestRunes {
		return textTestPayload{}, fmt.Errorf("%w: text必须为1到%d个字符", management.ErrInvalidResourceData, maxTextTestRunes)
	}
	// >>> 数据演变示例
	// 1. {text:" 免费 "} -> trim -> "免费"。
	// 2. {text:"",extra:1} -> 未知字段/空值 -> ErrInvalidResourceData。
	return payload, nil
}

// textTestRecord 编码临时试判结果。
// @param result：完整试判结论。
// @returns id/version固定为1的临时资源记录或编码错误。
// ⚠️副作用说明：分配JSON内存，不持久化。
func textTestRecord(result textTestResult) (management.ResourceRecord, error) {
	raw, err := json.Marshal(result)
	// [决策理由] 编码失败时不能向前端返回缺字段结论。
	if err != nil {
		return management.ResourceRecord{}, err
	}
	record := management.ResourceRecord{ID: time.Now().UnixNano(), Version: 1, Data: raw}
	// >>> 数据演变示例
	// 1. block结果 -> JSON记录version1。
	// 2. pass结果 -> JSON记录version1且不落库。
	return record, nil
}

// textTestDecision 把稳定动作枚举转换为中文结论。
// @param action：block、manual_review或pass。
// @returns 违规、人工复核或放行。
// ⚠️副作用说明：无。
func textTestDecision(action string) string {
	result := "人工复核"
	// [决策理由] 仅明确block视为违规。
	if action == "block" {
		result = "违规"
	} else {
		// [决策理由] 仅明确pass视为放行，其余安全回退人工复核。
		if action == "pass" {
			result = "放行"
		}
	}
	// >>> 数据演变示例
	// 1. block -> 违规。
	// 2. pass -> 放行；unknown -> 人工复核。
	return result
}
