// 📌 影响范围：使用内存 HTTP Transport 验证违禁监控配置与模型协议；不访问真实供应商、QQ 或数据库。
package forbiddenmessagemonitor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/w1ndys/w1ndys-bot/internal/plugin"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestApplyConfigKeepsSnapshotWhenBuildFails(t *testing.T) {
	instance := &implementation{httpClient: &http.Client{}}
	oldConfig := DefaultEngineConfig()
	oldEngine, err := NewEngine(oldConfig)
	if err != nil {
		t.Fatal(err)
	}
	oldSnapshot := &runtimeSnapshot{engine: oldEngine, engineConfig: oldConfig, minLLMMessageLength: 30}
	instance.snapshot.Store(oldSnapshot)

	raw := json.RawMessage(`{"violation_weight_threshold":1001,"llm_enabled":false,"llm_endpoint":"","llm_model":"","llm_api_key":"","llm_timeout_seconds":20,"llm_max_concurrency":2,"llm_daily_request_limit":500,"min_llm_message_length":40}`)
	if err := instance.ApplyConfig(context.Background(), raw); err == nil {
		t.Fatal("ApplyConfig() accepted an out-of-range violation threshold")
	}
	if got := instance.snapshot.Load(); got != oldSnapshot || got.minLLMMessageLength != 30 {
		t.Fatalf("snapshot=%p old=%p minimum=%d", got, oldSnapshot, got.minLLMMessageLength)
	}
}

func TestDefaultRuntimeConfigBuildsWithoutLLM(t *testing.T) {
	instance := &implementation{httpClient: &http.Client{}}
	raw, err := plugin.NormalizeConfig(instance.ConfigSchema(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := buildRuntimeSnapshot(raw, Lexicon{}, instance.httpClient)
	if err != nil || snapshot.engine == nil || snapshot.evaluator != nil {
		t.Fatalf("buildRuntimeSnapshot() snapshot=%+v error=%v", snapshot, err)
	}
	if snapshot.minLLMMessageLength != 30 || snapshot.engineConfig.ViolationThreshold != 60 {
		t.Fatalf("snapshot defaults = %+v", snapshot)
	}
}

func TestOpenAICompatibleEvaluatorSendsOnlyCleanedMessage(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		} else if len(body.Messages) != 2 || body.Messages[1].Content != `{"message":"测试消息"}` {
			t.Errorf("messages = %+v", body.Messages)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"{\"risk_level\":\"High\",\"total_score\":90,\"reason\":\"引流\",\"violations\":[\"微信\"],\"suggested_action\":\"block\"}"}}]}`)),
		}, nil
	})}
	evaluator := &openAICompatibleEvaluator{endpoint: "https://llm.example/v1/chat/completions", model: "test-model", apiKey: "secret", client: client}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := evaluator.Evaluate(ctx, LLMEvaluationRequest{Message: "测试消息"})
	if err != nil || result.SuggestedAction != "block" || result.TotalScore != 90 {
		t.Fatalf("Evaluate() result=%+v error=%v", result, err)
	}
}

func TestRuntimeConfigRejectsInvalidLLMAndLexicon(t *testing.T) {
	base := pluginConfig{
		ViolationWeightThreshold: 60,
		LLMTimeoutSeconds:        20,
		LLMMaxConcurrency:        2,
		LLMDailyRequestLimit:     500,
		MinLLMMessageLength:      30,
	}
	raw, _ := json.Marshal(base)
	if _, err := buildRuntimeSnapshot(raw, Lexicon{Terms: []WeightedKeyword{{Text: "x", Weight: -1}}}, &http.Client{}); err == nil {
		t.Fatal("negative weighted keyword accepted")
	}

	base.LLMEnabled = true
	base.LLMEndpoint = "file:///tmp/model"
	base.LLMModel = "test"
	raw, _ = json.Marshal(base)
	if _, err := buildRuntimeSnapshot(raw, Lexicon{}, &http.Client{}); err == nil {
		t.Fatal("file LLM endpoint accepted")
	}
	base.LLMEndpoint = "http://llm.example/v1/chat/completions"
	raw, _ = json.Marshal(base)
	if _, err := buildRuntimeSnapshot(raw, Lexicon{}, &http.Client{}); err == nil {
		t.Fatal("remote HTTP LLM endpoint accepted")
	}
	base.LLMEndpoint = "http://127.0.0.1:11434/v1/chat/completions"
	raw, _ = json.Marshal(base)
	if _, err := buildRuntimeSnapshot(raw, Lexicon{}, &http.Client{}); err != nil {
		t.Fatalf("loopback HTTP LLM endpoint rejected: %v", err)
	}
}

func TestSecureLLMHTTPClientRejectsUnsafeRedirect(t *testing.T) {
	client := secureLLMHTTPClient(&http.Client{})
	unsafeRequest, _ := http.NewRequest(http.MethodGet, "http://llm.example/v1/chat/completions", nil)
	secureRequest, _ := http.NewRequest(http.MethodGet, "https://llm.example/v1/chat/completions", nil)
	if err := client.CheckRedirect(unsafeRequest, []*http.Request{secureRequest}); err == nil {
		t.Fatal("unsafe remote HTTP redirect accepted")
	}
	loopbackRequest, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:11434/v1/chat/completions", nil)
	if err := client.CheckRedirect(loopbackRequest, []*http.Request{loopbackRequest}); err != nil {
		t.Fatalf("loopback redirect rejected: %v", err)
	}
}
