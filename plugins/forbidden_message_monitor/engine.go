// 📌 影响范围：在进程内清洗群消息并累计违禁词权重；不执行禁言、撤回或网络调用。
package forbiddenmessagemonitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

var meaninglessSingleRunes = map[rune]struct{}{'嗯': {}, '哦': {}, '好': {}, '哈': {}, '啊': {}}
var cqSegmentPattern = regexp.MustCompile(`\[CQ:[^\]]+\]`)

type compiledRule struct {
	text   string
	weight float64
}

// ScoreResult 描述清洗文本的累计词权重和直接处置分流。
type ScoreResult struct {
	Score        float64
	Band         RiskBand
	MatchedTerms []string
}

// LLMEvaluationRequest 是供应商无关的大模型研判输入。
type LLMEvaluationRequest struct {
	Message string `json:"message"`
}

// LLMEvaluationResult 是严格校验的大模型研判输出。
type LLMEvaluationResult struct {
	RiskLevel       string   `json:"risk_level"`
	TotalScore      int      `json:"total_score"`
	Reason          string   `json:"reason"`
	Violations      []string `json:"violations"`
	SuggestedAction string   `json:"suggested_action"`
}

// LLMEvaluator 定义可替换的大模型研判边界，具体供应商由调用方注入。
type LLMEvaluator interface {
	Evaluate(context.Context, LLMEvaluationRequest) (LLMEvaluationResult, error)
}

// Engine 是构造后只读、可并发使用的纯内存检测引擎。
type Engine struct {
	config EngineConfig
	rules  []compiledRule
}

// NewEngine 编译并创建检测引擎。
// @param config：直接处置阈值与加权词库。
// @returns 并发安全的引擎；配置非法时返回错误。
// ⚠️副作用说明：仅分配进程内内存，不启动协程或访问外部资源。
func NewEngine(config EngineConfig) (*Engine, error) {
	// [决策理由] 在接收流量前拒绝危险配置，防止运行时出现无界增长或错误分流。
	if err := config.validate(); err != nil {
		return nil, err
	}
	result := &Engine{config: config, rules: compileKeywords(config.WeightedKeywords)}

	// >>> 数据演变示例
	// 1. 合法默认配置 -> 编译加权词库 -> Engine,nil。
	// 2. 非正权重 -> validate拒绝 -> nil,error。
	return result, nil
}

// IsValidSpeech 判断消息是否计入活跃用户有效发言。
// @param message：群消息纯文本表示。
// @returns 包含实质Unicode字母或数字且不是无意义单字时为true。
// ⚠️副作用说明：无。
func IsValidSpeech(message string) bool {
	content := []rune(NormalizeDetectionText(message))
	// [决策理由] 没有字母数字的消息属于纯表情或纯标点。
	if len(content) == 0 {
		return false
	}
	// [决策理由] 方案明确排除常见无意义单字，其余单字仍可能有实质语义。
	if len(content) == 1 {
		_, meaningless := meaninglessSingleRunes[content[0]]
		return !meaningless
	}

	// >>> 数据演变示例
	// 1. "[CQ:face,id=1]👍！！" -> 去除消息段与符号后为空 -> false。
	// 2. "资料" -> 保留两个汉字 -> true。
	return true
}

// NormalizeDetectionText 移除 OneBot/CQ 消息段和噪音字符，只保留小写 Unicode 字母与数字。
// 原始消息仍由审计和 OneBot 动作使用；检测、长度门槛和外部模型只消费此结果。
func NormalizeDetectionText(message string) string {
	message = cqSegmentPattern.ReplaceAllString(message, "")
	var builder strings.Builder
	for _, current := range strings.ToLower(message) {
		if unicode.IsLetter(current) || unicode.IsNumber(current) {
			builder.WriteRune(current)
		}
	}
	return builder.String()
}

// Score 对清洗后的消息计算累计词权重。
// @param message：原始或已清洗文本；函数会统一规范化。
// @returns 每个已配置词最多累计一次，达到阈值时标记 high。
// ⚠️副作用说明：无；评分不写入引擎状态。
func (engine *Engine) Score(message string) ScoreResult {
	normalized := NormalizeDetectionText(message)
	score := 0.0
	matched := make([]string, 0)
	for _, rule := range engine.rules {
		if strings.Contains(normalized, rule.text) {
			score += rule.weight
			matched = append(matched, rule.text)
		}
	}
	band := RiskBandReview
	if score >= engine.config.ViolationThreshold {
		band = RiskBandHigh
	}
	return ScoreResult{Score: score, Band: band, MatchedTerms: matched}
}

// DecodeLLMEvaluationResult 严格解析并校验供应商返回的JSON对象。
// @param payload：单个JSON对象，未知字段或尾随内容不允许。
// @returns 字段和值域合法的研判结果，否则返回错误。
// ⚠️副作用说明：无；不会执行suggested_action。
func DecodeLLMEvaluationResult(payload []byte) (LLMEvaluationResult, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var result LLMEvaluationResult
	// [决策理由] 严格解码可阻止供应商响应漂移被静默忽略。
	if err := decoder.Decode(&result); err != nil {
		return LLMEvaluationResult{}, fmt.Errorf("decode LLM evaluation: %w", err)
	}
	var trailing any
	// [决策理由] 只允许单个JSON值，避免拼接响应产生歧义。
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return LLMEvaluationResult{}, errors.New("LLM evaluation must contain exactly one JSON object")
	}
	// [决策理由] 风险等级必须属于协议枚举。
	if !oneOf(result.RiskLevel, "High", "Medium", "Low", "Safe") {
		return LLMEvaluationResult{}, errors.New("invalid risk_level")
	}
	// [决策理由] 标准分值严格限制在约定范围内。
	if result.TotalScore < 0 || result.TotalScore > 100 {
		return LLMEvaluationResult{}, errors.New("total_score must be between 0 and 100")
	}
	// [决策理由] 处置建议必须属于调用方可显式处理的枚举。
	if !oneOf(result.SuggestedAction, "block", "manual_review", "pass") {
		return LLMEvaluationResult{}, errors.New("invalid suggested_action")
	}
	// [决策理由] 风险等级与动作矛盾时不能选择性信任字段，必须视为模型协议失败并安全降级。
	if (result.RiskLevel == "High" && result.SuggestedAction != "block") || (result.RiskLevel == "Medium" && result.SuggestedAction != "manual_review") || ((result.RiskLevel == "Low" || result.RiskLevel == "Safe") && result.SuggestedAction != "pass") {
		return LLMEvaluationResult{}, errors.New("risk_level and suggested_action are inconsistent")
	}
	// [决策理由] 可审计研判必须提供理由且敏感词数组不能为null。
	if strings.TrimSpace(result.Reason) == "" || result.Violations == nil {
		return LLMEvaluationResult{}, errors.New("reason and violations are required")
	}

	// >>> 数据演变示例
	// 1. 合法High/90/block对象 -> 完整校验 -> result,nil。
	// 2. 含unknown字段对象 -> DisallowUnknownFields拒绝 -> zero,error。
	return result, nil
}

// compileKeywords 使用与消息相同的规则规范词条，并将等价词合并为最大权重。
// @param keywords：已校验的加权词。
// @returns 供评分使用的内部规则副本。
// ⚠️副作用说明：无。
func compileKeywords(keywords []WeightedKeyword) []compiledRule {
	weights := make(map[string]float64, len(keywords))
	for _, keyword := range keywords {
		text := NormalizeDetectionText(keyword.Text)
		if text != "" && keyword.Weight > weights[text] {
			weights[text] = keyword.Weight
		}
	}
	result := make([]compiledRule, 0, len(weights))
	for text, weight := range weights {
		result = append(result, compiledRule{text: text, weight: weight})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].text < result[right].text })
	return result
}

// oneOf 判断字符串是否属于枚举集合。
// @param value：候选值；allowed：允许值列表。
// @returns 命中任一允许值时为true。
// ⚠️副作用说明：无。
func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		// [决策理由] 枚举采用严格大小写匹配，避免供应商输出漂移。
		if value == candidate {
			return true
		}
	}

	// >>> 数据演变示例
	// 1. "High"+["High","Low"] -> 命中 -> true。
	// 2. "high"+["High"] -> 大小写不符 -> false。
	return false
}
