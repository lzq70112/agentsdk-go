package toolbuiltin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lzq70112/agentsdk-go/pkg/tool"
)

// fakeAsker 是记录型提问原语：按脚本返回答案/错误。
type fakeAsker struct {
	got    tool.Question
	answer string
	err    error
}

func (f *fakeAsker) Ask(_ context.Context, q tool.Question) (string, error) {
	f.got = q
	if f.err != nil {
		return "", f.err
	}
	return f.answer, nil
}

// TestAskUserQuestionReturnsAnswer 固化答案路径：工具输出携带答案文本，
// 供回合继续时模型消费。
func TestAskUserQuestionReturnsAnswer(t *testing.T) {
	asker := &fakeAsker{answer: "B"}
	toolImpl := NewAskUserQuestionTool()
	result, err := toolImpl.Execute(tool.WithQuestionAsker(context.Background(), asker), map[string]interface{}{
		"question": "选哪个方案？",
		"options":  []interface{}{"A", "B"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.Success || result.Output != "用户回答：B" {
		t.Fatalf("结果不符: %+v", result)
	}
	if asker.got.Question != "选哪个方案？" || len(asker.got.Options) != 2 {
		t.Fatalf("问题透传不符: %+v", asker.got)
	}
}

// TestAskUserQuestionTimeoutNote 固化超时路径：超时是"成功结果 + 超时说明"，
// 模型据此自行继续，回合不悬挂。
func TestAskUserQuestionTimeoutNote(t *testing.T) {
	asker := &fakeAsker{err: tool.ErrQuestionTimeout}
	result, err := NewAskUserQuestionTool().Execute(
		tool.WithQuestionAsker(context.Background(), asker),
		map[string]interface{}{"question": "确认？", "timeout_seconds": float64(30)},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.Success {
		t.Fatalf("超时应为成功结果: %+v", result)
	}
	if !strings.Contains(result.Output, "30 秒") || !strings.Contains(result.Output, "超时") {
		t.Fatalf("超时说明应包含时长: %q", result.Output)
	}
	if asker.got.Timeout != 30*time.Second {
		t.Fatalf("超时参数透传不符: %v", asker.got.Timeout)
	}
}

// TestAskUserQuestionUnavailable 固化无提问原语时的降级：明确报错提示而非
// panic，模型可自行决策继续。
func TestAskUserQuestionUnavailable(t *testing.T) {
	result, err := NewAskUserQuestionTool().Execute(context.Background(), map[string]interface{}{
		"question": "确认？",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Success || !strings.Contains(result.Output, "提问通道不可用") {
		t.Fatalf("应明确提示通道不可用: %+v", result)
	}
}

// TestAskUserQuestionCanceled 固化取消路径：ctx 取消时错误上抛，由运行时
// 按取消语义收尾。
func TestAskUserQuestionCanceled(t *testing.T) {
	asker := &fakeAsker{err: context.Canceled}
	_, err := NewAskUserQuestionTool().Execute(
		tool.WithQuestionAsker(context.Background(), asker),
		map[string]interface{}{"question": "确认？"},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消错误应上抛, 得到 %v", err)
	}
}

// TestAskUserQuestionParams 固化入参校验。
func TestAskUserQuestionParams(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]interface{}
	}{
		{"缺 question", map[string]interface{}{}},
		{"question 类型错误", map[string]interface{}{"question": 1}},
		{"options 类型错误", map[string]interface{}{"question": "q", "options": "A"}},
		{"options 元素类型错误", map[string]interface{}{"question": "q", "options": []interface{}{1}}},
		{"timeout_seconds 非法", map[string]interface{}{"question": "q", "timeout_seconds": "x"}},
		{"timeout_seconds 非正", map[string]interface{}{"question": "q", "timeout_seconds": float64(0)}},
	}
	for _, tc := range cases {
		if _, _, _, err := parseAskQuestionParams(tc.params); err == nil {
			t.Errorf("%s: 期望报错", tc.name)
		}
	}
}
