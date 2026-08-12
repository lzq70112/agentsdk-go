package api

import (
	"errors"

	"github.com/stellarlinkco/agentsdk-go/pkg/model"
)

var (
	ErrTokenBudgetExceeded = errors.New("api: token budget exceeded")
	ErrDiminishingReturns  = errors.New("api: diminishing returns detected")
)

const (
	defaultDiminishingWindow    = 3
	defaultDiminishingThreshold = 50
)

type TokenBudgetConfig struct {
	MaxTokens            int `json:"max_tokens"`
	DiminishingWindow    int `json:"diminishing_window"`
	DiminishingThreshold int `json:"diminishing_threshold"`
}

func (c TokenBudgetConfig) withDefaults() TokenBudgetConfig {
	cfg := c
	if cfg.DiminishingWindow > 0 && cfg.DiminishingThreshold <= 0 {
		cfg.DiminishingThreshold = defaultDiminishingThreshold
	}
	if cfg.DiminishingThreshold > 0 && cfg.DiminishingWindow <= 0 {
		cfg.DiminishingWindow = defaultDiminishingWindow
	}
	return cfg
}

type tokenBudgetTracker struct {
	cfg         TokenBudgetConfig
	totalTokens int
	outputs     []int
}

func newTokenBudgetTracker(cfg TokenBudgetConfig) *tokenBudgetTracker {
	cfg = cfg.withDefaults()
	if cfg.MaxTokens <= 0 && (cfg.DiminishingWindow <= 0 || cfg.DiminishingThreshold <= 0) {
		return nil
	}
	return &tokenBudgetTracker{cfg: cfg}
}

func (t *tokenBudgetTracker) Observe(usage model.Usage) error {
	if t == nil {
		return nil
	}

	t.totalTokens += usageTotal(usage)
	if t.cfg.MaxTokens > 0 && t.totalTokens > t.cfg.MaxTokens {
		return ErrTokenBudgetExceeded
	}

	if t.cfg.DiminishingWindow <= 0 || t.cfg.DiminishingThreshold <= 0 {
		return nil
	}

	t.outputs = append(t.outputs, usage.OutputTokens)
	if len(t.outputs) < t.cfg.DiminishingWindow {
		return nil
	}
	window := t.outputs[len(t.outputs)-t.cfg.DiminishingWindow:]
	for _, output := range window {
		if output >= t.cfg.DiminishingThreshold {
			return nil
		}
	}
	return ErrDiminishingReturns
}

func usageTotal(usage model.Usage) int {
	if usage.TotalTokens > 0 {
		return usage.TotalTokens
	}
	return usage.InputTokens + usage.OutputTokens
}
