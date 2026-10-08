// 📌 影响范围：定义群内违禁消息监控的纯内存加权词规则；不读取环境变量或外部服务。
package forbiddenmessagemonitor

import (
	"errors"
	"fmt"
	"math"
)

// RiskBand 表示本地累计权重是否达到直接处置阈值。
type RiskBand string

const (
	RiskBandReview RiskBand = "review"
	RiskBandHigh   RiskBand = "high"
)

// WeightedKeyword 定义违禁词及其正向风险权重。
type WeightedKeyword struct {
	Text   string
	Weight float64
}

// EngineConfig 定义纯内存词库与直接处置阈值。
type EngineConfig struct {
	WeightedKeywords   []WeightedKeyword
	ViolationThreshold float64
}

// DefaultEngineConfig 返回默认直接处置阈值。
func DefaultEngineConfig() EngineConfig {
	return EngineConfig{ViolationThreshold: 60}
}

// validate 校验阈值与权重，避免无法解释或无效的规则。
func (config EngineConfig) validate() error {
	if !finite(config.ViolationThreshold) || config.ViolationThreshold <= 0 || config.ViolationThreshold > maxTermWeight {
		return fmt.Errorf("violation threshold must be between 1 and %d", maxTermWeight)
	}
	for index, keyword := range config.WeightedKeywords {
		if keyword.Text == "" || !finite(keyword.Weight) || keyword.Weight <= 0 || keyword.Weight > maxTermWeight {
			return fmt.Errorf("keyword %d must have text and weight between 1 and %d", index, maxTermWeight)
		}
	}
	if len(config.WeightedKeywords) > 100000 {
		return errors.New("too many weighted keywords")
	}
	return nil
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
