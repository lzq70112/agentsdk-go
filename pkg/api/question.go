package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lzq70112/agentsdk-go/pkg/tool"
	toolbuiltin "github.com/lzq70112/agentsdk-go/pkg/tool/builtin"
)

// QuestionInfo 是待答问题的宿主可见快照：宿主（IM 互动卡片、Admin UI）凭
// ID 作答，凭 SessionID 定位来源会话。
type QuestionInfo struct {
	ID        string        `json:"id"`
	SessionID string        `json:"session_id"`
	Question  string        `json:"question"`
	Options   []string      `json:"options,omitempty"`
	AskedAt   time.Time     `json:"asked_at"`
	Timeout   time.Duration `json:"timeout"`
}

// pendingQuestion 是 registry 内的待答问题：answered 缓冲 1，作答方投递后
// 无需等待挂起方收取。
type pendingQuestion struct {
	info     QuestionInfo
	answered chan string
}

// questionRegistry 维护在答问题。同一会话同一时刻至多一个待答问题
// （回合在工具上挂起时会话忙，不会有并发提问），但以问题 ID 为键路由，
// 会话维度查询仅供宿主恢复 UI 状态。
type questionRegistry struct {
	mu      sync.Mutex
	pending map[string]*pendingQuestion
	seq     int
}

func newQuestionRegistry() *questionRegistry {
	return &questionRegistry{pending: map[string]*pendingQuestion{}}
}

func (r *questionRegistry) register(sessionID string, q tool.Question) *pendingQuestion {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	p := &pendingQuestion{
		info: QuestionInfo{
			ID:        fmt.Sprintf("q-%d", r.seq),
			SessionID: sessionID,
			Question:  q.Question,
			Options:   append([]string(nil), q.Options...),
			AskedAt:   time.Now(),
			Timeout:   q.Timeout,
		},
		answered: make(chan string, 1),
	}
	r.pending[p.info.ID] = p
	return p
}

// remove 幂等摘除：超时、取消、作答三条路径竞争时先到先得。
func (r *questionRegistry) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pending, id)
}

// answer 摘除并投递答案；问题不存在或已过期时报错。
func (r *questionRegistry) answer(id, answer string) error {
	r.mu.Lock()
	p, ok := r.pending[id]
	if ok {
		delete(r.pending, id)
	}
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("api: 问题 %s 不存在或已过期", id)
	}
	// answered 缓冲 1 且本方法是唯一投递点（投递前已摘除登记，同一问题
	// 不会二次投递），直接投递不会阻塞。
	p.answered <- answer
	return nil
}

func (r *questionRegistry) bySession(sessionID string) (QuestionInfo, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.pending {
		if p.info.SessionID == sessionID {
			return p.info, true
		}
	}
	return QuestionInfo{}, false
}

// questionAskerAdapter 把提问原语接到 runtime：登记待答问题 → 经事件流
// 管道推送 question_asked（emit 为 nil 时仅登记，宿主可经 PendingQuestion
// 轮询）→ 阻塞等待答案/超时/取消，退出路径全部清理登记。
type questionAskerAdapter struct {
	registry  *questionRegistry
	sessionID string
	emit      streamEmitFunc
}

func (a *questionAskerAdapter) Ask(ctx context.Context, q tool.Question) (string, error) {
	pending := a.registry.register(a.sessionID, q)
	if a.emit != nil {
		a.emit(ctx, StreamEvent{
			Type:      toolbuiltin.QuestionAskedEventType,
			Name:      "ask_user_question",
			Output:    pending.info,
			SessionID: a.sessionID,
		})
	}
	// 未显式限时的提问无限等待：nil channel 在 select 中永不就绪，无需起
	// 定时器分支。仅显式限时（Timeout > 0）的提问超时摘除并报错。
	var timer *time.Timer
	var timeoutC <-chan time.Time
	if pending.info.Timeout > 0 {
		timer = time.NewTimer(pending.info.Timeout)
		defer timer.Stop()
		timeoutC = timer.C
	}
	select {
	case answer := <-pending.answered:
		return answer, nil
	case <-timeoutC:
		a.registry.remove(pending.info.ID)
		return "", tool.ErrQuestionTimeout
	case <-ctx.Done():
		a.registry.remove(pending.info.ID)
		return "", ctx.Err()
	}
}

// AnswerQuestion 送达用户对指定问题的回答：挂起中的回合携带答案继续。
// 问题已超时/不存在时报错。
func (rt *Runtime) AnswerQuestion(questionID, answer string) error {
	if rt == nil || rt.questions == nil {
		return errors.New("api: runtime question registry unavailable")
	}
	questionID = strings.TrimSpace(questionID)
	if questionID == "" {
		return errors.New("api: question id is empty")
	}
	return rt.questions.answer(questionID, answer)
}

// PendingQuestion 返回会话当前待答问题（提问挂起期间会话忙，至多一个）；
// 无待答问题时返回 false。
func (rt *Runtime) PendingQuestion(sessionID string) (QuestionInfo, bool) {
	if rt == nil || rt.questions == nil {
		return QuestionInfo{}, false
	}
	return rt.questions.bySession(strings.TrimSpace(sessionID))
}
