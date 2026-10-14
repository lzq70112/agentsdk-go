package toolbuiltin

import (
	"sync"
	"time"
)

// TaskStore 是清单的权威状态源：互斥保护、全量覆盖写入、快照深拷贝外出。
// 每个注册 task 工具的 Runtime 持有一份，生命周期与 Runtime 一致。
type TaskStore struct {
	mu        sync.Mutex
	subject   string
	items     []TodoItem
	updatedAt time.Time
}

// Replace 全量覆盖清单内容并刷新更新时间，返回覆盖后的快照。
func (s *TaskStore) Replace(subject string, items []TodoItem) TodoSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subject = subject
	s.items = make([]TodoItem, len(items))
	copy(s.items, items)
	s.updatedAt = time.Now()
	return s.snapshotLocked()
}

// Snapshot 返回当前全量快照；items 深拷贝，调用方修改不影响 store 内部状态。
func (s *TaskStore) Snapshot() TodoSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *TaskStore) snapshotLocked() TodoSnapshot {
	items := make([]TodoItem, len(s.items))
	copy(items, s.items)
	return TodoSnapshot{Subject: s.subject, Items: items, UpdatedAt: s.updatedAt}
}
