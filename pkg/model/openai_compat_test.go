package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openai/openai-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeOpenAIBaseURL(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty URL returns empty",
			input:    "",
			expected: "",
		},
		{
			name:     "root URL without trailing slash gets v1 slash",
			input:    "https://llm.hyc.com",
			expected: "https://llm.hyc.com/v1/",
		},
		{
			name:     "root URL with trailing slash gets v1 slash",
			input:    "https://llm.hyc.com/",
			expected: "https://llm.hyc.com/v1/",
		},
		{
			name:     "v1 URL without trailing slash gets trailing slash",
			input:    "https://llm.hyc.com/v1",
			expected: "https://llm.hyc.com/v1/",
		},
		{
			name:     "v1 URL with trailing slash stays unchanged",
			input:    "https://llm.hyc.com/v1/",
			expected: "https://llm.hyc.com/v1/",
		},
		{
			name:     "whitespace is trimmed",
			input:    "  https://llm.hyc.com  ",
			expected: "https://llm.hyc.com/v1/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeOpenAIBaseURL(tt.input)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestBuildOpenAIAssistantMessage_ToolCallsWithoutContent(t *testing.T) {
	msg := Message{
		Role: "assistant",
		ToolCalls: []ToolCall{
			{ID: "call_1", Name: "get_weather", Arguments: map[string]any{"location": "Tokyo"}},
		},
	}

	param := buildOpenAIAssistantMessage(msg)
	assistant := param.OfAssistant

	assert.NotNil(t, assistant)
	assert.Len(t, assistant.ToolCalls, 1)
	// Some OpenAI-compatible models (GLM/Qwen/Kimi) reject assistant messages
	// that simultaneously carry content and tool_calls. Empty content should
	// remain empty rather than being padded with a zero-width space.
	if assistant.Content.OfString.Valid() {
		assert.Empty(t, assistant.Content.OfString.Or(""))
	}
}

func TestBuildOpenAIAssistantMessage_NoToolCallsWithEmptyContent(t *testing.T) {
	msg := Message{
		Role:    "assistant",
		Content: "",
	}

	param := buildOpenAIAssistantMessage(msg)
	assistant := param.OfAssistant

	assert.NotNil(t, assistant)
	assert.Empty(t, assistant.ToolCalls)
	// When there are no tool calls, a zero-width space prevents models from
	// rejecting a completely empty assistant message.
	assert.True(t, assistant.Content.OfString.Valid())
	assert.Equal(t, "\u200b", assistant.Content.OfString.Or(""))
}

func TestBuildOpenAIAssistantMessage_ToolCallsWithContent(t *testing.T) {
	msg := Message{
		Role:    "assistant",
		Content: "Let me check that for you.",
		ToolCalls: []ToolCall{
			{ID: "call_1", Name: "get_weather", Arguments: map[string]any{"location": "Tokyo"}},
		},
	}

	param := buildOpenAIAssistantMessage(msg)
	assistant := param.OfAssistant

	assert.NotNil(t, assistant)
	assert.Len(t, assistant.ToolCalls, 1)
	assert.True(t, assistant.Content.OfString.Valid())
	assert.Equal(t, "Let me check that for you.", assistant.Content.OfString.Or(""))
}

func TestConvertToolsToOpenAI_DropsIncompatibleFields(t *testing.T) {
	tools := []ToolDefinition{
		{
			Name:        "get_weather",
			Description: "Get weather",
			Parameters: map[string]any{
				"type":                 "object",
				"strict":               true,
				"additionalProperties": false,
				"properties": map[string]any{
					"location": map[string]any{"type": "string"},
				},
				"required": []string{"location"},
			},
		},
	}

	result := convertToolsToOpenAI(tools)
	require.Len(t, result, 1)
	params := result[0].Function.Parameters

	// GLM/Qwen/Kimi OpenAI-compatible endpoints often reject strict mode and
	// explicit additionalProperties: false in function schemas.
	assert.Equal(t, "object", params["type"])
	assert.NotContains(t, params, "strict")
	assert.NotContains(t, params, "additionalProperties")
	assert.Contains(t, params, "properties")
	assert.Contains(t, params, "required")
}

func TestOpenAIProvider_SendsRequestsToV1ChatCompletions(t *testing.T) {
	var receivedPath string
	var receivedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &receivedBody))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		require.NoError(t, json.NewEncoder(w).Encode(openai.ChatCompletion{
			ID:    "chatcmpl-test",
			Model: "glm-5.2",
			Choices: []openai.ChatCompletionChoice{
				{
					Index:        0,
					FinishReason: "stop",
					Message: openai.ChatCompletionMessage{
						Role:    "assistant",
						Content: "Hello from test server",
					},
				},
			},
		}))
	}))
	defer srv.Close()

	mdl, err := NewOpenAI(OpenAIConfig{
		APIKey:     "sk-test",
		BaseURL:    srv.URL, // normalizeOpenAIBaseURL appends /v1/
		Model:      "glm-5.2",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	_, err = mdl.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	require.NoError(t, err)

	// BaseURL normalization must produce /v1/chat/completions, not /chat/completions.
	assert.Equal(t, "/v1/chat/completions", receivedPath)
	assert.Equal(t, "glm-5.2", receivedBody["model"])
}

func TestOpenAIProvider_AssistantMessageWithoutContentAndWithToolCalls(t *testing.T) {
	var receivedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &receivedBody))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		require.NoError(t, json.NewEncoder(w).Encode(openai.ChatCompletion{
			ID: "chatcmpl-test",
			Choices: []openai.ChatCompletionChoice{
				{
					FinishReason: "tool_calls",
					Message: openai.ChatCompletionMessage{
						Role: "assistant",
						ToolCalls: []openai.ChatCompletionMessageToolCall{
							{
								ID: "call_1",
								Function: openai.ChatCompletionMessageToolCallFunction{
									Name:      "get_weather",
									Arguments: `{"location":"Tokyo"}`,
								},
							},
						},
					},
				},
			},
		}))
	}))
	defer srv.Close()

	mdl, err := NewOpenAI(OpenAIConfig{
		APIKey:     "sk-test",
		BaseURL:    srv.URL,
		Model:      "glm-5.2",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	_, err = mdl.Complete(context.Background(), Request{
		Messages: []Message{
			{Role: "user", Content: "weather"},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{
					{ID: "call_1", Name: "get_weather", Arguments: map[string]any{"location": "Tokyo"}},
				},
			},
			{
				Role: "tool",
				ToolCalls: []ToolCall{
					{ID: "call_1", Result: "sunny"},
				},
			},
		},
	})
	require.NoError(t, err)

	messages, ok := receivedBody["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 3)

	assistantMsg, ok := messages[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "assistant", assistantMsg["role"])
	assert.Len(t, assistantMsg["tool_calls"], 1)
	// Empty content should not be padded when tool_calls are present.
	assert.Empty(t, assistantMsg["content"])
}

func TestOpenAIProvider_SystemMessagesAlwaysFirst(t *testing.T) {
	var receivedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &receivedBody))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		require.NoError(t, json.NewEncoder(w).Encode(openai.ChatCompletion{
			ID: "chatcmpl-test",
			Choices: []openai.ChatCompletionChoice{
				{
					FinishReason: "stop",
					Message:      openai.ChatCompletionMessage{Role: "assistant", Content: "ok"},
				},
			},
		}))
	}))
	defer srv.Close()

	mdl, err := NewOpenAI(OpenAIConfig{
		APIKey:     "sk-test",
		BaseURL:    srv.URL,
		Model:      "glm-5.2",
		System:     "default system",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	_, err = mdl.Complete(context.Background(), Request{
		System: "request system",
		Messages: []Message{
			{Role: "user", Content: "hi"},
			{Role: "system", Content: "inline system"},
			{Role: "assistant", Content: "hello"},
		},
	})
	require.NoError(t, err)

	messages, ok := receivedBody["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 3)

	// All system messages must precede any user/assistant messages. Some
	// OpenAI-compatible endpoints (GLM/Qwen/Kimi/MiniMax) reject requests where
	// a system message appears after a user or assistant message. To keep the
	// request compatible, multiple system contents are merged into a single
	// system message at the beginning.
	assert.Equal(t, "system", getMessageRole(messages[0]))
	assert.Equal(t, "user", getMessageRole(messages[1]))
	assert.Equal(t, "assistant", getMessageRole(messages[2]))
}

func getMessageRole(msg any) string {
	m, ok := msg.(map[string]any)
	if !ok {
		return ""
	}
	role, _ := m["role"].(string)
	return role
}

func TestOpenAIProvider_FunctionParametersDropIncompatibleFields(t *testing.T) {
	var receivedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &receivedBody))

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		require.NoError(t, json.NewEncoder(w).Encode(openai.ChatCompletion{
			ID: "chatcmpl-test",
			Choices: []openai.ChatCompletionChoice{
				{
					FinishReason: "stop",
					Message:      openai.ChatCompletionMessage{Role: "assistant", Content: "ok"},
				},
			},
		}))
	}))
	defer srv.Close()

	mdl, err := NewOpenAI(OpenAIConfig{
		APIKey:     "sk-test",
		BaseURL:    srv.URL,
		Model:      "glm-5.2",
		HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	_, err = mdl.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools: []ToolDefinition{
			{
				Name:        "get_weather",
				Description: "Get weather",
				Parameters: map[string]any{
					"type":                 "object",
					"strict":               true,
					"additionalProperties": false,
					"properties": map[string]any{
						"location": map[string]any{"type": "string"},
					},
					"required": []string{"location"},
				},
			},
		},
	})
	require.NoError(t, err)

	tools, ok := receivedBody["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 1)

	function, ok := tools[0].(map[string]any)["function"].(map[string]any)
	require.True(t, ok)
	params, ok := function["parameters"].(map[string]any)
	require.True(t, ok)

	assert.Equal(t, "object", params["type"])
	assert.NotContains(t, params, "strict")
	assert.NotContains(t, params, "additionalProperties")
	assert.Contains(t, params, "properties")
	assert.Contains(t, params, "required")
}
