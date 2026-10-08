// 📌 影响范围：声明违禁消息监控配置并调用管理员指定的 OpenAI-compatible HTTPS/HTTP 大模型端点；API Key 仅用于请求鉴权。
package forbiddenmessagemonitor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/w1ndys/w1ndys-bot/internal/plugin"
)

const maxLLMResponseBytes = 1 << 20

type pluginConfig struct {
	ViolationWeightThreshold int    `json:"violation_weight_threshold"`
	LLMEnabled               bool   `json:"llm_enabled"`
	LLMEndpoint              string `json:"llm_endpoint"`
	LLMModel                 string `json:"llm_model"`
	LLMAPIKey                string `json:"llm_api_key"`
	LLMTimeoutSeconds        int    `json:"llm_timeout_seconds"`
	LLMMaxConcurrency        int    `json:"llm_max_concurrency"`
	LLMDailyRequestLimit     int    `json:"llm_daily_request_limit"`
	MinLLMMessageLength      int    `json:"min_llm_message_length"`
}

type runtimeSnapshot struct {
	engine               *Engine
	engineConfig         EngineConfig
	evaluator            LLMEvaluator
	llmTimeout           time.Duration
	llmMaxConcurrency    int
	llmDailyRequestLimit int
	minLLMMessageLength  int
}

type openAICompatibleEvaluator struct {
	endpoint string
	model    string
	apiKey   string
	client   *http.Client
}

// ConfigSchema 声明检测规则、分流阈值和可选大模型设置。
// @param 无。
// @returns 可由通用 WebUI 渲染且密钥 write-only 的配置 Schema。
// ⚠️副作用说明：无。
func (p *implementation) ConfigSchema() plugin.ConfigSchema {
	result := plugin.ConfigSchema{Fields: []plugin.ConfigField{
		{Key: "violation_weight_threshold", DisplayName: "直接处置权重阈值", Description: "清洗后的消息累计命中权重达到此值时不调用大模型，直接进入处置", Type: plugin.FieldInteger, Default: json.RawMessage(`60`)},
		{Key: "min_llm_message_length", DisplayName: "大模型最短有效文本长度", Description: "累计权重未达到直接处置阈值时，清洗后文本达到此长度即提交大模型，包括本地权重为0的新型消息", Type: plugin.FieldInteger, Default: json.RawMessage(`30`)},
		{Key: "llm_enabled", DisplayName: "启用大模型研判", Description: "启用后只会将去除媒体、空白、换行、标点和符号后的文本发送到所配置的外部服务", Type: plugin.FieldBoolean, Default: json.RawMessage(`false`)},
		{Key: "llm_endpoint", DisplayName: "大模型接口地址", Description: "OpenAI-compatible chat/completions 完整 URL；远程服务必须使用 HTTPS，HTTP 仅允许本机回环地址", Type: plugin.FieldString, Default: json.RawMessage(`""`)},
		{Key: "llm_model", DisplayName: "大模型名称", Type: plugin.FieldString, Default: json.RawMessage(`""`)},
		{Key: "llm_api_key", DisplayName: "大模型 API Key", Description: "只写字段，留空保留原值", Type: plugin.FieldSecret},
		{Key: "llm_timeout_seconds", DisplayName: "大模型超时秒数", Type: plugin.FieldInteger, Default: json.RawMessage(`20`)},
		{Key: "llm_max_concurrency", DisplayName: "大模型最大并发", Description: "达到并发上限时新消息安全放行", Type: plugin.FieldInteger, Default: json.RawMessage(`2`)},
		{Key: "llm_daily_request_limit", DisplayName: "大模型每日请求上限", Description: "按UTC自然日跨重启累计，达到上限后消息安全放行", Type: plugin.FieldInteger, Default: json.RawMessage(`500`)},
	}}

	// >>> 数据演变示例
	// 1. 新插件 -> 默认空词库+直接处置阈值60+LLM关闭 -> 可启用本地规则。
	// 2. WebUI读取 -> llm_api_key被平台脱敏 -> 其余配置可编辑。
	return result
}

// ValidateConfig 校验完整插件配置且不发布运行状态。
// @param ctx：校验上下文；raw：平台规范化后的完整 JSON。
// @returns 取消、JSON、URL、阈值或检测引擎配置错误。
// ⚠️副作用说明：无；不发起大模型请求。
func (p *implementation) ValidateConfig(ctx context.Context, raw json.RawMessage) error {
	// [决策理由] 已取消的管理请求不应继续解析配置。
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := buildRuntimeSnapshot(raw, p.currentLexicon(), p.httpClient)

	// >>> 数据演变示例
	// 1. 空词库+阈值60+LLM关闭 -> Engine构造成功 -> nil。
	// 2. LLM启用但endpoint为空 -> 领域校验 -> error。
	return err
}

// ApplyConfig 原子发布检测引擎和大模型客户端快照。
// @param ctx：应用上下文；raw：平台规范化后的完整 JSON。
// @returns 取消或配置构造错误。
// ⚠️副作用说明：成功时替换后续群消息读取的运行快照；不立即访问外部服务。
func (p *implementation) ApplyConfig(ctx context.Context, raw json.RawMessage) error {
	p.publicationMu.Lock()
	defer p.publicationMu.Unlock()
	// [决策理由] 调用方取消后不得发布已经放弃的配置。
	if err := ctx.Err(); err != nil {
		return err
	}
	next, err := buildRuntimeSnapshot(raw, p.currentLexicon(), p.httpClient)
	// [决策理由] 只有完整解析和构造成功才能替换旧快照。
	if err != nil {
		return err
	}
	// [决策理由] 词库变更需要按最近一次生效配置重建引擎，因此保留已应用的原始配置。
	applied := append(json.RawMessage(nil), raw...)
	p.configJSON.Store(&applied)
	p.snapshot.Store(next)

	// >>> 数据演变示例
	// 1. 旧阈值60+新阈值70 -> 一次Store最终快照。
	// 2. 新词库构造失败 -> 不Store -> 旧引擎继续服务。
	return nil
}

// buildRuntimeSnapshot 严格解析配置并构造不可变运行依赖。
// @param raw：完整配置 JSON；client：共享且带全局连接复用的 HTTP Client。
// @returns 可原子发布的快照或配置错误。
// ⚠️副作用说明：构造规则快照并分配内存；不发起网络请求。
func buildRuntimeSnapshot(raw json.RawMessage, lexicon Lexicon, client *http.Client) (*runtimeSnapshot, error) {
	var config pluginConfig
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	// [决策理由] 插件自身必须防御绕过平台直接调用时的未知字段。
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("解析违禁监控配置: %w", err)
	}
	// [决策理由] 兼容新增限额字段前的包内配置和历史快照，缺失值采用保守默认值。
	if config.LLMMaxConcurrency == 0 {
		config.LLMMaxConcurrency = 2
	}
	// [决策理由] 历史配置没有每日额度字段时使用明确有限的默认请求数。
	if config.LLMDailyRequestLimit == 0 {
		config.LLMDailyRequestLimit = 500
	}
	// [决策理由] 历史配置缺少最短模型消息长度时采用新功能默认值30。
	if config.MinLLMMessageLength == 0 {
		config.MinLLMMessageLength = 30
	}
	if config.ViolationWeightThreshold == 0 {
		config.ViolationWeightThreshold = 60
	}
	engineConfig := DefaultEngineConfig()
	engineConfig.ViolationThreshold = float64(config.ViolationWeightThreshold)
	engineConfig.WeightedKeywords = append([]WeightedKeyword(nil), lexicon.Terms...)
	engine, err := NewEngine(engineConfig)
	// [决策理由] 引擎阈值或容量非法时不能发布不可运行快照。
	if err != nil {
		return nil, fmt.Errorf("构造检测引擎: %w", err)
	}
	// [决策理由] 外部请求必须具有有限超时，避免占满事件 worker。
	if config.LLMTimeoutSeconds < 1 || config.LLMTimeoutSeconds > 120 {
		return nil, fmt.Errorf("llm_timeout_seconds 必须在1到120之间")
	}
	// [决策理由] 并发必须为小型正整数，避免外部模型迟滞耗尽事件处理资源。
	if config.LLMMaxConcurrency < 1 || config.LLMMaxConcurrency > 32 {
		return nil, fmt.Errorf("llm_max_concurrency 必须在1到32之间")
	}
	// [决策理由] 每日额度必须为正且有合理上界，防止误配置导致无界费用。
	if config.LLMDailyRequestLimit < 1 || config.LLMDailyRequestLimit > 1000000 {
		return nil, fmt.Errorf("llm_daily_request_limit 必须在1到1000000之间")
	}
	// [决策理由] 模型长度门槛必须有界，避免误配置让全部合法消息永久绕过研判。
	if config.MinLLMMessageLength < 1 || config.MinLLMMessageLength > maxTextTestRunes {
		return nil, fmt.Errorf("min_llm_message_length 必须在1到%d之间", maxTextTestRunes)
	}
	result := &runtimeSnapshot{engine: engine, engineConfig: engineConfig, llmTimeout: time.Duration(config.LLMTimeoutSeconds) * time.Second, llmMaxConcurrency: config.LLMMaxConcurrency, llmDailyRequestLimit: config.LLMDailyRequestLimit, minLLMMessageLength: config.MinLLMMessageLength}
	// [决策理由] LLM关闭时不得要求凭据或创建外部调用器。
	if !config.LLMEnabled {
		return result, nil
	}
	endpoint, err := validateLLMEndpoint(config.LLMEndpoint)
	// [决策理由] 无效端点可能导致请求泄漏到意外协议或带用户信息的地址。
	if err != nil {
		return nil, err
	}
	// [决策理由] 模型名为空时服务端无法选择研判模型。
	if strings.TrimSpace(config.LLMModel) == "" {
		return nil, fmt.Errorf("启用大模型时 llm_model 不能为空")
	}
	result.evaluator = &openAICompatibleEvaluator{endpoint: endpoint, model: config.LLMModel, apiKey: config.LLMAPIKey, client: secureLLMHTTPClient(client)}

	// >>> 数据演变示例
	// 1. LLM关闭 -> engine+nil evaluator -> 长文本无法送模时放行。
	// 2. LLM开启+合法endpoint/model -> engine+HTTP evaluator -> 长文本调用模型。
	return result, nil
}

// secureLLMHTTPClient 为每次重定向重新应用LLM端点传输安全规则。
// @param client：共享连接配置；nil时使用标准客户端默认值。
// @returns 浅拷贝且拒绝不安全重定向的HTTP客户端。
// ⚠️副作用说明：分配客户端副本；不修改调用方传入实例。
func secureLLMHTTPClient(client *http.Client) *http.Client {
	result := &http.Client{}
	// [决策理由] 保留调用方Transport和Timeout等配置，同时避免覆盖共享客户端的重定向策略。
	if client != nil {
		clone := *client
		result = &clone
	}
	previousCheck := result.CheckRedirect
	result.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		// [决策理由] 每一跳都必须满足HTTPS或本机回环HTTP，防止安全初始地址降级外发隐私。
		if _, err := validateLLMEndpoint(request.URL.String()); err != nil {
			return err
		}
		// [决策理由] 调用方更严格的跳数或域名策略仍需保留。
		if previousCheck != nil {
			return previousCheck(request, via)
		}
		// [决策理由] 标准库默认最多允许10次重定向，复制该边界防止循环耗尽worker。
		if len(via) >= 10 {
			return fmt.Errorf("大模型重定向次数过多")
		}
		return nil
	}
	// >>> 数据演变示例
	// 1. HTTPS -> HTTPS跳转 -> 每跳校验通过 -> 继续请求。
	// 2. localhost HTTP -> remote HTTP跳转 -> 校验拒绝且不发送消息。
	return result
}

// validateLLMEndpoint 校验管理员配置的 OpenAI-compatible URL。
// @param raw：端点文本。
// @returns 规范化 URL 或协议、主机、用户信息错误。
// ⚠️副作用说明：无。
func validateLLMEndpoint(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	// [决策理由] 仅允许明确的HTTP(S)绝对地址，拒绝文件和自定义协议。
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return "", fmt.Errorf("llm_endpoint 必须是 HTTP(S) 绝对地址")
	}
	// [决策理由] URL用户信息容易在错误文本或代理日志泄露凭据。
	if parsed.User != nil {
		return "", fmt.Errorf("llm_endpoint 不能包含用户信息")
	}
	hostname := strings.ToLower(parsed.Hostname())
	loopback := hostname == "localhost"
	// [决策理由] 数字IP必须由标准库判断回环范围，覆盖127/8与IPv6 ::1。
	if address := net.ParseIP(hostname); address != nil {
		loopback = address.IsLoopback()
	}
	// [决策理由] 远程HTTP会明文传输API Key和群消息隐私，仅本机回环开发服务可例外。
	if parsed.Scheme == "http" && !loopback {
		return "", fmt.Errorf("远程 llm_endpoint 必须使用 HTTPS，HTTP 仅允许 localhost 或回环 IP")
	}
	result := parsed.String()

	// >>> 数据演变示例
	// 1. https://llm.example/v1/chat/completions -> 原样返回。
	// 2. file:///tmp/model -> 协议拒绝 -> error。
	return result, nil
}

// Evaluate 调用 OpenAI-compatible chat completions 并严格解析模型输出。
// @param ctx：已设置调用超时的上下文；request：仅含清洗后的消息文本。
// @returns 结构化研判或网络、HTTP、协议错误。
// ⚠️副作用说明：向管理员配置的外部端点发送清洗后的群消息文本；不记录 API Key 或消息。
func (e *openAICompatibleEvaluator) Evaluate(ctx context.Context, request LLMEvaluationRequest) (LLMEvaluationResult, error) {
	userPayload, err := json.Marshal(request)
	// [决策理由] 请求仅含可编码字段，仍需传播编码错误保持边界完整。
	if err != nil {
		return LLMEvaluationResult{}, fmt.Errorf("编码大模型研判输入: %w", err)
	}
	body, err := json.Marshal(map[string]any{
		"model":           e.model,
		"temperature":     0,
		"response_format": map[string]string{"type": "json_object"},
		"messages": []map[string]string{
			{"role": "system", "content": llmSystemPrompt},
			{"role": "user", "content": string(userPayload)},
		},
	})
	// [决策理由] 上游请求体构造失败时不得发出不完整请求。
	if err != nil {
		return LLMEvaluationResult{}, fmt.Errorf("编码大模型请求: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	// [决策理由] URL虽已校验，仍传播标准库构造错误。
	if err != nil {
		return LLMEvaluationResult{}, fmt.Errorf("创建大模型请求: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	// [决策理由] API Key可选以兼容本地模型服务，存在时仅放入Authorization头。
	if e.apiKey != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+e.apiKey)
	}
	response, err := e.client.Do(httpRequest)
	// [决策理由] 网络错误必须返回检测管线，由其按失败开放策略记录并放行。
	if err != nil {
		return LLMEvaluationResult{}, fmt.Errorf("调用大模型: %w", err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxLLMResponseBytes+1)
	responseBody, err := io.ReadAll(limited)
	// [决策理由] 响应读取失败时不能解析部分 JSON。
	if err != nil {
		return LLMEvaluationResult{}, fmt.Errorf("读取大模型响应: %w", err)
	}
	// [决策理由] 限制响应体可防止异常供应商耗尽机器人内存。
	if len(responseBody) > maxLLMResponseBytes {
		return LLMEvaluationResult{}, fmt.Errorf("大模型响应超过1MiB")
	}
	// [决策理由] 非2xx响应不可信，且错误体可能含敏感内容，不回显到日志链路。
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return LLMEvaluationResult{}, fmt.Errorf("大模型返回HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	// [决策理由] 缺少标准choices结构时无法取得模型JSON输出。
	if err := json.Unmarshal(responseBody, &envelope); err != nil || len(envelope.Choices) == 0 {
		return LLMEvaluationResult{}, fmt.Errorf("解析大模型响应结构失败")
	}
	result, err := DecodeLLMEvaluationResult([]byte(envelope.Choices[0].Message.Content))

	// >>> 数据演变示例
	// 1. choices[0].content含High/90/block JSON -> 严格解码 -> 研判结果。
	// 2. HTTP500或unknown字段 -> error -> 管线转人工复核。
	return result, err
}

const llmSystemPrompt = `你是QQ群广告与引流审核器。仅输出JSON对象，字段必须为risk_level、total_score、reason、violations、suggested_action。检查硬广告引流、紧迫性施压、利益诱导、身份伪装和内容真伪。risk_level只能是High/Medium/Low/Safe；High必须对应block，Medium必须对应manual_review，Low或Safe必须对应pass。violations只输出待审文本中实际出现的具体风险短词，不要输出抽象分类标签。不要输出Markdown。`
