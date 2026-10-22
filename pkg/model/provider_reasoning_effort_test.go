package model

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestOpenAIProviderSetReasoningEffortRebuildsModel 守住热更契约：档位变更
// 后下一次 Model() 必须以新档位重建实例（缓存失效），运行中的回合才能用到
// 新档位——Runtime 参数页的"立即生效"依赖它。
func TestOpenAIProviderSetReasoningEffortRebuildsModel(t *testing.T) {
	p := &OpenAIProvider{APIKey: "k", ModelName: "gpt-test", ReasoningEffort: "high", CacheTTL: time.Minute}
	ctx := context.Background()

	m1, err := p.Model(ctx)
	if err != nil {
		t.Fatalf("Model: %v", err)
	}
	if got := m1.(*openaiModel).reasoningEffort; got != "high" {
		t.Fatalf("初始档位应从配置快照（high），实际 %q", got)
	}

	p.SetReasoningEffort("low")
	m2, err := p.Model(ctx)
	if err != nil {
		t.Fatalf("Model after set: %v", err)
	}
	if m1 == m2 {
		t.Fatal("热更后必须重建 Model 实例（缓存应失效）")
	}
	if got := m2.(*openaiModel).reasoningEffort; got != "low" {
		t.Fatalf("重建实例应用新档位（low），实际 %q", got)
	}

	// 空串 = 回到模型默认（buildParams 不下发 effort 参数）。
	p.SetReasoningEffort("")
	m3, err := p.Model(ctx)
	if err != nil {
		t.Fatalf("Model after clear: %v", err)
	}
	if got := m3.(*openaiModel).reasoningEffort; got != "" {
		t.Fatalf("清空应回模型默认（空），实际 %q", got)
	}
}

// TestOpenAIProviderSetReasoningEffortConcurrent 固化并发安全：热更与回合
// 构建模型并发进行不得构成 data race（-race 下跑）。
func TestOpenAIProviderSetReasoningEffortConcurrent(t *testing.T) {
	p := &OpenAIProvider{APIKey: "k", ModelName: "gpt-test", ReasoningEffort: "medium", CacheTTL: time.Minute}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = p.Model(context.Background())
		}()
		go func(n int) {
			defer wg.Done()
			p.SetReasoningEffort([]string{"low", "medium", "high", ""}[n%4])
		}(i)
	}
	wg.Wait()
}
