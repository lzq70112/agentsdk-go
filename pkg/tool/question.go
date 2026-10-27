package tool

import (
	"context"
	"errors"
	"time"
)

// ErrQuestionTimeout 表示问题在显式限时的超时时间内未获回答。工具据此生成
// "超时收尾"的结果文本，让回合带着超时说明结束而不是悬挂。未显式限时的
// 提问永远等待，不会走到此错误。
var ErrQuestionTimeout = errors.New("tool: question timeout")

// Question 描述一次向用户的提问。Timeout <= 0 表示不限时，一直等待用户
// 回答；仅显式限时（如 timeout_seconds）的提问会在超时后返回 ErrQuestionTimeout。
type Question struct {
	ID       string   // 实现方生成，宿主作答时凭此路由
	Question string   // 问题文本
	Options  []string // 候选选项，可为空（自由文本回答）
	Timeout  time.Duration
}

// QuestionAsker 是提问挂起原语：实现方登记问题并阻塞等待答案。
// 约定：
//   - 问题登记后实现方负责把问题送达宿主（如经事件流推送 question_asked）；
//   - 收到答案返回答案文本；超时返回 ErrQuestionTimeout；调用方 ctx 取消时
//     返回 ctx.Err()；
//   - 无论哪条路径退出，登记的待答问题都必须被清理，不允许悬挂。
type QuestionAsker interface {
	Ask(ctx context.Context, q Question) (string, error)
}

type questionAskerCtxKey struct{}

// WithQuestionAsker 把提问原语挂到 ctx 上，供内建 ask_user_question 工具取用。
func WithQuestionAsker(ctx context.Context, asker QuestionAsker) context.Context {
	if ctx == nil || asker == nil {
		return ctx
	}
	return context.WithValue(ctx, questionAskerCtxKey{}, asker)
}

// QuestionAskerFromContext 返回 ctx 上的提问原语；未挂载时返回 nil。
func QuestionAskerFromContext(ctx context.Context) QuestionAsker {
	if ctx == nil {
		return nil
	}
	if asker, ok := ctx.Value(questionAskerCtxKey{}).(QuestionAsker); ok {
		return asker
	}
	return nil
}
