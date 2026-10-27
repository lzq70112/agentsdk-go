package toolbuiltin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lzq70112/agentsdk-go/pkg/tool"
)

// AskUserQuestionName 是 ask_user_question 工具的注册名。
const AskUserQuestionName = "ask_user_question"

// QuestionAskedEventType 是提问挂起事件的协议类型，负载为 QuestionInfo。
// api 层在问题登记后经事件流管道透传给消费方（IM 互动卡片、Admin UI 等）。
const QuestionAskedEventType = "question_asked"

// AskUserQuestionTool 让 agent 在需要用户决策时发起提问并挂起等待回答。
// 提问即挂起：工具阻塞到收到答案为止，期间不执行后续工具；收到答案后回合
// 携带答案继续。提问不限时（除非调用方显式传 timeout_seconds），用户何时
// 回答都有效，答后回合即继续。
type AskUserQuestionTool struct{}

// NewAskUserQuestionTool 创建提问工具。
func NewAskUserQuestionTool() *AskUserQuestionTool {
	return &AskUserQuestionTool{}
}

// Name 返回 tool 的唯一标识。
func (t *AskUserQuestionTool) Name() string { return AskUserQuestionName }

// Description 返回给 LLM 看的 tool 说明。
func (t *AskUserQuestionTool) Description() string {
	return `向用户发起提问并等待回答。适合需要用户拍板的决策点：方案选择、
参数确认、授权确认等。调用后当前回合会挂起等待，用户回答后你将收到
答案文本并继续执行。提问不限时：用户无论过多久回答都有效，无需催促
或代为决策；仅当你显式传 timeout_seconds 时才会限时。
有明确候选时务必给出 options（界面会渲染为可点选的选项）。`
}

// Schema 描述工具入参。
func (t *AskUserQuestionTool) Schema() *tool.JSONSchema {
	return &tool.JSONSchema{
		Type: "object",
		Properties: map[string]interface{}{
			"question": map[string]interface{}{
				"type":        "string",
				"description": "要问用户的问题，一句话说清需要对方决策什么。",
			},
			"options": map[string]interface{}{
				"type":        "array",
				"description": "候选选项（可选）。给出 2-4 个互斥选项，用户也可自由文本回答。",
				"items": map[string]interface{}{
					"type": "string",
				},
			},
			"timeout_seconds": map[string]interface{}{
				"type":        "integer",
				"description": "等待回答的超时秒数（可选）。不传则不限时，一直等待用户回答。",
			},
		},
		Required: []string{"question"},
	}
}

// Execute 登记问题并阻塞等待答案。返回值三种路径：
//   - 收到答案：输出 "用户回答：<answer>"，回合携带答案继续；
//   - 超时（仅显式限时）：输出超时说明并带 EndTurn 收尾标记，回合就此结束
//     等待用户指示，不悬挂、不自主决策；
//   - ctx 取消（回合被取消等）：返回错误，由运行时按取消语义收尾。
func (t *AskUserQuestionTool) Execute(ctx context.Context, params map[string]interface{}) (*tool.ToolResult, error) {
	asker := tool.QuestionAskerFromContext(ctx)
	if asker == nil {
		return &tool.ToolResult{
			Success: false,
			Output:  "ask_user_question: 提问通道不可用（宿主未挂载应答原语），请基于已有信息自行决策。",
			Data:    map[string]any{"error": "question_bridge_unavailable"},
		}, nil
	}
	question, options, timeout, err := parseAskQuestionParams(params)
	if err != nil {
		return nil, err
	}
	answer, err := asker.Ask(ctx, tool.Question{
		Question: question,
		Options:  options,
		Timeout:  timeout,
	})
	if errors.Is(err, tool.ErrQuestionTimeout) {
		seconds := int(timeout / time.Second)
		return &tool.ToolResult{
			Success: true,
			EndTurn: true,
			Output: fmt.Sprintf("用户在 %d 秒内未回答（超时）。本回合到此结束：不做自主决策、不执行任何后续操作，"+
				"等待用户给出进一步指示（如回复「继续」）后再行动。", seconds),
			Data: map[string]any{"timeout": true},
		}, nil
	}
	if err != nil {
		return nil, err
	}
	return &tool.ToolResult{
		Success: true,
		Output:  "用户回答：" + answer,
		Data:    map[string]any{"answer": answer},
	}, nil
}

// parseAskQuestionParams 解析并校验入参：question 必填，options 可选字符串
// 数组，timeout_seconds 可选正整数。
func parseAskQuestionParams(params map[string]interface{}) (string, []string, time.Duration, error) {
	question := ""
	if v, ok := params["question"]; ok {
		s, ok := v.(string)
		if !ok {
			return "", nil, 0, fmt.Errorf("ask_user_question: question 必须是字符串")
		}
		question = strings.TrimSpace(s)
	}
	if question == "" {
		return "", nil, 0, fmt.Errorf("ask_user_question: 缺少必填参数 question")
	}

	var options []string
	if raw, ok := params["options"]; ok && raw != nil {
		list, ok := raw.([]interface{})
		if !ok {
			return "", nil, 0, fmt.Errorf("ask_user_question: options 必须是字符串数组")
		}
		options = make([]string, 0, len(list))
		for i, item := range list {
			s, ok := item.(string)
			if !ok {
				return "", nil, 0, fmt.Errorf("ask_user_question: options[%d] 必须是字符串", i)
			}
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				options = append(options, trimmed)
			}
		}
	}

	var timeout time.Duration
	if raw, ok := params["timeout_seconds"]; ok && raw != nil {
		seconds, ok := raw.(float64)
		if !ok || seconds <= 0 {
			return "", nil, 0, fmt.Errorf("ask_user_question: timeout_seconds 必须是正数")
		}
		timeout = time.Duration(seconds * float64(time.Second))
	}
	return question, options, timeout, nil
}
