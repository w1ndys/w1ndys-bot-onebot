// 📌 影响范围：纯内存验证文本试判字段和分流；不访问 QQ、数据库或真实大模型。
package forbiddenmessagemonitor

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/w1ndys/w1ndys-bot/internal/management"
)

func TestTextTestResourceUsesWeightedRulesWithoutActions(t *testing.T) {
	instance := testImplementation()
	actions := instance.actions.(*fakeActions)
	repository := instance.repository.(*fakeMonitorRepository)
	config := EngineConfig{ViolationThreshold: 60, WeightedKeywords: []WeightedKeyword{{Text: "固定广告", Weight: 80}}}
	engine, err := NewEngine(config)
	if err != nil {
		t.Fatal(err)
	}
	instance.snapshot.Store(&runtimeSnapshot{engine: engine, engineConfig: config, llmTimeout: time.Second, llmMaxConcurrency: 2, llmDailyRequestLimit: 500, minLLMMessageLength: 30})
	record, err := (&textTestResourceHandler{owner: instance}).Create(context.Background(), management.Actor{}, json.RawMessage(`{"text":"固 定、广\n告[CQ:image,file=a.jpg]"}`))
	if err != nil {
		t.Fatal(err)
	}
	var result textTestResult
	if err := json.Unmarshal(record.Data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Decision != "违规" || result.Stage != "weighted_score" || result.SuggestedAction != "block" {
		t.Fatalf("result = %+v", result)
	}
	if result.CleanedText != "固定广告" || result.CleanedLength != 4 || result.LocalScore != 80 || !reflect.DeepEqual(result.MatchedTerms, []string{"固定广告"}) {
		t.Fatalf("cleaned result = %+v", result)
	}
	if len(actions.banParams) != 0 || len(actions.historyParams) != 0 || len(actions.deleted) != 0 || len(repository.created) != 0 {
		t.Fatalf("bans=%d history=%d deleted=%d audits=%d", len(actions.banParams), len(actions.historyParams), len(actions.deleted), len(repository.created))
	}
}

func TestTextTestResourceRejectsInvalidPayload(t *testing.T) {
	handler := &textTestResourceHandler{owner: testImplementation()}
	tests := []json.RawMessage{json.RawMessage(`{"text":""}`), json.RawMessage(`{"text":"x","extra":true}`), json.RawMessage(`{"text":1}`)}
	for _, raw := range tests {
		_, err := handler.Create(context.Background(), management.Actor{}, raw)
		if !errors.Is(err, management.ErrInvalidResourceData) {
			t.Fatalf("Create(%s) error=%v", raw, err)
		}
	}
}

func TestTextTestResourceShortZeroScorePassesWithoutLLM(t *testing.T) {
	instance := testImplementation()
	evaluator := &fakeLLMEvaluator{result: LLMEvaluationResult{RiskLevel: "Safe", TotalScore: 0, Reason: "普通交流", Violations: []string{}, SuggestedAction: "pass"}}
	current := instance.snapshot.Load()
	instance.snapshot.Store(&runtimeSnapshot{engine: current.engine, engineConfig: current.engineConfig, evaluator: evaluator, llmTimeout: time.Second, llmMaxConcurrency: 2, llmDailyRequestLimit: 500, minLLMMessageLength: 30})
	record, err := (&textTestResourceHandler{owner: instance}).Create(context.Background(), management.Actor{}, json.RawMessage(`{"text":"普通、短消息"}`))
	if err != nil || evaluator.calls != 0 {
		t.Fatalf("Create() error=%v calls=%d", err, evaluator.calls)
	}
	var result textTestResult
	if err := json.Unmarshal(record.Data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Stage != "llm_length_filter" || result.LLMUsed || result.CleanedText != "普通短消息" || result.CleanedLength != 5 || result.LocalScore != 0 || len(result.MatchedTerms) != 0 {
		t.Fatalf("result=%+v", result)
	}
}

func TestTextTestResourceLongZeroScoreUsesLLMWithCleanedText(t *testing.T) {
	instance := testImplementation()
	evaluator := &fakeLLMEvaluator{result: LLMEvaluationResult{RiskLevel: "Low", TotalScore: 8, Reason: "未发现违规", Violations: []string{}, SuggestedAction: "pass"}}
	current := instance.snapshot.Load()
	instance.snapshot.Store(&runtimeSnapshot{engine: current.engine, engineConfig: current.engineConfig, evaluator: evaluator, llmTimeout: time.Second, llmMaxConcurrency: 2, llmDailyRequestLimit: 500, minLLMMessageLength: 8})
	raw := json.RawMessage(`{"text":"全 新、模式\n可能没有入库[CQ:image,file=secret.jpg]"}`)
	record, err := (&textTestResourceHandler{owner: instance}).Create(context.Background(), management.Actor{}, raw)
	if err != nil {
		t.Fatal(err)
	}
	var result textTestResult
	if err := json.Unmarshal(record.Data, &result); err != nil {
		t.Fatal(err)
	}
	if evaluator.calls != 1 || len(evaluator.requests) != 1 || evaluator.requests[0].Message != "全新模式可能没有入库" {
		t.Fatalf("calls=%d requests=%+v", evaluator.calls, evaluator.requests)
	}
	if result.Stage != "llm" || !result.LLMUsed || result.CleanedText != "全新模式可能没有入库" || result.CleanedLength != 10 || result.LocalScore != 0 || len(result.MatchedTerms) != 0 {
		t.Fatalf("result=%+v", result)
	}
}
