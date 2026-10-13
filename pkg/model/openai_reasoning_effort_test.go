package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ReasoningEffort 必须能从配置透传到请求参数；模型对思考深度的控制依赖它。
func TestOpenAIModelBuildParamsReasoningEffortFromConfig(t *testing.T) {
	m := &openaiModel{
		model:           "gpt-4o",
		maxTokens:       100,
		maxRetries:      1,
		reasoningEffort: "medium",
	}

	params := m.buildParams(Request{})
	if string(params.ReasoningEffort) != "medium" {
		t.Fatalf("reasoning_effort=%q, want medium", params.ReasoningEffort)
	}
}

// Request 层显式指定的等级优先于配置默认值，便于单次调用覆盖。
func TestOpenAIModelBuildParamsReasoningEffortRequestOverridesConfig(t *testing.T) {
	m := &openaiModel{
		model:           "gpt-4o",
		maxTokens:       100,
		maxRetries:      1,
		reasoningEffort: "medium",
	}

	params := m.buildParams(Request{ReasoningEffort: "low"})
	if string(params.ReasoningEffort) != "low" {
		t.Fatalf("reasoning_effort=%q, want low", params.ReasoningEffort)
	}
}

// 未配置等级时不发送该参数，保持模型自身默认行为（向后兼容）。
func TestOpenAIModelBuildParamsReasoningEffortOmittedWhenEmpty(t *testing.T) {
	m := &openaiModel{
		model:      "gpt-4o",
		maxTokens:  100,
		maxRetries: 1,
	}

	params := m.buildParams(Request{})
	if params.ReasoningEffort != "" {
		t.Fatalf("reasoning_effort=%q, want empty", params.ReasoningEffort)
	}
}

// Provider 字段必须完整传递到底层 model，否则配置会静默失效。
func TestOpenAIProviderPassesReasoningEffort(t *testing.T) {
	p := &OpenAIProvider{
		APIKey:          "key",
		ModelName:       "gpt-4o",
		ReasoningEffort: "high",
	}

	mdl, err := p.Model(context.Background())
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	om, ok := mdl.(*openaiModel)
	if !ok {
		t.Fatalf("expected *openaiModel, got %T", mdl)
	}
	if om.reasoningEffort != "high" {
		t.Fatalf("reasoningEffort=%q, want high", om.reasoningEffort)
	}
}

// 真实请求体断言：配置了等级时 reasoning_effort 必须出现在 HTTP 请求里，
// 未配置时必须完全不出现（不能发空串，避免不兼容的网关报错）。
func TestOpenAIModelReasoningEffortWireFormat(t *testing.T) {
	cases := []struct {
		name    string
		effort  string
		wantSet bool
	}{
		{name: "configured", effort: "high", wantSet: true},
		{name: "unconfigured", effort: "", wantSet: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, "read body", http.StatusBadRequest)
					return
				}
				if err := json.Unmarshal(body, &got); err != nil {
					http.Error(w, "decode body", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			}))
			defer srv.Close()

			mdl, err := NewOpenAI(OpenAIConfig{
				APIKey:          "key",
				BaseURL:         srv.URL,
				Model:           "gpt-4o",
				ReasoningEffort: tc.effort,
			})
			if err != nil {
				t.Fatalf("NewOpenAI: %v", err)
			}
			if _, err := mdl.Complete(context.Background(), Request{}); err != nil {
				t.Fatalf("Complete: %v", err)
			}

			v, present := got["reasoning_effort"]
			if tc.wantSet && (!present || v != tc.effort) {
				t.Fatalf("reasoning_effort in body = %v (present=%v), want %q", v, present, tc.effort)
			}
			if !tc.wantSet && present {
				t.Fatalf("reasoning_effort must be absent, got %v", v)
			}
		})
	}
}
