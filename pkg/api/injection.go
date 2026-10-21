package api

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lzq70112/agentsdk-go/pkg/message"
)

// InjectionKind 显式枚举注入来源。switch 分发处（Inject 的 kind 校验、
// 消费侧转发）必须显式列出全部登记类型，default 拒绝——新增 kind 必须
// 加分支，防止未登记类型绕过路由语义。
type InjectionKind string

const (
	InjectionKindUser           InjectionKind = "user"
	InjectionKindExternal       InjectionKind = "external"
	InjectionKindScheduled      InjectionKind = "scheduled"
	InjectionKindSubagentResult InjectionKind = "subagent_result"
	InjectionKindReminder       InjectionKind = "reminder"
)

// BarrierPolicy 约定会话忙（该会话有在途回合）时注入的处置语义。
type BarrierPolicy int

const (
	// BarrierDefer 排队，当前回合结束后写入 history（下一轮模型可见）。
	BarrierDefer BarrierPolicy = iota
	// BarrierReject 拒绝并回报 ErrSessionBusy，由调用方决定如何告知来源。
	BarrierReject
	// BarrierDrop 静默丢弃，适用于可合并、可再生的提醒类注入。
	BarrierDrop
)

// ErrSessionBusy 是 BarrierReject 在会话忙时返回的错误。
var ErrSessionBusy = errors.New("api: session busy, injection rejected")

// Injection 描述一次框架向会话 history 的注入。
type Injection struct {
	Kind    InjectionKind
	Policy  BarrierPolicy
	Message message.Message
}

// Inject 是框架往会话里塞回合消息的唯一入口。所有跨 goroutine 的注入源
// （子 agent 完成回注、定时任务、提醒引擎、宿主外部事件）都必须经此路由，
// 禁止直连 history.Append——防多个注入源并发写坏会话历史。
//
// 非 user kind 的消息强制携带 synthetic 标记（缺失时补齐），与压缩豁免
// （compact_preserve.go）联动：注入的环境事实永不压缩。
func (rt *Runtime) Inject(sessionID string, in Injection) error {
	if rt == nil {
		return errors.New("api: runtime is nil")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return errors.New("api: injection requires session id")
	}
	switch in.Kind {
	case InjectionKindUser, InjectionKindExternal, InjectionKindScheduled,
		InjectionKindSubagentResult, InjectionKindReminder:
	default:
		return fmt.Errorf("api: unknown injection kind %q", in.Kind)
	}
	// 非 user kind 的消息强制携带 synthetic 标记（缺失时补齐），与压缩豁免
	// （compact_preserve.go）联动：注入的环境事实永不压缩。
	// 全部注入消息统一打时间戳（与 runLoop 的 user/assistant Append 一致），
	// 使注入的环境事实在历史里也有可渲染的时间位置。
	if in.Message.Metadata == nil {
		in.Message.Metadata = map[string]any{}
	}
	in.Message.Metadata[metadataTimestamp] = time.Now().UnixMilli()
	if in.Kind != InjectionKindUser && !isSyntheticMessage(in.Message) {
		in.Message.Metadata[metadataSynthetic] = true
		in.Message.Metadata[metadataSyntheticKey] = string(in.Kind)
	}

	rt.activeMu.Lock()
	busy := rt.activeRuns[sessionID] != nil
	rt.activeMu.Unlock()
	if busy {
		switch in.Policy {
		case BarrierDrop:
			return nil
		case BarrierReject:
			return ErrSessionBusy
		default:
			rt.injectMu.Lock()
			if rt.injectQueues == nil {
				rt.injectQueues = make(map[string][]Injection)
			}
			rt.injectQueues[sessionID] = append(rt.injectQueues[sessionID], in)
			rt.injectMu.Unlock()
			return nil
		}
	}

	history, ok := rt.histories.Loaded(sessionID)
	if !ok {
		return fmt.Errorf("api: session %q history not loaded", sessionID)
	}
	history.Append(in.Message)
	return nil
}

// drainDeferredInjections 取出会话上 DEFER 排队的注入并按序写入 history。
// 由回合结束（endActiveRun）调用：注销在途回合后队列整体落盘。
func (rt *Runtime) drainDeferredInjections(sessionID string) {
	if rt == nil {
		return
	}
	rt.injectMu.Lock()
	queued := rt.injectQueues[sessionID]
	delete(rt.injectQueues, sessionID)
	rt.injectMu.Unlock()
	if len(queued) == 0 {
		return
	}
	history, ok := rt.histories.Loaded(sessionID)
	if !ok {
		return
	}
	for _, in := range queued {
		history.Append(in.Message)
	}
}
