// 📌 影响范围：验证违禁消息清洗、单一正权重评分与严格 LLM 协议；无外部变量。
package forbiddenmessagemonitor

import (
	"math"
	"reflect"
	"testing"
)

func TestNormalizeDetectionText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{name: "删除全部 CQ 段", message: "[CQ:image,file=secret.jpg]广 [CQ:at,qq=123] 告\n[CQ:record,file=a.amr]！", want: "广告"},
		{name: "拼接规避字符", message: "免 费\n加、群！ＡＢＣ-123", want: "免费加群ａｂｃ123"},
		{name: "Unicode 与小写", message: "ПрИвЕт，世界 １２", want: "привет世界１２"},
		{name: "仅媒体", message: "[CQ:image,file=a.jpg][CQ:video,file=b.mp4] 👍！", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := NormalizeDetectionText(test.message); got != test.want {
				t.Fatalf("NormalizeDetectionText(%q) = %q, want %q", test.message, got, test.want)
			}
		})
	}
}

func TestIsValidSpeech(t *testing.T) {
	t.Parallel()
	tests := []struct {
		message string
		want    bool
	}{
		{message: "资料", want: true},
		{message: "行", want: true},
		{message: "嗯", want: false},
		{message: "👍！！！", want: false},
		{message: "[CQ:face,id=123]", want: false},
		{message: "Привет", want: true},
	}
	for _, test := range tests {
		if got := IsValidSpeech(test.message); got != test.want {
			t.Errorf("IsValidSpeech(%q) = %v, want %v", test.message, got, test.want)
		}
	}
}

func TestNewEngineRejectsUnsafeConfig(t *testing.T) {
	t.Parallel()
	tests := []EngineConfig{
		{ViolationThreshold: 0},
		{ViolationThreshold: 60, WeightedKeywords: []WeightedKeyword{{Text: "风险", Weight: math.NaN()}}},
		{ViolationThreshold: 60, WeightedKeywords: []WeightedKeyword{{Text: "风险", Weight: -1}}},
	}
	for _, config := range tests {
		if _, err := NewEngine(config); err == nil {
			t.Fatalf("NewEngine(%+v) unexpectedly succeeded", config)
		}
	}
}

func TestEngineSinglePositiveWeightScoring(t *testing.T) {
	t.Parallel()
	config := EngineConfig{
		ViolationThreshold: 60,
		WeightedKeywords: []WeightedKeyword{
			{Text: "广 告", Weight: 15},
			{Text: "广告", Weight: 35},
			{Text: "引流", Weight: 25},
		},
	}
	engine, err := NewEngine(config)
	if err != nil {
		t.Fatal(err)
	}

	// 同一词重复出现只累计一次；规范化后等价的词条只采用最大权重。
	repeated := engine.Score("广、告 广告 广\n告")
	if repeated.Score != 35 || repeated.Band != RiskBandReview || !reflect.DeepEqual(repeated.MatchedTerms, []string{"广告"}) {
		t.Fatalf("repeated score = %+v", repeated)
	}

	// 空白、换行和标点不能用于绕过拼接识别。
	high := engine.Score("广 、 告\n引！流")
	if high.Score != 60 || high.Band != RiskBandHigh || !reflect.DeepEqual(high.MatchedTerms, []string{"广告", "引流"}) {
		t.Fatalf("high score = %+v", high)
	}

	zero := engine.Score("普通交流[CQ:image,file=ad.jpg]")
	if zero.Score != 0 || zero.Band != RiskBandReview || len(zero.MatchedTerms) != 0 {
		t.Fatalf("zero score = %+v", zero)
	}
}

func TestDecodeLLMEvaluationResult(t *testing.T) {
	t.Parallel()
	valid := []byte(`{"risk_level":"High","total_score":91,"reason":"引流","violations":["微信"],"suggested_action":"block"}`)
	result, err := DecodeLLMEvaluationResult(valid)
	if err != nil || result.TotalScore != 91 {
		t.Fatalf("valid result = %#v, error = %v", result, err)
	}
	invalidPayloads := [][]byte{
		[]byte(`{"risk_level":"high","total_score":91,"reason":"x","violations":[],"suggested_action":"block"}`),
		[]byte(`{"risk_level":"Safe","total_score":101,"reason":"x","violations":[],"suggested_action":"pass"}`),
		[]byte(`{"risk_level":"Safe","total_score":1,"reason":"x","violations":[],"suggested_action":"pass","unknown":true}`),
		[]byte(`{"risk_level":"Safe","total_score":1,"reason":"x","violations":[],"suggested_action":"pass"} {}`),
		[]byte(`{"risk_level":"High","total_score":90,"reason":"x","violations":["x"],"suggested_action":"pass"}`),
	}
	for _, payload := range invalidPayloads {
		if _, decodeErr := DecodeLLMEvaluationResult(payload); decodeErr == nil {
			t.Fatalf("DecodeLLMEvaluationResult(%s) unexpectedly succeeded", payload)
		}
	}
}
